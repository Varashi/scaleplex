package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scaleplex#204: HDR-passthrough sub-burn (HDR source + Plex carried NO
// tonemap node + HEVC encoder). composeBurn used to emit
// `scale_vaapi=format=nv12` for every non-tonemap shape, even when the
// chain was meant to stay 10-bit; the Main10 pin (#189/#200) then
// contradicted the encoder's 8-bit input → iHD "No usable encoding profile
// found" / exit 218. The rewriter now keeps the chain 10-bit
// (`scale_vaapi=format=p010`, no tonemap stage) so the HEVC encoder gets a
// p010 surface that matches the Main10 pin.
func TestRewriter_HWDecode_HDRPassthrough_HEVC_KeepsTenBit(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0")
	dir := t.TempDir()
	srt := filepath.Join(dir, "temp-0.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:04,000\nHDR PT test\n\n"), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "av1",
		"-i", "/media/x.mkv",
		"-codec:1", "subrip",
		"-i", srt,
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-map_inlineass", "1:s:0",
		// Plex's HDR-passthrough shape: format=p010 throughout, NO tonemap node.
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=1600:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
		ProbeVideoColor: func(string) (string, string, string) {
			return "smpte2084", "bt2020", "bt2020nc"
		},
	})
	if !out.Applied {
		t.Fatalf("expected rewrite; changes=%v", out.Changes)
	}
	fc := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	// Main scale step must keep p010.
	if !strings.Contains(fc, "format=p010") {
		t.Errorf("HDR-passthrough must keep scale_vaapi format=p010; graph=%q", fc)
	}
	// No tonemap node (Plex didn't ask for one).
	for _, marker := range []string{"tonemap_opencl", "tonemap_vaapi", "tonemap_cuda"} {
		if strings.Contains(fc, marker) {
			t.Errorf("HDR-passthrough must NOT inject %s; graph=%q", marker, fc)
		}
	}
	// scale_vaapi=…format=nv12 would break Main10 — make sure it's gone.
	if strings.Contains(fc, "scale_vaapi=") && strings.Contains(fc, "format=nv12") {
		t.Errorf("HDR-passthrough must not downconvert main scale to nv12; graph=%q", fc)
	}
	// Main10 pin must still be present (encoder is fed p010).
	enc := -1
	for i, a := range out.Args {
		if a == "hevc_vaapi" {
			enc = i
			break
		}
	}
	if enc < 0 || enc+2 >= len(out.Args) || out.Args[enc+1] != "-profile:0" || out.Args[enc+2] != "main10" {
		t.Errorf("expected -profile:0 main10 right after hevc_vaapi; got: %v", out.Args[enc:])
	}
	// Tag sanity: HDR-source emitted, no tonemap-preserved tag, inlineass-vaapi
	// is the chosen filter swap.
	if !containsString(out.Changes, "video:hdr-source(smpte2084)") {
		t.Errorf("missing video:hdr-source(smpte2084) tag: %v", out.Changes)
	}
	if !containsString(out.Changes, "hw-decode:filter:inlineass-vaapi") {
		t.Errorf("missing hw-decode:filter:inlineass-vaapi tag: %v", out.Changes)
	}
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "hw-decode-sub:tonemap-preserved") {
			t.Errorf("unexpected tonemap-preserved tag on HDR-passthrough: %v", out.Changes)
		}
	}
	if !containsString(out.Changes, TagEncodeHEVCMain10) {
		t.Errorf("missing %s tag: %v", TagEncodeHEVCMain10, out.Changes)
	}
}

// Sanity: HDR source + sub-burn but Plex sent h264_vaapi → no 10-bit chain
// needed (h264 has no Main10), composeBurn keeps the nv12 default.
func TestRewriter_HWDecode_HDRPassthrough_H264_StaysEightBit(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0")
	dir := t.TempDir()
	srt := filepath.Join(dir, "temp-0.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:04,000\nh264 test\n\n"), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "av1",
		"-i", "/media/x.mkv",
		"-codec:1", "subrip",
		"-i", srt,
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "h264_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
		ProbeVideoColor: func(string) (string, string, string) {
			return "smpte2084", "bt2020", "bt2020nc"
		},
	})
	if !out.Applied {
		t.Fatalf("expected rewrite; changes=%v", out.Changes)
	}
	fc := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	if !strings.Contains(fc, "format=nv12") {
		t.Errorf("h264 encoder path should keep scale_vaapi format=nv12; graph=%q", fc)
	}
}
