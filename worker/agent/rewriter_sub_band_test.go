package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// HW-decode + sidecar SRT at 4K, native render height (cap opted out): the
// merged inlineass HW branch (patch 0115) keeps the VAAPI surface and burns
// via the fork's inlineass — no FIFO pre-render, no overlay_vaapi, no band
// sentinels. -map_inlineass + the decode sink stay; render_height=0 (no cap).
func TestRewriter_HWDecode_SRT_Sidecar_Inlineass(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0") // native render, no cap
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
	if !out.Applied {
		t.Fatalf("expected rewrite; changes=%v", out.Changes)
	}
	fc := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	if !strings.Contains(fc, "inlineass=") {
		t.Errorf("filter graph missing inlineass: %q", fc)
	}
	if !strings.Contains(fc, ":render_height=0") {
		t.Errorf("filter graph missing render_height=0 (native): %q", fc)
	}
	if strings.Contains(fc, "overlay_vaapi") || strings.Contains(fc, "hwdownload") {
		t.Errorf("merged HW branch must drop overlay_vaapi + the hwdownload/hwupload bracket: %q", fc)
	}
	// The fork's feed must survive: -map_inlineass + the decode sink.
	if indexOfArg(out.Args, "-map_inlineass", 0) < 0 {
		t.Error("-map_inlineass must be kept (drives the scaleplex_inlineass feed)")
	}
	if !containsString(out.Args, "ass") || indexOfArg(out.Args, "-codec", 0) < 0 {
		t.Error("decode sink (-map 1:s:0 -f null -codec ass nullfile) must be kept")
	}
	if !containsString(out.Changes, "hw-decode:filter:inlineass-vaapi") {
		t.Errorf("missing hw-decode:filter:inlineass-vaapi tag: %v", out.Changes)
	}
}

// SCALEPLEX_SUB_RENDER_HEIGHT=1080 on a 4K session: the cap is now a filter
// option (render_height=1080) — the fork rasterises libass low + VPP-upscales.
func TestRewriter_HWDecode_SRT_RenderHeightCap(t *testing.T) {
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
	if !out.Applied {
		t.Fatalf("expected merged inlineass branch (no pre-render); changes=%v", out.Changes)
	}
	fc := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	if !strings.Contains(fc, "inlineass=") || !strings.Contains(fc, ":render_height=1080") {
		t.Errorf("filter graph missing inlineass render_height=1080: %q", fc)
	}
	if strings.Contains(fc, "overlay_vaapi") {
		t.Errorf("merged HW branch must drop overlay_vaapi: %q", fc)
	}
}

// Embedded SRT (no sidecar file path, -map_inlineass 0:3): same merged HW
// branch. The fork's binding reads the stream directly via -map_inlineass —
// no extraction, no pre-render.
func TestRewriter_HWDecode_SRT_Embedded_Inlineass(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0")
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
		"-map", "0:3", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied {
		t.Fatalf("expected merged inlineass branch (no pre-render); changes=%v", out.Changes)
	}
	fc := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	if !strings.Contains(fc, "inlineass=") {
		t.Errorf("filter graph missing inlineass: %q", fc)
	}
	if mi := indexOfArg(out.Args, "-map_inlineass", 0); mi < 0 || out.Args[mi+1] != "0:3" {
		t.Errorf("-map_inlineass 0:3 must be kept for embedded SRT feed")
	}
}
