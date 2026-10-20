package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dyingHandler accepts /task, streams `partial`, then drops the connection —
// a worker pod dying mid-session.
func dyingHandler(t *testing.T, partial string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/capability", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ffmpeg_ok":true}`))
	})
	mux.HandleFunc("/task/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) })
	mux.HandleFunc("/task", func(w http.ResponseWriter, r *http.Request) {
		conn, bufrw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n")
		bufrw.WriteString(strings.ToUpper(strings.TrimPrefix(itoaHex(len(partial)), "0")) + "\r\n" + partial + "\r\n")
		bufrw.Flush()
		conn.Close()
	})
	return mux
}

func itoaHex(n int) string {
	const digits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(digits[n%16]) + s
		n /= 16
	}
	return s
}

// deadURL returns the URL of a server that has already shut down: connecting
// to it is refused, like a pod that just died.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func runTask(t *testing.T, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"session_id":"` + sessionID + `","args":["-i","x.mkv"]}`
	rec := httptest.NewRecorder()
	handleTask(rec, httptest.NewRequest(http.MethodPost, "/task", strings.NewReader(body)))
	return rec
}

func withRecoveryWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := recoveryWait
	recoveryWait = d
	t.Cleanup(func() { recoveryWait = prev })
}

// #219 item 1: a connection error before any response must fall through to the
// next candidate (it used to return 502) and take the dead worker out of
// rotation immediately.
func TestDispatch_ConnectErrorFallsThroughAndMarksDown(t *testing.T) {
	resetGlobals()
	good := &stubWorker{capability: capabilityResponse{FFmpegOK: true}}
	goodSrv := httptest.NewServer(good.handler())
	defer goodSrv.Close()

	dead := addWorker(t, deadURL(t), 0, 0, true) // ranks first (idle, cached healthy)
	addWorker(t, goodSrv.URL, 3, 0, true)

	rec := runTask(t, "s-connect")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ffmpeg exit: success") {
		t.Fatalf("want fall-through to the live worker; status=%d body=%q", rec.Code, rec.Body.String())
	}
	if _, _, _, healthy := dead.snapshot(); healthy {
		t.Errorf("unreachable worker must be marked down immediately")
	}
	if got := pl.schedule(); len(got) != 1 || got[0].url != goodSrv.URL {
		t.Errorf("dead worker must leave the schedule; got %d candidates", len(got))
	}
}

// #242 — the 2026-10-20 incident: worker A dies mid-stream, the recovery target
// B was ALSO replaced (connection refused). Recovery used to give up there; it
// must mark B down and move on to C.
func TestRecovery_SkipsRefusedTargetAndContinues(t *testing.T) {
	resetGlobals()
	withRecoveryWait(t, 3*time.Second)
	dyingSrv := httptest.NewServer(dyingHandler(t, "[stdout]"))
	defer dyingSrv.Close()
	good := &stubWorker{capability: capabilityResponse{FFmpegOK: true}}
	goodSrv := httptest.NewServer(good.handler())
	defer goodSrv.Close()

	addWorker(t, dyingSrv.URL, 0, 0, true)
	b := addWorker(t, deadURL(t), 1, 0, true) // next-best by load, but gone
	addWorker(t, goodSrv.URL, 2, 0, true)

	rec := runTask(t, "s-incident")
	out := rec.Body.String()
	if !strings.Contains(out, "[stdout]") || !strings.Contains(out, "ffmpeg exit: success") {
		t.Fatalf("session should resume on the live worker; body=%q", out)
	}
	if atomic.LoadInt32(&good.taskCallCount) != 1 {
		t.Errorf("live worker should get exactly one task, got %d", good.taskCallCount)
	}
	if _, _, _, healthy := b.snapshot(); healthy {
		t.Errorf("refused recovery target must be marked down")
	}
}

