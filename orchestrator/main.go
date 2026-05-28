// scaleplex-orchestrator — phase 3.
//
// Receives task envelopes from the PMS-side shim, picks the least-loaded
// worker, forwards the request, and streams the worker's response back.
// No arg rewriting (worker handles that). No socket.io. No LOCAL_RELAY.
//
// Worker discovery — three coexisting sources, deduped by URL:
//   1. DNS  — lookup of WORKERS_DNS (k8s headless Service or any A-record
//      set), re-resolved every WORKERS_REFRESH_SECONDS. Default for k8s
//      and docker compose (Docker's embedded DNS resolves a service name
//      to all replica IPs as multiple A records).
//   2. LIST — static comma-separated host[:port] list in WORKERS_LIST,
//      loaded once at startup. For fixed multi-host deployments without
//      a DNS layer.
//   3. PUSH — worker self-registers via POST /register on startup +
//      heartbeat. Activated when the worker has SCALEPLEX_ORCHESTRATOR_URL
//      set. Substrate for friction-free Docker multi-host and (future)
//      autoscaling. Reaped after 15s with no heartbeat.
//
// Each worker tracks which source(s) vouch for it (sources bitset). A
// worker is removed only when its source set becomes empty — e.g. a
// LIST + DNS worker survives a DNS dropout because LIST still vouches.
//
// Each worker's load is polled via GET /capability every
// WORKERS_PROBE_SECONDS regardless of source.
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
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
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
	// PUSH heartbeat reaping. Worker sends a /register every
	// defaultPushHeartbeatSeconds; orchestrator reaps workers whose
	// last heartbeat is older than defaultPushTimeoutSeconds.
	defaultPushReapSeconds    = 5
	defaultPushTimeoutSeconds = 15
)

type capabilityResponse struct {
	FFmpegOK       bool    `json:"ffmpeg_ok"`
	ActiveSessions int     `json:"active_sessions"`
	MaxSessions    int     `json:"max_sessions"`
	GPULoad        float64 `json:"gpu_load"`    // 0..1 mean across video engines
	GPUEngines     int     `json:"gpu_engines"` // vcs/vecs engine count
	Backend        string  `json:"backend"`     // "vaapi" | "nvenc" | "sw" (#77 PR4)
}

// sourceBits is a bit-set of the discovery sources currently vouching
// for a worker. A worker stays in the pool while any source bit is set.
type sourceBits uint8

const (
	srcDNS  sourceBits = 1 << 0
	srcList sourceBits = 1 << 1
	srcPush sourceBits = 1 << 2
)

