// scaleplex-relay — minimal HTTP forward proxy that runs as a sidecar
// on the PMS pod. Listens on LOCAL_RELAY_PORT (default 32499) and
// proxies every request to http://127.0.0.1:PMS_PORT. Source IP at the
// upstream becomes 127.0.0.1 because the proxy connects from the same
// pod's loopback — which is what makes Plex's progress endpoint accept
// the request (clusterplex uses an nginx mod for the same purpose).
//
// No path rewrites, no auth, no header tweaks — Plex Transcoder's
// `-progressurl` already includes a session token in the URL path that
// PMS validates.

package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"
)

func main() {
	listenPort, _ := strconv.Atoi(envOr("LOCAL_RELAY_PORT", "32499"))
	pmsPort, _ := strconv.Atoi(envOr("PMS_PORT", "32400"))
	if listenPort == pmsPort {
		log.Fatalf("LOCAL_RELAY_PORT (%d) must differ from PMS_PORT (%d)", listenPort, pmsPort)
	}

	upstream, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(pmsPort))
	rp := httputil.NewSingleHostReverseProxy(upstream)
	rp.ErrorLog = log.New(os.Stderr, "[relay] proxy: ", log.LstdFlags)
	// Long Plex requests (e.g. /header sometimes blocks until ready)
	// — uncap the response time.
	rp.Transport = &http.Transport{
		ResponseHeaderTimeout: 0,
		IdleConnTimeout:       90 * time.Second,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/", rp)

	addr := ":" + strconv.Itoa(listenPort)
	log.Printf("scaleplex-relay listening on %s → http://127.0.0.1:%d", addr, pmsPort)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// No timeouts on read/write — transcode segment uploads can be
		// arbitrarily long.
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}

func envOr(k, dflt string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return dflt
}
