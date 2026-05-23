package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempSRT(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sub.srt")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	return p
}

// Plain bottom-aligned SRT → the agent tightens the band below the static
// fallback and returns the matching y-offset.
func TestResolveAgentBand_TightensPlainSRT(t *testing.T) {
	fallback := subPrerenderBandHeight(2160)
	spec := &SubPrerenderSpec{Height: 2160, BandHeight: fallback, ResolveBandPostExtract: true}
	srt := writeTempSRT(t, "1\n00:00:01,000 --> 00:00:04,000\nHello world\n\n")

	bandY := ResolveAgentBand(spec, srt)

	tight := srtTightBandHeight(2160, 1)
	if spec.BandHeight != tight {
		t.Errorf("BandHeight = %d, want tight %d", spec.BandHeight, tight)
	}
	if spec.BandHeight >= fallback {
		t.Errorf("BandHeight = %d, want < fallback %d", spec.BandHeight, fallback)
	}
	if bandY != 2160-spec.BandHeight {
		t.Errorf("bandY = %d, want %d", bandY, 2160-spec.BandHeight)
	}
}

// Positioned cue (\an8 = top) → resolveSRTBand bails; the agent keeps the
// static fallback band (a tight crop would clip the off-band cue).
func TestResolveAgentBand_PositionedKeepsFallback(t *testing.T) {
	fallback := subPrerenderBandHeight(2160)
	spec := &SubPrerenderSpec{Height: 2160, BandHeight: fallback, ResolveBandPostExtract: true}
	srt := writeTempSRT(t,
		"1\n00:00:01,000 --> 00:00:04,000\n{\\an8}Top sign\n\n"+
			"2\n00:00:05,000 --> 00:00:09,000\nBottom dialogue\n\n")

	bandY := ResolveAgentBand(spec, srt)

	if spec.BandHeight != fallback {
		t.Errorf("BandHeight = %d, want fallback %d (positioned cue)", spec.BandHeight, fallback)
	}
	if bandY != 2160-fallback {
		t.Errorf("bandY = %d, want %d", bandY, 2160-fallback)
	}
}

func TestResolveAgentBand_NoOps(t *testing.T) {
	srt := writeTempSRT(t, "1\n00:00:01,000 --> 00:00:04,000\nHi\n\n")
	fallback := subPrerenderBandHeight(2160)

	cases := []struct {
		name string
		spec *SubPrerenderSpec
		file string
	}{
		{"not-flagged", &SubPrerenderSpec{Height: 2160, BandHeight: fallback}, srt},
		{"bitmap", &SubPrerenderSpec{Height: 2160, BandHeight: fallback, ResolveBandPostExtract: true, Bitmap: true}, srt},
		{"empty-file", &SubPrerenderSpec{Height: 2160, BandHeight: fallback, ResolveBandPostExtract: true}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := c.spec.BandHeight
			bandY := ResolveAgentBand(c.spec, c.file)
			if c.spec.BandHeight != before {
				t.Errorf("BandHeight mutated %d→%d, want no-op", before, c.spec.BandHeight)
			}
			if bandY != c.spec.Height-before {
				t.Errorf("bandY = %d, want %d", bandY, c.spec.Height-before)
			}
		})
	}
	// nil spec is tolerated.
	if got := ResolveAgentBand(nil, srt); got != 0 {
		t.Errorf("nil spec = %d, want 0", got)
	}
}

// NOTE: the rewriter→sentinel→agent-resolve→patch END-TO-END flow is gone
// from the HW path (merged inlineass branch, patch 0115 — no FIFO pre-render,
// no __SP_BAND* sentinels). The band.go helpers (ResolveAgentBand /
// PatchMainArgsBand / srtTightBandHeight) stay covered by the pure-function
// tests above + TestPatchMainArgsBand below until band.go is removed
// post-live-validation. The obsolete TestRewriter_AgentResolve_EndToEnd was
// dropped here.

func TestPatchMainArgsBand(t *testing.T) {
	args := []string{
		"-filter_complex",
		"[2:v]format=bgra,hwupload,scale_vaapi=w=3840:h=" + BandHSentinel + "[12];" +
			"[11][12]overlay_vaapi=x=0:y=" + BandYSentinel + ":eof_action=pass[4]",
		"-map", "[4]",
	}
	n := PatchMainArgsBand(args, 1620, 540)
	if n != 2 {
		t.Errorf("patched %d, want 2", n)
	}
	fc := args[1]
	if strings.Contains(fc, "__SP_BAND") {
		t.Errorf("sentinels remain: %q", fc)
	}
	if !strings.Contains(fc, "h=540[12]") || !strings.Contains(fc, "y=1620:") {
		t.Errorf("substituted values wrong: %q", fc)
	}
	// Idempotent: a second pass finds nothing.
	if n2 := PatchMainArgsBand(args, 1620, 540); n2 != 0 {
		t.Errorf("second pass patched %d, want 0", n2)
	}
}
