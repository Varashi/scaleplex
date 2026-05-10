package main

// load — multi-engine GPU busy% sampling for /capability + routing.
//
// Two reader implementations cover the kernel/hardware matrix:
//
//   sysfs reader: /sys/class/drm/card*/engine/<vcs|vecs>*/busy
//     - Older Intel iGPUs (Skylake..Tigerlake) on i915 < ~6.5
//     - No special capability required
//
//   PMU reader: perf_event_open() against /sys/bus/event_source/devices/i915_*
//     - Arc + recent kernels (i915 >= 6.5 dropped the sysfs busy file)
//     - Requires CAP_PERFMON in the container OR
//       sysctl kernel.perf_event_paranoid <= 0 on the host
//
// detectReader() tries sysfs first (cheaper, no caps), then PMU. If
// neither succeeds the worker reports gpu_load=0 forever and the
// orchestrator falls back to session-count routing — same behaviour as
// pre-Phase-3, no regression on CPU-only or unsupported workers.

import (
	"encoding/binary"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sys/unix"
)

var metricGPUEngineLoad = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "scaleplex_worker_gpu_engine_load",
	Help: "Mean busy fraction across video (vcs/vecs) engines, 0..1.",
})

// engineSamplerInst is the process-wide sampler started in main().
var engineSamplerInst *engineSampler

// engineReader supplies monotonic ns-busy counters per video engine.
// Implementations are NOT goroutine-safe; engineSampler serialises
// access under its own mutex.
type engineReader interface {
	sample() ([]uint64, error)
	names() []string
	close()
}

// ─── sysfs reader ─────────────────────────────────────────────────────

type sysfsReader struct {
	paths []string
	nms   []string
}

func newSysfsReader() (*sysfsReader, error) {
	if v := os.Getenv("WORKER_GPU_ENGINES"); v != "" {
		var paths, nms []string
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, err := os.Stat(p); err != nil {
				continue
			}
			paths = append(paths, p)
			nms = append(nms, filepath.Base(filepath.Dir(p)))
		}
		if len(paths) == 0 {
			return nil, errors.New("WORKER_GPU_ENGINES set but no readable paths")
		}
		return &sysfsReader{paths: paths, nms: nms}, nil
	}
	var paths, nms []string
	for _, glob := range []string{
		"/sys/class/drm/card*/engine/vcs*/busy",
		"/sys/class/drm/card*/engine/vecs*/busy",
	} {
		ms, _ := filepath.Glob(glob)
		for _, m := range ms {
			if _, err := os.Stat(m); err != nil {
				continue
			}
			paths = append(paths, m)
			nms = append(nms, filepath.Base(filepath.Dir(m)))
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("no sysfs engine/<name>/busy files found")
	}
	return &sysfsReader{paths: paths, nms: nms}, nil
}

func (r *sysfsReader) sample() ([]uint64, error) {
	out := make([]uint64, len(r.paths))
	for i, p := range r.paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // ignore vanished engine; sampler will skip via lastBusy compare
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			continue
		}
		out[i] = n
	}
	return out, nil
}
func (r *sysfsReader) names() []string { return r.nms }
func (r *sysfsReader) close()          {}

// ─── PMU reader ───────────────────────────────────────────────────────

type pmuReader struct {
	fds []int
	nms []string
}

