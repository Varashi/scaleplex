// scaleplex-agent — worker daemon.
//
// Listens on :3501. Accepts a task envelope on POST /task, spawns ffmpeg
// with the supplied args + env, streams stderr back over the same HTTP
// response (chunked). On client disconnect or POST /task/:id/kill, sends
// SIGTERM (then SIGKILL after 5s) to ffmpeg.

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
	"path/filepath"
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
	// ffprobeBin lives in probesize.go (next to its primary user).
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
	// GPULoad is the mean busy fraction across this worker's video
	// engines (vcs/vecs), 0..1. Multi-engine GPUs naturally report
	// lower values for the same session count, biasing routing toward
	// them. 0 on CPU-only workers.
	GPULoad float64 `json:"gpu_load"`
	// GPUEngines counts the discovered video engines so the orchestrator
	// (or a dashboard) can sanity-check the gpu_load reading.
	GPUEngines int `json:"gpu_engines"`
}

type taskRegistry struct {
	mu    sync.Mutex
	tasks map[string]*runningTask
}

type runningTask struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc

	// Frozen at spawn — captured so /task/<id>/checkpoint can hand a
	// recovering worker enough context to pick up where this one left
	// off without re-running the rewriter. argv/env are POST-REWRITE
	// (what we actually exec'd).
	argv         []string
	env          map[string]string
	cwd          string
	sourcePath   string
	progressURL  string
	seekOffsetS  float64
	startedAt    time.Time

	// Live counter. segwatch's chunk renumberer bumps this on each
	// successful rename so the checkpoint reports the highest segment
	// PMS could already have fetched. atomic so the checkpoint
	// handler can read without locking.
	lastSeq atomic.Int64
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

