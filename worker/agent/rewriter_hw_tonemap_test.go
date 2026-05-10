package main

import (
	"strings"
	"testing"
)

// PMS HW-decode HDR→SDR filter chain (the buggy one — naive
// format=nv12 conversion with no tonemap). Live observation
// 2026-05-10 PM during a forced videoCodec=h264 streaming session
// against Big Hero 6 4K HDR.
func TestInjectHWPassthroughTonemap_RewritesNV12ChainToTonemap(t *testing.T) {
	in := "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1];[1]hwupload[2]"
	out, ok := injectHWPassthroughTonemap(in)
	if !ok {
		t.Fatalf("expected match, got false")
	}
	want := "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=p010[1];[1]tonemap_vaapi=transfer=bt709:format=nv12[2]"
	if out != want {
		t.Fatalf("\ngot:  %s\nwant: %s", out, want)
	}
}

// HDR-preserving chain (format=p010) must NOT match — that's the
// HEVC-target shape where PMS keeps HDR through to encoder. Tonemap
// here would double-convert and break HDR pass-through.
func TestInjectHWPassthroughTonemap_LeavesP010ChainUntouched(t *testing.T) {
	in := "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=p010[1];[1]hwupload[2]"
	_, ok := injectHWPassthroughTonemap(in)
	if ok {
		t.Fatalf("p010 chain must not match SDR pattern")
	}
}

// Sub-burn / overlay / multi-step chains have their own dedicated
// rewrite paths — the simple pattern shouldn't snag them.
func TestInjectHWPassthroughTonemap_LeavesComplexChainUntouched(t *testing.T) {
	in := "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1];[1][2:s:0]overlay_vaapi[3]"
	_, ok := injectHWPassthroughTonemap(in)
	if ok {
		t.Fatalf("multi-step chain must not match the simple pattern")
	}
}

// End-to-end: HW-decode + HDR source + SDR encoder → rewriter splices
// tonemap into PMS's filter chain.
func TestRewriter_HWDecode_HDR_SDR_InjectsTonemap(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi",
		"-hwaccel_device:0", "vaapi",
		"-codec:1", "truehd_eae",
		"-eae_prefix:1", "tok_",
		"-i", "/media/Movies/HDRMovie.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1];[1]hwupload[2]",
		"-map", "[2]",
		"-codec:0", "h264_vaapi",
		"-qp:0", "22",
		"-map", "0:1",
		"-codec:1", "aac",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		ProbeVideoColor: func(string) (string, string, string) {
			return "smpte2084", "bt2020", "bt2020nc" // HDR PQ
		},
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:hw-passthrough-tonemap-injected") {
		t.Fatalf("expected tonemap-injected change tag: %v", out.Changes)
	}
	// Find the rewritten filter; verify p010 + tonemap_vaapi present.
	var rewritten string
	for i, a := range out.Args {
		if a == "-filter_complex" && i+1 < len(out.Args) {
			rewritten = out.Args[i+1]
		}
	}
	if !strings.Contains(rewritten, "format=p010") {
		t.Errorf("scale_vaapi must output p010 before tonemap: %s", rewritten)
	}
	if !strings.Contains(rewritten, "tonemap_vaapi=transfer=bt709:format=nv12") {
		t.Errorf("tonemap_vaapi must be in the chain: %s", rewritten)
	}
}

// Same shape but SDR source — no tonemap needed; chain passes
// through unchanged.
func TestRewriter_HWDecode_SDR_NoTonemapInjected(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi",
		"-hwaccel_device:0", "vaapi",
		"-i", "/media/Movies/SDRMovie.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1];[1]hwupload[2]",
		"-map", "[2]",
		"-codec:0", "h264_vaapi",
		"-qp:0", "22",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		ProbeVideoColor: func(string) (string, string, string) {
			return "bt709", "bt709", "bt709" // SDR
		},
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Changes, "filter:hw-passthrough-tonemap-injected") {
		t.Fatalf("SDR source must not trigger tonemap: %v", out.Changes)
	}
}