func (s sourceBits) String() string {
	parts := make([]string, 0, 3)
	if s&srcDNS != 0 {
		parts = append(parts, "dns")
	}
	if s&srcList != 0 {
		parts = append(parts, "list")
	}
	if s&srcPush != 0 {
		parts = append(parts, "push")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

type worker struct {
	host    string     // hostname or IP (whatever the source provided)
	url     string     // http://<host>:<port>
	sources sourceBits // which discovery sources vouch for this worker

	mu             sync.Mutex
	healthy        bool
	activeSessions int   // last reported by /capability poll (5s stale)
	inFlight       int32 // dispatched-here-but-not-yet-finished count
	maxSessions    int
	gpuLoad        float64 // 0..1 mean across video engines (last reported)
	gpuEngines     int
	backend        string // "vaapi" | "nvenc" | "sw" (#77 PR4 — routing tier)
	lastErr        string
	lastUpdated    time.Time
	lastHeartbeat  time.Time // only meaningful when sources&srcPush != 0
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

// isHW reports whether this worker has a hardware backend (vaapi/nvenc). A
// worker that hasn't reported a backend yet ("" — e.g. a pre-PR4 image during a
// rolling upgrade) is treated as HW, so it stays in the primary tier rather than
// being demoted to the SW overflow tier.
func (w *worker) isHW() bool {
	w.mu.Lock()
	b := w.backend
	w.mu.Unlock()
	return b != "sw"
}

// sessions is the active+in-flight session count, the ranking key for the
// least-sessions strategy.
func (w *worker) sessions() int {
	w.mu.Lock()
	a := w.activeSessions
	w.mu.Unlock()
	return a + int(atomic.LoadInt32(&w.inFlight))
}

func (w *worker) backendStr() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.backend == "" {
		return "unknown"
	}
	return w.backend
}

type pool struct {
	mu      sync.RWMutex
	workers map[string]*worker // keyed by url ("http://host:port")

	// Scheduling policy, set once from env at startup (#77 PR4). Zero values
	// are the defaults: lbLoad + tiered (flatPool=false) = HW-preferred,
	// least-loaded — the pre-PR4 behavior on a homogeneous fleet.
	strategy lbStrategy
	flatPool bool   // SCALEPLEX_PREFER_HW=0 → one flat pool (no HW/SW tier split)
	rrCursor uint64 // round-robin dispatch cursor (atomic)
}

// hostPort is the parsed form of a single WORKERS_LIST entry.
type hostPort struct {
	host string
	port int
}

func (hp hostPort) url() string {
	return fmt.Sprintf("http://%s:%d", hp.host, hp.port)
}

// parseWorkerList parses the WORKERS_LIST env: comma-separated host[:port]
// entries. Whitespace tolerant. Empty input → nil. Malformed entries are
// skipped with a log warning; the rest of the list is honored. Entries
// without an explicit :port get defaultPort.
func parseWorkerList(s string, defaultPort int) []hostPort {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []hostPort
	for _, raw := range strings.Split(s, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		host := entry
		port := defaultPort
		if i := strings.LastIndex(entry, ":"); i >= 0 {
			host = strings.TrimSpace(entry[:i])
			portStr := strings.TrimSpace(entry[i+1:])
			p, err := strconv.Atoi(portStr)
			if err != nil {
				log.Printf("list: skipping %q (bad port %q: %v)", entry, portStr, err)
				continue
			}
			if p < 1 || p > 65535 {
				log.Printf("list: skipping %q (port %d out of range)", entry, p)
				continue
			}
			port = p
		}
		if host == "" {
			log.Printf("list: skipping %q (empty host)", entry)
			continue
		}
		out = append(out, hostPort{host: host, port: port})
	}
	return out
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
		url := fmt.Sprintf("http://%s:%d", ip, port)
		seen[url] = struct{}{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for url := range seen {
		if w, ok := p.workers[url]; ok {
			if w.sources&srcDNS == 0 {
				w.sources |= srcDNS
				log.Printf("dns: %s now also vouched by DNS (sources=%s)", url, w.sources)
			}
			continue
		}
		// Extract host from "http://<host>:<port>" for the worker's
		// host field (used for logging + future capability reporting).
		host := strings.TrimPrefix(url, "http://")
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		p.workers[url] = &worker{host: host, url: url, sources: srcDNS}
		log.Printf("dns: added worker %s", url)
	}
	for url, w := range p.workers {
		if w.sources&srcDNS == 0 {
			continue
		}
		if _, ok := seen[url]; ok {
			continue
		}
		w.sources &^= srcDNS
		if w.sources == 0 {
			log.Printf("dns: removed worker %s", url)
			delete(p.workers, url)
		} else {
			log.Printf("dns: %s dropped from DNS but kept by %s", url, w.sources)
		}
	}
}

// loadList vouches for each entry under the LIST source. Idempotent —
// re-applying the same list (e.g. on SIGHUP, future) just re-sets the
// bit. Pre-existing DNS/PUSH bits on a matching URL are preserved.
func (p *pool) loadList(entries []hostPort) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range entries {
		url := e.url()
		if w, ok := p.workers[url]; ok {
			if w.sources&srcList == 0 {
				w.sources |= srcList
				log.Printf("list: %s now also vouched by LIST (sources=%s)", url, w.sources)
			}
			continue
		}
		p.workers[url] = &worker{host: e.host, url: url, sources: srcList}
		log.Printf("list: added worker %s", url)
	}
}

// register vouches for a worker under the PUSH source and refreshes its
// last-heartbeat timestamp. Idempotent — a worker re-registering or
// heartbeating goes through this same path. Returns true if this call
// created the worker (newly seen), false if it just refreshed an
// existing one.
func (p *pool) register(host string, port int, now time.Time) bool {
	url := fmt.Sprintf("http://%s:%d", host, port)
	p.mu.Lock()
	defer p.mu.Unlock()
	if w, ok := p.workers[url]; ok {
		newBit := w.sources&srcPush == 0
		w.sources |= srcPush
		w.lastHeartbeat = now
		if newBit {
			log.Printf("push: %s now also vouched by PUSH (sources=%s)", url, w.sources)
		}
		return false
	}
	p.workers[url] = &worker{
		host:          host,
		url:           url,
		sources:       srcPush,
		lastHeartbeat: now,
	}
	log.Printf("push: added worker %s", url)
	return true
}

// reapPush clears the PUSH bit on workers whose lastHeartbeat is older
// than timeout. Workers reduced to zero sources are removed from the
// pool. Workers without the PUSH bit are ignored.
func (p *pool) reapPush(now time.Time, timeout time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for url, w := range p.workers {
		if w.sources&srcPush == 0 {
			continue
		}
		if now.Sub(w.lastHeartbeat) <= timeout {
			continue
		}
		w.sources &^= srcPush
		if w.sources == 0 {
			log.Printf("push: reaped worker %s (no heartbeat for %s)", url, now.Sub(w.lastHeartbeat).Round(time.Second))
			delete(p.workers, url)
		} else {
			log.Printf("push: %s dropped from PUSH (no heartbeat for %s) but kept by %s",
				url, now.Sub(w.lastHeartbeat).Round(time.Second), w.sources)
		}
	}
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
	w.backend = c.Backend
	w.lastErr = ""
	w.lastUpdated = time.Now()
	w.mu.Unlock()
}

// lbStrategy is the within-tier worker-ordering policy (SCALEPLEX_LB_STRATEGY).
type lbStrategy int

const (
	lbLoad          lbStrategy = iota // ascending load() — default
	lbRoundRobin                      // even rotation via an atomic cursor
	lbLeastSessions                   // fewest active+in-flight sessions
	lbRandom                          // shuffle
)

func parseLBStrategy(s string) lbStrategy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "round-robin", "roundrobin", "rr":
		return lbRoundRobin
	case "least-sessions", "least_sessions", "sessions":
		return lbLeastSessions
	case "random":
		return lbRandom
	default: // "", "load", or anything unrecognized
		return lbLoad
	}
}

