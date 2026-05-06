// scaleplex-agent — phase 1 minimal worker daemon.
//
// Listens on :3501. Accepts a task envelope on POST /task, spawns ffmpeg
// with the supplied args + env, streams stderr back over the same HTTP
// response (chunked). On client disconnect or POST /task/:id/kill, sends
// SIGTERM (then SIGKILL after 5s) to ffmpeg.
//
// This is intentionally barebones: no arg translation yet, no Plex
// coupling, no orchestrator. Phase 2 layers those on top once we've
// proven the worker image + HW filter chain work in our cluster.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	listenAddr = ":3501"
	ffmpegBin  = "/usr/bin/ffmpeg"
)

type taskRequest struct {
	SessionID string            `json:"session_id"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	// Rewrite=true → run the args through argRewriter (Plex SW → VAAPI HW)
	// before spawning ffmpeg. Off by default for raw smoke-test calls.
	Rewrite bool `json:"rewrite,omitempty"`
}

type capabilityResponse struct {
	FFmpegPath  string   `json:"ffmpeg_path"`
	FFmpegOK    bool     `json:"ffmpeg_ok"`
	HWAccels    []string `json:"hwaccels,omitempty"`
	HWFilters   []string `json:"hw_filters,omitempty"`
	RenderNodes []string `json:"render_nodes,omitempty"`
	// Active session count. Used by the orchestrator for least-loaded
	// worker selection.
	ActiveSessions int `json:"active_sessions"`
	// MaxSessions is a soft cap; 0 means unlimited (default until we
	// have real Arc A310 capacity numbers from phase 6 testing).
	MaxSessions int `json:"max_sessions"`
}

type taskRegistry struct {
	mu    sync.Mutex
	tasks map[string]*runningTask
}

type runningTask struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

func (r *taskRegistry) register(id string, t *runningTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.tasks[id]; ok {
		log.Printf("session %s: replacing prior task pid=%d", id, prev.cmd.Process.Pid)
		prev.cancel()
	}
	r.tasks[id] = t
}

func (r *taskRegistry) unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tasks, id)
}

func (r *taskRegistry) activeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tasks)
}

func (r *taskRegistry) kill(id string) bool {
	r.mu.Lock()
	t, ok := r.tasks[id]
	r.mu.Unlock()
	if !ok {
		return false
	}
	log.Printf("session %s: kill requested", id)
	t.cancel()
	return true
}

var registry = &taskRegistry{tasks: make(map[string]*runningTask)}

// ready flips to 1 once pre-warm is done. Until then /readyz returns 503.
// Liveness (/healthz) is always 200 once HTTP is up so the kubelet doesn't
// kill us mid-warmup.
var ready int32

// renderFDs holds open fds on /dev/dri/renderD* so the i915 device + iHD
// driver shared libs stay paged in and the first real session doesn't pay
// open()/mmap() cost.
var renderFDs []*os.File

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/readyz", handleReady)
	mux.HandleFunc("/capability", handleCapability)
	mux.HandleFunc("/task", handleTask)
	mux.HandleFunc("/task/", handleTaskByID)
	mux.Handle("/metrics", promhttp.Handler())

	// Initialise static gauges so they show up in the first scrape.
	metricMaxSessions.Set(float64(maxSessions()))
	if _, err := os.Stat(ffmpegBin); err == nil {
		metricFFmpegOK.Set(1)
	}

	go prewarm()

	log.Printf("scaleplex-agent listening on %s", listenAddr)
	srv := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ok\n")
}

func handleReady(w http.ResponseWriter, r *http.Request) {
	if atomic.LoadInt32(&ready) == 1 {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ready\n")
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	io.WriteString(w, "warming\n")
}

// prewarm runs once at startup. It pulls every dynamic dep ffmpeg will need
// into page cache so the first real /task spawn doesn't pay cold-start cost.
//
// Steps, all best-effort (a failure here means a slower first session, not a
// broken worker — log and move on):
//  1. Open every /dev/dri/renderD* and hang on to the fd.
//  2. Run `ffmpeg -version` so libavcodec/libavfilter/libva/libass mmap.
//  3. Run a 1s testsrc → vaapi encode → null pipeline so the iHD driver
//     JIT-compiles its VPP + encoder programs and caches them.
func prewarm() {
	t0 := time.Now()
	defer func() {
		atomic.StoreInt32(&ready, 1)
		dur := time.Since(t0)
		metricPrewarmSeconds.Set(dur.Seconds())
		metricReady.Set(1)
		log.Printf("pre-warm complete in %s", dur)
	}()

	for _, dev := range listRenderNodes() {
		f, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err != nil {
			log.Printf("pre-warm: open %s: %v", dev, err)
			continue
		}
		renderFDs = append(renderFDs, f)
	}

	if err := exec.Command(ffmpegBin, "-hide_banner", "-version").Run(); err != nil {
		log.Printf("pre-warm: ffmpeg -version: %v", err)
	}

	dev := "/dev/dri/renderD128"
	if len(renderFDs) > 0 {
		dev = renderFDs[0].Name()
	}
	dummy := exec.Command(ffmpegBin,
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "vaapi=va:"+dev,
		"-filter_hw_device", "va",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=30",
		"-vf", "format=nv12,hwupload",
		"-c:v", "h264_vaapi", "-t", "1",
		"-f", "null", "-",
	)
	if out, err := dummy.CombinedOutput(); err != nil {
		log.Printf("pre-warm: dummy vaapi transcode: %v: %s", err, out)
	}
}

func handleCapability(w http.ResponseWriter, r *http.Request) {
	resp := capabilityResponse{FFmpegPath: ffmpegBin}
	if _, err := os.Stat(ffmpegBin); err == nil {
		resp.FFmpegOK = true
	}
	resp.HWAccels = collectFFmpegList("-hwaccels")
	resp.HWFilters = filterVAAPIFilters(collectFFmpegList("-filters"))
	resp.RenderNodes = listRenderNodes()
	resp.ActiveSessions = registry.activeCount()
	resp.MaxSessions = maxSessions()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// maxSessions returns the soft cap on concurrent ffmpeg spawns. 0 means
// unlimited — the default until we have real concurrency numbers from
// the Arc A310 in production.
func maxSessions() int {
	v := os.Getenv("WORKER_MAX_SESSIONS")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func collectFFmpegList(flag string) []string {
	out, err := exec.Command(ffmpegBin, "-hide_banner", flag).Output()
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range splitLines(string(out)) {
		lines = append(lines, line)
	}
	return lines
}

func filterVAAPIFilters(lines []string) []string {
	var out []string
	for _, line := range lines {
		// `ffmpeg -filters` lines look like "  T.. scale_vaapi V->V Scale VAAPI ..."
		if containsToken(line, "vaapi") {
			out = append(out, line)
		}
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func containsToken(s, tok string) bool {
	for i := 0; i+len(tok) <= len(s); i++ {
		if s[i:i+len(tok)] == tok {
			return true
		}
	}
	return false
}

func listRenderNodes() []string {
	entries, err := os.ReadDir("/dev/dri")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if len(name) > 7 && name[:7] == "renderD" {
			out = append(out, "/dev/dri/"+name)
		}
	}
	return out
}

func handleTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req taskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	if len(req.Args) == 0 {
		http.Error(w, "args required", http.StatusBadRequest)
		return
	}

	// Soft cap: when WORKER_MAX_SESSIONS is set and we're at it, refuse
	// with 503 so the orchestrator falls through to the next-best worker.
	// 0 = unlimited (default).
	if cap := maxSessions(); cap > 0 && registry.activeCount() >= cap {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "worker at capacity", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	finalArgs := req.Args
	finalEnv := req.Env
	progressURL := ""
	manifestURL := ""
	if req.Rewrite {
		res := Rewrite(req.Args, req.Env, nil)
		if res.Applied {
			finalArgs = res.Args
			finalEnv = res.Env
			progressURL = res.ProgressURL
			manifestURL = res.ManifestURL
			metricRewriteApplied.WithLabelValues("applied").Inc()
			log.Printf("session %s: rewriter applied: %s", req.SessionID, strings.Join(res.Changes, ","))
		} else {
			reason := "unknown"
			for _, c := range res.Changes {
				if strings.HasPrefix(c, "skip:") {
					reason = c
					break
				}
			}
			metricRewriteApplied.WithLabelValues(reason).Inc()
			log.Printf("session %s: rewriter NOT applied (%s) — running original args", req.SessionID, strings.Join(res.Changes, ","))
		}
		// Adaptive probesize runs whether the HW rewrite applied or not —
		// it's a pure latency win on any ffmpeg invocation that has
		// -probesize / -analyzeduration set conservatively.
		var psChanges []string
		finalArgs, psChanges = applyAdaptiveProbesize(finalArgs)
		if len(psChanges) > 0 {
			log.Printf("session %s: probesize: %s", req.SessionID, strings.Join(psChanges, ","))
		}
	}

	cmd := exec.CommandContext(ctx, ffmpegBin, finalArgs...)
	cmd.Env = buildEnv(finalEnv)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM, Setpgid: true}

	// Wire `-progress pipe:N` for the worker-side reporter when the
	// rewriter captured a Plex progress URL. Stock ffmpeg's chunked
	// `-progress <http>` confuses Plex's PUT handler; the reporter
	// reissues each block as its own PUT.
	var progressReader, progressWriter *os.File
	if progressURL != "" {
		pr, pw, err := os.Pipe()
		if err != nil {
			http.Error(w, "progress pipe: "+err.Error(), http.StatusInternalServerError)
			return
		}
		extraIdx := len(cmd.ExtraFiles)
		cmd.ExtraFiles = append(cmd.ExtraFiles, pw)
		cmd.Args = append(cmd.Args, progressPipeArg(extraIdx)...)
		progressReader = pr
		progressWriter = pw
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		http.Error(w, "stderr pipe: "+err.Error(), http.StatusInternalServerError)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "stdout pipe: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := cmd.Start(); err != nil {
		if progressReader != nil {
			progressReader.Close()
			progressWriter.Close()
		}
		http.Error(w, "spawn: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// After Start, the child holds its own copy of pw. Close ours so
	// the reader gets EOF when ffmpeg exits.
	if progressWriter != nil {
		progressWriter.Close()
		progressWriter = nil
	}
	log.Printf("session %s: spawned ffmpeg pid=%d", req.SessionID, cmd.Process.Pid)
	spawnedAt := time.Now()

	registry.register(req.SessionID, &runningTask{cmd: cmd, cancel: cancel})
	metricActiveSessions.Set(float64(registry.activeCount()))
	defer func() {
		registry.unregister(req.SessionID)
		metricActiveSessions.Set(float64(registry.activeCount()))
		metricSessionDurationSeconds.Observe(time.Since(spawnedAt).Seconds())
	}()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Scaleplex-Pid", fmt.Sprintf("%d", cmd.Process.Pid))
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// All response writes (stdout pipe, stderr pipe, segwatch) go through
	// this lock — http.ResponseWriter.Write is not safe for concurrent use.
	resp := newLockedWriter(w)

	// Sliding window over stderr so we can pull the final speed=Xx value
	// once ffmpeg exits. 8KB easily covers the last ~100 progress lines.
	stderrTail := newRingBuffer(8192)

	// Watch ffmpeg's output dir for the first segment file. If req.Cwd
	// isn't set we skip the watcher silently; the orchestrator/shim sets
	// cwd to the per-session transcode dir.
	if req.Cwd != "" {
		go watchFirstSegment(ctx, req.Cwd, req.SessionID, resp, spawnedAt)
		go watchAndRenumberChunks(ctx, req.Cwd, req.SessionID)
		if manifestURL != "" {
			go runManifestPublisher(ctx, req.Cwd, manifestURL, req.SessionID)
		}
	}

	streamDone := make(chan struct{}, 2)
	go streamPrefixed(stdout, resp, "[stdout] ", streamDone, nil)

	// stderrPeek hooks the existing stderr stream (no separate pipe
	// needed). Today only feeds stderrTail (the speed=Xx ring buffer
	// scraped on exit). The log forwarder used to fan out per-line
	// POSTs to /progress/log here, but slammed PMS with 60+ concurrent
	// HTTP connections; defaults to off until we throttle properly.
	stderrPeek := stderrTail.Append
	go streamPrefixed(stderr, resp, "[stderr] ", streamDone, stderrPeek)

	progressDone := make(chan struct{}, 1)
	if progressReader != nil {
		inputPath := extractInputPath(finalArgs)
		streams := probeInputStreams(ctx, inputPath)
		if len(streams) == 0 {
			// ffprobe failed — fall back to argv-derived output stream
			// info. Less accurate (codec is the output encoder, not the
			// source) but better than empty.
			streams = extractOutputStreams(finalArgs)
		} else {
			// Plex Transcoder hardcodes level=5 for h264 and emits the
			// codec name without "-vaapi" / "-nvenc" suffixes.
			for i := range streams {
				if streams[i].Type == "video" && streams[i].Level == 0 {
					streams[i].Level = 5
				}
			}
		}
		rc := reportContext{
			URL:       progressURL,
			SessionID: req.SessionID,
			Streams:   streams,
			DurationS: probeDurationSeconds(ctx, inputPath),
		}
		// Fire the one-shot prelude PUTs (duration + streamDetail +
		// dimensions) before starting the periodic reporter. PMS uses
		// these to fill in codec metadata so /header can return without
		// falling back to a 124s disk-probe.
		go func() {
			httpClient := &http.Client{Timeout: 4 * time.Second}
			sendPrelude(ctx, httpClient, rc)
		}()
		go func() {
			defer close(progressDone)
			defer progressReader.Close()
			runProgressReporter(ctx, progressReader, rc)
		}()
	} else {
		close(progressDone)
	}

	waitErr := cmd.Wait()
	<-streamDone
	<-streamDone
	<-progressDone

	recordSpeedFromOutput(stderrTail.String())

	if waitErr != nil {
		// Was it killed via context (registry.kill or client disconnect)
		// vs ffmpeg crash? Distinguish for the metric.
		if ctx.Err() != nil {
			metricSessionsTotal.WithLabelValues("killed").Inc()
		} else {
			metricSessionsTotal.WithLabelValues("error").Inc()
		}
		// Log stderr tail so failures don't need manual replay to debug.
		tail := stderrTail.String()
		const maxTail = 1024
		if len(tail) > maxTail {
			tail = "..." + tail[len(tail)-maxTail:]
		}
		tail = strings.ReplaceAll(strings.ReplaceAll(tail, "\r", "\\r"), "\n", "\\n")
		fmt.Fprintf(resp, "[scaleplex] ffmpeg exit: %v\n", waitErr)
		log.Printf("session %s: ffmpeg exit: %v stderr_tail=%s", req.SessionID, waitErr, tail)
	} else {
		metricSessionsTotal.WithLabelValues("success").Inc()
		fmt.Fprintf(resp, "[scaleplex] ffmpeg exit: success\n")
		log.Printf("session %s: ffmpeg ok", req.SessionID)
	}
}

// ringBuffer keeps the last N bytes appended to it. Cheap thread-safe
// sliding-window for stderr scrubbing.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newRingBuffer(max int) *ringBuffer { return &ringBuffer{max: max} }

func (r *ringBuffer) Append(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

func handleTaskByID(w http.ResponseWriter, r *http.Request) {
	// Path looks like /task/<id>/kill or /task/<id>
	id := r.URL.Path[len("/task/"):]
	suffix := ""
	for i := 0; i < len(id); i++ {
		if id[i] == '/' {
			suffix = id[i+1:]
			id = id[:i]
			break
		}
	}
	if suffix != "kill" {
		http.Error(w, "unknown task subpath", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !registry.kill(id) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// streamPrefixed copies pipe → response writer with a prefix per chunk.
// The optional `peek` callback receives every raw chunk before prefixing
// — used to scrub stderr for `speed=` and the like without re-reading.
func streamPrefixed(rc io.ReadCloser, w *lockedWriter, prefix string, done chan<- struct{}, peek func([]byte)) {
	defer rc.Close()
	defer func() { done <- struct{}{} }()
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if peek != nil {
				peek(buf[:n])
			}
			_, _ = w.writePrefixed(prefix, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func buildEnv(supplied map[string]string) []string {
	base := os.Environ()
	if len(supplied) == 0 {
		return base
	}
	// Strip any base entries the caller explicitly overrides.
	overrides := make(map[string]struct{}, len(supplied))
	for k := range supplied {
		overrides[k] = struct{}{}
	}
	out := make([]string, 0, len(base)+len(supplied))
	for _, kv := range base {
		eq := -1
		for i, c := range kv {
			if c == '=' {
				eq = i
				break
			}
		}
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		if _, replaced := overrides[kv[:eq]]; replaced {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range supplied {
		out = append(out, k+"="+v)
	}
	return out
}

// graceful shutdown placeholder — wire up signal handling once the agent
// has work it actually needs to drain. For phase 1 the kubelet's
// pre-stop hook + 5s grace is enough.
