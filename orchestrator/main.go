// scaleplex-orchestrator — phase 3.
//
// Receives task envelopes from the PMS-side shim, picks the least-loaded
// worker, forwards the request, and streams the worker's response back.
// No arg rewriting (worker handles that). No socket.io. No LOCAL_RELAY.
//
// Worker discovery: DNS lookup of WORKERS_DNS (a headless k8s Service),
// re-resolved every WORKERS_REFRESH_SECONDS. Each resolved pod IP gets
// its load polled via GET /capability every WORKERS_PROBE_SECONDS.
//
// Selection: pick the worker with the lowest (active_sessions /
// max_sessions) ratio. max_sessions=0 (unlimited) treats the worker as
// having one slot — the cheapest. If a worker returns 503 (at cap), the
// orchestrator falls through to the next candidate.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	defaultListenAddr      = ":3500"
	defaultWorkerPort      = 3501
	defaultRefreshSeconds  = 5
	defaultProbeSeconds    = 5
	defaultProbeTimeoutSec = 2
)

type capabilityResponse struct {
	FFmpegOK       bool    `json:"ffmpeg_ok"`
	ActiveSessions int     `json:"active_sessions"`
	MaxSessions    int     `json:"max_sessions"`
	GPULoad        float64 `json:"gpu_load"`    // 0..1 mean across video engines
	GPUEngines     int     `json:"gpu_engines"` // vcs/vecs engine count
}

type worker struct {
	host string // pod IP
	url  string // http://<ip>:3501

	mu             sync.Mutex
	healthy        bool
	activeSessions int   // last reported by /capability poll (5s stale)
	inFlight       int32 // dispatched-here-but-not-yet-finished count
	maxSessions    int
	gpuLoad        float64 // 0..1 mean across video engines (last reported)
	gpuEngines     int
	lastErr        string
	lastUpdated    time.Time
}

func (w *worker) snapshot() (active, max int, gpuLoad float64, healthy bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.activeSessions, w.maxSessions, w.gpuLoad, w.healthy
}

// load is the ranking score. Takes the dominant of two signals:
//  1. session-count saturation: (active+inflight)/max  — caps spawn rate
//  2. GPU saturation: mean busy% across video engines — caps real work
//
// Multi-engine GPUs (Arc A310 has 2 vcs engines) naturally report half
// the gpu_load for the same number of sessions, so this score biases
// routing toward them — exactly the behaviour we want when scheduling
// a new transcode.
//
// In-flight count is added to active so two concurrent requests don't
// both pick the same worker while /capability is mid-stale.
func (w *worker) load() float64 {
	active, max, gpuLoad, healthy := w.snapshot()
	if !healthy {
		return float64(1 << 30)
	}
	combined := float64(active) + float64(atomic.LoadInt32(&w.inFlight))
	var sessLoad float64
	if max > 0 {
		sessLoad = combined / float64(max)
	} else {
		sessLoad = combined
	}
	if gpuLoad > sessLoad {
		return gpuLoad
	}
	return sessLoad
}

func (w *worker) dispatchBegin() { atomic.AddInt32(&w.inFlight, 1) }
func (w *worker) dispatchEnd()   { atomic.AddInt32(&w.inFlight, -1) }

type pool struct {
	mu      sync.RWMutex
	workers map[string]*worker // keyed by host (pod IP)
}

func (p *pool) list() []*worker {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*worker, 0, len(p.workers))
	for _, w := range p.workers {
		out = append(out, w)
	}
	return out
}

func (p *pool) get(url string) *worker {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, w := range p.workers {
		if w.url == url {
			return w
		}
	}
	return nil
}

