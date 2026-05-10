package main

// progress_report — drives Plex's /progress endpoint from the worker.
//
// Reverse-engineered from Plex Transcoder.real (musl ffmpeg fork in the
// PMS image, run offline against /sess/uuid/progress on a netcat sink
// 2026-05-05). Plex's progress wire format is NOT the same as stock
// ffmpeg's `-progress <http>` — Plex Transcoder PUTs query-string-only,
// empty-body messages of four shapes:
//
//   PUT <base>/streamDetail?index=N&id=M&codec=X&type=video|audio
//                          &profile=...&width=W&height=H&...   (per output stream)
//   PUT <base>?duration=<sec.frac>                              (once at start)
//   PUT <base>?width=W&height=H                                 (once after probe)
//   PUT <base>?progress=<pct>&size=<bytes>&remaining=<sec>&speed=<x>
//                                                              (every ~1s)
//
// PMS reads only the query string; the body is ignored. Without these
// PUTs PMS's /header handler sits ~125s waiting for codec/duration
// metadata before it falls back to disk-probing the init segment, which
// is exactly the stall scaleplex showed pre-fix.
//
// Wiring: ffmpeg writes its native `-progress` stream to a pipe we
// own; we parse it block-by-block and translate each block into a
// query-string PUT in Plex shape.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var metricProgressPUT = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "scaleplex_worker_progress_put_total",
	Help: "PUTs to Plex's /progress endpoint, labelled by kind and HTTP class.",
}, []string{"kind", "result"}) // kind: progress|streamDetail|duration|dimensions ; result: 2xx|3xx|4xx|5xx|err

// outputStream describes one of the output streams scaleplex needs to
// register with PMS via a streamDetail PUT.
type outputStream struct {
	Index      int     // -map index (0, 1, ...)
	ID         int     // Plex Transcoder always sends 0
	Codec      string  // av1 | h264 | hevc | aac | eac3 | ... (SOURCE codec)
	Type       string  // video | audio
	Width      int     // video only
	Height     int     // video only
	FrameRate  float64 // video only (e.g. 23.976)
	Profile    string  // video: Main | High | ...; audio: LC | HE-AAC | ...
	Level      int     // video only — h264/hevc level; PT hardcodes 5 for h264
	Channels   int     // audio only
	Layout     string  // audio only, e.g. "stereo" / "5.1"
	SampleRate int     // audio only (Hz)
	Language   string  // ISO-639 code or empty
}

// reportContext holds the per-session state the reporter needs.
type reportContext struct {
	URL        string
	Streams    []outputStream
	DurationS  float64 // total source duration in seconds (0 = unknown)
	SessionID  string
	Throttle   *throttleSignal // updated from progress PUT response bodies
}

// runProgressReporter consumes ffmpeg -progress output from r and PUTs
// each completed block to ctx.URL in Plex Transcoder format.
//
// Caller must have already issued the prelude PUTs (streamDetail+
// duration+dimensions) via sendPrelude before this is called — ffmpeg's
// -progress doesn't carry codec/duration info, so the prelude is the
// only source.
func runProgressReporter(ctx context.Context, r io.Reader, rc reportContext) {
	if rc.URL == "" {
		return
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 1<<20)

	httpClient := &http.Client{Timeout: 4 * time.Second}
	block := map[string]string{}
	first := true
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		block[k] = v
		if k == "progress" {
			if first {
				first = false
				log.Printf("session %s: first progress block out_time_us=%s total_size=%s speed=%s base_url=%s", rc.SessionID, block["out_time_us"], block["total_size"], block["speed"], rc.URL)
			}
			putProgressTick(ctx, httpClient, rc, block)
			block = map[string]string{}
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		log.Printf("session %s: progress reporter scan: %v", rc.SessionID, err)
	}
}


