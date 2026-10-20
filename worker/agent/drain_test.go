package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func setDraining(t *testing.T, v int32) {
	t.Helper()
	prev := atomic.LoadInt32(&draining)
	atomic.StoreInt32(&draining, v)
	t.Cleanup(func() { atomic.StoreInt32(&draining, prev) })
}

func setReady(t *testing.T, v int32) {
	t.Helper()
	prev := atomic.LoadInt32(&ready)
	atomic.StoreInt32(&ready, v)
	t.Cleanup(func() { atomic.StoreInt32(&ready, prev) })
}

func TestDrainTimeout(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", defaultDrainSeconds * time.Second},
		{"0", 0},
		{"25", 25 * time.Second},
		{"abc", defaultDrainSeconds * time.Second},
		{"-3", defaultDrainSeconds * time.Second},
	} {
		t.Setenv("SCALEPLEX_DRAIN_SECONDS", tc.env)
		if got := drainTimeout(); got != tc.want {
			t.Errorf("SCALEPLEX_DRAIN_SECONDS=%q: got %s want %s", tc.env, got, tc.want)
		}
	}
}

func TestWaitForSessions(t *testing.T) {
	// Sessions finish before the deadline → returns 0 early.
	var n int32 = 2
	go func() { time.Sleep(30 * time.Millisecond); atomic.StoreInt32(&n, 0) }()
	start := time.Now()
	if left := waitForSessions(func() int { return int(atomic.LoadInt32(&n)) }, 2*time.Second, 5*time.Millisecond); left != 0 {
		t.Fatalf("want 0 left, got %d", left)
	}
	if time.Since(start) > time.Second {
		t.Errorf("should return as soon as sessions hit 0, took %s", time.Since(start))
	}
	// Sessions never finish → returns the remaining count at the deadline.
	if left := waitForSessions(func() int { return 3 }, 40*time.Millisecond, 5*time.Millisecond); left != 3 {
		t.Fatalf("want 3 left at timeout, got %d", left)
	}
}

func TestReadyz_Draining(t *testing.T) {
	setReady(t, 1)
	setDraining(t, 1)
	rec := httptest.NewRecorder()
	handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "draining") {
		t.Fatalf("draining worker must fail /readyz: %d %q", rec.Code, rec.Body.String())
	}
}

func TestTask_RefusedWhileDraining(t *testing.T) {
	setDraining(t, 1)
	rec := httptest.NewRecorder()
	body := `{"session_id":"s1","args":["-i","x"]}`
	handleTask(rec, httptest.NewRequest(http.MethodPost, "/task", strings.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining worker must 503 new tasks so the orchestrator falls through; got %d", rec.Code)
	}
}

func TestCapability_ReportsReadyAndDraining(t *testing.T) {
	if activeDialect == nil {
		activeDialect = vaapiDialect{}
	}
	get := func() capabilityResponse {
		rec := httptest.NewRecorder()
		handleCapability(rec, httptest.NewRequest(http.MethodGet, "/capability", nil))
		var c capabilityResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		return c
	}
	setReady(t, 0)
	setDraining(t, 0)
	if c := get(); c.Ready || c.Draining {
		t.Errorf("warming worker: want ready=false draining=false, got %+v", c)
	}
	atomic.StoreInt32(&ready, 1)
	if c := get(); !c.Ready || c.Draining {
		t.Errorf("warm worker: want ready=true draining=false, got ready=%v draining=%v", c.Ready, c.Draining)
	}
	atomic.StoreInt32(&draining, 1)
	if c := get(); c.Ready || !c.Draining {
		t.Errorf("draining worker: want ready=false draining=true, got ready=%v draining=%v", c.Ready, c.Draining)
	}
}
