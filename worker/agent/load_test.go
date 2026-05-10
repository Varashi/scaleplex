package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeBusy creates a fake "engine/<name>/busy" file with the given
// counter value (nanoseconds). Returns the absolute path.
func writeBusy(t *testing.T, dir, name string, ns uint64) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strconv.FormatUint(ns, 10)+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// engineSampler.sample with a single video engine: zero busy time
// gives load 0; busy time equal to elapsed gives load 1; half gives 0.5.
func TestEngineSampler_SingleEngine(t *testing.T) {
	dir := t.TempDir()
	p := writeBusy(t, dir, "vcs0.busy", 0)
	t.Setenv("WORKER_GPU_ENGINES", p)

	s := newEngineSampler()
	if got := s.sample(); got != 0 {
		t.Fatalf("baseline sample %v want 0", got)
	}

	// Simulate 100ms wall clock with the engine busy 100% of the time.
	time.Sleep(100 * time.Millisecond)
	writeBusy(t, dir, "vcs0.busy", 100*1e6) // 100ms in ns
	got := s.sample()
	if got < 0.5 || got > 1.05 {
		t.Fatalf("100%% busy: got %v want ~1.0", got)
	}
}

// Two engines, one busy and one idle → mean ≈ 0.5. This is the
// multi-engine awareness Phase 3 was added for.
func TestEngineSampler_TwoEnginesMeanBusy(t *testing.T) {
	dir := t.TempDir()
	p0 := writeBusy(t, dir, "vcs0.busy", 0)
	p1 := writeBusy(t, dir, "vcs1.busy", 0)
	t.Setenv("WORKER_GPU_ENGINES", p0+","+p1)

	s := newEngineSampler()
	s.sample()

	time.Sleep(100 * time.Millisecond)
	writeBusy(t, dir, "vcs0.busy", 100*1e6) // engine 0 busy 100ms
	writeBusy(t, dir, "vcs1.busy", 0)       // engine 1 idle
	got := s.sample()
	if got < 0.3 || got > 0.7 {
		t.Fatalf("1 of 2 engines busy: got %v want ~0.5", got)
	}
	if s.numEngines() != 2 {
		t.Fatalf("numEngines=%d want 2", s.numEngines())
	}
}

// Counter rollback (engine reset / kernel oddity) must not produce a
// negative or huge load — the sampler ignores the affected engine for
// that interval.
func TestEngineSampler_CounterRollback(t *testing.T) {
	dir := t.TempDir()
	p := writeBusy(t, dir, "vcs0.busy", 1_000_000_000) // start high
	t.Setenv("WORKER_GPU_ENGINES", p)

	s := newEngineSampler()
	s.sample()
	time.Sleep(50 * time.Millisecond)
	writeBusy(t, dir, "vcs0.busy", 0) // counter reset
	got := s.sample()
	if got != 0 {
		t.Fatalf("rollback should give 0, got %v", got)
	}
}

// No engines discovered → 0 forever, never panics.
func TestEngineSampler_NoEngines(t *testing.T) {
	t.Setenv("WORKER_GPU_ENGINES", "")
	// Ensure no real /sys/class/drm matches by overriding to an empty
	// list via the env var path (empty string means "use default
	// glob"); use a non-existent path to force the empty-list branch.
	t.Setenv("WORKER_GPU_ENGINES", "/nonexistent/no/such/file")

	s := newEngineSampler()
	if got := s.sample(); got != 0 {
		t.Fatalf("baseline %v want 0", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := s.sample(); got != 0 {
		t.Fatalf("with bad path %v want 0", got)
	}
}
