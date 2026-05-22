package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// HW-decode + sidecar SRT burn-in at 4K with plain bottom-aligned cues
// should pick a tighter pre-render band than the static 40% fallback,
// emit the `sub-prerender:band:tight` change tag, and place the overlay
// at y = Height - BandHeight.
func TestRewriter_SubPrerender_SRT_TightBand(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0") // pin native: test band logic, not the render cap
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
	fallback := subPrerenderBandHeight(2160)
	tight := srtTightBandHeight(2160, 1)
	if sp.BandHeight != tight {
		t.Errorf("BandHeight = %d, want %d (tight 1-line)", sp.BandHeight, tight)
	}
	if sp.BandHeight >= fallback {
		t.Errorf("BandHeight = %d, want < fallback %d", sp.BandHeight, fallback)
	}
	wantY := 2160 - sp.BandHeight
	if !strings.Contains(out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1],
		fmt.Sprintf("overlay_vaapi=x=0:y=%d:", wantY)) {
		t.Errorf("filter graph missing tight overlay y-offset y=%d", wantY)
	}
	gotTight := false
	for _, c := range out.Changes {
		if c == "sub-prerender:band:tight" {
			gotTight = true
		}
	}
	if !gotTight {
		t.Errorf("missing sub-prerender:band:tight tag: %v", out.Changes)
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
	fc := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	wantUp := fmt.Sprintf("hwupload,scale_vaapi=w=3840:h=%d[12]", sp.BandHeight)
	if !strings.Contains(fc, wantUp) {
		t.Errorf("filter graph missing sub upscale %q: %q", wantUp, fc)
	}
	// Overlay y-offset stays in output coords.
	if !strings.Contains(fc, fmt.Sprintf("overlay_vaapi=x=0:y=%d:", 2160-sp.BandHeight)) {
		t.Errorf("overlay y-offset must stay in output coords: %q", fc)
	}
	gotTag := false
	for _, c := range out.Changes {
		if c == "sub-prerender:render=1920x1080" {
			gotTag = true
		}
	}
	if !gotTag {
		t.Errorf("missing sub-prerender:render tag: %v", out.Changes)
	}
}

// Positional override (\an8 = top) in an otherwise plain SRT must
// fall back to the static band — libass would render the cue off the
// band, and a tight crop would clip it.
func TestRewriter_SubPrerender_SRT_PositionedFallback(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0") // pin native: test band logic, not the render cap
	dir := t.TempDir()
	srt := filepath.Join(dir, "temp-0.srt")
	if err := os.WriteFile(srt, []byte(
		"1\n00:00:01,000 --> 00:00:04,000\n{\\an8}Top sign\n\n"+
			"2\n00:00:05,000 --> 00:00:09,000\nBottom dialogue\n\n"), 0o644); err != nil {
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
		"-start_at_zero", "-copyts",
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
	if got, want := out.SubPrerender.BandHeight, subPrerenderBandHeight(2160); got != want {
		t.Errorf("BandHeight = %d, want %d (static fallback for positioned cue)", got, want)
	}
	for _, c := range out.Changes {
		if c == "sub-prerender:band:tight" {
			t.Errorf("tight tag should NOT emit when positioned cue forces fallback: %v", out.Changes)
		}
	}
}

// Embedded SRT (no sidecar file path) keeps the static band — the file
// isn't on disk yet at rewrite time, the agent extracts it later. The
// rewriter has no path to parse; v1.2.1 leaves embedded on the fallback.
func TestRewriter_SubPrerender_SRT_EmbeddedFallback(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0") // pin native: test band logic, not the render cap
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
	if got, want := out.SubPrerender.BandHeight, subPrerenderBandHeight(2160); got != want {
		t.Errorf("BandHeight = %d, want %d (static fallback for embedded SRT)", got, want)
	}
	for _, c := range out.Changes {
		if c == "sub-prerender:band:tight" {
			t.Errorf("tight tag should NOT emit for embedded SRT (no file at rewrite time): %v", out.Changes)
		}
	}
}