// putProgressTick translates one ffmpeg progress block into the
// Plex-shaped query-string PUT.
//
//	progress = clamp(out_time / duration * 100, 0, 100)
//	size     = total_size (bytes; ffmpeg may emit "N/A" → -1)
//	remaining= (duration - out_time) / speed
//	speed    = ffmpeg "<x>" minus the trailing "x"
func putProgressTick(ctx context.Context, c *http.Client, rc reportContext, blk map[string]string) {
	outUs, _ := strconv.ParseInt(blk["out_time_us"], 10, 64)
	outS := float64(outUs) / 1e6

	var pct float64
	if rc.DurationS > 0 {
		pct = outS / rc.DurationS * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
	}

	size := int64(-1)
	if v := blk["total_size"]; v != "" && v != "N/A" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			size = n
		}
	}

	speedF := math.NaN()
	if v := blk["speed"]; v != "" && v != "N/A" {
		v = strings.TrimSuffix(strings.TrimSpace(v), "x")
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			speedF = n
		}
	}

	remaining := -1.0
	if rc.DurationS > 0 && !math.IsNaN(speedF) && speedF > 0 {
		remaining = (rc.DurationS - outS) / speedF
		if remaining < 0 {
			remaining = 0
		}
	}

	q := url.Values{}
	q.Set("progress", fmt.Sprintf("%.1f", pct))
	q.Set("size", strconv.FormatInt(size, 10))
	if remaining >= 0 {
		q.Set("remaining", fmt.Sprintf("%.0f", remaining))
	} else {
		q.Set("remaining", "-1")
	}
	// Plex Transcoder omits &speed= when its own throttle is engaged
	// (fftools/ffmpeg.c: "Only pass back speed if we're not throttled").
	// PMS uses speed to estimate buffer-build-up; signalling "no speed"
	// while throttled prevents PMS from concluding the session is fast
	// enough to clear canThrottle prematurely.
	throttled := rc.Throttle != nil && rc.Throttle.on()
	if !throttled {
		if !math.IsNaN(speedF) {
			q.Set("speed", strconv.FormatFloat(speedF, 'f', -1, 64))
		} else {
			q.Set("speed", "inf")
		}
	}

	fullURL := joinQuery(rc.URL, q)
	doPlexPUT(ctx, c, rc, "progress", fullURL)
}

// sendPrelude fires the one-time PUTs Plex Transcoder sends at startup:
// duration, then a streamDetail per output stream, then a dimensions
// PUT for the (first) video stream. PMS uses these to fill in codec
// metadata so /header can return the init segment without falling back
// to disk-probing.
//
// All PUTs are best-effort — a failure here only delays /header by a
// few hundred ms (PMS retries) and the periodic progress PUTs continue
// regardless.
func sendPrelude(ctx context.Context, c *http.Client, rc reportContext) {
	if rc.URL == "" {
		return
	}
	if rc.DurationS > 0 {
		q := url.Values{}
		// Plex Transcoder capture (2026-05-05): "duration=6461.876000"
		// for a 108-min film → SECONDS, not ms. Send seconds.
		q.Set("duration", fmt.Sprintf("%.6f", rc.DurationS))
		doPlexPUT(ctx, c, rc, "duration", joinQuery(rc.URL, q))
	}
	var firstVideo *outputStream
	for i := range rc.Streams {
		s := rc.Streams[i]
		q := url.Values{}
		q.Set("index", strconv.Itoa(s.Index))
		q.Set("id", strconv.Itoa(s.ID))
		q.Set("codec", s.Codec)
		q.Set("type", s.Type)
		switch s.Type {
		case "video":
			if s.Profile != "" {
				q.Set("profile", s.Profile)
			}
			if s.Width > 0 {
				q.Set("width", strconv.Itoa(s.Width))
			}
			if s.Height > 0 {
				q.Set("height", strconv.Itoa(s.Height))
			}
			q.Set("interlaced", "0")
			if s.Level > 0 {
				q.Set("level", strconv.Itoa(s.Level))
			}
			if s.FrameRate > 0 {
				q.Set("frameRate", strconv.FormatFloat(s.FrameRate, 'f', 3, 64))
			}
			q.Set("disp_default", "1")
			if firstVideo == nil {
				firstVideo = &rc.Streams[i]
			}
		case "audio":
			if s.Profile != "" {
				q.Set("profile", s.Profile)
			}
			if s.Language != "" {
				q.Set("language", s.Language)
			}
			if s.Channels > 0 {
				q.Set("channels", strconv.Itoa(s.Channels))
			}
			if s.Layout != "" {
				q.Set("layout", s.Layout)
			}
			if s.SampleRate > 0 {
				q.Set("sampleRate", strconv.Itoa(s.SampleRate))
			}
			q.Set("disp_default", "1")
		}
		doPlexPUT(ctx, c, rc, "streamDetail", joinQueryWithSuffix(rc.URL, "/streamDetail", q))
		// /stream (singular) registers the output stream's existence —
		// Plex Transcoder fires this once per stream alongside
		// streamDetail. We observed in offline capture that PMS keeps
		// /header pending until both have arrived.
		streamQ := url.Values{}
		streamQ.Set("index", strconv.Itoa(s.Index))
		streamQ.Set("id", strconv.Itoa(s.ID))
		streamQ.Set("codec", s.Codec)
		streamQ.Set("type", s.Type)
		// Audio streams in Plex Transcoder also include `profile=LC`
		// here. Match.
		if s.Type == "audio" && s.Profile != "" {
			streamQ.Set("profile", s.Profile)
		}
		doPlexPUT(ctx, c, rc, "stream", joinQueryWithSuffix(rc.URL, "/stream", streamQ))
	}
	if firstVideo != nil && firstVideo.Width > 0 && firstVideo.Height > 0 {
		q := url.Values{}
		q.Set("width", strconv.Itoa(firstVideo.Width))
		q.Set("height", strconv.Itoa(firstVideo.Height))
		doPlexPUT(ctx, c, rc, "dimensions", joinQuery(rc.URL, q))
	}
}

