package main

// Cell is one synthetic replay-corpus entry. Its JSON shape is the
// subset of cmd/argv-extract's Capture that worker/agent/replay_test.go's
// replayCapture decodes — plus a `synthesized: true` marker so a
// synthetic cell is never mistaken for an organic capture.
//
// Keep these json tags in lockstep with replayCapture in
// worker/agent/replay_test.go; the replay harness silently ignores
// fields it doesn't know, so a drift here fails quietly (the cell just
// loses whatever the harness couldn't read).
type Cell struct {
	SessionID       string            `json:"session_id"`
	CaptureSource   string            `json:"capture_source"` // always "synthesized"
	Argv            []string          `json:"argv"`
	Env             map[string]string `json:"env"`
	SourcePath      string            `json:"source_path"`
	HasMapInlineass bool              `json:"has_map_inlineass"`
	Synthesized     bool              `json:"synthesized"` // identifies the file as generator output
}
