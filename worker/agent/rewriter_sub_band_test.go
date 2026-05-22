package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// HW-decode + sidecar SRT at 4K: the rewriter now DEFERS band resolution
// to the agent — it seeds the static-fallback band, flags
// ResolveBandPostExtract, emits the __SP_BANDY__ overlay sentinel, and
// tags `sub-prerender:band:agent-resolve` (no rewrite-time `band:tight`).
// The actual tight-band decision is tested agent-side in band_test.go.
func TestRewriter_SubPrerender_SRT_DefersBand_Sidecar(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0") // native: only the y sentinel, no h sentinel
	dir := t.TempDir()
	srt := filepath.Join(dir, "temp-0.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:04,000\nHello world\n\n"), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}

	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc",
		"-i", "/media/x.mkv",
		"-codec:1", "subrip",
		"-i", srt,
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied || out.SubPrerender == nil {
		t.Fatalf("expected rewrite + SubPrerender; changes=%v", out.Changes)
	}
	sp := out.SubPrerender
	if !sp.ResolveBandPostExtract {
		t.Error("ResolveBandPostExtract = false, want true")
	}
	if sp.BandHeight != subPrerenderBandHeight(2160) {
		t.Errorf("BandHeight = %d, want seeded fallback %d", sp.BandHeight, subPrerenderBandHeight(2160))
	}
	fc := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	if !strings.Contains(fc, "overlay_vaapi=x=0:y="+BandYSentinel+":") {
		t.Errorf("filter graph missing y sentinel: %q", fc)
	}
	if strings.Contains(fc, BandHSentinel) {
		t.Errorf("native render must not emit the h sentinel: %q", fc)
	}
	gotAgent, gotTight := false, false
	for _, c := range out.Changes {
		if c == "sub-prerender:band:agent-resolve" {
			gotAgent = true
		}
		if c == "sub-prerender:band:tight" {
			gotTight = true
		}
	}
	if !gotAgent {
		t.Errorf("missing sub-prerender:band:agent-resolve tag: %v", out.Changes)
	}
	if gotTight {
		t.Errorf("rewriter must not emit band:tight (agent decides now): %v", out.Changes)
	}
}

func TestSubRenderDims(t *testing.T) {
	cases := []struct {
		name         string
		env          string
		outW, outH   int
		wantW, wantH int
		wantDown     bool
	}{
		{"default-1080-on-4k", "", 3840, 2160, 1920, 1080, true},
		{"explicit-720-on-4k", "720", 3840, 2160, 1280, 720, true},
		{"explicit-1440-on-4k", "1440", 3840, 2160, 2560, 1440, true},
		{"opt-out-native", "0", 3840, 2160, 3840, 2160, false},
		{"1080p-output-native", "", 1920, 1080, 1920, 1080, false}, // cap >= outH
		{"720p-output-native", "", 1280, 720, 1280, 720, false},
		{"1440p-output-capped", "", 2560, 1440, 1920, 1080, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", c.env)
			w, h, down := subRenderDims(c.outW, c.outH)
			if w != c.wantW || h != c.wantH || down != c.wantDown {
				t.Errorf("subRenderDims(%d,%d) env=%q = (%d,%d,%v), want (%d,%d,%v)",
					c.outW, c.outH, c.env, w, h, down, c.wantW, c.wantH, c.wantDown)
			}
		})
	}
}

// SCALEPLEX_SUB_RENDER_HEIGHT=1080 on a 4K session: the pre-render
// renders the band at 1920x1080 and the main graph adds a scale_vaapi
// upscale (to Width × BandHeight) before overlay_vaapi. The overlay
// y-offset and BandHeight stay in output coords.
func TestRewriter_SubPrerender_SRT_LowRes(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "1080")
	dir := t.TempDir()
	srt := filepath.Join(dir, "temp-0.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:04,000\nHello world\n\n"), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc",
		"-i", "/media/x.mkv",
		"-codec:1", "subrip",
		"-i", srt,
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied || out.SubPrerender == nil {
		t.Fatalf("expected rewrite + SubPrerender; changes=%v", out.Changes)
	}
	sp := out.SubPrerender
	if sp.RenderWidth != 1920 || sp.RenderHeight != 1080 {
		t.Errorf("render dims = %dx%d, want 1920x1080", sp.RenderWidth, sp.RenderHeight)
	}
	if !sp.ResolveBandPostExtract {
		t.Error("ResolveBandPostExtract = false, want true")
	}
	fc := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	// Both band-dependent values are sentinels the agent patches post-resolve:
	// the scale_vaapi upscale target height and the overlay y-offset.
	if !strings.Contains(fc, "hwupload,scale_vaapi=w=3840:h="+BandHSentinel+"[12]") {
		t.Errorf("filter graph missing sub upscale h sentinel: %q", fc)
	}
	if !strings.Contains(fc, "overlay_vaapi=x=0:y="+BandYSentinel+":") {
		t.Errorf("filter graph missing overlay y sentinel: %q", fc)
	}
	gotRender, gotAgent := false, false
	for _, c := range out.Changes {
		if c == "sub-prerender:render=1920x1080" {
			gotRender = true
		}
		if c == "sub-prerender:band:agent-resolve" {
			gotAgent = true
		}
	}
	if !gotRender || !gotAgent {
		t.Errorf("missing render/agent-resolve tags: %v", out.Changes)
	}
}

// Embedded SRT (no sidecar file path) now ALSO defers to the agent — the
// key win of agent-side resolve. At rewrite time the file isn't on disk,
// so the rewriter seeds the fallback band + flags ResolveBandPostExtract
// + emits the y sentinel; the agent extracts the SRT and runs the tight
// resolve post-extraction (covered in band_test.go). Previously embedded
// was stuck on the static band.
func TestRewriter_SubPrerender_SRT_EmbeddedDefersBand(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0") // native: only the y sentinel
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc",
		"-i", "/media/x.mkv",
		"-map_inlineass", "0:3",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied || out.SubPrerender == nil {
		t.Fatalf("expected rewrite + SubPrerender; changes=%v", out.Changes)
	}
	sp := out.SubPrerender
	if !sp.Embedded {
		t.Error("Embedded = false, want true")
	}
	if !sp.ResolveBandPostExtract {
		t.Error("ResolveBandPostExtract = false, want true (embedded SRT now defers to agent)")
	}
	fc := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	if !strings.Contains(fc, "overlay_vaapi=x=0:y="+BandYSentinel+":") {
		t.Errorf("filter graph missing y sentinel for embedded SRT: %q", fc)
	}
	gotAgent := false
	for _, c := range out.Changes {
		if c == "sub-prerender:band:agent-resolve" {
			gotAgent = true
		}
	}
	if !gotAgent {
		t.Errorf("missing sub-prerender:band:agent-resolve tag: %v", out.Changes)
	}
}
