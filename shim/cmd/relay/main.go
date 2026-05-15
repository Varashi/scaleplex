// scaleplex-relay — minimal HTTP forward proxy that runs as a sidecar
// on the PMS pod. Listens on LOCAL_RELAY_PORT (default 32499) and
// proxies every request to http://127.0.0.1:PMS_PORT. Source IP at the
// upstream becomes 127.0.0.1 because the proxy connects from the same
// pod's loopback — which is what makes Plex's transcode-session
// endpoints accept the request without extra auth.
//
// Two protocol fixes:
//
//  1. Stock ffmpeg's `-progress <url>` POSTs key=value status, but Plex's
//     progress handler is registered for PUT only and 404s on POST. We
//     translate POST → PUT for `^/video/:/transcode/session/.+/progress$`.
//
//  2. HLS segment_list CSV rewrite for seek sessions. Plex's ssegment
//     fork writes CSV entries with start_time on the *global* timeline
//     (e.g. chunk 111 → 888.0). Stock segment muxer can't be coaxed into
//     producing both proper splits AND global-time CSV at the same time
//     (verified locally: with `-copyts` PMS gets correct CSV but ffmpeg
//     never splits; without `-copyts` splits work but CSV is 0-based).
//     PMS reads CSV start_time and serves a 0-byte body whenever it
//     mismatches the chunk's expected window — so seek chunks return 200
//     with empty payloads and the player gets no frames. Fix: when the
//     worker rewrites the segment_list URL it appends
//     `?scaleplex_seg_time=<N>`; on the manifest POST path we strip that
//     param before forwarding and use it to rewrite each
//     `media-NNNNN.ts,start,end` row to
//     `media-NNNNN.ts,N*seg_time,(N+1)*seg_time`.
//
// Auth: ffmpeg `-progress` doesn't attach headers, so the rewriter on
// the worker side appends `?X-Plex-Token=$X_PLEX_TOKEN` to the URL —
// no extra work here.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"
)

var (
	progressPathRE = regexp.MustCompile(`^/video/:/transcode/session/[^/]+/[^/]+/progress$`)
	manifestPathRE = regexp.MustCompile(`^/video/:/transcode/session/[^/]+/[^/]+/manifest$`)
	csvRowRE       = regexp.MustCompile(`^(media-(\d+)\.ts),([0-9.]+),([0-9.]+)\s*$`)
)

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

		// HLS manifest CSV rewrite (see file header). The worker's rewriter
		// passes `scaleplex_seg_time=<N>` on the segment_list URL only for
		// HLS sessions; we strip it from the upstream query and use it to
		// rewrite the body rows. Non-HLS or non-manifest traffic skips this
		// branch and forwards the body unchanged.
		var bodyReader io.Reader = r.Body
		var contentLen int64 = r.ContentLength
		query := r.URL.Query()
		segTimeStr := query.Get("scaleplex_seg_time")
		if segTimeStr != "" {
			query.Del("scaleplex_seg_time")
			u.RawQuery = query.Encode()
		} else {
			u.RawQuery = r.URL.RawQuery
		}
		if r.Method == http.MethodPost && manifestPathRE.MatchString(r.URL.Path) && segTimeStr != "" {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				segTime, err := strconv.ParseFloat(segTimeStr, 64)
				if err == nil && segTime > 0 {
					body = rewriteSegmentListCSV(body, segTime)
				}
				bodyReader = bytes.NewReader(body)
				contentLen = int64(len(body))
			}
		}

		// Detach from r.Context() for manifest POSTs: ffmpeg's segment
		// muxer fire-and-forgets the body then closes its end of the
		// connection, which cancels r.Context() before we can finish
		// reading the body, patching chunks, and forwarding to PMS.
		// `client.Do` sees the canceled context and the forward never
		// reaches PMS. Use a fresh context with a generous timeout so
		// the forward survives ffmpeg's eager close.
		forwardCtx := r.Context()
		if manifestPathRE.MatchString(r.URL.Path) {
			var cancel context.CancelFunc
			forwardCtx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
		}
		out, err := http.NewRequestWithContext(forwardCtx, method, u.String(), bodyReader)
		if err != nil {
			log.Printf("relay forward NewRequest %s %s: %v", method, u.String(), err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if manifestPathRE.MatchString(r.URL.Path) {
			log.Printf("relay forward %s %s contentLen=%d", method, u.Path, contentLen)
		}
		if contentLen >= 0 && bodyReader != r.Body {
			out.ContentLength = contentLen
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
			log.Printf("relay forward client.Do %s %s: %v", method, u.Path, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if manifestPathRE.MatchString(r.URL.Path) {
			log.Printf("relay forward done %s %s -> %d", method, u.Path, resp.StatusCode)
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

// rewriteSegmentListCSV rewrites every `media-NNNNN.ts,start,end` row so
// start = NNNNN * segTime and end = (NNNNN+1) * segTime. PMS reads
// segment_list rows and returns 0-byte 200s when the timestamps don't
// match the chunk's expected playlist window (chunk N must land at
// N*segDur..(N+1)*segDur). Stock ffmpeg writes 0-based timestamps after
// the rewriter strips `-copyts` (which it must — copyts blocks splits).
// Non-matching lines pass through unchanged so headers and other rows
// the muxer might emit aren't mangled.
func rewriteSegmentListCSV(body []byte, segTime float64) []byte {
	lines := bytes.Split(body, []byte("\n"))
	for i, line := range lines {
		if len(line) == 0 {
			continue
		}
		m := csvRowRE.FindSubmatch(line)
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(string(m[2]))
		if err != nil {
			continue
		}
		start := float64(idx) * segTime
		end := float64(idx+1) * segTime
		lines[i] = []byte(fmt.Sprintf("%s,%.6f,%.6f", m[1], start, end))
	}
	return bytes.Join(lines, []byte("\n"))
}
