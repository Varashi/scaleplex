package main

import (
	"strings"
	"testing"
)

// Bare short codec name in -codec:0 (hevc / h264 / av1 / vp9) without
// a -hwaccel:0 flag, paired with a SW-shaped encoder + filter chain.
// PMS sometimes emits this when its HW probe failed mid-session or
// when an older client negotiated SW but a newer ffmpeg fork would
// have shaped HW.
//
// The rewriter should treat this the same as a libfoo→hw upgrade:
// inject hwaccel flags after the bare codec, then run the SW-upgrade
// tail (encoder swap, scale_vaapi filter rewrite).
func TestRewriter_BareHEVC_NoHWAccel_UpgradesToVAAPI(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/media/Movies/HEVCSource.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264",
		"-crf:0", "16",
		"-preset:0", "veryfast",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "decode:bare-hw-upgrade:hevc") {
		t.Fatalf("expected decode:bare-hw-upgrade:hevc: %v", out.Changes)
	}
	// hwaccel flags must be present after the bare codec.
	for _, want := range []string{
		"-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi",
		"-hwaccel_device:0", "vaapi",
	} {
		if !containsString(out.Args, want) {
			t.Errorf("missing hwaccel flag %q in args", want)
		}
	}
	// SW encoder must have been swapped to VAAPI.
	if containsString(out.Args, "libx264") {
		t.Errorf("libx264 must have been swapped to h264_vaapi: %v", out.Args)
	}
	if !containsString(out.Args, "h264_vaapi") {
		t.Errorf("expected h264_vaapi encoder: %v", out.Args)
	}
	// Filter chain must have been hardware-shaped.
	var rewritten string
	for i, a := range out.Args {
		if a == "-filter_complex" && i+1 < len(out.Args) {
			rewritten = out.Args[i+1]
		}
	}
	if !strings.Contains(rewritten, "scale_vaapi") {
		t.Errorf("expected scale_vaapi in filter chain: %s", rewritten)
	}
}

// Same shape but for bare h264. PMS upgrade should be symmetric.
func TestRewriter_BareH264_NoHWAccel_UpgradesToVAAPI(t *testing.T) {
	args := []string{
		"-codec:0", "h264",
		"-i", "/media/Movies/H264Source.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1280:h=720[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264",
		"-crf:0", "18",
		"-preset:0", "veryfast",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "decode:bare-hw-upgrade:h264") {
		t.Fatalf("expected decode:bare-hw-upgrade:h264: %v", out.Changes)
	}
}

// Bare codec WITH -hwaccel:0 must still take the hw-passthrough path
// (regression guard — the new branch must not steal this case).
func TestRewriter_BareHEVC_WithHWAccel_StaysPassthrough(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi",
		"-hwaccel_device:0", "vaapi",
		"-i", "/media/Movies/HEVCSource.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1];[1]hwupload[2]",
		"-map", "[2]",
		"-codec:0", "h264_vaapi",
		"-qp:0", "22",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "decode:hw-passthrough:hevc") {
		t.Fatalf("expected hw-passthrough path: %v", out.Changes)
	}
	if containsString(out.Changes, "decode:bare-hw-upgrade:hevc") {
		t.Fatalf("must NOT take bare-hw-upgrade path when -hwaccel:0 is set")
	}
}
