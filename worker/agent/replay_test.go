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
	"regexp"
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

// --- #147: bail classification --------------------------------------
//
// A "bail" is the rewriter declining to reshape, surfaced as a
// "skip:<reason>" entry in Changes. It can carry Applied=true (a *soft*
// bail: the rewriter scrubbed Plex flags / normalized stream-specs but
// still gave up on the transcode reshape). Those soft bails previously
// slipped through as PASS because replay only inspected !res.Applied —
// the masking that let PR #144's :#0xNN class (hevc_vaapi/h264_vaapi +
// inlineass=, bailing skip:no-decoder) ride the corpus as PASS for ~3
// weeks. For a shape that MUST be reshaped — an inlineass= sub-burn
// paired with a HW video encoder — a bail is always a regression.
const (
	shapeHWSubBurn     = "hw-subburn-transcode" // inlineass= + HW video encoder — MUST reshape
	shapeOptimizeRemux = "optimize-remux"
	shapeOther         = "other"
)

// allowedBailReasons lists the bail reasons that are a CORRECT outcome
// per shape. "*" = any reason allowed (permissive — today's behavior).
// An empty slice = no bail is acceptable; any skip:<reason> fails. Only
// shapeHWSubBurn is strict for now; tightening the rest is #150.
var allowedBailReasons = map[string][]string{
	shapeHWSubBurn: {
		// Deliberate defensive fallback (NOT the silent skip:no-decoder
		// masking #144 fixed): the rewriter declines to reshape a HW-decode
		// sub-burn filtergraph whose shape it doesn't model, and runs Plex's
		// SW-inlineass graph instead — functional, just not the GPU-resident
		// reshape. A known, explicit perf gap; modeling these graphs is a
		// rewriter follow-up. Matched by prefix (the reason carries the full
		// graph string).
		TagPrefixBailHWDecodeSubUnmodeled,
	},
	shapeOptimizeRemux: {"*"},
	shapeOther:         {"*"},
}

// bailAllowed reports whether reason is an acceptable bail for shape.
// Allowlist entries ending in ":" are dynamic-reason prefixes (the bail
// appends a variable suffix, e.g. the filtergraph) and match by prefix.
func bailAllowed(shape, reason string) bool {
	allowed, ok := allowedBailReasons[shape]
	if !ok {
		return true // unknown shape → don't fail
	}
	for _, a := range allowed {
		switch {
		case a == "*", a == reason:
			return true
		case strings.HasSuffix(a, ":") && strings.HasPrefix(reason, a):
			return true
		}
	}
	return false
}

// bailReasonOf returns the bail reason (without the "skip:" prefix) from
// a rewriter result's changes, or "" if it didn't bail. Independent of
// Applied — a soft bail carries Applied=true.
func bailReasonOf(changes []string) string {
	for _, ch := range changes {
		if strings.HasPrefix(ch, TagPrefixSkip) {
			return strings.TrimPrefix(ch, TagPrefixSkip)
		}
	}
	return ""
}

// Matches a hardware video encoder token in any backend — the shape is
// about what PMS *sent*, not the replay host's GPU, so cover all suffixes.
var reHWVideoEncoder = regexp.MustCompile(`^(h264|hevc|av1|vp9|mpeg4|mpeg2video)_(vaapi|nvenc|qsv|cuda|amf|vulkan|videotoolbox|mf|v4l2m2m)$`)

func hasHWVideoEncoder(argv []string) bool {
	for _, a := range argv {
		if reHWVideoEncoder.MatchString(a) {
			return true
		}
	}
	return false
}

func hasInlineassFilter(argv []string) bool {
	for _, a := range argv {
		if strings.Contains(a, "inlineass=") {
			return true
		}
	}
	return false
}

// classifyShape buckets a captured argv for bail-expectation purposes.
// Only shapeHWSubBurn is strict; the rest stay permissive — this change
// catches must-reshape regressions, not every legitimate bail.
func classifyShape(argv []string) string {
	switch {
	case hasInlineassFilter(argv) && hasHWVideoEncoder(argv):
		return shapeHWSubBurn
	case isOptimizeRemux(argv):
		return shapeOptimizeRemux
	default:
		return shapeOther
	}
}

func TestReplayCorpus(t *testing.T) {
	// Initialize the HW dialect from WORKER_BACKEND, exactly as main()
	// does at worker startup. Without this the replay binary (which
	// never runs main) would leave activeDialect at its vaapiDialect{}
	// default and validate the WRONG backend — e.g. emit scale_vaapi /
	// h264_vaapi / init_hw_device vaapi against a NVENC corpus on a
	// NVIDIA worker, producing spurious FAIL-argv. Set REPLAY_BACKEND
	// (or WORKER_BACKEND) to pick; defaults to auto-probe like prod.
	if b := os.Getenv("REPLAY_BACKEND"); b != "" {
		t.Setenv("WORKER_BACKEND", b)
	}
	activeDialect = selectDialect()
	t.Logf("replay backend: %s", activeDialect.backendName())

	// The replay validates the REWRITER (+ ffmpeg), not the Plex-Pass gate
	// (#78, which has its own tests). The corpus carries a captured
	// SCALEPLEX_PMS_BASE_URL pointing at the origin PMS (often an in-cluster
	// address unreachable from the replay host) — the real gate query would
	// fail-closed and skip every HW re-accel/cross-backend reshape, masking
	// the rewriter under spurious FAIL-argv. Stub the gate active so the
	// rewriter paths are exercised. (Set REPLAY_REAL_PASS_CHECK to opt out.)
	if os.Getenv("REPLAY_REAL_PASS_CHECK") == "" {
		passCheck = func(_, _ string) (bool, error) { return true, nil }
	}

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
			// #147: a bail (skip:<reason>) on a shape that MUST be reshaped is
			// a regression even when Applied=true masked it as PASS. Check
			// every bail regardless of Applied, and hard-fail the disallowed
			// ones. Allowed bails fall through to the unchanged paths below.
			if reason := bailReasonOf(res.Changes); reason != "" {
				if shape := classifyShape(c.Argv); !bailAllowed(shape, reason) {
					results = append(results, result{c.SessionID, c.CaptureSource, "FAIL bail",
						fmt.Sprintf("shape=%s skip:%s — must reshape; fix rewriter or add an explicit allowedBailReasons entry", shape, reason)})
					t.Errorf("rewriter bailed skip:%s on shape %q that must be reshaped — regression (#147)", reason, shape)
					return
				}
			}

			if !res.Applied {
				// Allowed hard bail (no scrub ran, rewriter declined). Record +
				// stop — the bailed argv is ~Plex's original, not a rewriter
				// product. (Allowed *soft* bails keep Applied=true and fall
				// through to the ffmpeg run below, unchanged.)
				reason := bailReasonOf(res.Changes)
				results = append(results, result{c.SessionID, c.CaptureSource, "BAIL ok", "skip:" + reason})
				t.Logf("expected bail: skip:%s", reason)
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
