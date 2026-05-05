// scaleplex-relay — minimal HTTP forward proxy that runs as a sidecar
// on the PMS pod. Listens on LOCAL_RELAY_PORT (default 32499) and
// proxies every request to http://127.0.0.1:PMS_PORT. Source IP at the
// upstream becomes 127.0.0.1 because the proxy connects from the same
// pod's loopback — which is what makes Plex's transcode-session
// endpoints accept the request without extra auth.
//
// One protocol fix: stock ffmpeg's `-progress <url>` POSTs key=value
// status, but Plex's progress handler is registered for PUT only and
// 404s on POST. So we translate POST → PUT for paths matching
// `^/video/:/transcode/session/.+/progress$` before forwarding. All
// other traffic passes through verbatim.
//
// Auth: ffmpeg `-progress` doesn't attach headers, so the rewriter on
// the worker side appends `?X-Plex-Token=$X_PLEX_TOKEN` to the URL —
// no extra work here.

package main

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"
)

var progressPathRE = regexp.MustCompile(`^/video/:/transcode/session/[^/]+/[^/]+/progress$`)

func main() {
	listenPort, _ := strconv.Atoi(envOr("LOCAL_RELAY_PORT", "32499"))
	pmsPort, _ := strconv.Atoi(envOr("PMS_PORT", "32400"))
	if listenPort == pmsPort {
		log.Fatalf("LOCAL_RELAY_PORT (%d) must differ from PMS_PORT (%d)", listenPort, pmsPort)
	}
	upstream := "http://127.0.0.1:" + strconv.Itoa(pmsPort)

	transport := &http.Transport{
		ResponseHeaderTimeout: 0,
		IdleConnTimeout:       90 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 0}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		// Plex's progress endpoint is PUT-only, but ffmpeg's `-progress`
		// only does POST. Promote the method when the path matches.
		if r.Method == http.MethodPost && progressPathRE.MatchString(r.URL.Path) {
			method = http.MethodPut
		}

		u, err := url.Parse(upstream)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		u.Path = r.URL.Path
		u.RawQuery = r.URL.RawQuery

		out, err := http.NewRequestWithContext(r.Context(), method, u.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// Forward original headers verbatim. Strip per-hop ones; PMS
		// reads X-Plex-Token from the URL query (the rewriter put it
		// there), no header injection needed.
		for k, vv := range r.Header {
			switch k {
			case "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
				continue
			}
			for _, v := range vv {
				out.Header.Add(k, v)
			}
		}

		resp, err := client.Do(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})

	addr := ":" + strconv.Itoa(listenPort)
	log.Printf("scaleplex-relay listening on %s → %s", addr, upstream)
	srv := &http.Server{Addr: addr, Handler: mux}
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
