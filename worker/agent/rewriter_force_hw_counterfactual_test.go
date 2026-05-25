package main

import (
	"strings"
	"testing"
)

// hasSkip reports whether any change tag is a bail marker ("skip:...").
func hasSkip(changes []string) bool {
	for _, c := range changes {
		if len(c) >= 5 && c[:5] == "skip:" {
			return true
		}
	}
	return false
}

// Counterfactual logging (docs/HW_PROFILE.md, FORCE_HW=0 readiness): when
// FORCE_HW=1 masks a session we WOULD have honored as full-SW, emit
// `force-hw:would-honor-sw` so prod logs quantify real SW exposure before
// flipping FORCE_HW off. The session still re-accelerates to HW.
func TestRewriter_Counterfactual_WouldHonorSW(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "1")
	out := Rewrite(swArgsAV1H264, nil, nil)
	if !containsString(out.Changes, "force-hw:would-honor-sw") {
		t.Fatalf("expected force-hw:would-honor-sw under FORCE_HW=1 on SW argv: %v", out.Changes)
	}
	// Still re-accelerated (not honored).
	if containsString(out.Changes, "honor:plex-sw") {
		t.Fatalf("FORCE_HW=1 must re-accelerate, not honor: %v", out.Changes)
	}
	if !containsString(out.Changes, "encode:libx264->h264_vaapi") {
		t.Errorf("SW argv should reshape to HW under FORCE_HW: %v", out.Changes)
	}
}

// Honoring (FORCE_HW=0) is the real thing, not a counterfactual — no
// would-honor tag should appear.
func TestRewriter_Counterfactual_NotEmittedWhenHonoring(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "0")
	out := Rewrite(swArgsAV1H264, nil, nil)
	if containsString(out.Changes, "force-hw:would-honor-sw") {
		t.Fatalf("FORCE_HW=0 honors for real, must not emit counterfactual: %v", out.Changes)
	}
	if !containsString(out.Changes, "honor:plex-sw") {
		t.Fatalf("expected honor:plex-sw under FORCE_HW=0: %v", out.Changes)
	}
}

// A genuinely-HW argv (PMS chose HW encode) is never a would-honor — Plex
// staged no SW encoder.
func TestRewriter_Counterfactual_NotEmittedForHWArgv(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "1")
	args := []string{
		"-codec:0", "hevc", "-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/Movies/HEVCSource.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1];[1]hwupload[2]",
		"-map", "[2]", "-codec:0", "h264_vaapi", "-qp:0", "22",
	}
	out := Rewrite(args, nil, nil)
	if containsString(out.Changes, "force-hw:would-honor-sw") ||
		containsString(out.Changes, "force-hw:would-honor-hwdec-swenc") {
		t.Fatalf("HW argv must not emit a counterfactual would-honor tag: %v", out.Changes)
	}
}

