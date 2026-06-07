// SNI-aware TCP front for clientfix.
//
// clientfix sits on the LB/port-forward hot path, which carries BOTH the
// plaintext-HTTP gateway-forwarded traffic AND raw TLS from remote clients
// and Plex's reachability probe (plex.direct is HTTPS). A plain http.Server
// can't terminate that TLS, so the probe fails → PMS flaps "remote access
// down". This front fixes that without needing Plex's own cert:
//
//	plaintext (non-TLS)            -> existing HTTP handler (decision rewrite)
//	TLS, SNI within termSuffix     -> terminate with OUR wildcard cert, then
//	                                  the existing HTTP handler (rewrite over
//	                                  HTTPS for clients using our custom URL)
//	TLS, any other SNI (plex.direct, no SNI) -> raw TCP passthrough to PMS,
//	                                  which answers with its own real
//	                                  plex.direct cert. The probe + plex.direct
//	                                  clients work unchanged; clientfix just
//	                                  can't see/rewrite those (acceptable —
//	                                  the ATV fix applies via the custom URL).
//
// Enabled only when CLIENTFIX_TLS_CERT/KEY are set; otherwise main keeps the
// original plain-HTTP listener (backward compatible, e.g. behind the gateway).
package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// serveSNI listens on listenAddr and dispatches each connection by peeking
// the first byte (and, for TLS, the ClientHello SNI). It runs until the
// listener errors. termSuffix is matched case-insensitively with a leading
// dot (e.g. ".boeye.net"); passAddr is the host:port PMS serves TLS on.
func serveSNI(listenAddr, passAddr, termSuffix string, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), h http.Handler) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	// Terminated-TLS and plaintext conns are fed to a stdlib http.Server via
	// a channel-backed listener so the existing handler is reused verbatim.
	hl := newChanListener(ln.Addr())
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(hl) }()

	tlsCfg := &tls.Config{
		GetCertificate: getCert,
		// Force HTTP/1.1 over terminated TLS — the handler is HTTP/1.1 and
		// h2 needs ServeTLS/h2 wiring we deliberately avoid here. plex.direct
		// h2 is unaffected (that path is raw passthrough, ALPN end-to-end).
		NextProtos: []string{"http/1.1"},
	}
	termSuffix = strings.ToLower(termSuffix)
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go dispatch(c, passAddr, termSuffix, tlsCfg, hl)
	}
}

// dispatch peeks one connection and routes it. On any peek error the conn is
// closed; on any SNI ambiguity it defaults to passthrough (safest — PMS sees
// the original TLS and serves its real cert).
func dispatch(c net.Conn, passAddr, termSuffix string, tlsCfg *tls.Config, hl *chanListener) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	// Buffer must hold a full ClientHello record (up to ~16 KB).
	br := bufio.NewReaderSize(c, 18*1024)
	first, err := br.Peek(1)
	if err != nil {
		_ = c.Close()
		return
	}
	pc := &peekedConn{Conn: c, r: br}
	_ = c.SetReadDeadline(time.Time{}) // clear; handlers manage their own timeouts

	if first[0] != 0x16 { // not a TLS handshake → plaintext HTTP
		hl.push(pc)
		return
	}

	sni := peekSNI(br)
	if termSuffix != "" && sni != "" && strings.HasSuffix(strings.ToLower(sni), termSuffix) {
		// Our domain: terminate TLS with our wildcard cert, hand the
		// decrypted conn to the HTTP server (handshake happens lazily on
		// first read inside http.Server).
		hl.push(tls.Server(pc, tlsCfg))
		return
	}
	// plex.direct / unknown / no SNI → transparent passthrough to PMS.
	passthrough(pc, passAddr)
}

// peekSNI returns the SNI host from a buffered TLS ClientHello without
// consuming any bytes (Peek only), or "" if absent/malformed.
func peekSNI(br *bufio.Reader) string {
	hdr, err := br.Peek(5)
	if err != nil || hdr[0] != 0x16 {
		return ""
	}
	recLen := int(hdr[3])<<8 | int(hdr[4])
	if recLen <= 0 || recLen > 17*1024 {
		return ""
	}
	rec, err := br.Peek(5 + recLen)
	if err != nil {
		return ""
	}
	return parseSNI(rec[5:])
}

