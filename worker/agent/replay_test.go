//go:build replay
// +build replay

// Replay test — walks the argv corpus, runs each captured argv
// through Rewrite(), then spawns ffmpeg with the rewritten args (plus
// replay-safe patches) and reports per-entry pass/fail/timeout.
//
// Catches what unit tests can't: argv-parse failures, filter graph
// build errors, device-binding failures, encoder open errors. The
// rewriter can be "structurally correct" by unit-test standards but
// still produce an argv ffmpeg refuses to run.
//
// Build-tagged so it doesn't run on every `go test`. Two ways to use:
//
//  1. Inside a worker pod (full validation against real ffmpeg +
//     VAAPI + iHD + libass, same versions as production):
//
//     go test -tags=replay -c -o /tmp/replay.test ./worker/agent
//     POD=$(kubectl -n clusterplex get pod -l app.kubernetes.io/controller=worker -o jsonpath='{.items[0].metadata.name}')
//     kubectl -n clusterplex cp /tmp/replay.test "$POD:/tmp/replay.test"
//     kubectl -n clusterplex exec "$POD" -- /tmp/replay.test \
//     -test.v -test.run TestReplayCorpus -test.timeout 30m
//
//  2. Locally (rewriter-only validation, skips ffmpeg execution):
//
//     REPLAY_NO_FFMPEG=1 go test -tags=replay -v -run TestReplayCorpus
//
// Per-cell classification:
//
//	PASS         — rewriter applied, ffmpeg ran ≥0.05s without error
//	FAIL bail    — rewriter bailed (skip:<reason> in changes)
//	FAIL argv    — ffmpeg exited <0.5s with non-zero (argv parse / filter
//	               graph build / encoder open failure)
//	FAIL run     — ffmpeg ran but exited non-zero after >0.5s
//	TIMEOUT      — ffmpeg ran past replay timeout (10s default)
//	SKIP         — corpus entry missing argv or source file
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Subset of cmd/argv-extract's Capture struct — only the fields we
// need for replay.
type replayCapture struct {
	SessionID       string            `json:"session_id"`
	CaptureSource   string            `json:"capture_source"`
	Argv            []string          `json:"argv"`
	Env             map[string]string `json:"env"`
	SourcePath      string            `json:"source_path"`
	HasMapInlineass bool              `json:"has_map_inlineass"`
}

