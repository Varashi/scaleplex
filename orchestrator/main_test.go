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

	got := pl.pickOrder()
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

func TestProbeWorker_ParsesCapability(t *testing.T) {
	resetGlobals()
	stub := &stubWorker{capability: capabilityResponse{FFmpegOK: true, ActiveSessions: 2, MaxSessions: 0}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	wk := addWorker(t, srv.URL, 0, 0, false)
	probeWorker(probeClient, wk)
	active, max, healthy := wk.snapshot()
	if !healthy || active != 2 || max != 0 {
		t.Fatalf("active=%d max=%d healthy=%v", active, max, healthy)
	}
}