// parseSNI parses the SNI host_name from a TLS ClientHello handshake message
// (the bytes after the 5-byte record header). Returns "" on any malformation
// — every length is bounds-checked so a crafted hello can't panic or overrun.
func parseSNI(hs []byte) string {
	// handshake: type(1)=ClientHello + len(3) + version(2) + random(32) = 38
	if len(hs) < 38 || hs[0] != 0x01 {
		return ""
	}
	i := 38
	// session_id
	if i >= len(hs) {
		return ""
	}
	i += 1 + int(hs[i])
	// cipher_suites
	if i+2 > len(hs) {
		return ""
	}
	i += 2 + (int(hs[i])<<8 | int(hs[i+1]))
	// compression_methods
	if i+1 > len(hs) {
		return ""
	}
	i += 1 + int(hs[i])
	// extensions
	if i+2 > len(hs) {
		return ""
	}
	end := i + 2 + (int(hs[i])<<8 | int(hs[i+1]))
	i += 2
	if end > len(hs) {
		end = len(hs)
	}
	for i+4 <= end {
		etype := int(hs[i])<<8 | int(hs[i+1])
		elen := int(hs[i+2])<<8 | int(hs[i+3])
		i += 4
		if i+elen > end {
			return ""
		}
		if etype == 0x0000 { // server_name
			ext := hs[i : i+elen]
			// server_name_list: list_len(2) then entries name_type(1)+len(2)+name
			j := 2
			for j+3 <= len(ext) {
				nt := ext[j]
				nl := int(ext[j+1])<<8 | int(ext[j+2])
				j += 3
				if j+nl > len(ext) {
					return ""
				}
				if nt == 0 { // host_name
					return string(ext[j : j+nl])
				}
				j += nl
			}
			return ""
		}
		i += elen
	}
	return ""
}

// passthrough opens a raw TCP tunnel to PMS and copies bytes both ways. The
// buffered ClientHello replays through pc, so PMS sees the original TLS
// stream and completes the handshake with its own cert.
func passthrough(pc net.Conn, passAddr string) {
	defer pc.Close()
	up, err := net.DialTimeout("tcp", passAddr, 10*time.Second)
	if err != nil {
		log.Printf("passthrough dial %s: %v", passAddr, err)
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, pc); done <- struct{}{} }()
	go func() { _, _ = io.Copy(pc, up); done <- struct{}{} }()
	<-done // first half-close ends the tunnel; defers close both
}

// peekedConn replays bytes buffered by a bufio.Reader (from Peek) before
// reading from the underlying conn — so a consumer (http.Server, tls.Server,
// io.Copy) sees the original byte stream intact.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// chanListener is a net.Listener whose Accept yields conns pushed onto it,
// letting a stdlib http.Server consume connections we've already classified.
type chanListener struct {
	ch     chan net.Conn
	addr   net.Addr
	once   sync.Once
	closed chan struct{}
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{ch: make(chan net.Conn), addr: addr, closed: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

func (l *chanListener) push(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.closed:
		_ = c.Close()
	}
}

// certReloader serves a TLS cert from disk and reloads it periodically, so a
// cert-manager renewal of the mounted secret is picked up without a restart.
type certReloader struct {
	certFile, keyFile string
	mu                sync.RWMutex
	cert              *tls.Certificate
}

func (cr *certReloader) load() error {
	c, err := tls.LoadX509KeyPair(cr.certFile, cr.keyFile)
	if err != nil {
		return err
	}
	cr.mu.Lock()
	cr.cert = &c
	cr.mu.Unlock()
	return nil
}

func (cr *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	if cr.cert == nil {
		return nil, errors.New("clientfix: no TLS cert loaded")
	}
	return cr.cert, nil
}

// watch reloads the cert every d until the process exits.
func (cr *certReloader) watch(d time.Duration) {
	for range time.Tick(d) {
		if err := cr.load(); err != nil {
			log.Printf("clientfix: tls cert reload failed: %v", err)
		}
	}
}