func TestReplayCorpus(t *testing.T) {
	corpusDir := os.Getenv("REPLAY_CORPUS_DIR")
	if corpusDir == "" {
		home, _ := os.UserHomeDir()
		corpusDir = filepath.Join(home, "scaleplex-corpus")
	}
	skipFFmpeg := os.Getenv("REPLAY_NO_FFMPEG") != ""
	timeout := 10 * time.Second
	if v := os.Getenv("REPLAY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}

	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("read corpus dir %s: %v", corpusDir, err)
	}

	type result struct {
		sid    string
		source string
		status string
		detail string
	}
	var results []result

	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		path := filepath.Join(corpusDir, ent.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			results = append(results, result{ent.Name(), "", "SKIP", "read err: " + err.Error()})
			continue
		}
		var c replayCapture
		if err := json.Unmarshal(body, &c); err != nil {
			results = append(results, result{ent.Name(), "", "SKIP", "json: " + err.Error()})
			continue
		}
		if len(c.Argv) == 0 || c.SessionID == "" {
			results = append(results, result{ent.Name(), c.CaptureSource, "SKIP", "no argv"})
			continue
		}

		t.Run(c.SessionID, func(t *testing.T) {
			res := Rewrite(c.Argv, c.Env, &RewriteOpts{
				SessionDir: filepath.Join("/tmp/replay", c.SessionID),
			})
			if !res.Applied {
				skipReason := ""
				for _, ch := range res.Changes {
					if strings.HasPrefix(ch, "skip:") {
						skipReason = ch
						break
					}
				}
				results = append(results, result{c.SessionID, c.CaptureSource, "FAIL bail", skipReason})
				t.Logf("rewriter bailed: %s", skipReason)
				return
			}

			if skipFFmpeg {
				results = append(results, result{c.SessionID, c.CaptureSource, "PASS rewrite", strings.Join(res.Changes[:min(5, len(res.Changes))], ",")})
				return
			}

			// Sanity: source mkv must exist on this host. If not (e.g.
			// production capture from a different Plex install), skip
			// — replay isn't meaningful without it.
			if c.SourcePath != "" {
				if _, err := os.Stat(c.SourcePath); err != nil {
					results = append(results, result{c.SessionID, c.CaptureSource, "SKIP", "source missing: " + c.SourcePath})
					return
				}
			}

			// Patch rewritten argv for safe replay. We're not actually
			// trying to play the file, just validate that ffmpeg accepts
			// the argv structure + filter graph + encoder open.
			args := patchForReplay(res.Args, c.SessionID)

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			start := time.Now()
			cmd := exec.CommandContext(ctx, "/usr/bin/ffmpeg", args...)
			cmd.Env = appendOrSetEnv(os.Environ(), res.Env)
			// chdir to the per-session tmp dir so relative paths in
			// the captured argv (e.g. `-segment_header_filename header`,
			// `-segment_list dash`, the segment muxer's transient
			// `dash.tmp`) land in our writable replay sandbox instead
			// of the test process's CWD (often /, unwritable for
			// uid 1000). Without this ffmpeg fast-fails with the
			// misleading "Could not write header (incorrect codec
			// parameters ?): Permission denied" the moment the
			// segment muxer tries to open `header`.
			cmd.Dir = filepath.Join("/tmp/replay", c.SessionID)
			_ = os.MkdirAll(cmd.Dir, 0o755)
			out, runErr := cmd.CombinedOutput()
			dur := time.Since(start)

			tail := string(out)
			if len(tail) > 1024 {
				tail = "..." + tail[len(tail)-1024:]
			}
			tail = strings.ReplaceAll(strings.ReplaceAll(tail, "\r", "\\r"), "\n", "\\n")

			switch {
			case ctx.Err() == context.DeadlineExceeded:
				results = append(results, result{c.SessionID, c.CaptureSource, "TIMEOUT",
					fmt.Sprintf("after %s (likely hung in encode); tail=%s", timeout, tail)})
				t.Errorf("ffmpeg hung past %s", timeout)
			case runErr == nil:
				results = append(results, result{c.SessionID, c.CaptureSource, "PASS",
					fmt.Sprintf("ran %s", dur)})
			case dur < 500*time.Millisecond:
				results = append(results, result{c.SessionID, c.CaptureSource, "FAIL argv",
					fmt.Sprintf("exit in %s: %s", dur, tail)})
				t.Errorf("ffmpeg fast-fail (likely argv/filter/device issue): %s", tail)
			default:
				results = append(results, result{c.SessionID, c.CaptureSource, "FAIL run",
					fmt.Sprintf("exit after %s: %s", dur, tail)})
				t.Errorf("ffmpeg run-time failure after %s: %s", dur, tail)
			}
		})
	}

	// Summary report — useful when running with -test.v on a large
	// corpus.
	t.Cleanup(func() {
		sort.Slice(results, func(i, j int) bool {
			if results[i].status != results[j].status {
				return results[i].status < results[j].status
			}
			return results[i].sid < results[j].sid
		})
		fmt.Fprintln(os.Stderr, "=== REPLAY SUMMARY ===")
		bySrc := map[string]map[string]int{}
		for _, r := range results {
			if bySrc[r.source] == nil {
				bySrc[r.source] = map[string]int{}
			}
			bySrc[r.source][r.status]++
		}
		var sources []string
		for s := range bySrc {
			sources = append(sources, s)
		}
		sort.Strings(sources)
		for _, s := range sources {
			fmt.Fprintf(os.Stderr, "  source=%s\n", s)
			byStatus := bySrc[s]
			var stats []string
			for k := range byStatus {
				stats = append(stats, k)
			}
			sort.Strings(stats)
			for _, k := range stats {
				fmt.Fprintf(os.Stderr, "    %-13s %d\n", k, byStatus[k])
			}
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "=== FAILURES (first 20) ===")
		shown := 0
		for _, r := range results {
			if !strings.HasPrefix(r.status, "FAIL") && r.status != "TIMEOUT" {
				continue
			}
			if shown >= 20 {
				break
			}
			fmt.Fprintf(os.Stderr, "  %-12s %s\n    detail: %s\n", r.status, r.sid, r.detail)
			shown++
		}
	})
}

