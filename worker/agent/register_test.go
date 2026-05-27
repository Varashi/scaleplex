package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostRegister_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/register" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := postRegister(context.Background(), srv.URL+"/register", []byte(`{"host":"h","port":3501}`), srv.Client())
	if err != nil {
		t.Fatalf("postRegister: %v", err)
	}
}

func TestPostRegister_BadStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := postRegister(context.Background(), srv.URL+"/register", []byte(`{}`), srv.Client())
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
}

func TestPostRegister_NetworkErrorReturnsError(t *testing.T) {
	// Connect to a port that has no listener (assume 1 is closed).
	err := postRegister(
		context.Background(),
		"http://127.0.0.1:1/register",
		[]byte(`{}`),
		&http.Client{Timeout: 100 * time.Millisecond},
	)
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
}

func TestPostRegister_BodyReachesServer(t *testing.T) {
	var got registerPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	payload, _ := json.Marshal(registerPayload{Host: "worker-a", Port: 3501, Version: "v1.7.2"})
	if err := postRegister(context.Background(), srv.URL+"/register", payload, srv.Client()); err != nil {
		t.Fatalf("postRegister: %v", err)
	}
	if got.Host != "worker-a" || got.Port != 3501 || got.Version != "v1.7.2" {
		t.Errorf("server saw payload = %+v", got)
	}
}

func TestRegisterLoop_PostsRepeatedly(t *testing.T) {
	var hits int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
		if n == 3 {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go registerLoop(ctx, srv.URL+"/register", []byte(`{"host":"h","port":3501}`),
		10*time.Millisecond, srv.Client())

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("registerLoop didn't reach 3 hits; got %d", atomic.LoadInt64(&hits))
	}
}

func TestRegisterLoop_BacksOffOnFailure(t *testing.T) {
	// First two POSTs fail (500), third succeeds. We verify the loop
	// eventually succeeds — exact backoff schedule is timing-sensitive,
	// so just confirm "still calling after failures."
	var hits int64
	successOnHit := int64(3)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		if n < successOnHit {
			http.Error(w, "fail", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		select {
		case <-done:
		default:
			close(done)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go registerLoop(ctx, srv.URL+"/register", []byte(`{"host":"h","port":3501}`),
		5*time.Millisecond, srv.Client())

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("loop didn't recover; hits=%d", atomic.LoadInt64(&hits))
	}
}

func TestRegisterLoop_StopsOnContextCancel(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go registerLoop(ctx, srv.URL+"/register", []byte(`{}`), 5*time.Millisecond, srv.Client())

	time.Sleep(30 * time.Millisecond)
	cancel()
	mid := atomic.LoadInt64(&hits)

	time.Sleep(60 * time.Millisecond)
	final := atomic.LoadInt64(&hits)

	if final > mid+1 {
		t.Errorf("loop kept running after cancel: mid=%d final=%d", mid, final)
	}
}

func TestResolveAdvertisedHost_PrefersEnv(t *testing.T) {
	t.Setenv("SCALEPLEX_WORKER_HOST", "explicit.lan")
	if got := resolveAdvertisedHost(); got != "explicit.lan" {
		t.Errorf("got %q, want explicit.lan", got)
	}
}

func TestResolveAdvertisedHost_FallsBackToHostname(t *testing.T) {
	t.Setenv("SCALEPLEX_WORKER_HOST", "")
	got := resolveAdvertisedHost()
	want, _ := os.Hostname()
	if got != want {
		t.Errorf("got %q, want %q (os.Hostname)", got, want)
	}
}

func TestResolveAdvertisedHost_TrimsWhitespace(t *testing.T) {
	t.Setenv("SCALEPLEX_WORKER_HOST", "  spaced.lan  ")
	if got := resolveAdvertisedHost(); got != "spaced.lan" {
		t.Errorf("got %q, want spaced.lan", got)
	}
}

func TestStartRegisterLoop_NoOpWhenURLUnset(t *testing.T) {
	t.Setenv("SCALEPLEX_ORCHESTRATOR_URL", "")
	// Should return without launching any goroutine. We can't easily
	// observe "no goroutine launched", but we can at least confirm it
	// returns promptly without panicking and without requiring a host.
	t.Setenv("SCALEPLEX_WORKER_HOST", "")
	startRegisterLoop()
}

func TestEnvSecondsOr(t *testing.T) {
	cases := []struct {
		set  bool
		val  string
		dflt int
		want int
	}{
		{false, "", 5, 5},
		{true, "", 5, 5},
		{true, "10", 5, 10},
		{true, "abc", 5, 5},
		{true, "-1", 5, 5},
		{true, "0", 5, 5},
	}
	for _, tc := range cases {
		if tc.set {
			t.Setenv("__TEST_ENV_SECONDS", tc.val)
		} else {
			os.Unsetenv("__TEST_ENV_SECONDS")
		}
		if got := envSecondsOr("__TEST_ENV_SECONDS", tc.dflt); got != tc.want {
			t.Errorf("envSecondsOr set=%v val=%q dflt=%d → %d, want %d",
				tc.set, tc.val, tc.dflt, got, tc.want)
		}
	}
}