func (p *pool) refresh(dns string, port int) {
	ips, err := net.LookupHost(dns)
	if err != nil {
		log.Printf("dns: lookup %s: %v", dns, err)
		return
	}
	seen := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		seen[ip] = struct{}{}
	}
	p.mu.Lock()
	for ip := range seen {
		if _, ok := p.workers[ip]; !ok {
			p.workers[ip] = &worker{host: ip, url: fmt.Sprintf("http://%s:%d", ip, port)}
			log.Printf("dns: added worker %s", p.workers[ip].url)
		}
	}
	for ip, w := range p.workers {
		if _, ok := seen[ip]; !ok {
			log.Printf("dns: removed worker %s", w.url)
			delete(p.workers, ip)
		}
	}
	p.mu.Unlock()
}

// probeAll fans out a /capability poll per worker. Cheap (one HTTP GET
// each), parallel, bounded by individual timeout.
func (p *pool) probeAll(client *http.Client) {
	for _, w := range p.list() {
		go probeWorker(client, w)
	}
}

func probeWorker(client *http.Client, w *worker) {
	req, err := http.NewRequest(http.MethodGet, w.url+"/capability", nil)
	if err != nil {
		w.mu.Lock()
		w.healthy = false
		w.lastErr = err.Error()
		w.mu.Unlock()
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		w.mu.Lock()
		w.healthy = false
		w.lastErr = err.Error()
		w.lastUpdated = time.Now()
		w.mu.Unlock()
		return
	}
	defer resp.Body.Close()
	var c capabilityResponse
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		w.mu.Lock()
		w.healthy = false
		w.lastErr = "bad json: " + err.Error()
		w.lastUpdated = time.Now()
		w.mu.Unlock()
		return
	}
	w.mu.Lock()
	w.healthy = c.FFmpegOK
	w.activeSessions = c.ActiveSessions
	w.maxSessions = c.MaxSessions
	w.gpuLoad = c.GPULoad
	w.gpuEngines = c.GPUEngines
	w.lastErr = ""
	w.lastUpdated = time.Now()
	w.mu.Unlock()
}

// pickWorker returns workers ordered by ascending load. The caller iterates
// and tries each in turn (worker may 503 if it raced to cap between
// probe and request).
func (p *pool) pickOrder() []*worker {
	all := p.list()
	healthy := make([]*worker, 0, len(all))
	for _, w := range all {
		if _, _, _, ok := w.snapshot(); ok {
			healthy = append(healthy, w)
		}
	}
	// stable sort by load ascending
	for i := 1; i < len(healthy); i++ {
		for j := i; j > 0 && healthy[j].load() < healthy[j-1].load(); j-- {
			healthy[j], healthy[j-1] = healthy[j-1], healthy[j]
		}
	}
	return healthy
}

// sessionTracker records which worker URL a given session_id was sent to,
// so /task/<id>/kill knows where to forward.
type sessionTracker struct {
	mu sync.RWMutex
	m  map[string]string
}

