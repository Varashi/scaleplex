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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
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
		log.Printf("pre-warm complete in %s", time.Since(t0))
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
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

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	finalArgs := req.Args
	finalEnv := req.Env
	if req.Rewrite {
		res := Rewrite(req.Args, req.Env, nil)
		if res.Applied {
			finalArgs = res.Args
			finalEnv = res.Env
			log.Printf("session %s: rewriter applied: %s", req.SessionID, strings.Join(res.Changes, ","))
		} else {
			log.Printf("session %s: rewriter NOT applied (%s) — running original args", req.SessionID, strings.Join(res.Changes, ","))
		}
	}

	cmd := exec.CommandContext(ctx, ffmpegBin, finalArgs...)
	cmd.Env = buildEnv(finalEnv)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM, Setpgid: true}

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
		http.Error(w, "spawn: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("session %s: spawned ffmpeg pid=%d", req.SessionID, cmd.Process.Pid)

	registry.register(req.SessionID, &runningTask{cmd: cmd, cancel: cancel})
	defer registry.unregister(req.SessionID)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Scaleplex-Pid", fmt.Sprintf("%d", cmd.Process.Pid))
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Stream both pipes to the response so the orchestrator/shim can see
	// what's happening. Multiplex by prefixing.
	streamDone := make(chan struct{}, 2)
	go streamPrefixed(stdout, w, "[stdout] ", streamDone)
	go streamPrefixed(stderr, w, "[stderr] ", streamDone)

	waitErr := cmd.Wait()
	<-streamDone
	<-streamDone

	if waitErr != nil {
		fmt.Fprintf(w, "[scaleplex] ffmpeg exit: %v\n", waitErr)
		log.Printf("session %s: ffmpeg exit: %v", req.SessionID, waitErr)
	} else {
		fmt.Fprintf(w, "[scaleplex] ffmpeg exit: success\n")
		log.Printf("session %s: ffmpeg ok", req.SessionID)
	}
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

func streamPrefixed(rc io.ReadCloser, w io.Writer, prefix string, done chan<- struct{}) {
	defer rc.Close()
	defer func() { done <- struct{}{} }()
	buf := make([]byte, 4096)
	flusher, _ := w.(http.Flusher)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			fmt.Fprint(w, prefix)
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
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