func (s lbStrategy) String() string {
	switch s {
	case lbRoundRobin:
		return "round-robin"
	case lbLeastSessions:
		return "least-sessions"
	case lbRandom:
		return "random"
	default:
		return "load"
	}
}

// schedule returns the healthy workers in dispatch-preference order. The caller
// iterates and tries each in turn (a worker may 503 if it raced to cap between
// probe and request). Computed ONCE per dispatch — round-robin needs a stable
// rotation, so the order can't be re-derived per retry.
//
// Two knobs (set once from env in main):
//   - flatPool=false (default): partition into [HW tier, SW tier] and order each
//     by the strategy, HW first — so a heavy job prefers a GPU and only spills to
//     a CPU worker when the GPU tier is saturated/absent. flatPool=true
//     (SCALEPLEX_PREFER_HW=0) makes one flat pool.
//   - strategy: the within-tier ordering (load | round-robin | least-sessions |
//     random).
//
// A homogeneous fleet (all one backend) collapses to a single tier, so the
// behavior reduces to the chosen strategy — and load (the default) reproduces
// the pre-PR4 ranking exactly.
func (p *pool) schedule() []*worker {
	all := p.list()
	healthy := make([]*worker, 0, len(all))
	for _, w := range all {
		if _, _, _, ok := w.snapshot(); ok {
			healthy = append(healthy, w)
		}
	}
	var rr uint64
	if p.strategy == lbRoundRobin {
		rr = atomic.AddUint64(&p.rrCursor, 1)
	}
	if p.flatPool {
		return p.orderTier(healthy, rr)
	}
	hw := make([]*worker, 0, len(healthy))
	sw := make([]*worker, 0, len(healthy))
	for _, w := range healthy {
		if w.isHW() {
			hw = append(hw, w)
		} else {
			sw = append(sw, w)
		}
	}
	return append(p.orderTier(hw, rr), p.orderTier(sw, rr)...)
}

