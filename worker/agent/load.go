package main

// load — multi-engine GPU busy% sampling for /capability + routing.
//
// Intel iGPU and Arc cards expose per-engine ns counters under
// /sys/class/drm/card<N>/engine/<name>/busy. Engine names:
//   rcs0      render (3D / compute)         — not used by transcode
//   bcs0      blitter                       — not used
//   vcs0..N   video (decode + encode)       — what we care about
//   vecs0..N  video enhance                 — used by some VAAPI filters
//
// Arc A310 has 2 vcs engines: a single transcode session uses one,
// a second concurrent session lands on the other without contention.
// Mean busy% across video engines is therefore the right routing
// signal: 1 session → 50%, 2 sessions → 100%, etc.
//
// A dedicated sampler goroutine reads counters every 2s; the
// /capability handler reads the cached float64 and reports it as
// gpu_load (0..1). Orchestrator routing uses max(gpu_load, sessions
// /max) so whichever signal is closer to saturation drives the choice.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var metricGPUEngineLoad = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "scaleplex_worker_gpu_engine_load",
	Help: "Mean busy fraction across video (vcs/vecs) engines, 0..1.",
})

// engineSamplerInst is the process-wide sampler started in main(); the
// /capability handler reads its cached load.
var engineSamplerInst *engineSampler

// videoEngineBusyPaths returns absolute paths to all video-class
// engine busy files. Override via WORKER_GPU_ENGINES (comma-separated)
// for tests or unusual kernel layouts.
func videoEngineBusyPaths() []string {
	if v := os.Getenv("WORKER_GPU_ENGINES"); v != "" {
		out := make([]string, 0)
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	vcs, _ := filepath.Glob("/sys/class/drm/card*/engine/vcs*/busy")
	vecs, _ := filepath.Glob("/sys/class/drm/card*/engine/vecs*/busy")
	return append(vcs, vecs...)
}

type engineSampler struct {
	mu       sync.Mutex
	paths    []string
	lastBusy []uint64
	lastTime time.Time
	cached   float64 // last computed mean fraction, 0..1
}

func newEngineSampler() *engineSampler {
	return &engineSampler{paths: videoEngineBusyPaths()}
}

func readUint64File(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// sample reads counters and recomputes the cached mean. First call
// only establishes a baseline (returns 0). Returns the new cached
// value.
func (s *engineSampler) sample() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.paths) == 0 {
		s.cached = 0
		return 0
	}
	cur := make([]uint64, len(s.paths))
	for i, p := range s.paths {
		v, err := readUint64File(p)
		if err != nil {
			continue // engine vanished or unreadable; treat as 0 contribution
		}
		cur[i] = v
	}
	if s.lastBusy == nil {
		s.lastBusy = cur
		s.lastTime = now
		return s.cached // 0 on first sample
	}
	elapsed := now.Sub(s.lastTime)
	if elapsed <= 0 {
		return s.cached
	}
	elapsedNs := float64(elapsed.Nanoseconds())
	var sum float64
	n := 0
	for i := range cur {
		if cur[i] < s.lastBusy[i] {
			continue // counter rolled / engine reset
		}
		frac := float64(cur[i]-s.lastBusy[i]) / elapsedNs
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1 // clamp; can exceed slightly with concurrent counter reads
		}
		sum += frac
		n++
	}
	s.lastBusy = cur
	s.lastTime = now
	if n == 0 {
		s.cached = 0
	} else {
		s.cached = sum / float64(n)
	}
	return s.cached
}

// load returns the most-recent cached value without touching sysfs.
// Cheap; safe for repeated calls from /capability handlers.
func (s *engineSampler) load() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cached
}

// numEngines reports how many video engines this worker enumerated at
// startup. 0 means no GPU detected (CPU-only worker — gpu_load will
// stay at 0 and routing falls back to session-count-only signal).
func (s *engineSampler) numEngines() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths)
}

// startEngineSampler establishes a baseline read and launches a 2s
// background sampler. Returns the sampler immediately; first
// non-zero load reading appears after one tick (~2s).
func startEngineSampler() *engineSampler {
	s := newEngineSampler()
	s.sample() // baseline
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			metricGPUEngineLoad.Set(s.sample())
		}
	}()
	return s
}
