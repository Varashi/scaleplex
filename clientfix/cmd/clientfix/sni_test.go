package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// genCert makes a self-signed cert for the given DNS names (test only).
func genCert(t *testing.T, names ...string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     names,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// TestParseSNI feeds a genuine Go ClientHello (for the given ServerName) into
// parseSNI and checks the host is recovered, incl. the no-SNI case.
func TestParseSNI(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"plex-svc.boeye.net", "plex-svc.boeye.net"},
		{"172-16-0-1.abc123.plex.direct", "172-16-0-1.abc123.plex.direct"},
		{"", ""}, // no SNI extension
	} {
		cli, srv := net.Pipe()
		go func() {
			_ = tls.Client(cli, &tls.Config{ServerName: tc.name, InsecureSkipVerify: true}).Handshake()
			_ = cli.Close()
		}()
		_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
		got := peekSNI(bufio.NewReaderSize(srv, 18*1024))
		_ = srv.Close()
		if got != tc.want {
			t.Errorf("SNI %q: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// startFront boots serveSNI on a random port and returns its address.
func startFront(t *testing.T, passAddr string, cert tls.Certificate, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // serveSNI re-listens; tiny race acceptable in test
	cr := &certReloader{cert: &cert}
	go func() {
		_ = serveSNI(addr, passAddr, ".boeye.net", cr.get, h)
	}()
	// wait for it to come up
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return addr
}

// TestSNITerminate: a *.boeye.net SNI is terminated with our cert and the
// request reaches the HTTP handler.
func TestSNITerminate(t *testing.T) {
	cert, pool := genCert(t, "*.boeye.net")
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "handled")
	})
	addr := startFront(t, "127.0.0.1:1", cert, h) // passAddr unused here

	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "plex-svc.boeye.net"}}
	c := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	u := (&url.URL{Scheme: "https", Host: addr, Path: "/x"}).String()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("terminate GET: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "handled" {
		t.Fatalf("terminate: got %d %q", resp.StatusCode, b)
	}
}

// TestSNIPassthrough: a non-.boeye.net SNI (plex.direct) is passed through to
// the upstream PMS, which completes TLS with its OWN cert (proving clientfix
// did not terminate it).
func TestSNIPassthrough(t *testing.T) {
	pmsCert, pmsPool := genCert(t, "node.plex.direct")
	pms, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pmsCert}})
	if err != nil {
		t.Fatal(err)
	}
	defer pms.Close()
	go func() {
		for {
			c, err := pms.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }() // echo
		}
	}()

	ourCert, _ := genCert(t, "*.boeye.net")
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	addr := startFront(t, pms.Addr().String(), ourCert, h)

	// Dial the front with a plex.direct SNI; expect PMS's cert, not ours.
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pmsPool, ServerName: "node.plex.direct"})
	if err != nil {
		t.Fatalf("passthrough dial: %v", err)
	}
	defer conn.Close()
	if cn := conn.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "node.plex.direct" {
		t.Fatalf("passthrough served wrong cert CN=%q (clientfix terminated instead of passing through)", cn)
	}
	// echo round-trip through the tunnel
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("passthrough echo: %q err=%v", buf, err)
	}
}

// TestSNIPlaintext: a non-TLS HTTP request still reaches the handler.
func TestSNIPlaintext(t *testing.T) {
	cert, _ := genCert(t, "*.boeye.net")
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "plain")
	})
	addr := startFront(t, "127.0.0.1:1", cert, h)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/x")
	if err != nil {
		t.Fatalf("plaintext GET: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "plain" {
		t.Fatalf("plaintext: got %d %q", resp.StatusCode, b)
	}
}