// orderTier sorts one tier in place per the strategy. rr is the per-dispatch
// round-robin offset (ignored by other strategies).
func (p *pool) orderTier(ws []*worker, rr uint64) []*worker {
	switch p.strategy {
	case lbLeastSessions:
		sort.SliceStable(ws, func(i, j int) bool { return ws[i].sessions() < ws[j].sessions() })
	case lbRandom:
		rand.Shuffle(len(ws), func(i, j int) { ws[i], ws[j] = ws[j], ws[i] })
	case lbRoundRobin:
		// Stable base order by url, then rotate by the per-dispatch cursor so
		// successive dispatches start on the next worker.
		sort.SliceStable(ws, func(i, j int) bool { return ws[i].url < ws[j].url })
		if n := len(ws); n > 0 {
			off := int((rr - 1) % uint64(n)) // rr is post-increment ≥1; first dispatch → offset 0
			ws = append(ws[off:], ws[:off]...)
		}
	default: // lbLoad
		sort.SliceStable(ws, func(i, j int) bool { return ws[i].load() < ws[j].load() })
	}
	return ws
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

	listEntries := parseWorkerList(os.Getenv("WORKERS_LIST"), workerPort)
	if len(listEntries) > 0 {
		pl.loadList(listEntries)
		log.Printf("workers: loaded %d static entries from WORKERS_LIST", len(listEntries))
	}

	pushReap := envSecondsOr("WORKERS_PUSH_REAP_SECONDS", defaultPushReapSeconds)
	pushTimeout := envSecondsOr("WORKERS_PUSH_TIMEOUT_SECONDS", defaultPushTimeoutSeconds)

	// Scheduling policy (#77 PR4). Defaults: load + HW-preferred tiering.
	pl.strategy = parseLBStrategy(os.Getenv("SCALEPLEX_LB_STRATEGY"))
	pl.flatPool = os.Getenv("SCALEPLEX_PREFER_HW") == "0"
	log.Printf("scheduler: strategy=%s prefer-hw=%t", pl.strategy, !pl.flatPool)

	go discoveryLoop(workersDNS, workerPort, refresh, probe)
	go pushReaperLoop(time.Duration(pushReap)*time.Second, time.Duration(pushTimeout)*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/readyz", handleReady)
	mux.HandleFunc("/workers", handleWorkers)
	mux.HandleFunc("/register", handleRegister)
	mux.HandleFunc("/task", handleTask)
	mux.HandleFunc("/task/", handleTaskByID)
	mux.Handle("/metrics", promhttp.Handler())

	log.Printf("scaleplex-orchestrator listening on %s, dns=%s:%d list=%d entries, push-reap=%ds push-timeout=%ds",
		listen, workersDNS, workerPort, len(listEntries), pushReap, pushTimeout)
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

// registerRequest is the body shape for POST /register. Fields beyond
// host/port are reserved for future use (autoscaling / per-node
// HWProfile capability tagging); unknown keys are ignored by Go's
// JSON decoder so workers can add metadata forward-compatibly.
type registerRequest struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Version string `json:"version,omitempty"`
}

// handleRegister accepts POST /register from a worker. Idempotent —
// covers both initial registration and periodic heartbeat (worker just
// re-POSTs the same body every WORKERS_PUSH_HEARTBEAT_SECONDS).
//
// Body: { "host": "...", "port": 3501, "version": "..." }
// host can be a hostname or IP; orchestrator must be able to reach
// http://host:port for /capability polling and task dispatch.
func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req registerRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host == "" {
		http.Error(w, "host required", http.StatusBadRequest)
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		http.Error(w, "port out of range", http.StatusBadRequest)
		return
	}
	pl.register(req.Host, req.Port, time.Now())
	w.WriteHeader(http.StatusNoContent)
}

