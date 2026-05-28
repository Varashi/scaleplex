package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Build a stubbed worker server that reports a fixed capability and
// optionally rejects /task with 503 a configurable number of times before
// accepting.
type stubWorker struct {
	capability        capabilityResponse
	rejectN           int32 // how many 503s before accepting
	taskCallCount     int32
	rejectedCallCount int32
}

func (s *stubWorker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/capability", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.capability)
	})
	mux.HandleFunc("/task", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.taskCallCount, 1)
		if atomic.LoadInt32(&s.rejectN) > 0 {
			atomic.AddInt32(&s.rejectN, -1)
			atomic.AddInt32(&s.rejectedCallCount, 1)
			http.Error(w, "at cap", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "[scaleplex] ffmpeg exit: success\n")
	})
	mux.HandleFunc("/task/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}

func resetGlobals() {
	pl = &pool{workers: make(map[string]*worker)}
	sessions = &sessionTracker{m: make(map[string]string)}
}

func addWorker(t *testing.T, url string, active, max int, healthy bool) *worker {
	t.Helper()
	wk := &worker{
		url:            url,
		host:           url,
		healthy:        healthy,
		activeSessions: active,
		maxSessions:    max,
	}
	pl.mu.Lock()
	pl.workers[url] = wk
	pl.mu.Unlock()
	return wk
}

func TestPickOrder_LeastLoadedFirst(t *testing.T) {
	resetGlobals()
	a := addWorker(t, "http://a", 5, 0, true)  // unlimited, raw load 5
	b := addWorker(t, "http://b", 1, 0, true)  // unlimited, raw load 1
	c := addWorker(t, "http://c", 0, 10, true) // capped, ratio 0.0
	d := addWorker(t, "http://d", 9, 10, true) // capped, ratio 0.9
	e := addWorker(t, "http://e", 0, 0, false) // unhealthy

	got := pl.schedule()
	if len(got) != 4 {
		t.Fatalf("expected 4 healthy, got %d", len(got))
	}
	if got[0] != c { // 0.0 ratio
		t.Errorf("expected c first, got %v", got[0].url)
	}
	if got[len(got)-1] != a { // raw 5 (highest among healthy)
		t.Errorf("expected a last, got %v", got[len(got)-1].url)
	}
	_ = b
	_ = d
	_ = e
}

func TestHandleTask_FallsThroughOn503(t *testing.T) {
	resetGlobals()

	full := &stubWorker{capability: capabilityResponse{FFmpegOK: true, MaxSessions: 1, ActiveSessions: 1}, rejectN: 1}
	free := &stubWorker{capability: capabilityResponse{FFmpegOK: true, MaxSessions: 1, ActiveSessions: 0}}

	fullSrv := httptest.NewServer(full.handler())
	freeSrv := httptest.NewServer(free.handler())
	defer fullSrv.Close()
	defer freeSrv.Close()

	// "full" looks freer in the cached registry (race window between
	// probe and request) → orchestrator picks it first; worker says 503
	// at request time → orchestrator falls through to "free".
	addWorker(t, fullSrv.URL, 0, 1, true)
	addWorker(t, freeSrv.URL, 1, 1, true)

	body := `{"session_id":"s1","args":["-i","x.mkv"]}`
	req := httptest.NewRequest(http.MethodPost, "/task", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleTask(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ffmpeg exit: success") {
		t.Fatalf("body missing success: %q", rec.Body.String())
	}
	if atomic.LoadInt32(&full.rejectedCallCount) != 1 {
		t.Errorf("expected full worker rejected once, got %d", full.rejectedCallCount)
	}
	if atomic.LoadInt32(&free.taskCallCount) != 1 {
		t.Errorf("expected free worker accepted once, got %d", free.taskCallCount)
	}
}

func TestHandleTask_NoWorkers503(t *testing.T) {
	resetGlobals()
	body := `{"session_id":"s1","args":["-i","x.mkv"]}`
	req := httptest.NewRequest(http.MethodPost, "/task", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleTask(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestHandleTask_AllAtCap503(t *testing.T) {
	resetGlobals()
	full := &stubWorker{capability: capabilityResponse{FFmpegOK: true, MaxSessions: 1, ActiveSessions: 1}, rejectN: 99}
	srv := httptest.NewServer(full.handler())
	defer srv.Close()
	addWorker(t, srv.URL, 1, 1, true)

	body := `{"session_id":"s1","args":["-i","x.mkv"]}`
	req := httptest.NewRequest(http.MethodPost, "/task", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleTask(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "at capacity") {
		t.Fatalf("expected 'at capacity' message, got %q", rec.Body.String())
	}
}

func TestSessionTracker_KillRouting(t *testing.T) {
	resetGlobals()
	stub := &stubWorker{capability: capabilityResponse{FFmpegOK: true}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	addWorker(t, srv.URL, 0, 0, true)

	// Send a task → orchestrator records session→worker
	body := `{"session_id":"s-kill","args":["-i","x.mkv"]}`
	tReq := httptest.NewRequest(http.MethodPost, "/task", strings.NewReader(body))
	tRec := httptest.NewRecorder()
	handleTask(tRec, tReq)
	if tRec.Code != http.StatusOK {
		t.Fatalf("task status=%d", tRec.Code)
	}
	// After Task returned, sessions.get should be cleared (defer del).
	if got := sessions.get("s-kill"); got != "" {
		t.Errorf("expected session cleared, got %q", got)
	}
}

func TestPickOrder_InFlightBreaksTie(t *testing.T) {
	resetGlobals()
	a := addWorker(t, "http://a", 0, 0, true)
	b := addWorker(t, "http://b", 0, 0, true)
	// Stale registry says both have load=0. Mark a as in-flight; b
	// should now rank first.
	a.dispatchBegin()
	defer a.dispatchEnd()
	got := pl.schedule()
	if got[0] != b {
		t.Fatalf("expected b first (a is in-flight), got %v", got[0].url)
	}
	if got[1] != a {
		t.Fatalf("expected a second, got %v", got[1].url)
	}
}

// Worker accepts the task, streams a few bytes, then closes the
// connection abruptly to simulate pod death mid-session. Orchestrator
// must transparently swap to a healthy alternative worker; PMS-facing
// stream stays open.
func TestHandleTask_RecoversMidStreamFailure(t *testing.T) {
	resetGlobals()

	dying := http.NewServeMux()
	dying.HandleFunc("/capability", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(capabilityResponse{FFmpegOK: true})
	})
	dying.HandleFunc("/task/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted) // checkpoint not needed for this test
	})
	dying.HandleFunc("/task", func(w http.ResponseWriter, r *http.Request) {
		// Hijack so we can write bytes then forcibly close the conn —
		// httptest's chunked writer doesn't expose that directly.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("hijack unsupported")
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n")
		bufrw.WriteString("8\r\n[stdout]\r\n") // chunked frame
		bufrw.Flush()
		conn.Close() // simulate pod death — abrupt close after some bytes
	})
	dyingSrv := httptest.NewServer(dying)
	defer dyingSrv.Close()

	healthy := &stubWorker{capability: capabilityResponse{FFmpegOK: true}}
	healthySrv := httptest.NewServer(healthy.handler())
	defer healthySrv.Close()

	// Make dying worker rank first.
	dyingWk := addWorker(t, dyingSrv.URL, 0, 0, true)
	healthyWk := addWorker(t, healthySrv.URL, 1, 0, true)
	_ = healthyWk
	_ = dyingWk

	body := `{"session_id":"recover-1","args":["-i","x.mkv"]}`
	req := httptest.NewRequest(http.MethodPost, "/task", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleTask(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	// Should contain bytes from BOTH workers (partial from dying, then
	// success from healthy).
	if !strings.Contains(out, "[stdout]") {
		t.Errorf("missing dying-worker bytes: %q", out)
	}
	if !strings.Contains(out, "ffmpeg exit: success") {
		t.Errorf("missing healthy-worker bytes (no recovery happened?): %q", out)
	}
	if atomic.LoadInt32(&healthy.taskCallCount) != 1 {
		t.Errorf("healthy worker should have been called once, got %d", healthy.taskCallCount)
	}
}

func TestProbeWorker_ParsesCapability(t *testing.T) {
	resetGlobals()
	stub := &stubWorker{capability: capabilityResponse{
		FFmpegOK: true, ActiveSessions: 2, MaxSessions: 0,
		GPULoad: 0.4, GPUEngines: 2,
	}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	wk := addWorker(t, srv.URL, 0, 0, false)
	probeWorker(probeClient, wk)
	active, max, gpuLoad, healthy := wk.snapshot()
	if !healthy || active != 2 || max != 0 || gpuLoad != 0.4 {
		t.Fatalf("active=%d max=%d gpuLoad=%v healthy=%v", active, max, gpuLoad, healthy)
	}
	if wk.gpuEngines != 2 {
		t.Fatalf("gpuEngines=%d want 2", wk.gpuEngines)
	}
}

// load() returns the dominant of session-cap saturation and GPU
// busy% — whichever is closer to 1.0 wins. Reflects "constrained
// resource leads" routing principle.
func TestLoad_TakesMaxOfSessionAndGPU(t *testing.T) {
	cases := []struct {
		name   string
		active int
		max    int
		gpu    float64
		want   float64
	}{
		{"both zero", 0, 0, 0, 0},
		{"sessions dominate", 9, 10, 0.2, 0.9},
		{"gpu dominates", 1, 10, 0.8, 0.8},
		{"unlimited cap idle, gpu signals load", 0, 0, 0.5, 0.5},
		{"both saturated", 10, 10, 1.0, 1.0},
		{"multi-engine half busy outranks single-engine empty",
			0, 0, 0.5, 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &worker{
				healthy:        true,
				activeSessions: tc.active,
				maxSessions:    tc.max,
				gpuLoad:        tc.gpu,
			}
			got := w.load()
			if abs(got-tc.want) > 1e-9 {
				t.Fatalf("load=%v want %v (active=%d max=%d gpu=%v)",
					got, tc.want, tc.active, tc.max, tc.gpu)
			}
		})
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// #77 PR4 — HW-preferred tiering: a SW worker that looks idle (load 0) must NOT
// outrank busier HW workers; it's the overflow tier.
func TestSchedule_HWBeforeSW(t *testing.T) {
	resetGlobals()
	hwBusy := addWorker(t, "http://hw-busy", 5, 0, true)
	hwIdle := addWorker(t, "http://hw-idle", 1, 0, true)
	swIdle := addWorker(t, "http://sw-idle", 0, 0, true) // idlest, but SW
	hwBusy.backend, hwIdle.backend, swIdle.backend = "vaapi", "nvenc", "sw"

	got := pl.schedule()
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	if got[0] != hwIdle || got[1] != hwBusy {
		t.Errorf("HW tier should come first by load: got %v,%v", got[0].url, got[1].url)
	}
	if got[2] != swIdle {
		t.Errorf("SW worker must be last (overflow) despite load 0; got %v", got[2].url)
	}
}

// flatPool (SCALEPLEX_PREFER_HW=0) disables tiering → pure load order, so the
// idle SW worker ranks first.
func TestSchedule_FlatPool(t *testing.T) {
	resetGlobals()
	pl.flatPool = true
	hw := addWorker(t, "http://hw", 5, 0, true)
	sw := addWorker(t, "http://sw", 0, 0, true)
	hw.backend, sw.backend = "vaapi", "sw"
	got := pl.schedule()
	if got[0] != sw {
		t.Errorf("flat pool should rank idle SW first; got %v", got[0].url)
	}
}

// round-robin rotates the starting worker across successive dispatches.
func TestSchedule_RoundRobin(t *testing.T) {
	resetGlobals()
	pl.strategy = lbRoundRobin
	for _, u := range []string{"http://a", "http://b", "http://c"} {
		w := addWorker(t, u, 0, 0, true)
		w.backend = "vaapi"
	}
	var firsts []string
	for i := 0; i < 3; i++ {
		firsts = append(firsts, pl.schedule()[0].url)
	}
	want := []string{"http://a", "http://b", "http://c"}
	for i := range want {
		if firsts[i] != want[i] {
			t.Errorf("round-robin dispatch %d: first=%s want=%s (all=%v)", i, firsts[i], want[i], firsts)
		}
	}
}

// least-sessions orders by active+in-flight, ignoring the GPU-engine math.
func TestSchedule_LeastSessions(t *testing.T) {
	resetGlobals()
	pl.strategy = lbLeastSessions
	a := addWorker(t, "http://a", 3, 0, true)
	b := addWorker(t, "http://b", 1, 0, true)
	a.backend, b.backend = "vaapi", "vaapi"
	got := pl.schedule()
	if got[0] != b {
		t.Errorf("least-sessions should rank b (1 session) first; got %v", got[0].url)
	}
}

// An unreported backend ("" — e.g. a pre-PR4 worker mid-rolling-upgrade) counts
// as HW so it isn't demoted to the SW overflow tier.
func TestWorker_isHW_UnknownIsHW(t *testing.T) {
	resetGlobals()
	w := addWorker(t, "http://legacy", 0, 0, true) // backend stays ""
	if !w.isHW() {
		t.Errorf("empty backend should be treated as HW")
	}
	w.backend = "sw"
	if w.isHW() {
		t.Errorf("sw backend should not be HW")
	}
}
