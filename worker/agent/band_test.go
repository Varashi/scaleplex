package main

import (
	"os"
	"path/filepath"
	"strconv"
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

// End-to-end: rewriter emits sentinels → agent resolves the band →
// patches the main argv. Proves no sentinel leaks into the final ffmpeg
// command (a leaked __SP_BAND* would hard-fail the transcode) and that
// the patched overlay y= / scale_vaapi h= match the agent-resolved tight
// band. 4K + SCALEPLEX_SUB_RENDER_HEIGHT=1080 exercises BOTH sentinels.
func TestRewriter_AgentResolve_EndToEnd(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "1080")
	srt := writeTempSRT(t, "1\n00:00:01,000 --> 00:00:04,000\nHello world\n\n")
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:", "-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc", "-i", "/media/x.mkv",
		"-codec:1", "subrip", "-i", srt,
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]", "-codec:0", "hevc_vaapi", "-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied || out.SubPrerender == nil {
		t.Fatalf("rewrite failed: %v", out.Changes)
	}
	// Agent steps (mirror main.go): resolve the band, then patch the argv.
	bandY := ResolveAgentBand(out.SubPrerender, srt)
	// Both sentinels (overlay y + scale_vaapi h) must be present and patched
	// under the 1080 render cap — a regression that drops either would patch
	// fewer than 2 and is caught here, ahead of the no-leak check below.
	if patched := PatchMainArgsBand(out.Args, bandY, out.SubPrerender.BandHeight); patched != 2 {
		t.Fatalf("PatchMainArgsBand patched %d sentinels, want 2 (y + h)", patched)
	}

	tight := srtTightBandHeight(2160, 1)
	if out.SubPrerender.BandHeight != tight {
		t.Errorf("resolved BandHeight = %d, want tight %d", out.SubPrerender.BandHeight, tight)
	}
	fc := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	for _, a := range out.Args {
		if strings.Contains(a, "__SP_BAND") {
			t.Fatalf("sentinel leaked into final argv: %q", a)
		}
	}
	if !strings.Contains(fc, "scale_vaapi=w=3840:h="+itoa(tight)+"[12]") {
		t.Errorf("scale_vaapi h not patched to tight band: %q", fc)
	}
	if !strings.Contains(fc, "overlay_vaapi=x=0:y="+itoa(2160-tight)+":") {
		t.Errorf("overlay y not patched to tight band: %q", fc)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

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
