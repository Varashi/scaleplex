//go:build replay
// +build replay

// Dash-muxer end-to-end replay — #148.
//
// TestReplayCorpus walks the corpus through Rewrite() and (optionally)
// spawns ffmpeg, but it strips `-manifest_name` in patchForReplay, so
// the dash muxer's HTTP manifest publish (fork patch 0095: ffmpeg PUTs
// the MPD to the `-manifest_name` URL) is NEVER exercised. PR #144 Bug B
// lived exactly there: the bail path failed to rewrite the loopback
// `-manifest_name http://127.0.0.1:32400/...` URL → ECONNREFUSED →
// exit-145 on every dash session. No test slot caught it.
//
// This test closes that gap. It stands up an httptest fake-PMS, points
// the rewriter at it via SCALEPLEX_PMS_BASE_URL (so the loopback
// `-manifest_name` is rewritten to the fake), runs the REAL `-f dash`
// muxer, and asserts ffmpeg both PUT the manifest to the fake-PMS and
// exited 0. A bail-path argv that still carried the loopback URL would
// never reach the fake → assertion FAIL.
//
// Requires real ffmpeg (the fork) + a GPU matching the corpus backend —
// in-pod only, like TestReplayCorpus's ffmpeg mode. Skipped when
// REPLAY_NO_FFMPEG is set (so the rewriter-only PR-CI lane skips it) or
// ffmpeg is absent. Run it in a worker pod:
//
//	go test -tags=replay -c -o /tmp/replay.test ./worker/agent
//	POD=$(kubectl -n plex-test get pod -l app.kubernetes.io/controller=worker -o jsonpath='{.items[0].metadata.name}')
//	kubectl -n plex-test cp /tmp/replay.test "$POD:/tmp/replay.test"
//	kubectl -n plex-test exec "$POD" -- /tmp/replay.test \
//	  -test.run TestReplayCorpus_DashMuxer -test.v -test.timeout 20m
//
// Knobs: REPLAY_CORPUS_DIR (default ~/scaleplex-corpus), REPLAY_DASH_MAX
// (max cells to run, default 6 — keeps the in-pod run bounded),
// REPLAY_TIMEOUT (per-cell ffmpeg timeout, default 20s).

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReplayCorpus_DashMuxer(t *testing.T) {
	if os.Getenv("REPLAY_NO_FFMPEG") != "" {
		t.Skip("dash-muxer e2e needs real ffmpeg; unset REPLAY_NO_FFMPEG and run in a worker pod")
	}
	if _, err := os.Stat("/usr/bin/ffmpeg"); err != nil {
		t.Skip("no /usr/bin/ffmpeg on this host — in-pod test")
	}

	// Same backend init + Pass-gate stub as TestReplayCorpus: validate the
	// rewriter+muxer, not the Plex-Pass gate.
	if b := os.Getenv("REPLAY_BACKEND"); b != "" {
		t.Setenv("WORKER_BACKEND", b)
	}
	activeDialect = selectDialect()
	t.Logf("replay backend: %s", activeDialect.backendName())
	passCheck = func(_, _ string) (bool, error) { return true, nil }

	// Fake-PMS: accept the dashenc manifest PUT (and anything else ffmpeg
	// POSTs — progress/segment_list, though this test strips those) and
	// tally manifest hits. ffmpeg PUTs to <base>/.../session/<sid>/.../manifest.
	var mu sync.Mutex
	manifestHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "manifest") {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			manifestHits++
			mu.Unlock()
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hits := func() int { mu.Lock(); defer mu.Unlock(); return manifestHits }

	corpusDir := os.Getenv("REPLAY_CORPUS_DIR")
	if corpusDir == "" {
		home, _ := os.UserHomeDir()
		corpusDir = filepath.Join(home, "scaleplex-corpus")
	}
	maxCells := 6
	if v := os.Getenv("REPLAY_DASH_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxCells = n
		}
	}
	timeout := 20 * time.Second
	if v := os.Getenv("REPLAY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}

	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("read corpus dir %s: %v", corpusDir, err)
	}

	ran := 0
	for _, ent := range entries {
		if ran >= maxCells {
			break
		}
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(corpusDir, ent.Name()))
		if err != nil {
			continue
		}
		var c replayCapture
		if err := json.Unmarshal(body, &c); err != nil || len(c.Argv) == 0 || c.SessionID == "" {
			continue
		}
		// Dash-shape only, and only cells that carry a loopback
		// `-manifest_name` — those are the ones whose manifest MUST be
		// republished to the relay; a cell with no manifest URL has nothing
		// to assert.
		if !argvHasMuxer(c.Argv, "dash") || !hasLoopbackManifest(c.Argv) {
			continue
		}
		// Real ffmpeg needs the source on disk.
		if c.SourcePath == "" {
			continue
		}
		if _, err := os.Stat(c.SourcePath); err != nil {
			t.Logf("skip %s: source missing %s", c.SessionID, c.SourcePath)
			continue
		}
		// Guard the codec-swapped synthetic cells (#150): their `-i` no
		// longer matches the decode `-codec`, so real ffmpeg would fail to
		// decode. ffprobe the source and skip a mismatch. (No-op for organic
		// captures — their decode codec always matches the source.)
		if dec := decodeCodecOf(c.Argv); dec != "" {
			probed := probeVideoCodec(c.SourcePath)
			if probed != "" && !codecMatches(dec, probed) {
				t.Logf("skip %s: decode=%s but source is %s (codec-swapped synthetic)", c.SessionID, dec, probed)
				continue
			}
		}

		ran++
		t.Run(c.SessionID, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range c.Env {
				env[k] = v
			}
			env["SCALEPLEX_PMS_BASE_URL"] = srv.URL
			env["X_PLEX_TOKEN"] = "replaytoken"

			res := Rewrite(c.Argv, env, &RewriteOpts{
				SessionDir: filepath.Join("/tmp/replay", c.SessionID),
			})
			if !res.Applied {
				t.Skipf("rewriter hard-bailed (skip:%s) — argv isn't a rewriter product", bailReasonOf(res.Changes))
			}
			// A reshaped cell that started with a loopback `-manifest_name`
			// MUST now point it at the relay (SCALEPLEX_PMS_BASE_URL=the
			// fake). We deliberately DON'T skip when the rewrite tag is
			// absent: a regression that drops the rewrite leaves the loopback
			// URL in res.Args, ffmpeg PUTs to 127.0.0.1:32400 (nothing
			// listening) → ECONNREFUSED → non-zero exit, which the run
			// assertion below catches. That's exactly PR #144's Bug B.
			if hasLoopbackManifest(res.Args) {
				t.Logf("WARNING: res.Args still carries a loopback -manifest_name — expecting the ffmpeg run to fail (Bug-B-style regression)")
			}

			args := patchForDashReplay(res.Args, c.SessionID)
			dir := filepath.Join("/tmp/replay", c.SessionID)
			_ = os.MkdirAll(dir, 0o755)

			before := hits()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/usr/bin/ffmpeg", args...)
			cmd.Env = appendOrSetEnv(os.Environ(), res.Env)
			cmd.Dir = dir
			out, runErr := cmd.CombinedOutput()

			tail := string(out)
			if len(tail) > 1500 {
				tail = "..." + tail[len(tail)-1500:]
			}
			tail = strings.ReplaceAll(strings.ReplaceAll(tail, "\r", "\\r"), "\n", "\\n")

			if ctx.Err() == context.DeadlineExceeded {
				t.Fatalf("ffmpeg hung past %s; tail=%s", timeout, tail)
			}
			if runErr != nil {
				t.Fatalf("ffmpeg exited non-zero (%v) — dash muxer / manifest PUT failure; tail=%s", runErr, tail)
			}
			got := hits() - before
			if got <= 0 {
				t.Fatalf("ffmpeg exited 0 but fake-PMS received no manifest PUT — dashenc never published (regression in -manifest_name rewrite / patch 0095); tail=%s", tail)
			}
			t.Logf("ok: %d manifest PUT(s) to fake-PMS, ffmpeg exit 0", got)
		})
	}

	if ran == 0 {
		t.Skip("no runnable dash cell (dash-shape + source present + codec match) in corpus — run in-pod against ~/scaleplex-corpus or the synth fixture")
	}
}

