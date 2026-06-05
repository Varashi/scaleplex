// scaleplex-clientfix — a tiny content-aware reverse proxy that sits in
// front of PMS's transcode DECISION endpoint to work around per-client
// Plex bugs that live entirely in the client's
// X-Plex-Client-Profile-Extra negotiation.
//
// First (and currently only) rule — Plex for Apple TV 8.45:
//
//	The "Enhanced Player" forces HLS transcodes into container=mkv via
//	  add-transcode-target(...protocol=hls&container=mkv&replace=true)
//	The tvOS player demuxes a COPY / Direct-Stream of mkv-in-HLS fine
//	(Dennis watches whole Optimize-version episodes that way), but
//	rejects an *encoder-produced* (re-encode) mkv-in-HLS stream →
//	infinite buffer. Confirmed byte-level: copy and re-encode produce
//	byte-identical Matroska container framing; only the payload differs,
//	so this is a client demuxer bug, fixed client-side in ATV 2025.31.x.
//	See forums.plex.tv/t/.../933250 and Varashi/scaleplex#122.
//
// The fix must change PMS's container DECISION — the worker can't, because
// PMS builds the served .m3u8 from its own decision (rewriter.go:3157).
// And it must NOT disturb the working copy path. So clientfix is
// content-aware:
//
//  1. Forward the client's decision request to PMS UNCHANGED (mkv).
//  2. If PMS decided the video stream = COPY (Direct Stream) → return it
//     verbatim. The mkv copy plays; nothing changes.
//  3. If PMS decided the video stream = TRANSCODE (re-encode, the broken
//     case) → re-issue the SAME request with container=mkv→mp4 in the
//     profile-extra. PMS caches the fMP4 decision under this session, and
//     the client's later start.m3u8 (which never carries a profile-extra)
//     replays it → 4K-HEVC fMP4, which tvOS plays. Validated byte-level:
//     container=mp4 + the real ATV profile yields `ftyp`/fMP4 segments;
//     the session-keyed re-issue overwrites the cached decision cleanly.
//
// Fail-open: any parse/re-issue error returns PMS's original response
// (status quo). The gateway routes ONLY the matched client's decision
// request here (Exact match on X-Plex-Product + X-Plex-Version); every
// other request, client, and the whole streaming hot path go straight to
// PMS and never touch this proxy.
package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// main wires up the proxy from env (CLIENTFIX_LISTEN_ADDR,
// CLIENTFIX_PMS_UPSTREAM, CLIENTFIX_UPSTREAM_TIMEOUT) and serves until
// killed. /healthz is an unconditional 200 for k8s probes.
func main() {
	listen := envOr("CLIENTFIX_LISTEN_ADDR", ":8080")
	upstreamRaw := os.Getenv("CLIENTFIX_PMS_UPSTREAM")
	if upstreamRaw == "" {
		log.Fatal("CLIENTFIX_PMS_UPSTREAM required (e.g. http://plex-test-pms.plex-test.svc:32400)")
	}
	upstream, err := url.Parse(upstreamRaw)
	if err != nil {
		log.Fatalf("invalid CLIENTFIX_PMS_UPSTREAM %q: %v", upstreamRaw, err)
	}
	timeout := envDur("CLIENTFIX_UPSTREAM_TIMEOUT", 30*time.Second)

	p := &proxy{
		upstream: upstream,
		client: &http.Client{
			Timeout: timeout,
			// We terminate + re-issue ourselves; never auto-follow Plex
			// redirects (would lose headers / the rewrite).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/", p.handle)

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("scaleplex-clientfix listening on %s → PMS %s", listen, upstream)
	log.Fatal(srv.ListenAndServe())
}

// proxy reverse-proxies requests to a single PMS upstream, applying the
// per-client decision fix where it matches.
type proxy struct {
	upstream *url.URL
	client   *http.Client
}

// handle implements the content-aware two-pass: forward unchanged, and
// only re-issue with the container rewritten when the matched client got
// a re-encode decision. Everything else is a transparent passthrough.
func (p *proxy) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()

	// Pass 1: forward the request to PMS exactly as the client sent it.
	resp1, b1, err := p.forward(r.Method, r.URL.Path, r.URL.RawQuery, r.Header, r.Host, body)
	if err != nil {
		// PMS unreachable — nothing works regardless; surface 502.
		log.Printf("upstream error on %s: %v", r.URL.Path, err)
		http.Error(w, "scaleplex-clientfix: upstream error", http.StatusBadGateway)
		return
	}

	// Only the Apple-TV-8.45 decision request is a candidate for the fix.
	// (The gateway already scopes routing here; this predicate keeps
	// clientfix correct + safe on its own if anything else reaches it.)
	if isDecision(r.URL.Path) && appleTV845(r.Header) && videoIsTranscode(b1) {
		hdr2 := rewriteContainer(r.Header)
		q2 := rewriteRawQuery(r.URL.RawQuery)
		resp2, b2, err := p.forward(r.Method, r.URL.Path, q2, hdr2, r.Host, body)
		if err == nil {
			log.Printf("ATV-8.45 re-encode → rewrote container mkv→mp4 (%s, %d→%d B)", r.URL.Path, len(b1), len(b2))
			writeResponse(w, resp2, b2)
			return
		}
		log.Printf("ATV-8.45 re-issue failed, passing original through: %v", err)
		// fail-open ↓
	} else if isDecision(r.URL.Path) && appleTV845(r.Header) {
		log.Printf("ATV-8.45 decision = copy/direct-stream → passthrough (%s)", r.URL.Path)
	}

	writeResponse(w, resp1, b1)
}

// forward sends one request to PMS and returns the (already-drained)
// response. Accept-Encoding is stripped so PMS replies uncompressed,
// making the decision XML trivial to parse; the original client Host is
// preserved so PMS sees an identical request to a direct call.
func (p *proxy) forward(method, path, rawQuery string, hdr http.Header, host string, body []byte) (*http.Response, []byte, error) {
	u := *p.upstream
	u.Path = path
	u.RawQuery = rawQuery
	req, err := http.NewRequest(method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Del("Accept-Encoding")
	req.Host = host
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return resp, b, nil
}

// writeResponse copies an upstream response (status, headers, body) to the
// client, dropping Content-Length so the writer recomputes it.
func writeResponse(w http.ResponseWriter, resp *http.Response, body []byte) {
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue // recomputed by the writer
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// isDecision reports whether the path is PMS's transcode decision endpoint.
func isDecision(path string) bool {
	return strings.Contains(path, "/transcode/universal/decision")
}

// appleTV845 is the rule's client matcher. Keep it here (not only at the
// gateway) so the proxy is self-contained + safe if mis-routed.
func appleTV845(h http.Header) bool {
	return h.Get("X-Plex-Product") == "Plex for Apple TV" &&
		strings.HasPrefix(h.Get("X-Plex-Version"), "8.45")
}

// videoIsTranscode reports whether PMS's MDE decision re-encodes the
// video (decision="transcode" on the selected video stream) vs copies it
// (Direct Stream remux, decision="copy"). Only re-encodes hit the ATV
// bug; copies play fine and must be left alone. Parse failure → false
// (fail-open: leave the request untouched).
func videoIsTranscode(xmlBody []byte) bool {
	var mc struct {
		Videos []struct {
			Medias []struct {
				Selected string `xml:"selected,attr"`
				Parts    []struct {
					Selected string `xml:"selected,attr"`
					Streams  []struct {
						StreamType string `xml:"streamType,attr"`
						Decision   string `xml:"decision,attr"`
					} `xml:"Stream"`
				} `xml:"Part"`
			} `xml:"Media"`
		} `xml:"Video"`
	}
	if err := xml.Unmarshal(xmlBody, &mc); err != nil {
		return false
	}
	for _, v := range mc.Videos {
		for _, m := range v.Medias {
			if m.Selected != "1" && len(v.Medias) > 1 {
				continue
			}
			for _, part := range m.Parts {
				if part.Selected != "1" && len(m.Parts) > 1 {
					continue
				}
				for _, s := range part.Streams {
					if s.StreamType == "1" && s.Decision == "transcode" {
						return true
					}
				}
			}
		}
	}
	return false
}

// rewriteContainer returns a copy of the headers with container=mkv→mp4
// in X-Plex-Client-Profile-Extra. The Apple-TV profile carries exactly
// one such token (the protocol=hls video add-transcode-target); other
// occurrences don't appear in this client's profile.
func rewriteContainer(h http.Header) http.Header {
	out := h.Clone()
	if pe := out.Get("X-Plex-Client-Profile-Extra"); pe != "" {
		out.Set("X-Plex-Client-Profile-Extra", strings.ReplaceAll(pe, "container=mkv", "container=mp4"))
	}
	return out
}

// rewriteRawQuery applies the same container rewrite to the query string,
// in case a client carries the profile-extra (or container) as a query
// param rather than a header. URL-encoded form is `container%3Dmkv`.
func rewriteRawQuery(q string) string {
	q = strings.ReplaceAll(q, "container=mkv", "container=mp4")
	q = strings.ReplaceAll(q, "container%3Dmkv", "container%3Dmp4")
	q = strings.ReplaceAll(q, "container%3dmkv", "container%3dmp4")
	return q
}

// envOr returns env var k, or def if unset/empty.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envDur parses env var k as a Go duration, or returns def.
func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