func (s *sessionTracker) set(id, workerURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = workerURL
}
func (s *sessionTracker) get(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[id]
}
func (s *sessionTracker) del(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// taskRequest is the envelope sent by the PMS-side shim. Verbatim shape
// of the worker's expected body — orchestrator forwards untouched.
type taskRequest struct {
	SessionID string            `json:"session_id"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Rewrite   bool              `json:"rewrite,omitempty"`
}

var (
	pl       = &pool{workers: make(map[string]*worker)}
	sessions = &sessionTracker{m: make(map[string]string)}

	probeClient = &http.Client{Timeout: time.Duration(defaultProbeTimeoutSec) * time.Second}
	// proxyClient has no per-request timeout — task streams are arbitrarily
	// long. Cancellation comes via request context (client disconnect).
	proxyClient = &http.Client{Timeout: 0}
)

func main() {
	listen := envOr("LISTEN_ADDR", defaultListenAddr)
	workersDNS := envOr("WORKERS_DNS", "scaleplex-worker.scaleplex.svc.cluster.local")
	workerPort, _ := strconv.Atoi(envOr("WORKER_PORT", strconv.Itoa(defaultWorkerPort)))
	refresh := envSecondsOr("WORKERS_REFRESH_SECONDS", defaultRefreshSeconds)
	probe := envSecondsOr("WORKERS_PROBE_SECONDS", defaultProbeSeconds)

	go discoveryLoop(workersDNS, workerPort, refresh, probe)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/readyz", handleReady)
	mux.HandleFunc("/workers", handleWorkers)
	mux.HandleFunc("/task", handleTask)
	mux.HandleFunc("/task/", handleTaskByID)
	mux.Handle("/metrics", promhttp.Handler())

	log.Printf("scaleplex-orchestrator listening on %s, workers=%s:%d", listen, workersDNS, workerPort)
	srv := &http.Server{Addr: listen, Handler: mux}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

func discoveryLoop(dns string, port, refreshSec, probeSec int) {
	pl.refresh(dns, port)
	pl.probeAll(probeClient)

	refreshTick := time.NewTicker(time.Duration(refreshSec) * time.Second)
	probeTick := time.NewTicker(time.Duration(probeSec) * time.Second)
	defer refreshTick.Stop()
	defer probeTick.Stop()
	updateWorkerMetrics()
	for {
		select {
		case <-refreshTick.C:
			pl.refresh(dns, port)
			updateWorkerMetrics()
		case <-probeTick.C:
			pl.probeAll(probeClient)
			// Probes run concurrently in goroutines; give them a moment
			// to settle before we re-snapshot the pool for metrics.
			time.AfterFunc(time.Duration(defaultProbeTimeoutSec+1)*time.Second, updateWorkerMetrics)
		}
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ok\n")
}

func handleReady(w http.ResponseWriter, r *http.Request) {
	for _, wk := range pl.list() {
		if _, _, _, healthy := wk.snapshot(); healthy {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "ready\n")
			return
		}
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	io.WriteString(w, "no healthy workers\n")
}

// handleWorkers returns the current pool snapshot — useful for debugging
// and for the eventual /status dashboard.
func handleWorkers(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		URL            string  `json:"url"`
		Healthy        bool    `json:"healthy"`
		ActiveSessions int     `json:"active_sessions"`
		InFlight       int32   `json:"in_flight"`
		MaxSessions    int     `json:"max_sessions"`
		GPULoad        float64 `json:"gpu_load"`
		GPUEngines     int     `json:"gpu_engines"`
		Load           float64 `json:"load"`
		LastErr        string  `json:"last_err,omitempty"`
		LastUpdatedAgo string  `json:"last_updated_ago,omitempty"`
	}
	out := []entry{}
	for _, wk := range pl.list() {
		load := wk.load()
		wk.mu.Lock()
		e := entry{
			URL:            wk.url,
			Healthy:        wk.healthy,
			ActiveSessions: wk.activeSessions,
			InFlight:       atomic.LoadInt32(&wk.inFlight),
			MaxSessions:    wk.maxSessions,
			GPULoad:        wk.gpuLoad,
			GPUEngines:     wk.gpuEngines,
			Load:           load,
			LastErr:        wk.lastErr,
		}
		if !wk.lastUpdated.IsZero() {
			e.LastUpdatedAgo = time.Since(wk.lastUpdated).Round(time.Millisecond).String()
		}
		wk.mu.Unlock()
		out = append(out, e)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func handleTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req taskRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}

	// Phase 4b/4d: if Plex's transcoder supervisor is retrying after
	// a worker crash or graceful migration, our checkpoint cache keyed
	// on the Plex transcode-session-UUID will hold the last segment
	// seq. Inject resume flags before forwarding so the worker picks
	// up exactly where the previous one stopped instead of redoing
	// chunks the client has already buffered.
	if newArgs, resumed := resumeIfApplicable(req.Args); resumed {
		req.Args = newArgs
		// Re-encode the body so the worker sees the rewritten argv.
		if nb, err := json.Marshal(req); err == nil {
			body = nb
		}
		metricDispatchTotal.WithLabelValues("resumed").Inc()
	}

	// Re-pick before each try so the in-flight count from earlier
	// dispatches is reflected — sequential candidates after a 503 should
	// also be ranked freshly.
	tried := make(map[*worker]bool)
	for {
		var wk *worker
		for _, c := range pl.pickOrder() {
			if !tried[c] {
				wk = c
				break
			}
		}
		if wk == nil {
			break
		}
		tried[wk] = true
		log.Printf("session %s: try worker %s (load=%.3f, attempt=%d)", req.SessionID, wk.url, wk.load(), len(tried))
		wk.dispatchBegin()
		// Start a checkpoint poller scoped to this PMS-facing request
		// — it shuts down when the request context is cancelled.
		pollCtx, cancelPoll := context.WithCancel(r.Context())
		go pollCheckpoint(pollCtx, wk.url, req.SessionID, req.Args)
		ok := proxyToWorker(w, r, wk.url, body, req.SessionID)
		cancelPoll()
		wk.dispatchEnd()
		if ok {
			metricDispatchAttempts.Observe(float64(len(tried)))
			metricDispatchTotal.WithLabelValues("success").Inc()
			return
		}
		metricDispatchTotal.WithLabelValues("fallthrough_503").Inc()
	}
	if len(tried) == 0 {
		metricDispatchTotal.WithLabelValues("no_workers").Inc()
	} else {
		metricDispatchTotal.WithLabelValues("all_at_cap").Inc()
		metricDispatchAttempts.Observe(float64(len(tried)))
	}
	http.Error(w, "all workers at capacity", http.StatusServiceUnavailable)
}

// proxyToWorker forwards the PMS-facing POST to a worker and streams
// the worker's chunked response back. Returns true on success / fatal
// PMS-side write error / non-503 worker error (meaning "don't try
// another worker"); false on 503 ("try the next candidate").
//
// Phase 4d: if the worker stream errors mid-flight (pod death, network
// drop) AFTER we've already started streaming bytes back to PMS, we
// transparently swap to a healthy alternative worker — using the
// checkpoint cache to inject resume flags so the new ffmpeg picks up
// where the old one stopped. The PMS-facing connection stays open;
// the shim sees a brief stall (1-3s for the new worker to spin up)
// then segments resume.
func proxyToWorker(w http.ResponseWriter, r *http.Request, workerURL string, body []byte, sessionID string) bool {
	currentURL := workerURL
	currentBody := body
	headersWritten := false
	tried := map[string]bool{currentURL: true}

	for attempt := 0; attempt < 3; attempt++ {
		ok, status, didHeader, midStream := streamFromUpstream(
			w, r, currentURL, currentBody, sessionID, headersWritten,
		)
		if status == http.StatusServiceUnavailable && !headersWritten {
			// 503 from initial connect — caller should pick the next candidate.
			return false
		}
		if didHeader {
			headersWritten = true
		}
		if ok {
			// Upstream finished cleanly OR PMS-side write failed
			// (client gone). Either way we're done.
			return true
		}
		if !midStream {
			// Initial-connect failure with no bytes flowed and not 503;
			// surface as 502 for caller telemetry. http.Error already
			// fired in streamFromUpstream → done.
			return true
		}
		// Mid-stream failure with PMS still listening. Try recovery.
		next := pickRecoveryWorker(tried)
		if next == nil {
			log.Printf("session %s: mid-stream failure on %s, no alt worker available", sessionID, currentURL)
			return true
		}
		log.Printf("session %s: mid-stream failure on %s, recovering to %s (attempt=%d)", sessionID, currentURL, next.url, attempt+1)
		metricDispatchTotal.WithLabelValues("recovered").Inc()
		// Inject resume flags from checkpoint cache into the body
		// (idempotent — replaces values if already injected).
		var req taskRequest
		if err := json.Unmarshal(currentBody, &req); err == nil {
			if newArgs, resumed := resumeIfApplicable(req.Args); resumed {
				req.Args = newArgs
				if nb, err := json.Marshal(req); err == nil {
					currentBody = nb
				}
			}
		}
		currentURL = next.url
		tried[currentURL] = true
	}
	log.Printf("session %s: recovery exhausted attempts", sessionID)
	return true
}

// streamFromUpstream opens one connection to the named worker and
// pipes its body to w until either side errors. On the very first
// successful response it writes the PMS-facing status+headers (gated
// by `headersWritten`).
//
// Returns:
//   - ok        : true if upstream completed cleanly OR pms-side write failed
//   - status    : HTTP status from upstream (0 if dial failed)
//   - didHeader : true if we wrote PMS-facing headers this call
//   - midStream : true if the failure happened AFTER any bytes flowed
//     (signals 4d crash-recovery should kick in)
func streamFromUpstream(
	w http.ResponseWriter,
	r *http.Request,
	workerURL string,
	body []byte,
	sessionID string,
	headersWritten bool,
) (ok bool, status int, didHeader bool, midStream bool) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	preq, err := http.NewRequestWithContext(ctx, http.MethodPost, workerURL+"/task", strings.NewReader(string(body)))
	if err != nil {
		if !headersWritten {
			http.Error(w, "build request: "+err.Error(), http.StatusInternalServerError)
		}
		return true, 0, false, false
	}
	preq.Header.Set("Content-Type", "application/json")
	resp, err := proxyClient.Do(preq)
	if err != nil {
		log.Printf("session %s: worker %s error: %v", sessionID, workerURL, err)
		if !headersWritten {
			http.Error(w, "worker dial: "+err.Error(), http.StatusBadGateway)
		}
		return true, 0, false, false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		_, _ = io.Copy(io.Discard, resp.Body)
		log.Printf("session %s: worker %s at capacity, falling through", sessionID, workerURL)
		return false, http.StatusServiceUnavailable, false, false
	}

	sessions.set(sessionID, workerURL)
	defer sessions.del(sessionID)

	if !headersWritten {
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		didHeader = true
	}
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	bytesFlowed := false
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			bytesFlowed = true
			if _, werr := w.Write(buf[:n]); werr != nil {
				// PMS gone — give up entirely, no recovery.
				return true, resp.StatusCode, didHeader, false
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return true, resp.StatusCode, didHeader, false
			}
			// Mid-stream failure = recover. Initial failure = return cleanly
			// (the caller's outer loop is responsible for picking another
			// worker via pickOrder, just like a 503).
			return false, resp.StatusCode, didHeader, bytesFlowed
		}
	}
}

// pickRecoveryWorker returns a healthy worker not in `tried`, ranked by
// the same load score as fresh dispatch. Distinct from the outer
// dispatch loop because we may need to swap to a previously-tried-and-
// rejected (503) worker if it's the only healthy option left.
func pickRecoveryWorker(tried map[string]bool) *worker {
	for _, w := range pl.pickOrder() {
		if !tried[w.url] {
			return w
		}
	}
	return nil
}

func handleTaskByID(w http.ResponseWriter, r *http.Request) {
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
	workerURL := sessions.get(id)
	if workerURL == "" {
		metricKillTotal.WithLabelValues("not_found").Inc()
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	preq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, workerURL+"/task/"+id+"/kill", nil)
	resp, err := proxyClient.Do(preq)
	if err != nil {
		metricKillTotal.WithLabelValues("error").Inc()
		http.Error(w, "kill forward: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	metricKillTotal.WithLabelValues("success").Inc()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func envOr(k, dflt string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return dflt
}

func envSecondsOr(k string, dflt int) int {
	v := os.Getenv(k)
	if v == "" {
		return dflt
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return dflt
	}
	return n
}
