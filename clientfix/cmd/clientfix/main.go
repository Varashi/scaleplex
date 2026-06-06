// scaleplex-clientfix — a transparent reverse proxy that fronts PMS and
// fixes per-client Plex bugs living in the client's
// X-Plex-Client-Profile-Extra negotiation. It STREAMS everything
// transparently to PMS, and only special-cases the transcode DECISION
// request for matched clients.
//
// It must front ALL of PMS:32400 (not just a gateway route) because most
// remote clients connect via Plex's plex.direct hashed-IP URL straight to
// the WAN port-forward → PMS, bypassing any HTTP gateway. So the LB /
// port-forward points here, and clientfix forwards to PMS.
//
// First (and currently only) rule — Plex for Apple TV 8.45:
//
//	The "Enhanced Player" forces HLS transcodes into container=mkv via
//	  add-transcode-target(...protocol=hls&container=mkv&replace=true)
//	The tvOS player demuxes a COPY / Direct-Stream of mkv-in-HLS fine, but
//	rejects an *encoder-produced* (re-encode) one → infinite buffer.
//	Confirmed byte-level: copy and re-encode produce byte-identical
//	Matroska framing; only the payload differs — a client demuxer bug,
//	fixed client-side in ATV 2025.31.x. See scaleplex#122.
//
// The fix must change PMS's container DECISION (the worker can't — PMS
// builds the served .m3u8 from its own decision), without disturbing the
// working copy path. So for the matched 8.45 decision request clientfix:
//
//  1. Forwards it to PMS UNCHANGED (mkv).
//  2. video=COPY (Direct Stream) → returns it verbatim (mkv copy plays).
//  3. video=TRANSCODE (re-encode) → re-issues with container=mkv→mp4. PMS
//     caches the fMP4 decision under the session GUID; the later
//     start.m3u8 (no profile-extra) replays it → 4K-HEVC fMP4, tvOS-OK.
//
// Fail-open: parse/re-issue errors return PMS's original response. Every
// other request — every other client, every other path, all media
// streaming — is a transparent streaming passthrough.
package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// main wires the proxy from env (CLIENTFIX_LISTEN_ADDR,
// CLIENTFIX_PMS_UPSTREAM, CLIENTFIX_UPSTREAM_TIMEOUT) and serves until
// killed. /healthz is an unconditional 200 for k8s probes.
func main() {
	listen := envOr("CLIENTFIX_LISTEN_ADDR", ":8080")
	upstreamRaw := os.Getenv("CLIENTFIX_PMS_UPSTREAM")
	if upstreamRaw == "" {
		log.Fatal("CLIENTFIX_PMS_UPSTREAM required (e.g. http://plex.plex.svc:32400)")
	}
	upstream, err := url.Parse(upstreamRaw)
	if err != nil {
		log.Fatalf("invalid CLIENTFIX_PMS_UPSTREAM %q: %v", upstreamRaw, err)
	}
	timeout := envDur("CLIENTFIX_UPSTREAM_TIMEOUT", 30*time.Second)
	// Decision mode for the matched ATV-8.45 re-encode:
	//   "strip" (default) — remove X-Plex-Client-Profile-Extra entirely so
	//     PMS falls back to the base tvOS profile (h264 1080p). Known-good
	//     for av1 sources whose Enhanced Player hevc/4K/mkv target is unplayable.
	//   anything else is a target CONTAINER — keep the client's hevc/4K/HDR
	//     caps and only swap the Enhanced Player's mkv target to that value:
	//       "mp4"    — mkv->mp4   (legacy #122; failed on real ATV 8.45)
	//       "mpegts" — mkv->mpegts (custom-profile ceiling test: does ATV
	//                  play hevc/4K in TS, restoring full quality?)
	mode := envOr("CLIENTFIX_DECISION_MODE", "strip")

	// Streaming transparent passthrough for everything that isn't the
	// matched decision request. FlushInterval -1 flushes immediately so
	// Plex's eventsource/notification streams aren't buffered; preserves
	// the client Host so PMS builds URLs exactly as for a direct hit.
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
		},
		FlushInterval: -1,
		ErrorLog:      log.Default(),
	}

	p := &proxy{
		upstream: upstream,
		rp:       rp,
		mode:     mode,
		// Used only for the small, buffered decision two-pass.
		client: &http.Client{
			Timeout:       timeout,
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
	log.Printf("scaleplex-clientfix fronting PMS %s, listening on %s (decision mode=%s)", upstream, listen, mode)
	log.Fatal(srv.ListenAndServe())
}

// proxy fronts a single PMS upstream: a streaming reverse proxy for all
// traffic, plus a buffered two-pass on the matched decision request.
type proxy struct {
	upstream *url.URL
	rp       *httputil.ReverseProxy
	client   *http.Client
	mode     string // "strip" (default) | "mp4"
}

// handle routes the matched Apple-TV-8.45 decision request through the
// content-aware two-pass; everything else streams straight to PMS.
func (p *proxy) handle(w http.ResponseWriter, r *http.Request) {
	if isDecision(r.URL.Path) && appleTV845(r.Header) {
		p.handleDecision(w, r)
		return
	}
	p.rp.ServeHTTP(w, r)
}

// handleDecision buffers the (small) decision exchange: forward unchanged,
// and only re-issue with the container rewritten when PMS decided a
// re-encode. Copy decisions and any error pass the original through.
func (p *proxy) handleDecision(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()

	resp1, b1, err := p.forward(r.Method, r.URL.Path, r.URL.RawQuery, r.Header, r.Host, body)
	if err != nil {
		log.Printf("upstream error on %s: %v", r.URL.Path, err)
		http.Error(w, "scaleplex-clientfix: upstream error", http.StatusBadGateway)
		return
	}

	if videoIsTranscode(b1) {
		var hdr2 http.Header
		var q2, action string
		if p.mode == "strip" {
			hdr2, q2, action = stripProfileExtraHeader(r.Header), stripProfileExtraQuery(r.URL.RawQuery), "stripped Client-Profile-Extra → base profile"
		} else {
			// container-rewrite mode: p.mode is the target container — keep
			// the client's hevc/4K/HDR caps, swap only the mkv target.
			hdr2, q2, action = rewriteContainer(r.Header, p.mode), rewriteRawQuery(r.URL.RawQuery, p.mode), "rewrote container mkv→"+p.mode
		}
		resp2, b2, err := p.forward(r.Method, r.URL.Path, q2, hdr2, r.Host, body)
		if err == nil {
			log.Printf("ATV-8.45 re-encode → %s (%d→%d B)", action, len(b1), len(b2))
			writeResponse(w, resp2, b2)
			return
		}
		log.Printf("ATV-8.45 re-issue failed, passing original through: %v", err)
	} else {
		log.Printf("ATV-8.45 decision = copy/direct-stream → passthrough")
	}
	writeResponse(w, resp1, b1)
}

// forward sends one buffered request to PMS and returns the drained
// response. Accept-Encoding is stripped so PMS replies uncompressed (easy
// to parse); the client Host is preserved.
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
			continue
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

// appleTV845 is the rule's client matcher. Kept here (not only at the
// network layer) so the proxy is self-contained + safe.
func appleTV845(h http.Header) bool {
	return h.Get("X-Plex-Product") == "Plex for Apple TV" &&
		strings.HasPrefix(h.Get("X-Plex-Version"), "8.45")
}

// videoIsTranscode reports whether PMS's MDE decision re-encodes the video
// (decision="transcode" on the selected video stream) vs copies it (Direct
// Stream remux, decision="copy"). Only re-encodes hit the ATV bug. Parse
// failure → false (fail-open: leave the request untouched).
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

// rewriteContainer returns a copy of the headers with container=mkv swapped
// to the target container in X-Plex-Client-Profile-Extra, keeping the rest
// of the profile (hevc/4K/HDR caps). The Apple-TV profile carries exactly
// one such token (the protocol=hls video add-transcode-target).
func rewriteContainer(h http.Header, target string) http.Header {
	out := h.Clone()
	if pe := out.Get("X-Plex-Client-Profile-Extra"); pe != "" {
		out.Set("X-Plex-Client-Profile-Extra", strings.ReplaceAll(pe, "container=mkv", "container="+target))
	}
	return out
}

// stripProfileExtraHeader returns a copy of the headers with the
// X-Plex-Client-Profile-Extra header removed, so PMS falls back to the
// client's base device profile (tvOS.xml → h264 1080p) instead of the 8.45
// Enhanced Player's hevc/4K/mkv add-transcode-target.
func stripProfileExtraHeader(h http.Header) http.Header {
	out := h.Clone()
	out.Del("X-Plex-Client-Profile-Extra")
	return out
}

// profileExtraQueryRe matches the X-Plex-Client-Profile-Extra query param.
// Its value is URL-encoded, so it never contains a raw '&'.
var profileExtraQueryRe = regexp.MustCompile(`(^|&)X-Plex-Client-Profile-Extra=[^&]*`)

// stripProfileExtraQuery removes the X-Plex-Client-Profile-Extra param from
// the raw query, leaving every other param byte-identical. Plex carries the
// profile-extra in the query as well as the header — a gateway filter can
// only touch the header, which is why the strip must live in this L7 proxy.
func stripProfileExtraQuery(q string) string {
	return strings.TrimPrefix(profileExtraQueryRe.ReplaceAllString(q, ""), "&")
}

// rewriteRawQuery applies the same container rewrite to the query string,
// in case a client carries it there rather than as a header.
func rewriteRawQuery(q, target string) string {
	q = strings.ReplaceAll(q, "container=mkv", "container="+target)
	q = strings.ReplaceAll(q, "container%3Dmkv", "container%3D"+target)
	q = strings.ReplaceAll(q, "container%3dmkv", "container%3d"+target)
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
