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

// Emit-equality harness for the orthogonal-detector refactor: for every corpus
// argv, compare the CURRENT rewrite emit against the UNIFIED path
// (extractGraphFacts -> composeBurn) on abstract properties — scale w/h,
// tonemap algo, has-inlineass, has-overlay_vaapi — so label numbers and the
// SW-text hwdownload/hwupload bracket wash out. Reports parity vs the intended
// improvements (so the dispatch swap can be done knowing exactly what changes).
// Build-tagged replay; honors REPLAY_CORPUS_DIR (default ~/scaleplex-corpus).
func TestReplayCorpus_OrthogonalEmitParity(t *testing.T) {
	dir := os.Getenv("REPLAY_CORPUS_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "scaleplex-corpus")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus dir %s: %v", dir, err)
	}
	tm := resolveTonemapConfig()
	var considered, parity, diffAlgo, diffOther int
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
		inGraph := videoFilterComplex(c.Argv)
		if inGraph == "" {
			continue
		}
		facts := extractGraphFacts(inGraph, nil)
		if !facts.ok {
			continue // unmodeled shape — unified path bails too (parity by bail)
		}
		out := Rewrite(c.Argv, c.Env, &RewriteOpts{SessionDir: filepath.Join("/tmp/replay", c.SessionID)})
		if !out.Applied {
			continue // honor/skip — not a reshape
		}
		curGraph := videoFilterComplex(out.Args)
		if curGraph == "" {
			continue
		}
		considered++

		// Unified emit from the extracted facts.
		vaResident := indexOfArg(c.Argv, "-hwaccel:0", 0) >= 0
		uniGraph, _ := tm.composeBurn(burnSpec{
			vaResident: vaResident, w: facts.w, h: facts.h, hdr: facts.hdr, algo: facts.algo,
			burnSub: facts.subKind != "", subParams: facts.subParams,
		})

		cur := graphProps(curGraph)
		uni := graphProps(uniGraph)
		switch {
		case cur == uni:
			parity++
		case cur.scaleWH == uni.scaleWH && cur.hasInlineass == uni.hasInlineass &&
			cur.hasOverlay == uni.hasOverlay && cur.hasTonemap == uni.hasTonemap && cur.algo != uni.algo:
			diffAlgo++ // only the honored tonemap algo differs (intended: unified honors Plex's)
			if diffAlgo <= 8 {
				t.Logf("ALGO-DIFF %s: cur.algo=%q uni.algo=%q (%s)", ent.Name(), cur.algo, uni.algo, cur.scaleWH)
			}
		default:
			diffOther++
			if diffOther <= 12 {
				t.Logf("DIFF %s:\n  cur=%+v\n  uni=%+v", ent.Name(), cur, uni)
			}
		}
	}
	t.Logf("orthogonal emit parity: considered=%d parity=%d algo-only-diff=%d other-diff=%d",
		considered, parity, diffAlgo, diffOther)
	if diffOther > 0 {
		t.Errorf("%d corpus emits diverge beyond the allow-listed tonemap-algo difference — investigate before swapping the dispatch", diffOther)
	}
}

type emitProps struct {
	scaleWH      string
	hasTonemap   bool
	algo         string
	hasInlineass bool
	hasOverlay   bool
}

func graphProps(g string) emitProps {
	p := emitProps{
		hasInlineass: strings.Contains(g, "inlineass="),
		hasOverlay:   strings.Contains(g, "overlay_vaapi"),
	}
	if m := reGraphScaleWH.FindStringSubmatch(g); m != nil {
		p.scaleWH = m[1] + "x" + m[2]
	}
	switch {
	case reTonemapOpenCLAlgo.MatchString(g):
		p.hasTonemap, p.algo = true, reTonemapOpenCLAlgo.FindStringSubmatch(g)[1]
	case reGraphTonemapSW.MatchString(g):
		p.hasTonemap, p.algo = true, reGraphTonemapSW.FindStringSubmatch(g)[1]
	case strings.Contains(g, "tonemap_vaapi"):
		p.hasTonemap = true
	}
	return p
}

// videoFilterComplex returns the -filter_complex value that drives the video
// chain (the one referencing [0:0]), or the first if none reference it.
func videoFilterComplex(args []string) string {
	first := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-filter_complex" {
			continue
		}
		if first == "" {
			first = args[i+1]
		}
		if strings.Contains(args[i+1], "[0:0]") {
			return args[i+1]
		}
	}
	return first
}