// #242: with every cached candidate gone, recovery must re-probe and wait for a
// worker to become healthy (a replacement pod finishing pre-warm) instead of
// failing at once.
func TestRecovery_WaitsForWorkerToBecomeHealthy(t *testing.T) {
	resetGlobals()
	withRecoveryWait(t, 5*time.Second)
	dyingSrv := httptest.NewServer(dyingHandler(t, "[stdout]\n"))
	defer dyingSrv.Close()

	var warm atomic.Bool
	fresh := &stubWorker{}
	mux := http.NewServeMux()
	mux.HandleFunc("/capability", func(w http.ResponseWriter, r *http.Request) {
		if warm.Load() {
			_, _ = w.Write([]byte(`{"ffmpeg_ok":true,"ready":true}`))
		} else {
			_, _ = w.Write([]byte(`{"ffmpeg_ok":true,"ready":false}`))
		}
	})
	mux.Handle("/task", fresh.handler())
	mux.Handle("/task/", fresh.handler())
	freshSrv := httptest.NewServer(mux)
	defer freshSrv.Close()

	addWorker(t, dyingSrv.URL, 0, 0, true)
	addWorker(t, freshSrv.URL, 0, 0, false) // still warming
	go func() { time.Sleep(1500 * time.Millisecond); warm.Store(true) }()

	start := time.Now()
	rec := runTask(t, "s-wait")
	if !strings.Contains(rec.Body.String(), "ffmpeg exit: success") {
		t.Fatalf("recovery should wait for the warming worker; body=%q", rec.Body.String())
	}
	if el := time.Since(start); el < time.Second || el > 4*time.Second {
		t.Errorf("expected recovery after ~1.5s of waiting, took %s", el)
	}
}

// Recovery gives up after SCALEPLEX_RECOVERY_WAIT_SECONDS when nothing healthy
// appears — and doesn't hang.
func TestRecovery_GivesUpAfterWait(t *testing.T) {
	resetGlobals()
	withRecoveryWait(t, 1500*time.Millisecond)
	dyingSrv := httptest.NewServer(dyingHandler(t, "[stdout]\n"))
	defer dyingSrv.Close()
	addWorker(t, dyingSrv.URL, 0, 0, true)

	start := time.Now()
	rec := runTask(t, "s-giveup")
	if el := time.Since(start); el > 6*time.Second {
		t.Fatalf("recovery should give up near the wait budget, took %s", el)
	}
	if !strings.Contains(rec.Body.String(), "[stdout]") {
		t.Errorf("partial output should still have reached PMS: %q", rec.Body.String())
	}
}

// #219 item 4: a fragment from the dead worker must not merge with the new
// worker's first line.
func TestRecovery_TerminatesPartialLine(t *testing.T) {
	resetGlobals()
	withRecoveryWait(t, 3*time.Second)
	dyingSrv := httptest.NewServer(dyingHandler(t, "frame=12 fps=2"))
	defer dyingSrv.Close()
	good := &stubWorker{capability: capabilityResponse{FFmpegOK: true}}
	goodSrv := httptest.NewServer(good.handler())
	defer goodSrv.Close()
	addWorker(t, dyingSrv.URL, 0, 0, true)
	addWorker(t, goodSrv.URL, 1, 0, true)

	out := runTask(t, "s-line").Body.String()
	if !strings.Contains(out, "frame=12 fps=2\n[scaleplex] ffmpeg exit: success") {
		t.Fatalf("partial line must be newline-terminated before the new stream; got %q", out)
	}
}