// pushReaperLoop clears the PUSH bit on workers whose heartbeat has
// gone stale. Runs every reapInterval; uses pushTimeout as the
// no-heartbeat cutoff.
func pushReaperLoop(reapInterval, pushTimeout time.Duration) {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for range t.C {
		pl.reapPush(time.Now(), pushTimeout)
	}
}

// handleWorkers returns the current pool snapshot — useful for debugging
// and for the eventual /status dashboard.
func handleWorkers(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		URL            string  `json:"url"`
		Sources        string  `json:"sources"`
		Healthy        bool    `json:"healthy"`
		ActiveSessions int     `json:"active_sessions"`
		InFlight       int32   `json:"in_flight"`
		MaxSessions    int     `json:"max_sessions"`
		GPULoad        float64 `json:"gpu_load"`
		GPUEngines     int     `json:"gpu_engines"`
		Backend        string  `json:"backend"`
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
			Sources:        wk.sources.String(),
			Healthy:        wk.healthy,
			ActiveSessions: wk.activeSessions,
			InFlight:       atomic.LoadInt32(&wk.inFlight),
			MaxSessions:    wk.maxSessions,
			GPULoad:        wk.gpuLoad,
			GPUEngines:     wk.gpuEngines,
			Backend:        wk.backend,
			Load:           load,
			LastErr:        wk.lastErr,
		}
		if e.Backend == "" {
			e.Backend = "unknown" // pre-PR4 worker; scheduler classifies it as HW
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

	// Compute the candidate order ONCE per dispatch (round-robin needs a
	// stable rotation). The in-flight count is folded into load()/sessions()
	// so concurrent dispatches still spread; on a 503 we fall through to the
	// next candidate in this order.
	order := pl.schedule()
	if len(order) == 0 {
		metricDispatchTotal.WithLabelValues("no_workers").Inc()
		http.Error(w, "all workers at capacity", http.StatusServiceUnavailable)
		return
	}
	for i, wk := range order {
		log.Printf("session %s: try worker %s (load=%.3f, backend=%s, attempt=%d/%d)",
			req.SessionID, wk.url, wk.load(), wk.backendStr(), i+1, len(order))
		wk.dispatchBegin()
		// Start a checkpoint poller scoped to this PMS-facing request
		// — it shuts down when the request context is cancelled.
		pollCtx, cancelPoll := context.WithCancel(r.Context())
		go pollCheckpoint(pollCtx, wk.url, req.SessionID, req.Args)
		ok := proxyToWorker(w, r, wk.url, body, req.SessionID)
		cancelPoll()
		wk.dispatchEnd()
		if ok {
			metricDispatchAttempts.Observe(float64(i + 1))
			metricDispatchTotal.WithLabelValues("success").Inc()
			return
		}
		metricDispatchTotal.WithLabelValues("fallthrough_503").Inc()
	}
	metricDispatchTotal.WithLabelValues("all_at_cap").Inc()
	metricDispatchAttempts.Observe(float64(len(order)))
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
	for _, w := range pl.schedule() {
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