// argvHasMuxer reports whether argv selects output muxer mux via `-f <mux>`.
func argvHasMuxer(argv []string, mux string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-f" && argv[i+1] == mux {
			return true
		}
	}
	return false
}

// decodeCodecOf returns the input video decoder — the value of the first
// `-codec:<spec>` (ordinal or `:#0xNN`) appearing before the first `-i`.
func decodeCodecOf(argv []string) string {
	for i, a := range argv {
		if a == "-i" {
			return ""
		}
		if strings.HasPrefix(a, "-codec:") && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// probeVideoCodec ffprobes the first video stream's codec_name. Returns
// "" on any error (caller treats "" as "can't tell — don't gate").
func probeVideoCodec(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/ffprobe",
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", path).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// codecMatches compares a PMS decode-codec token to an ffprobe codec_name,
// tolerating the hevc/h265 spelling split.
func codecMatches(decode, probed string) bool {
	norm := func(s string) string {
		switch s {
		case "h265":
			return "hevc"
		default:
			return s
		}
	}
	return norm(decode) == norm(probed)
}

// hasLoopbackManifest reports whether argv carries a `-manifest_name`
// pointing at the PMS loopback (http://127.0.0.1:32400) — the URL the
// rewriter must republish to the relay and the worker pod can't reach.
func hasLoopbackManifest(argv []string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-manifest_name" && strings.HasPrefix(argv[i+1], "http://127.0.0.1:32400") {
			return true
		}
	}
	return false
}

// patchForDashReplay prepares a rewritten dash argv for a real in-pod
// ffmpeg run that exercises the manifest PUT. Unlike patchForReplay it
// KEEPS the rewritten `-manifest_name <fake-PMS-url>` (the whole point),
// and additionally strips the seek options (`-ss`, `-skip_to_segment`)
// so the muxer publishes a manifest from the file start within the short
// `-t` window regardless of the captured cell's seek offset. Network
// options other than the manifest (`-progressurl`, `-segment_list`) are
// dropped so ffmpeg doesn't block on PMS endpoints this test doesn't fake.
func patchForDashReplay(args []string, sid string) []string {
	tmpBase := filepath.Join("/tmp/replay", sid)
	_ = os.MkdirAll(tmpBase, 0o755)
	stubSRT := filepath.Join(tmpBase, "stub.srt")
	_ = os.WriteFile(stubSRT, []byte("1\n00:00:00,000 --> 00:01:00,000\nreplay\n\n"), 0o644)

	out := make([]string, 0, len(args)+4)
	skipNext := 0
	for i, a := range args {
		if skipNext > 0 {
			skipNext--
			continue
		}
		switch a {
		case "-progressurl", "-segment_list", "-ss", "-skip_to_segment":
			if i+1 < len(args) {
				skipNext = 1
			}
			continue
		}
		if strings.Contains(a, "subtitles=filename='") && strings.Contains(a, "/temp-") {
			a = replaceSubtitlesFilename(a, stubSRT)
		}
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
		out = append(out, a)
	}

	// Cap encode duration and redirect the local manifest positional into
	// the sandbox (the muxer still PUTs the manifest to -manifest_name; the
	// positional is just where it writes a local copy + segment base).
	if len(out) > 0 && !strings.HasPrefix(out[len(out)-1], "-") {
		out[len(out)-1] = filepath.Join(tmpBase, "out.mpd")
	}
	out = append(out[:len(out)-1], append([]string{"-t", "0.3"}, out[len(out)-1])...)
	return out
}