func (r *taskRegistry) get(id string) *runningTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasks[id]
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

	engineSamplerInst = startEngineSampler()
	log.Printf("gpu engines discovered: %d (mode=%s)", engineSamplerInst.numEngines(), engineSamplerInst.modeTag())

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
	if engineSamplerInst != nil {
		resp.GPULoad = engineSamplerInst.load()
		resp.GPUEngines = engineSamplerInst.numEngines()
	}
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
	seekOffsetSeconds := 0.0
	isMatroskaSegment := false
	if req.Rewrite {
		// Env-gated argv capture for debugging new PMS argv shapes
		// (HW-decode mode, Plex version bumps, etc.). Off by default to
		// keep logs clean; flip WORKER_DUMP_ARGV=1 on the worker pod or
		// DaemonSet env when investigating. Logs to stderr AND, when
		// the dir exists / can be created, writes a JSON capture under
		// $WORKER_ARGV_CORPUS_DIR (default /transcode/_argv-corpus,
		// shared NFS so it survives pod restarts and is reachable from
		// outside the cluster). Idempotent on session_id.
		if os.Getenv("WORKER_DUMP_ARGV") != "" {
			log.Printf("session %s: argv=%q", req.SessionID, req.Args)
			persistArgvCapture(req.SessionID, req.Cwd, req.Args, req.Env)
		}
		res := Rewrite(req.Args, req.Env, &RewriteOpts{
			SessionDir:         req.Cwd,
			ProbeSubtitleCodec: probeSubtitleCodec,
			ProbeVideoColor:    probeVideoColor,
		})
		if res.Applied {
			finalArgs = res.Args
			finalEnv = res.Env
			progressURL = res.ProgressURL
			seekOffsetSeconds = res.SeekOffsetSeconds
			isMatroskaSegment = res.IsMatroskaSegment
			metricRewriteApplied.WithLabelValues("applied").Inc()
			log.Printf("session %s: rewriter applied: %s", req.SessionID, strings.Join(res.Changes, ","))
			// Extract embedded subtitle stream to disk before spawning
			// the main encoder. Stock ffmpeg's `subtitles=` filter
			// reads from a file path, not a stream index — Plex's
			// private `inlineass` filter does the latter, but we
			// translated it to `subtitles=`. See rewriter.go for the
			// argv shapes Plex emits.
			if res.SubtitleExtract != nil {
				if err := extractSubtitleStream(ctx, res.SubtitleExtract); err != nil {
					http.Error(w, "subtitle extract: "+err.Error(), http.StatusInternalServerError)
					return
				}
				log.Printf("session %s: extracted subtitle %s → %s",
					req.SessionID, res.SubtitleExtract.StreamSpec, res.SubtitleExtract.OutputFile)
			}
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

	task := &runningTask{
		cmd:         cmd,
		cancel:      cancel,
		argv:        finalArgs,
		env:         finalEnv,
		cwd:         req.Cwd,
		sourcePath:  extractInputPath(finalArgs),
		progressURL: progressURL,
		seekOffsetS: seekOffsetSeconds,
		startedAt:   spawnedAt,
	}
	registry.register(req.SessionID, task)
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
		// Track chunk sequence for /task/<id>/checkpoint resumption.
		// scaleplex-ffmpeg7 handles chunk renumber + tfdt rebasing
		// natively (dashenc patch 0095), so the watcher only observes
		// — no renames, no in-place mp4 box surgery.
		go watchChunkSequence(ctx, req.Cwd, req.SessionID, &task.lastSeq)
		if isMatroskaSegment {
			// Worker patcher no longer applies the Cluster.Timecode
			// shift — the relay does that in-line with each CSV POST
			// to PMS (only synchronous way to win the race against
			// mpv parsing chunk-0). Watcher stays only to bump
			// task.lastSeq for /task/<id>/checkpoint reporting; pass
			// offsetMs=0 so patching is skipped.
			go watchAndPatchMatroskaChunks(ctx, req.Cwd, req.SessionID, 0, &task.lastSeq)
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
		throttle := &throttleSignal{}
		rc := reportContext{
			URL:       progressURL,
			SessionID: req.SessionID,
			Streams:   streams,
			DurationS: probeDurationSeconds(ctx, inputPath),
			Throttle:  throttle,
		}
		go throttleController(ctx, req.SessionID, cmd.Process.Pid, throttle)
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

	rawTail := stderrTail.String()
	const maxTail = 1024
	tail := rawTail
	if len(tail) > maxTail {
		tail = "..." + tail[len(tail)-maxTail:]
	}
	tailEscaped := strings.ReplaceAll(strings.ReplaceAll(tail, "\r", "\\r"), "\n", "\\n")

	if waitErr != nil {
		// Was it killed via context (registry.kill or client disconnect)
		// vs ffmpeg crash? Distinguish for the metric.
		if ctx.Err() != nil {
			metricSessionsTotal.WithLabelValues("killed").Inc()
		} else {
			metricSessionsTotal.WithLabelValues("error").Inc()
		}
		fmt.Fprintf(resp, "[scaleplex] ffmpeg exit: %v\n", waitErr)
		log.Printf("session %s: ffmpeg exit: %v stderr_tail=%s", req.SessionID, waitErr, tailEscaped)
	} else {
		metricSessionsTotal.WithLabelValues("success").Inc()
		fmt.Fprintf(resp, "[scaleplex] ffmpeg exit: success\n")
		log.Printf("session %s: ffmpeg ok", req.SessionID)
	}

	// Stamp the outcome onto the corpus capture so replay can flag
	// historical regressions (this argv exited 0 once but exits N now).
	if os.Getenv("WORKER_DUMP_ARGV") == "1" {
		oc := captureOutcome{
			DurationMs: time.Since(spawnedAt).Milliseconds(),
			StderrTail: tail,
			Segments:   countSegments(req.Cwd),
			EndedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		if waitErr == nil {
			oc.ExitStatus = 0
		} else if exitErr, ok := waitErr.(*exec.ExitError); ok {
			oc.ExitStatus = exitErr.ExitCode()
			if exitErr.ProcessState != nil && !exitErr.ProcessState.Exited() {
				oc.Signal = exitErr.ProcessState.String()
			}
		} else {
			oc.ExitStatus = -1
			oc.Signal = waitErr.Error()
		}
		persistArgvOutcome(req.SessionID, oc)
	}
}

// countSegments counts ts/m4s segment files written under the per-session
// cwd. Cheap stat-only walk; logs failures and returns 0. Cwd may be
// empty (spawned without cwd) — treat as 0.
func countSegments(cwd string) int {
	if cwd == "" {
		return 0
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".m4s") {
			n++
		}
	}
	return n
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
	switch suffix {
	case "kill":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !registry.kill(id) {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "checkpoint":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		handleCheckpoint(w, id)
	default:
		http.Error(w, "unknown task subpath", http.StatusNotFound)
	}
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

// captureClientInfo extracts Plex client identification from the env
// the shim forwards. Plex Transcoder inherits X_PLEX_* vars from the
// parent PMS process, so the shim's collectEnv() picks them up. Lets
// corpus analysis cluster bugs by client class (PS4 vs LG WebOS vs
// Android vs Apple TV). Returns nil if no identifying env present.
func captureClientInfo(env map[string]string) *captureClient {
	if env == nil {
		return nil
	}
	c := &captureClient{
		Product:    env["X_PLEX_PRODUCT"],
		DeviceName: env["X_PLEX_DEVICE_NAME"],
		Platform:   env["X_PLEX_PLATFORM"],
		Version:    env["X_PLEX_VERSION"],
		Username:   env["X_PLEX_USERNAME"],
	}
	if c.Product == "" && c.DeviceName == "" && c.Platform == "" && c.Version == "" && c.Username == "" {
		return nil
	}
	return c
}

type captureClient struct {
	Product    string `json:"product,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	Platform   string `json:"platform,omitempty"`
	Version    string `json:"version,omitempty"`
	Username   string `json:"username,omitempty"`
}

// captureOutcome is stamped onto the JSON after ffmpeg exits. Turns the
// corpus into a regression-detection set: replay can compare a session's
// historical outcome (exit_status, segments_created, stderr_tail) to
// what the current rewriter produces.
type captureOutcome struct {
	ExitStatus int    `json:"exit_status"`
	Signal     string `json:"signal,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Segments   int    `json:"segments_created,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
	EndedAt    string `json:"ended_at"`
}

// persistArgvCapture writes a JSON capture of the PMS argv to the
// shared corpus dir (default /transcode/_argv-corpus on NFS). Survives
// pod restarts, accessible from anywhere the NFS is mounted. The
// captures feed cmd/argv-extract for pattern recognition and seed
// rewriter test fixtures. Best-effort — failures are logged but never
// fail the session.
func persistArgvCapture(sessionID, cwd string, args []string, env map[string]string) {
	dir := os.Getenv("WORKER_ARGV_CORPUS_DIR")
	if dir == "" {
		dir = "/transcode/_argv-corpus"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("argv-capture mkdir %s: %v", dir, err)
		return
	}
	path := filepath.Join(dir, sessionID+".json")
	if _, err := os.Stat(path); err == nil {
		return // idempotent
	}
	type capture struct {
		SessionID  string            `json:"session_id"`
		CapturedAt string            `json:"captured_at"`
		WorkerPod  string            `json:"worker_pod,omitempty"`
		WorkerHost string            `json:"worker_host,omitempty"`
		Cwd        string            `json:"session_cwd,omitempty"`
		Argv       []string          `json:"argv"`
		Env        map[string]string `json:"env,omitempty"`
		Client     *captureClient    `json:"client,omitempty"`
		Outcome    *captureOutcome   `json:"outcome,omitempty"`
	}
	host, _ := os.Hostname()
	c := capture{
		SessionID:  sessionID,
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		WorkerPod:  os.Getenv("HOSTNAME"),
		WorkerHost: host,
		Cwd:        cwd,
		Argv:       args,
		Env:        env,
		Client:     captureClientInfo(env),
	}
	if err := writeCaptureJSON(path, dir, sessionID, &c); err != nil {
		log.Printf("argv-capture %s: %v", sessionID, err)
	}
}

// persistArgvOutcome merges an outcome record into an existing capture
// JSON. Best-effort: missing capture (corpus dir down, capture skipped
// for this session) is logged and ignored. Atomic via tmp+rename so
// concurrent sweeps never read a half-written record.
func persistArgvOutcome(sessionID string, outcome captureOutcome) {
	dir := os.Getenv("WORKER_ARGV_CORPUS_DIR")
	if dir == "" {
		dir = "/transcode/_argv-corpus"
	}
	path := filepath.Join(dir, sessionID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		// Capture didn't land (capture disabled / NFS hiccup) — nothing
		// to merge into.
		return
	}
	var existing map[string]any
	if err := json.Unmarshal(raw, &existing); err != nil {
		log.Printf("argv-outcome %s decode: %v", sessionID, err)
		return
	}
	existing["outcome"] = outcome
	tmp, err := os.CreateTemp(dir, sessionID+".outcome.*.tmp")
	if err != nil {
		log.Printf("argv-outcome create %s: %v", sessionID, err)
		return
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&existing); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		log.Printf("argv-outcome encode %s: %v", sessionID, err)
		return
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		log.Printf("argv-outcome rename %s: %v", sessionID, err)
	}
}

func writeCaptureJSON(path, dir, sessionID string, c any) error {
	tmp, err := os.CreateTemp(dir, sessionID+".*.tmp")
	if err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("encode: %w", err)
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("rename: %w", err)
	}
	return nil
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