func newPMUReader() (*pmuReader, error) {
	devs, _ := filepath.Glob("/sys/bus/event_source/devices/i915*")
	if len(devs) == 0 {
		return nil, errors.New("no i915 PMU devices")
	}
	var fds []int
	var nms []string
	for _, dev := range devs {
		b, err := os.ReadFile(filepath.Join(dev, "type"))
		if err != nil {
			continue
		}
		pmuType, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
		if err != nil {
			continue
		}
		eventDir := filepath.Join(dev, "events")
		entries, err := os.ReadDir(eventDir)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			n := ent.Name()
			if !strings.HasSuffix(n, "-busy") {
				continue
			}
			engine := strings.TrimSuffix(n, "-busy")
			if !(strings.HasPrefix(engine, "vcs") || strings.HasPrefix(engine, "vecs")) {
				continue
			}
			cfgBytes, err := os.ReadFile(filepath.Join(eventDir, n))
			if err != nil {
				continue
			}
			cfg, ok := parseEventConfig(string(cfgBytes))
			if !ok {
				continue
			}
			attr := &unix.PerfEventAttr{
				Type:   uint32(pmuType),
				Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
				Config: cfg,
			}
			// pid=-1 + cpu=0: system-wide PMU read on the boot CPU.
			// i915 PMU is per-device, not per-CPU, so any CPU works.
			fd, err := unix.PerfEventOpen(attr, -1, 0, -1, 0)
			if err != nil {
				log.Printf("gpu PMU: perf_event_open %s/%s: %v", filepath.Base(dev), n, err)
				continue
			}
			if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
				log.Printf("gpu PMU: ENABLE fd=%d: %v", fd, err)
				unix.Close(fd)
				continue
			}
			fds = append(fds, fd)
			nms = append(nms, engine)
		}
	}
	if len(fds) == 0 {
		return nil, errors.New("no PMU video-engine events opened (need CAP_PERFMON or perf_event_paranoid<=0)")
	}
	return &pmuReader{fds: fds, nms: nms}, nil
}

// parseEventConfig pulls the `config=0xNN` value out of an i915 PMU
// events file. Other fields (umask, cmask) are ignored — i915 video
// engine busy events use only `config`.
func parseEventConfig(s string) (uint64, bool) {
	for _, p := range strings.Split(strings.TrimSpace(s), ",") {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) != "config" {
			continue
		}
		v := strings.TrimSpace(kv[1])
		base := 10
		if strings.HasPrefix(v, "0x") || strings.HasPrefix(v, "0X") {
			v = v[2:]
			base = 16
		}
		n, err := strconv.ParseUint(v, base, 64)
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

func (r *pmuReader) sample() ([]uint64, error) {
	out := make([]uint64, len(r.fds))
	buf := make([]byte, 8)
	for i, fd := range r.fds {
		if _, err := unix.Read(fd, buf); err != nil {
			continue // leave 0 — sampler handles with lastBusy compare
		}
		out[i] = binary.LittleEndian.Uint64(buf)
	}
	return out, nil
}
func (r *pmuReader) names() []string { return r.nms }
func (r *pmuReader) close() {
	for _, fd := range r.fds {
		_ = unix.Close(fd)
	}
}

// ─── detection + sampler ──────────────────────────────────────────────

// detectReader returns the best available reader plus a tag describing
// which mode was selected ("sysfs", "pmu", or "none"). Sysfs is tried
// first because it requires no caps; PMU is the fallback for kernels
// that dropped the sysfs busy file.
func detectReader() (engineReader, string) {
	if os.Getenv("WORKER_GPU_DISABLE") == "1" {
		return nil, "disabled"
	}
	if r, err := newSysfsReader(); err == nil {
		return r, "sysfs"
	} else {
		log.Printf("gpu sysfs reader unavailable: %v", err)
	}
	if r, err := newPMUReader(); err == nil {
		return r, "pmu"
	} else {
		log.Printf("gpu PMU reader unavailable: %v", err)
	}
	return nil, "none"
}

type engineSampler struct {
	mu       sync.Mutex
	reader   engineReader
	mode     string
	nms      []string
	lastBusy []uint64
	lastTime time.Time
	cached   float64
}

func newEngineSampler() *engineSampler {
	r, mode := detectReader()
	s := &engineSampler{reader: r, mode: mode}
	if r != nil {
		s.nms = r.names()
	}
	return s
}

func (s *engineSampler) sample() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reader == nil {
		s.cached = 0
		return 0
	}
	now := time.Now()
	cur, err := s.reader.sample()
	if err != nil {
		return s.cached
	}
	if s.lastBusy == nil {
		s.lastBusy = cur
		s.lastTime = now
		return s.cached
	}
	elapsed := now.Sub(s.lastTime)
	if elapsed <= 0 {
		return s.cached
	}
	elapsedNs := float64(elapsed.Nanoseconds())
	var sum float64
	n := 0
	for i := range cur {
		if i >= len(s.lastBusy) || cur[i] < s.lastBusy[i] {
			continue // engine vanished or counter rolled
		}
		frac := float64(cur[i]-s.lastBusy[i]) / elapsedNs
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
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

func (s *engineSampler) load() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cached
}

func (s *engineSampler) numEngines() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.nms)
}

func (s *engineSampler) modeTag() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

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
