package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two ffmpeg-shaped progress blocks back-to-back — the reporter must
// emit one PUT per `progress=...` line, not one PUT for the whole
// stream. Validates the core fix versus stock `-progress <http>`.
const sampleStream = `frame=15
fps=15.0
bitrate=1024kbits/s
total_size=2048
out_time_us=1000000
out_time_ms=1000
out_time=00:00:01.000000
dup_frames=0
drop_frames=0
speed=1.0x
progress=continue
frame=30
fps=15.0
bitrate=2048kbits/s
total_size=4096
out_time_us=2000000
out_time_ms=2000
out_time=00:00:02.000000
dup_frames=0
drop_frames=0
speed=1.0x
progress=end
`

func TestRunProgressReporter_OnePUTPerBlock(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		methods = append(methods, r.Method)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte(sampleStream))
		pw.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runProgressReporter(ctx, pr, srv.URL, "test-sess")

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected 2 PUTs, got %d: %q", len(bodies), bodies)
	}
	for i, m := range methods {
		if m != http.MethodPut {
			t.Errorf("PUT[%d] method = %s, want PUT", i, m)
		}
	}
	if !strings.Contains(bodies[0], "progress=continue\n") {
		t.Errorf("first body missing progress=continue: %q", bodies[0])
	}
	if !strings.Contains(bodies[1], "progress=end\n") {
		t.Errorf("second body missing progress=end: %q", bodies[1])
	}
	if strings.Contains(bodies[0], "progress=end") {
		t.Errorf("first body leaked second block: %q", bodies[0])
	}
	if !strings.Contains(bodies[0], "frame=15") {
		t.Errorf("first body missing frame=15: %q", bodies[0])
	}
	if !strings.Contains(bodies[1], "frame=30") {
		t.Errorf("second body missing frame=30: %q", bodies[1])
	}
}

func TestRunProgressReporter_EmptyURL(t *testing.T) {
	pr, pw := io.Pipe()
	go pw.Close()
	// Returns immediately when url is empty; just make sure it doesn't
	// panic or block on the pipe.
	done := make(chan struct{})
	go func() {
		runProgressReporter(context.Background(), pr, "", "x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reporter blocked on empty url")
	}
}

func TestProgressPipeArg(t *testing.T) {
	got := progressPipeArg(0)
	if len(got) != 2 || got[0] != "-progress" || got[1] != "pipe:3" {
		t.Errorf("extraIdx=0 → %v, want [-progress pipe:3]", got)
	}
	got = progressPipeArg(2)
	if got[1] != "pipe:5" {
		t.Errorf("extraIdx=2 → %v, want pipe:5", got)
	}
}