// patchForReplay rewrites the args to be safe for a no-content dry
// run: 0.1s duration cap, redirect outputs to /tmp/, drop network
// callbacks, stub sidecar paths if missing.
func patchForReplay(args []string, sid string) []string {
	out := make([]string, 0, len(args)+4)
	tmpBase := filepath.Join("/tmp/replay", sid)
	_ = os.MkdirAll(tmpBase, 0o755)

	// Stub SRT in case the staged temp-0.srt PMS extracted is gone.
	stubSRT := filepath.Join(tmpBase, "stub.srt")
	_ = os.WriteFile(stubSRT, []byte("1\n00:00:00,000 --> 00:01:00,000\nreplay\n\n"), 0o644)

	// Walk + replace.
	skipNext := 0
	injectedT := false
	for i, a := range args {
		if skipNext > 0 {
			skipNext--
			continue
		}
		// Drop network-bound options that would block on PMS.
		switch a {
		case "-progressurl", "-segment_list", "-manifest_name":
			if i+1 < len(args) {
				skipNext = 1
			}
			continue
		}
		// Replace SRT-staged paths that don't exist with the stub.
		if strings.Contains(a, "subtitles=filename='") && strings.Contains(a, "/temp-") {
			a = replaceSubtitlesFilename(a, stubSRT)
		}
		// Replace -i temp-0.srt with stub if file missing.
		if a == "-i" && i+1 < len(args) {
			next := args[i+1]
			if strings.Contains(next, "/temp-") && strings.HasSuffix(next, ".srt") {
				if _, err := os.Stat(next); err != nil {
					out = append(out, "-i", stubSRT)
					skipNext = 1
					continue
				}
			}
		}
		// After the first -i (source), inject -t 0.1 to keep the
		// encode loop short.
		out = append(out, a)
		if !injectedT && a == "-i" && i+1 < len(args) {
			// Output the path arg, then the cap.
			// Actually we need to keep `-i path` together; insert -t
			// AFTER the path. Track via injectedT flag set on next
			// iteration. Simplest: handle inline.
		}
	}

	// Inject -t 0.1 right before the LAST positional arg (the output
	// template / filename). This caps encode duration without changing
	// where the per-input options sit.
	if !injectedT {
		// Find last positional (anything not starting with - and not a
		// value to a -flag) — heuristic: last arg in the slice.
		out = append(out[:len(out)-1], append([]string{"-t", "0.1"}, out[len(out)-1])...)
		injectedT = true
	}

	// Replace last positional arg (output filename / template) with
	// /tmp/replay/<sid>/output target.
	if len(out) > 0 {
		last := out[len(out)-1]
		if !strings.HasPrefix(last, "-") {
			ext := filepath.Ext(last)
			if ext == "" {
				ext = ".ts"
			}
			out[len(out)-1] = filepath.Join(tmpBase, "out"+ext)
			if strings.Contains(last, "%") {
				// segment template like media-%05d.ts → keep %d
				out[len(out)-1] = filepath.Join(tmpBase, "out-%05d"+ext)
			}
		}
	}

	return out
}

// replaceSubtitlesFilename swaps the path inside a subtitles= filter
// component without disturbing the rest of the filter chain string.
func replaceSubtitlesFilename(filterStr, newPath string) string {
	const needle = "subtitles=filename='"
	i := strings.Index(filterStr, needle)
	if i < 0 {
		return filterStr
	}
	start := i + len(needle)
	end := strings.Index(filterStr[start:], "'")
	if end < 0 {
		return filterStr
	}
	return filterStr[:start] + newPath + filterStr[start+end:]
}

// appendOrSetEnv merges supplied (rewriter-emitted) env into the host
// environment, replacing any duplicate keys.
func appendOrSetEnv(base []string, supplied map[string]string) []string {
	if len(supplied) == 0 {
		return base
	}
	overrides := map[string]struct{}{}
	for k := range supplied {
		overrides[k] = struct{}{}
	}
	out := make([]string, 0, len(base)+len(supplied))
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		if _, replaced := overrides[kv[:eq]]; replaced {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range supplied {
		out = append(out, k+"="+v)
	}
	return out
}