// EAE safety net on EVERY bail + the hybrid counterfactual, in one scenario.
// Under FORCE_HW=1 a HW-decode + SW-encode (libx264) hybrid bails
// `hw-decode:unexpected-encoder:libx264`. bail() returns before the main
// path's step-9 swapEAEAudioDecoders, so without the safety net the
// `-codec:1 eac3_eae` input hint survives → ffmpeg exits 8 "Unknown decoder
// 'eac3_eae'" → client transcoder error on ~all EAC3/TrueHD/Atmos remuxes.
// The bail must swap it to stock + drop the orphaned -eae_prefix. The same
// session also carries the would-honor-hwdec-swenc counterfactual.
func TestRewriter_BailEAESafetyNet_HybridForceHW(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "1")
	args := []string{
		"-codec:0", "av1", "-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-codec:1", "eac3_eae", "-eae_prefix:1", "tok_",
		"-i", "/media/m.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-map_inlineass", "0:3",
		"-filter_complex", "[0:0]scale_vaapi=w=1920:h=1080:format=p010[1];[1]hwdownload,format=p010[2];" +
			"[2]inlineass=font_scale=1.0:language=en:overrides=foo:outline=2.6:shadow=1.7:font_size=54[3]",
		"-map", "[3]", "-codec:0", "libx264", "-crf:0", "16", "-preset:0", "veryfast",
		"-map", "0:3", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, nil)
	if !hasSkip(out.Changes) {
		t.Fatalf("hybrid under FORCE_HW=1 should bail: %v", out.Changes)
	}
	if !out.Applied {
		t.Fatalf("a bail that swaps EAE must set Applied=true: %v", out.Changes)
	}
	if !containsString(out.Changes, "force-hw:would-honor-hwdec-swenc") {
		t.Errorf("expected force-hw:would-honor-hwdec-swenc counterfactual: %v", out.Changes)
	}
	if !containsString(out.Changes, "audio:eac3_eae->eac3(bail)") {
		t.Errorf("expected EAE swap on bail: %v", out.Changes)
	}
	if !containsString(out.Changes, "drop:-eae_prefix:1(bail)") {
		t.Errorf("expected orphaned -eae_prefix drop on bail: %v", out.Changes)
	}
	if containsString(out.Args, "eac3_eae") {
		t.Errorf("eac3_eae must not survive a bail: %v", out.Args)
	}
	if containsString(out.Args, "-eae_prefix:1") {
		t.Errorf("-eae_prefix:1 must not survive a bail: %v", out.Args)
	}
}

// Item 3: FORCE_HW=1 reshapes a Plex hybrid (HW decode + SW filter+encode)
// to full HW instead of bailing. Plex's real hybrid argv HW-decodes
// (`-codec:0 av1 -hwaccel:0 vaapi`) but runs the filter chain + encoder in
// software (`[0:0]scale=...`, `inlineass=...`, libx264). The SW tail is
// shape-identical to a SW-decode session, so it reshapes through
// rewriteVideoFilter + encoder swap, keeping the HW decode.

// SDR sub-burn hybrid → scale_vaapi + h264_vaapi, HW decode kept.
func TestRewriter_HybridForceHW_SDRSubReshape(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "1")
	args := []string{
		"-codec:0", "av1", "-hwaccel:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-codec:1", "eac3_eae", "-eae_prefix:1", "tok_",
		"-i", "/media/m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-map_inlineass", "0:3",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=yuv420p|nv12[1];" +
			"[1]inlineass=font_scale=1.0:language=en:overrides=foo:outline=2.6:shadow=1.7:font_size=54[2]",
		"-map", "[2]",
		"-codec:0", "libx264", "-crf:0", "16", "-preset:0", "veryfast",
		"-map", "0:3", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, nil)
	if hasSkip(out.Changes) {
		t.Fatalf("hybrid SW-graph under FORCE_HW=1 must reshape, not bail: %v", out.Changes)
	}
	if !containsString(out.Changes, "force-hw:reshape-hybrid:av1") {
		t.Errorf("expected force-hw:reshape-hybrid tag: %v", out.Changes)
	}
	if !containsString(out.Changes, "encode:libx264->h264_vaapi") {
		t.Errorf("encoder must reshape to h264_vaapi: %v", out.Changes)
	}
	// HW decode kept (passthrough, not stripped).
	if !containsString(out.Args, "-hwaccel:0") {
		t.Error("HW decode (-hwaccel:0) must be kept under hybrid force-HW")
	}
	dIdx := indexOfArg(out.Args, "-codec:0", 0)
	if out.Args[dIdx+1] != "av1" {
		t.Errorf("decoder=%q want av1 (HW passthrough kept)", out.Args[dIdx+1])
	}
	// Filter chain reshaped to VAAPI.
	if !containsString(out.Args, "-filter_complex") || !anyContains(out.Args, "scale_vaapi") {
		t.Errorf("filter chain must reshape to scale_vaapi: %v", out.Args)
	}
	// EAE input hint still swapped via the common tail (not a bail).
	if containsString(out.Args, "eac3_eae") {
		t.Errorf("eac3_eae must not survive: %v", out.Args)
	}
}

