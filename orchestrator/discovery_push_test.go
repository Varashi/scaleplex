package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func postJSON(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handleRegister(rr, req)
	return rr
}

func TestHandleRegister_AddsPushWorker(t *testing.T) {
	resetGlobals()
	rr := postJSON(t, `{"host":"h1","port":3501}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	w := pl.workers["http://h1:3501"]
	if w == nil {
		t.Fatalf("worker not added: keys=%v", keysOf(pl.workers))
	}
	if w.sources != srcPush {
		t.Errorf("sources=%v, want srcPush", w.sources)
	}
	if w.lastHeartbeat.IsZero() {
		t.Errorf("lastHeartbeat not set")
	}
}

func TestHandleRegister_IdempotentRefreshesHeartbeat(t *testing.T) {
	resetGlobals()
	if rr := postJSON(t, `{"host":"h1","port":3501}`); rr.Code != http.StatusNoContent {
		t.Fatalf("first register: code=%d", rr.Code)
	}
	w := pl.workers["http://h1:3501"]
	first := w.lastHeartbeat

	// Small sleep to make sure the second timestamp is strictly later.
	time.Sleep(2 * time.Millisecond)

	if rr := postJSON(t, `{"host":"h1","port":3501}`); rr.Code != http.StatusNoContent {
		t.Fatalf("second register: code=%d", rr.Code)
	}
	if got := len(pl.workers); got != 1 {
		t.Fatalf("expected 1 worker (idempotent), got %d", got)
	}
	if !w.lastHeartbeat.After(first) {
		t.Errorf("heartbeat not refreshed: first=%v second=%v", first, w.lastHeartbeat)
	}
}

func TestHandleRegister_BadMethodReturns405(t *testing.T) {
	resetGlobals()
	req := httptest.NewRequest(http.MethodGet, "/register", nil)
	rr := httptest.NewRecorder()
	handleRegister(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestHandleRegister_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad json", `{not json`},
		{"missing host", `{"port":3501}`},
		{"empty host", `{"host":"","port":3501}`},
		{"port zero", `{"host":"h","port":0}`},
		{"port negative", `{"host":"h","port":-1}`},
		{"port too high", `{"host":"h","port":70000}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetGlobals()
			rr := postJSON(t, tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (body=%s)", rr.Code, rr.Body.String())
			}
			if len(pl.workers) != 0 {
				t.Errorf("rejected request still added worker: %v", keysOf(pl.workers))
			}
		})
	}
}

func TestHandleRegister_BodySizeLimited(t *testing.T) {
	resetGlobals()
	huge := bytes.Repeat([]byte("x"), 8192)
	body := `{"host":"h1","port":3501,"junk":"` + string(huge) + `"}`
	rr := postJSON(t, body)
	// Body exceeds the 4096 byte LimitReader → json decode fails or
	// truncates; either way the handler must NOT crash and must return
	// a 4xx (not 2xx).
	if rr.Code >= 200 && rr.Code < 300 {
		t.Fatalf("expected 4xx for oversized body, got %d", rr.Code)
	}
}

func TestRegister_MergesWithExistingDNSWorker(t *testing.T) {
	resetGlobals()
	pl.workers["http://10.0.0.5:3501"] = &worker{
		host: "10.0.0.5", url: "http://10.0.0.5:3501", sources: srcDNS,
	}
	pl.register("10.0.0.5", 3501, time.Now())

	if got := len(pl.workers); got != 1 {
		t.Fatalf("expected 1 worker (deduped), got %d: %v", got, keysOf(pl.workers))
	}
	w := pl.workers["http://10.0.0.5:3501"]
	if w.sources != srcDNS|srcPush {
		t.Errorf("sources=%v, want srcDNS|srcPush", w.sources)
	}
}

func TestReapPush_RemovesAfterTimeout(t *testing.T) {
	resetGlobals()
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	pl.register("h1", 3501, now)
	pl.register("h2", 3501, now)
	if got := len(pl.workers); got != 2 {
		t.Fatalf("expected 2 registered workers, got %d", got)
	}

	// 20s later — both stale (timeout 15s).
	pl.reapPush(now.Add(20*time.Second), 15*time.Second)
	if got := len(pl.workers); got != 0 {
		t.Errorf("expected 0 after reap, got %d: %v", got, keysOf(pl.workers))
	}
}

func TestReapPush_PreservesFreshHeartbeat(t *testing.T) {
	resetGlobals()
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	pl.register("h1", 3501, now)

	// 5s later — well within 15s timeout.
	pl.reapPush(now.Add(5*time.Second), 15*time.Second)
	if got := len(pl.workers); got != 1 {
		t.Errorf("expected 1 worker after reap, got %d", got)
	}
}

func TestReapPush_DropsPushBitButKeepsListWorker(t *testing.T) {
	resetGlobals()
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	pl.loadList([]hostPort{{"h1", 3501}})
	pl.register("h1", 3501, now)

	w := pl.workers["http://h1:3501"]
	if w == nil || w.sources != srcList|srcPush {
		t.Fatalf("setup: expected list+push, got sources=%v", w.sources)
	}

	pl.reapPush(now.Add(30*time.Second), 15*time.Second)
	w = pl.workers["http://h1:3501"]
	if w == nil {
		t.Fatalf("worker removed; list bit should have kept it alive")
	}
	if w.sources != srcList {
		t.Errorf("sources=%v, want srcList only", w.sources)
	}
}

func TestReapPush_IgnoresNonPushWorkers(t *testing.T) {
	resetGlobals()
	pl.workers["http://dns:3501"] = &worker{
		host: "dns", url: "http://dns:3501", sources: srcDNS,
	}
	pl.workers["http://list:3501"] = &worker{
		host: "list", url: "http://list:3501", sources: srcList,
	}
	// Both have zero-value lastHeartbeat which is ancient — reap must
	// still leave them alone because srcPush bit is unset.
	pl.reapPush(time.Now(), 15*time.Second)
	if got := len(pl.workers); got != 2 {
		t.Errorf("non-push workers wrongly reaped: have %d, want 2", got)
	}
}

func TestRegister_StoresHostAsProvided(t *testing.T) {
	// Worker may register with a hostname even when DNS doesn't know it
	// — we must preserve the registered host string verbatim so
	// /capability + dispatch hit the same address.
	resetGlobals()
	pl.register("worker-a.lan", 3501, time.Now())
	w := pl.workers["http://worker-a.lan:3501"]
	if w == nil {
		t.Fatalf("worker not registered under hostname URL")
	}
	if w.host != "worker-a.lan" {
		t.Errorf("host=%q, want worker-a.lan", w.host)
	}
	if w.url != "http://worker-a.lan:3501" {
		t.Errorf("url=%q", w.url)
	}
}