// joinQuery merges existing URL query string with extra params and
// returns the full URL. Plex's progress URL already carries
// X-Plex-Token in the query; we keep it.
func joinQuery(rawURL string, extra url.Values) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL // best-effort; caller handles non-200
	}
	q := u.Query()
	for k, v := range extra {
		for _, vv := range v {
			q.Add(k, vv)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// joinQueryWithSuffix appends pathSuffix to the URL's path (preserving
// the existing query) and merges extra params. String concatenation
// breaks because rc.URL ends with `?X-Plex-Token=…`; the suffix would
// land inside the query string.
func joinQueryWithSuffix(rawURL, pathSuffix string, extra url.Values) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.Path += pathSuffix
	q := u.Query()
	for k, v := range extra {
		for _, vv := range v {
			q.Add(k, vv)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func doPlexPUT(ctx context.Context, c *http.Client, rc reportContext, kind, fullURL string) {
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodPut, fullURL, http.NoBody)
	if err != nil {
		metricProgressPUT.WithLabelValues(kind, "err").Inc()
		return
	}
	// Plex Transcoder sends Range: bytes=0-, no body. Mimic that so PMS
	// path matching matches Plex Transcoder's wire frame exactly.
	req.Header.Set("Range", "bytes=0-")
	req.ContentLength = 0

	resp, err := c.Do(req)
	if err != nil {
		metricProgressPUT.WithLabelValues(kind, "err").Inc()
		if ctx.Err() == nil {
			log.Printf("session %s: progress PUT (%s): %v", rc.SessionID, kind, err)
		}
		// Fail open: clear throttle on transport error so we don't strand
		// a session paused if PMS becomes unreachable.
		if rc.Throttle != nil {
			rc.Throttle.set(false)
		}
		return
	}
	// Read up to 4KB of the body so the throttle signal can be parsed
	// (`canThrottle` substring per fftools/ffmpeg.c). 4KB matches Plex
	// Transcoder's PMS_IssueHttpRequest fast-path; PMS replies are tiny.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	metricProgressPUT.WithLabelValues(kind, httpClass(resp.StatusCode)).Inc()
	if resp.StatusCode >= 400 {
		log.Printf("session %s: progress PUT (%s) status=%d", rc.SessionID, kind, resp.StatusCode)
		// Fail open on 4xx/5xx — same reason as transport error above.
		if rc.Throttle != nil {
			rc.Throttle.set(false)
		}
		return
	}
	if rc.Throttle != nil && kind == "progress" {
		rc.Throttle.set(bytes.Contains(body, []byte("canThrottle")))
	}
}

func httpClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	}
	return "1xx"
}

// progressPipeArg returns the argv pair to pass `-progress pipe:N` to
// ffmpeg, where N is the fd the child will see for the writer placed at
// extraIdx in cmd.ExtraFiles (0-based).
func progressPipeArg(extraIdx int) []string {
	return []string{"-progress", "pipe:" + strconv.Itoa(3+extraIdx)}
}

// logForwarder buffers ffmpeg stderr bytes, splits on newlines, and
// POSTs each line as a Plex Transcoder-style /progress/log message.
// PMS appears to gate /header on a "transcoder is alive and producing"
// signal fed by these POSTs in production; without them /header sits
// at SegmentedTranscoderTimeout (~125s) before falling back to a disk
// probe of init-stream0.m4s.
//
// Append() is called from the existing stderr peek callback in
// streamPrefixed, so we don't need an extra pipe. POSTs are
// fire-and-forget on a dedicated http.Client.
type logForwarder struct {
	ctx       context.Context
	baseURL   string
	sessionID string
	mu        sync.Mutex
	buf       []byte
	client    *http.Client
}

func newLogForwarder(ctx context.Context, baseURL, sessionID string) *logForwarder {
	log.Printf("session %s: log-forwarder armed url=%s", sessionID, baseURL)
	return &logForwarder{
		ctx:       ctx,
		baseURL:   baseURL,
		sessionID: sessionID,
		client:    &http.Client{Timeout: 2 * time.Second},
	}
}

func (l *logForwarder) Append(p []byte) {
	if l == nil || l.baseURL == "" {
		return
	}
	l.mu.Lock()
	l.buf = append(l.buf, p...)
	// Split on \n or \r — ffmpeg uses \r for the periodic
	// "size= time= speed=" stats line, \n for everything else.
	for {
		idx := -1
		for i, c := range l.buf {
			if c == '\n' || c == '\r' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(l.buf[:idx]), "\r\n ")
		l.buf = l.buf[idx+1:]
		if line == "" {
			continue
		}
		if len(line) > 8192 {
			line = line[:8192]
		}
		// Plex Transcoder uses level=2 for info-ish, level=3 for debug.
		// Coarse classifier: bracketed component logs + the periodic
		// stats line are 2; everything else 3.
		level := "3"
		if strings.HasPrefix(line, "[") || strings.HasPrefix(line, "frame=") || strings.HasPrefix(line, "size=") {
			level = "2"
		}
		q := url.Values{}
		q.Set("level", level)
		q.Set("message", line)
		full := joinQueryWithSuffix(l.baseURL, "/log", q)
		go l.post(full)
	}
	l.mu.Unlock()
}

func (l *logForwarder) post(fullURL string) {
	pctx, cancel := context.WithTimeout(l.ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodPost, fullURL, http.NoBody)
	if err != nil {
		return
	}
	req.Header.Set("Range", "bytes=0-")
	req.ContentLength = 0
	resp, err := l.client.Do(req)
	if err != nil {
		// Log first few errors so we can tell if POSTs are reaching the relay.
		log.Printf("session %s: log POST err: %v", l.sessionID, err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("session %s: log POST status=%d url=%s", l.sessionID, resp.StatusCode, fullURL)
	}
}

// extractInputPath returns the value passed after the first -i flag.
// Empty if -i isn't present or has no value.
func extractInputPath(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" {
			return args[i+1]
		}
	}
	return ""
}