// HDR sub-burn hybrid (the real captured Avatar 4K shape: scale+tonemap+
// inlineass, libx264) → scale_vaapi + tonemap_vaapi + h264_vaapi.
func TestRewriter_HybridForceHW_HDRSubReshape(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "1")
	t.Setenv("SCALEPLEX_TONEMAP", "vaapi") // deterministic fixed-curve VAAPI tonemap
	args := []string{
		"-codec:0", "av1", "-hwaccel:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-map_inlineass", "0:3",
		"-filter_complex", "[0:0]scale=w=3840:h=2160:force_divisible_by=4[0];" +
			"[0]format=p010,tonemap=mobius[1];[1]format=pix_fmts=yuv420p|nv12[2];" +
			"[2]inlineass=font_scale=1.0:language=en:overrides=foo:outline=2.6:shadow=1.7:font_size=54[3]",
		"-map", "[3]",
		"-codec:0", "libx264", "-crf:0", "16", "-preset:0", "veryfast",
		"-map", "0:3", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		ProbeVideoColor: func(string) (string, string, string) { return "smpte2084", "", "" },
	})
	if hasSkip(out.Changes) {
		t.Fatalf("HDR hybrid must reshape, not bail: %v", out.Changes)
	}
	if !containsString(out.Changes, "force-hw:reshape-hybrid:av1") {
		t.Errorf("expected force-hw:reshape-hybrid tag: %v", out.Changes)
	}
	if !containsString(out.Changes, "encode:libx264->h264_vaapi") {
		t.Errorf("encoder must reshape to h264_vaapi: %v", out.Changes)
	}
	if !anyContains(out.Args, "scale_vaapi") || !anyContains(out.Args, "tonemap_vaapi") {
		t.Errorf("HDR hybrid must reshape to scale_vaapi + tonemap_vaapi: %v", out.Args)
	}
	if !anyContains(out.Args, "inlineass=") {
		t.Errorf("inlineass burn must survive the reshape: %v", out.Args)
	}
}

// No-sub hybrid → drop to plain VAAPI scale + h264_vaapi.
func TestRewriter_HybridForceHW_NoSubReshape(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "1")
	args := []string{
		"-codec:0", "av1", "-hwaccel:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1280:h=720[0];[0]format=pix_fmts=yuv420p|nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264", "-crf:0", "16", "-preset:0", "veryfast",
	}
	out := Rewrite(args, nil, nil)
	if hasSkip(out.Changes) {
		t.Fatalf("no-sub hybrid must reshape, not bail: %v", out.Changes)
	}
	if !containsString(out.Changes, "force-hw:reshape-hybrid:av1") {
		t.Errorf("expected force-hw:reshape-hybrid tag: %v", out.Changes)
	}
	if !containsString(out.Changes, "encode:libx264->h264_vaapi") {
		t.Errorf("encoder must reshape to h264_vaapi: %v", out.Changes)
	}
	if !anyContains(out.Args, "scale_vaapi") {
		t.Errorf("no-sub hybrid must reshape to scale_vaapi: %v", out.Args)
	}
}

// Under FORCE_HW=0 the same hybrid is HONORED (honorHybrid), not reshaped —
// the force-HW reshape must not fire.
func TestRewriter_HybridForceHW_NotWhenHonoring(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "0")
	args := []string{
		"-codec:0", "av1", "-hwaccel:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1280:h=720[0];[0]format=pix_fmts=yuv420p|nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264", "-crf:0", "16", "-preset:0", "veryfast",
	}
	out := Rewrite(args, nil, nil)
	if containsString(out.Changes, "force-hw:reshape-hybrid:av1") {
		t.Fatalf("FORCE_HW=0 must honor, not reshape: %v", out.Changes)
	}
	if !containsString(out.Changes, "honor:plex-hwdec-swenc") {
		t.Fatalf("expected honor:plex-hwdec-swenc under FORCE_HW=0: %v", out.Changes)
	}
}

// anyContains reports whether any arg contains substr (filtergraph values
// are single args, so containsString's exact match won't find scale_vaapi).
func anyContains(args []string, substr string) bool {
	for _, a := range args {
		if strings.Contains(a, substr) {
			return true
		}
	}
	return false
}
