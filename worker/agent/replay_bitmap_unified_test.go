//go:build replay
// +build replay

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Focused corpus check for the bitmap-burn unification: every captured argv
// whose filter graph contains overlay_vaapi (Plex's bitmap sub2video burn —
// SDR or the HDR tonemap variant) must, after Rewrite(), carry NO overlay_vaapi
// and burn via inlineass instead. Proves the unification holds against real
// captures, not just the hand-built fixtures. Run with -tags replay; honors
// REPLAY_CORPUS_DIR (default ~/scaleplex-corpus).
//
// A bitmap-overlay argv is expected to either reshape (the unified inlineass
// path) or take a recognized non-reshape route (honor-SW/hybrid passthrough, or
// a skip: bail). An UNEXPECTED bail — no skip reason, no honor — is surfaced as
// a failure so a regression in this branch can't pass silently.
func TestReplayCorpus_BitmapOverlayUnified(t *testing.T) {
	dir := os.Getenv("REPLAY_CORPUS_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "scaleplex-corpus")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus dir %s: %v", dir, err)
	}
	var candidates, reshaped, hdr, honored, skipped int
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		var c replayCapture
		if json.Unmarshal(body, &c) != nil || len(c.Argv) == 0 || c.SessionID == "" {
			continue
		}
		// Only entries whose INPUT has a bitmap overlay burn (scan ALL
		// -filter_complex graphs, not just the first — the first can be audio).
		inVF := overlayGraph(c.Argv)
		if inVF == "" {
			continue
		}
		candidates++
		isHDR := strings.Contains(inVF, "tonemap_opencl") || strings.Contains(inVF, "tonemap_vaapi")

		out := Rewrite(c.Argv, c.Env, &RewriteOpts{
			SessionDir: filepath.Join("/tmp/replay", c.SessionID),
		})
		if !out.Applied {
			if r := changePrefix(out.Changes, "skip:"); r != "" {
				skipped++ // legit non-reshape (e.g. no input) — tolerate
				continue
			}
			t.Errorf("%s: overlay argv bailed without a skip reason: %v", ent.Name(), out.Changes)
			continue
		}
		// Honor modes pass Plex's filter through unchanged (overlay survives by
		// design) — out of scope for the reshape assertion.
		if changePrefix(out.Changes, "honor:") != "" {
			honored++
			continue
		}

		reshaped++
		if isHDR {
			hdr++
		}
		// Output: overlay gone from EVERY graph, burn routed to inlineass.
		var outBurn string
		for i := 0; i+1 < len(out.Args); i++ {
			if out.Args[i] != "-filter_complex" {
				continue
			}
			g := out.Args[i+1]
			if strings.Contains(g, "overlay_vaapi") {
				t.Errorf("%s: overlay_vaapi survived the unification:\n%s", ent.Name(), g)
			}
			if strings.Contains(g, "inlineass") {
				outBurn = g
			}
		}
		if outBurn == "" {
			t.Errorf("%s: bitmap burn did not route to inlineass: %v", ent.Name(), out.Args)
			continue
		}
		// HDR variant must keep the tonemap (honored) and stay VA-resident.
		if isHDR {
			if !strings.Contains(outBurn, "tonemap_opencl") && !strings.Contains(outBurn, "tonemap_vaapi") {
				t.Errorf("%s: HDR bitmap burn dropped the tonemap:\n%s", ent.Name(), outBurn)
			}
			if strings.HasPrefix(outBurn, "[0:0]hwupload") {
				t.Errorf("%s: HDR bitmap burn kept the decode->sysmem round-trip:\n%s", ent.Name(), outBurn)
			}
		}
	}
	t.Logf("bitmap-overlay corpus: candidates=%d reshaped=%d (HDR=%d) honored=%d skipped=%d", candidates, reshaped, hdr, honored, skipped)
	if candidates == 0 {
		t.Skip("no overlay_vaapi candidates in corpus")
	}
	if reshaped == 0 {
		t.Errorf("%d overlay candidate(s) but none reshaped (honored=%d skipped=%d) — branch regression?", candidates, honored, skipped)
	}
}

// overlayGraph returns the first -filter_complex value containing overlay_vaapi,
// scanning every graph (the first -filter_complex may be audio), or "".
func overlayGraph(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-filter_complex" && strings.Contains(args[i+1], "overlay_vaapi") {
			return args[i+1]
		}
	}
	return ""
}

// changePrefix returns the first change with the given prefix, or "".
func changePrefix(changes []string, prefix string) string {
	for _, ch := range changes {
		if strings.HasPrefix(ch, prefix) {
			return ch
		}
	}
	return ""
}
