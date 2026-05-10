package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"
)

// 404 when the session is not in the registry.
func TestCheckpoint_404Unknown(t *testing.T) {
	registry = &taskRegistry{tasks: make(map[string]*runningTask)}

	req := httptest.NewRequest(http.MethodGet, "/task/missing/checkpoint", nil)
	rr := httptest.NewRecorder()
	handleTaskByID(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rr.Code)
	}
}

// Checkpoint reflects the state captured in runningTask, including the
// current value of the lastSeq atomic counter that segwatch updates.
func TestCheckpoint_ReturnsCapturedState(t *testing.T) {
	registry = &taskRegistry{tasks: make(map[string]*runningTask)}
	cmd := exec.Command("sleep", "1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	defer cmd.Process.Kill()
	defer cmd.Wait()

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Now().UTC().Truncate(time.Millisecond)
	task := &runningTask{
		cmd:         cmd,
		cancel:      cancel,
		argv:        []string{"-i", "/media/m.mkv", "-c:v", "hevc_vaapi"},
		env:         map[string]string{"LIBVA_DRIVER_NAME": "iHD"},
		cwd:         "/transcode/sess1",
		sourcePath:  "/media/m.mkv",
		progressURL: "http://pms.local:32400/progress",
		manifestURL: "http://pms.local:32400/manifest",
		seekOffsetS: 12.5,
		startedAt:   now,
	}
	task.lastSeq.Store(42)
	registry.register("sess1", task)

	req := httptest.NewRequest(http.MethodGet, "/task/sess1/checkpoint", nil)
	rr := httptest.NewRecorder()
	handleTaskByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	var cp checkpoint
	if err := json.Unmarshal(rr.Body.Bytes(), &cp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cp.SessionID != "sess1" || cp.LastSegmentSeq != 42 {
		t.Fatalf("session=%q seq=%d", cp.SessionID, cp.LastSegmentSeq)
	}
	if cp.SourcePath != "/media/m.mkv" || cp.SeekOffsetSeconds != 12.5 {
		t.Fatalf("source=%q seek=%v", cp.SourcePath, cp.SeekOffsetSeconds)
	}
	if cp.ProgressURL == "" || cp.ManifestURL == "" || cp.Cwd == "" {
		t.Fatalf("urls/cwd missing in checkpoint: %+v", cp)
	}
	if len(cp.Args) != 4 || cp.Args[0] != "-i" {
		t.Fatalf("args malformed: %v", cp.Args)
	}
	if cp.Env["LIBVA_DRIVER_NAME"] != "iHD" {
		t.Fatalf("env not surfaced: %v", cp.Env)
	}
	if cp.Pid <= 0 {
		t.Fatalf("pid=%d", cp.Pid)
	}
}

// POST returns 405 — checkpoint is read-only.
func TestCheckpoint_GETOnly(t *testing.T) {
	registry = &taskRegistry{tasks: make(map[string]*runningTask)}
	req := httptest.NewRequest(http.MethodPost, "/task/sess/checkpoint", nil)
	rr := httptest.NewRecorder()
	handleTaskByID(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", rr.Code)
	}
}
