package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// throttleSignal: state transitions are atomic and visible across
// goroutines.
func TestThrottleSignal_SetGet(t *testing.T) {
	var s throttleSignal
	if s.on() {
		t.Fatal("zero value should be off")
	}
	s.set(true)
	if !s.on() {
		t.Fatal("set(true) should leave on=true")
	}
	s.set(false)
	if s.on() {
		t.Fatal("set(false) should leave on=false")
	}
}

// doPlexPUT must update rc.Throttle based on whether the response body
// contains the literal `canThrottle` substring. Mirrors Plex
// Transcoder's fftools/ffmpeg.c parser exactly.
func TestDoPlexPUT_BodyTogglesThrottle(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty body", "", false},
		{"plain canThrottle", "canThrottle", true},
		{"key=value", "canThrottle=1", true},
		{"trailing newline", "canThrottle\n", true},
		{"absent token", "throttle=1", false}, // 'canThrottle' substring missing
		{"unrelated payload", "speed=0.5\nremaining=120", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			sig := &throttleSignal{}
			rc := reportContext{URL: srv.URL, SessionID: "test", Throttle: sig}
			// Start with the opposite of `want` to prove the call flips it.
			sig.set(!tc.want)
			doPlexPUT(context.Background(), &http.Client{Timeout: time.Second}, rc, "progress", srv.URL)
			if got := sig.on(); got != tc.want {
				t.Fatalf("body=%q: throttle=%v want=%v", tc.body, got, tc.want)
			}
		})
	}
}

// 4xx response: clear throttle (fail open) so a flapping PMS doesn't
// strand the session paused.
func TestDoPlexPUT_4xxClearsThrottle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "canThrottle") // body shouldn't matter on 4xx
	}))
	defer srv.Close()

	sig := &throttleSignal{}
	sig.set(true)
	rc := reportContext{URL: srv.URL, SessionID: "test", Throttle: sig}
	doPlexPUT(context.Background(), &http.Client{Timeout: time.Second}, rc, "progress", srv.URL)
	if sig.on() {
		t.Fatal("4xx should clear throttle (fail open)")
	}
}

// Non-progress kinds (streamDetail / duration / dimensions) must not
// touch the throttle signal — only the periodic progress PUT body
// carries the canThrottle marker per Plex's design.
func TestDoPlexPUT_OtherKindsDontTouchThrottle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "canThrottle") // present, but kind != progress
	}))
	defer srv.Close()

	sig := &throttleSignal{}
	rc := reportContext{URL: srv.URL, SessionID: "test", Throttle: sig}
	doPlexPUT(context.Background(), &http.Client{Timeout: time.Second}, rc, "streamDetail", srv.URL)
	if sig.on() {
		t.Fatal("non-progress PUTs must not flip throttle")
	}
}

// throttleController issues SIGSTOP/SIGCONT to a real child process.
// Uses `sleep 5` as the victim — it's portable, has no GPU side
// effects, and we can detect SIGSTOP via /proc/<pid>/status State=T.
func TestThrottleController_PausesAndResumes(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary")
	}
	cmd := exec.Command("sleep", "5")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGCONT)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	sig := &throttleSignal{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		throttleController(ctx, "test", cmd.Process.Pid, sig)
		close(done)
	}()

	// Off → process state should remain 'S' (sleeping) or 'R'.
	time.Sleep(150 * time.Millisecond)
	if got := procState(t, cmd.Process.Pid); got == 'T' {
		t.Fatalf("expected running, got state=%c", got)
	}

	// On → controller starts duty-cycling SIGSTOP. Within ~250ms we
	// should observe 'T' (stopped) at some sample.
	sig.set(true)
	if !waitForState(t, cmd.Process.Pid, 'T', 500*time.Millisecond) {
		t.Fatal("did not observe SIGSTOP within 500ms of throttle on")
	}

	// Off → controller SIGCONTs and stops cycling. Within ~250ms the
	// process must no longer be 'T'.
	sig.set(false)
	if !waitForStateNot(t, cmd.Process.Pid, 'T', 500*time.Millisecond) {
		t.Fatal("did not resume after throttle off")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("controller did not exit after ctx cancel")
	}
}

// procState reads /proc/<pid>/status and returns the State byte
// ('R'/'S'/'T'/'D'/'Z' etc).
func procState(t *testing.T, pid int) byte {
	t.Helper()
	b, err := os.ReadFile("/proc/" + itoa(pid) + "/status")
	if err != nil {
		t.Fatalf("read /proc/%d/status: %v", pid, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "State:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && len(fields[1]) > 0 {
				return fields[1][0]
			}
		}
	}
	t.Fatalf("no State: line in /proc/%d/status", pid)
	return 0
}

func waitForState(t *testing.T, pid int, want byte, deadline time.Duration) bool {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if procState(t, pid) == want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func waitForStateNot(t *testing.T, pid int, avoid byte, deadline time.Duration) bool {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if procState(t, pid) != avoid {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// itoa avoids the strconv import for a single use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