// A client that disconnects is not a worker failure: no recovery, no other
// worker contacted.
func TestDispatch_ClientGoneIsNotRecovered(t *testing.T) {
	resetGlobals()
	withRecoveryWait(t, 3*time.Second)
	dyingSrv := httptest.NewServer(dyingHandler(t, "[stdout]\n"))
	defer dyingSrv.Close()
	other := &stubWorker{capability: capabilityResponse{FFmpegOK: true}}
	otherSrv := httptest.NewServer(other.handler())
	defer otherSrv.Close()
	addWorker(t, dyingSrv.URL, 0, 0, true)
	addWorker(t, otherSrv.URL, 1, 0, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // PMS already gone
	body := `{"session_id":"s-gone","args":["-i","x.mkv"]}`
	req := httptest.NewRequest(http.MethodPost, "/task", strings.NewReader(body)).WithContext(ctx)
	handleTask(httptest.NewRecorder(), req)
	if n := atomic.LoadInt32(&other.taskCallCount); n != 0 {
		t.Errorf("no recovery when the client is gone; other worker got %d tasks", n)
	}
}

// #242: probe honours ready/draining; a worker image without the fields
// (rolling upgrade) still counts as healthy.
func TestProbe_ReadyAndDraining(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"legacy worker (no ready field)", `{"ffmpeg_ok":true}`, true},
		{"ready", `{"ffmpeg_ok":true,"ready":true}`, true},
		{"warming", `{"ffmpeg_ok":true,"ready":false}`, false},
		{"draining", `{"ffmpeg_ok":true,"ready":false,"draining":true}`, false},
		{"no ffmpeg", `{"ffmpeg_ok":false,"ready":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGlobals()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			wk := addWorker(t, srv.URL, 0, 0, !tc.want)
			probeWorker(probeClient, wk)
			if _, _, _, healthy := wk.snapshot(); healthy != tc.want {
				t.Errorf("healthy=%v want %v", healthy, tc.want)
			}
		})
	}
}

// #219 item 3: an accepted session counts toward load only until a probe that
// started after the accept reports it — then the worker's own activeSessions
// includes it and counting it twice would skew routing.
func TestInFlight_NotDoubleCountedAfterProbe(t *testing.T) {
	resetGlobals()
	wk := addWorker(t, "http://w", 0, 0, true)
	wk.mu.Lock()
	wk.lastProbeStart = time.Now().Add(-time.Minute)
	wk.mu.Unlock()

	tk := wk.begin()
	if got := wk.inFlight(); got != 1 {
		t.Fatalf("pending dispatch: inFlight=%d want 1", got)
	}
	tk.accept()
	if got := wk.inFlight(); got != 1 {
		t.Fatalf("accepted, not yet probed: inFlight=%d want 1", got)
	}
	// A probe that started after the accept now reports the session.
	wk.mu.Lock()
	wk.lastProbeStart = time.Now()
	wk.activeSessions = 1
	wk.mu.Unlock()
	if got := wk.inFlight(); got != 0 {
		t.Fatalf("after probe: inFlight=%d want 0 (activeSessions already counts it)", got)
	}
	if got := wk.sessions(); got != 1 {
		t.Fatalf("sessions()=%d want 1", got)
	}
	tk.end()
	tk.end() // idempotent
	if got := wk.inFlight(); got != 0 {
		t.Fatalf("after end: inFlight=%d", got)
	}
	// A dispatch that never gets accepted releases its pending slot.
	tk2 := wk.begin()
	tk2.end()
	if p := atomic.LoadInt32(&wk.pending); p != 0 {
		t.Fatalf("pending=%d after unaccepted end", p)
	}
}

// #219 item 4: IPv6 hosts need brackets.
func TestWorkerURL_IPv6(t *testing.T) {
	if got := workerURL("fd00::12", 3501); got != "http://[fd00::12]:3501" {
		t.Errorf("ipv6: %s", got)
	}
	if got := workerURL("10.0.0.5", 3501); got != "http://10.0.0.5:3501" {
		t.Errorf("ipv4: %s", got)
	}
}

// #219 item 4: /task body read is bounded.
func TestHandleTask_BodyTooLarge(t *testing.T) {
	resetGlobals()
	big := `{"session_id":"s","args":["` + strings.Repeat("a", maxTaskBody) + `"]}`
	rec := httptest.NewRecorder()
	handleTask(rec, httptest.NewRequest(http.MethodPost, "/task", strings.NewReader(big)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413", rec.Code)
	}
}