// probeDurationSeconds runs ffprobe against `path` and returns the
// container duration in seconds. Returns 0 on any failure (caller
// treats 0 as "unknown" and skips the duration PUT).
//
// Bound the probe at 2s so a slow NFS mount doesn't stall ffmpeg
// startup; first-segment latency is the design goal.
func probeDurationSeconds(ctx context.Context, path string) float64 {
	if path == "" {
		return 0
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, "/usr/bin/ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	v := strings.TrimSpace(string(out))
	d, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return d
}

// probeInputStreams ffprobes the source file and returns one
// outputStream entry per input stream Plex Transcoder would register
// via /progress/streamDetail. Reverse-engineered against a real PT
// run: the streamDetail's `codec`, `profile`, `width/height`,
// `channels`, `layout`, `sampleRate`, `frameRate`, `level`, and
// `language` all reflect the SOURCE stream, not the output encoder.
//
// Returns nil on any error; caller falls back to argv-derived fallback.
func probeInputStreams(ctx context.Context, path string) []outputStream {
	if path == "" {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, "/usr/bin/ffprobe",
		"-v", "error",
		"-show_entries", "stream=index,codec_name,codec_type,profile,width,height,r_frame_rate,channels,channel_layout,sample_rate,level:stream_tags=language",
		"-of", "default=noprint_wrappers=1",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var streams []outputStream
	cur := outputStream{}
	finalize := func() {
		if cur.Type == "video" || cur.Type == "audio" {
			streams = append(streams, cur)
		}
		cur = outputStream{}
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "index":
			finalize()
			n, _ := strconv.Atoi(v)
			cur.Index = n
		case "codec_name":
			cur.Codec = v
		case "codec_type":
			cur.Type = v
		case "profile":
			if v != "N/A" && v != "unknown" {
				cur.Profile = v
			}
		case "width":
			n, _ := strconv.Atoi(v)
			cur.Width = n
		case "height":
			n, _ := strconv.Atoi(v)
			cur.Height = n
		case "r_frame_rate":
			// e.g. "24000/1001" → 23.976
			if num, den, ok := strings.Cut(v, "/"); ok {
				n, _ := strconv.ParseFloat(num, 64)
				d, _ := strconv.ParseFloat(den, 64)
				if d > 0 {
					cur.FrameRate = n / d
				}
			}
		case "channels":
			n, _ := strconv.Atoi(v)
			cur.Channels = n
		case "channel_layout":
			cur.Layout = v
		case "sample_rate":
			n, _ := strconv.Atoi(v)
			cur.SampleRate = n
		case "TAG:language":
			cur.Language = v
		}
	}
	finalize()
	return streams
}

// extractOutputStreams parses the rewritten ffmpeg argv to derive the
// list of output streams Plex Transcoder normally registers via
// streamDetail. Best-effort: anything missing is left zero/empty and
// the prelude PUT skips it.
//
// Pattern observed in PMS-emitted argv:
//
//	-map [N]    -codec:0 h264_vaapi  ... -metadata:s:0 language=eng
//	-map 0:1    -codec:1 aac         -b:1 256k          ...
//
// Width/height for video come from the rewritten filter_complex
// `scale_vaapi=w=W:h=H` (or `scale=w=W:h=H` if HW rewrite skipped).
func extractOutputStreams(args []string) []outputStream {
	var out []outputStream
	w, h := extractScaleWH(args)
	for i := 0; i < len(args)-1; i++ {
		if !strings.HasPrefix(args[i], "-codec:") {
			continue
		}
		idxPart := strings.TrimPrefix(args[i], "-codec:")
		idx, err := strconv.Atoi(idxPart)
		if err != nil {
			continue
		}
		// Skip the input-side -codec (just before -i). The rule is:
		// if the first -i appears AFTER this -codec, this is an input
		// codec hint, not an output encoder.
		inputIdx := -1
		for j := i + 1; j < len(args); j++ {
			if args[j] == "-i" {
				inputIdx = j
				break
			}
		}
		if inputIdx > i {
			continue
		}
		// Don't double-register the same -map index (Plex sometimes
		// repeats `-codec:1 X ... -codec:1 X`).
		dup := false
		for _, s := range out {
			if s.Index == idx {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		codec := args[i+1]
		typ := codecType(codec)
		s := outputStream{Index: idx, ID: 0, Codec: codecPlexName(codec), Type: typ}
		if typ == "video" {
			s.Width = w
			s.Height = h
			s.Profile = "Main" // matches Plex Transcoder's per-encoder default
			// Pull frame rate from `-r:N <fps>` if present.
			rNeedle := "-r:" + strconv.Itoa(idx)
			for j := 0; j+1 < len(args); j++ {
				if args[j] == rNeedle {
					if f, err := strconv.ParseFloat(args[j+1], 64); err == nil {
						s.FrameRate = f
					}
					break
				}
			}
			if s.FrameRate <= 0 {
				s.FrameRate = 23.976 // film default; better than nothing
			}
		}
		if typ == "audio" {
			s.Channels = 2
			s.Layout = "stereo"
			s.SampleRate = 48000
		}
		// language metadata
		needle := "-metadata:s:" + strconv.Itoa(idx)
		for j := 0; j < len(args)-1; j++ {
			if args[j] == needle && strings.HasPrefix(args[j+1], "language=") {
				s.Language = strings.TrimPrefix(args[j+1], "language=")
				break
			}
		}
		out = append(out, s)
	}
	return out
}

var reScaleWH = regexp.MustCompile(`scale(?:_vaapi)?=w=(\d+):h=(\d+)`)

func extractScaleWH(args []string) (int, int) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-filter_complex" {
			continue
		}
		if m := reScaleWH.FindStringSubmatch(args[i+1]); m != nil {
			w, _ := strconv.Atoi(m[1])
			h, _ := strconv.Atoi(m[2])
			return w, h
		}
	}
	return 0, 0
}

// codecType classifies an ffmpeg codec/encoder into Plex's video|audio
// bucket. Anything unfamiliar returns "" so the caller can skip the
// streamDetail PUT for it (subtitles, attachments, etc.).
func codecType(codec string) string {
	switch codec {
	case "h264_vaapi", "hevc_vaapi", "h264_nvenc", "hevc_nvenc",
		"libx264", "libx265", "av1", "h264", "hevc", "vp9":
		return "video"
	case "aac", "eac3", "ac3", "mp3", "opus", "flac", "libopus", "libvorbis":
		return "audio"
	}
	return ""
}

// codecPlexName collapses ffmpeg's encoder names back into the codec
// name Plex uses in streamDetail (`h264_vaapi` → `h264`, etc.).
func codecPlexName(codec string) string {
	switch codec {
	case "h264_vaapi", "h264_nvenc", "libx264":
		return "h264"
	case "hevc_vaapi", "hevc_nvenc", "libx265":
		return "hevc"
	case "libopus":
		return "opus"
	case "libvorbis":
		return "vorbis"
	}
	return codec
}
