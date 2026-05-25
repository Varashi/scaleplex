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
	var checked, hdr int
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
		// Only entries whose INPUT graph is a bitmap overlay burn.
		inVF := filterComplexValue(c.Argv)
		if !strings.Contains(inVF, "overlay_vaapi") {
			continue
		}
		isHDR := strings.Contains(inVF, "tonemap_opencl") || strings.Contains(inVF, "tonemap_vaapi")

		out := Rewrite(c.Argv, c.Env, &RewriteOpts{
			SessionDir: filepath.Join("/tmp/replay", c.SessionID),
		})
		if !out.Applied {
			// A bail is reported by the main replay test; not this test's job.
			continue
		}
		checked++
		if isHDR {
			hdr++
		}
		outVF := filterComplexValue(out.Args)
		if strings.Contains(outVF, "overlay_vaapi") {
			t.Errorf("%s: overlay_vaapi survived the unification:\n%s", ent.Name(), outVF)
		}
		if !strings.Contains(outVF, "inlineass") {
			t.Errorf("%s: bitmap burn did not route to inlineass:\n%s", ent.Name(), outVF)
		}
		// HDR variant must keep the tonemap (honored) and stay VA-resident.
		if isHDR {
			if !strings.Contains(outVF, "tonemap_opencl") && !strings.Contains(outVF, "tonemap_vaapi") {
				t.Errorf("%s: HDR bitmap burn dropped the tonemap:\n%s", ent.Name(), outVF)
			}
			if strings.HasPrefix(outVF, "[0:0]hwupload") {
				t.Errorf("%s: HDR bitmap burn kept the decode->sysmem round-trip:\n%s", ent.Name(), outVF)
			}
		}
	}
	t.Logf("bitmap-overlay corpus entries reshaped: %d (HDR-tonemap variant: %d)", checked, hdr)
	if checked == 0 {
		t.Skip("no overlay_vaapi entries in corpus")
	}
}

// filterComplexValue returns the value following -filter_complex, or "".
func filterComplexValue(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-filter_complex" {
			return args[i+1]
		}
	}
	return ""
}
