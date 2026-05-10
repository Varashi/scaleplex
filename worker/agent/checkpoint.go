package main

// checkpoint — GET /task/<id>/checkpoint
//
// Returns enough state for an orchestrator-driven resume on a different
// worker (Phase 4b/4d):
//   - the post-rewrite ffmpeg argv + env we exec'd
//   - the shared NFS cwd PMS reads chunks from
//   - the source media path
//   - PMS progress / manifest URLs (so the new worker keeps reporting)
//   - the original session-start seek offset
//   - the highest segment seq emitted across streams
//
// Pure introspection: no behaviour change to the running session.
// Read-only, safe to poll. The orchestrator is expected to combine
// last_segment_seq with HLS segment_time (typically 1s) to compute
// the input -ss offset for the resume worker.

import (
	"encoding/json"
	"net/http"
	"time"
)

type checkpoint struct {
	SessionID         string            `json:"session_id"`
	Args              []string          `json:"args"`
	Env               map[string]string `json:"env,omitempty"`
	Cwd               string            `json:"cwd,omitempty"`
	SourcePath        string            `json:"source_path,omitempty"`
	ProgressURL       string            `json:"progress_url,omitempty"`
	ManifestURL       string            `json:"manifest_url,omitempty"`
	SeekOffsetSeconds float64           `json:"seek_offset_seconds"`
	LastSegmentSeq    int64             `json:"last_segment_seq"`
	StartedAt         time.Time         `json:"started_at"`
	Pid               int               `json:"pid"`
}

func handleCheckpoint(w http.ResponseWriter, sessionID string) {
	t := registry.get(sessionID)
	if t == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	cp := checkpoint{
		SessionID:         sessionID,
		Args:              t.argv,
		Env:               t.env,
		Cwd:               t.cwd,
		SourcePath:        t.sourcePath,
		ProgressURL:       t.progressURL,
		ManifestURL:       t.manifestURL,
		SeekOffsetSeconds: t.seekOffsetS,
		LastSegmentSeq:    t.lastSeq.Load(),
		StartedAt:         t.startedAt,
	}
	if t.cmd != nil && t.cmd.Process != nil {
		cp.Pid = t.cmd.Process.Pid
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cp)
}
