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
	t.Setenv("SCALEPLEX_TONEMAP", "vaapi") // assert the fixed-curve fallback shape
	in := "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1];[1]hwupload[2]"
	out, ok := injectHWPassthroughTonemap(in, resolveTonemapConfig(nil))
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
	_, ok := injectHWPassthroughTonemap(in, resolveTonemapConfig(nil))
	if ok {
		t.Fatalf("p010 chain must not match SDR pattern")
	}
}

// Sub-burn / overlay / multi-step chains have their own dedicated
// rewrite paths — the simple pattern shouldn't snag them.
func TestInjectHWPassthroughTonemap_LeavesComplexChainUntouched(t *testing.T) {
	in := "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1];[1][2:s:0]overlay_vaapi[3]"
	_, ok := injectHWPassthroughTonemap(in, resolveTonemapConfig(nil))
	if ok {
		t.Fatalf("multi-step chain must not match the simple pattern")
	}
}

// End-to-end: HW-decode + HDR source + SDR encoder → rewriter splices
// tonemap into PMS's filter chain.
func TestRewriter_HWDecode_HDR_SDR_InjectsTonemap(t *testing.T) {
	t.Setenv("SCALEPLEX_TONEMAP", "vaapi") // assert the fixed-curve fallback shape
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

// hwDecodeHDRArgs is the PMS HW-decode HDR→SDR argv shape (naive
// format=nv12 chain, no tonemap) used by the OpenCL-tonemap tests.
func hwDecodeHDRArgs() []string {
	return []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi",
		"-hwaccel_device:0", "vaapi",
		"-i", "/media/Movies/HDRMovie.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1];[1]hwupload[2]",
		"-map", "[2]",
		"-codec:0", "h264_vaapi",
		"-qp:0", "22",
	}
}

func filterComplexOf(args []string) string {
	for i, a := range args {
		if a == "-filter_complex" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// Default (no SCALEPLEX_TONEMAP set) → the injected tonemap stage runs
// on tonemap_opencl with the hable algorithm, self-deriving the OpenCL
// device from the VAAPI frame context.
func TestTonemap_OpenCL_DefaultInjectsHableChain(t *testing.T) {
	out := Rewrite(hwDecodeHDRArgs(), nil, &RewriteOpts{
		ProbeVideoColor: func(string) (string, string, string) {
			return "smpte2084", "bt2020", "bt2020nc"
		},
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	f := filterComplexOf(out.Args)
	for _, want := range []string{
		"scale_vaapi=w=1280:h=720:format=p010",
		"hwmap=derive_device=opencl",
		"tonemap_opencl=tonemap=hable",
		"hwmap=derive_device=vaapi:reverse=1",
	} {
		if !strings.Contains(f, want) {
			t.Errorf("filter missing %q:\n%s", want, f)
		}
	}
	if strings.Contains(f, "tonemap_vaapi") {
		t.Errorf("default mode must not emit tonemap_vaapi:\n%s", f)
	}
}

// SCALEPLEX_TONEMAP_ALGO overrides the injected algorithm.
func TestTonemap_OpenCL_AlgoEnvOverride(t *testing.T) {
	t.Setenv("SCALEPLEX_TONEMAP_ALGO", "mobius")
	out := Rewrite(hwDecodeHDRArgs(), nil, &RewriteOpts{
		ProbeVideoColor: func(string) (string, string, string) {
			return "smpte2084", "bt2020", "bt2020nc"
		},
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if f := filterComplexOf(out.Args); !strings.Contains(f, "tonemap_opencl=tonemap=mobius") {
		t.Errorf("expected tonemap=mobius:\n%s", f)
	}
}

// When PMS itself sends an OpenCL tonemap chain, the algorithm it chose
// (TranscoderTonemapAlgorithm) is preserved, not discarded.
func TestSubstituteOpenCLTonemap_PreservesPlexAlgorithm(t *testing.T) {
	args := []string{
		"-filter_complex",
		"[0]hwmap=derive_device=opencl[1];" +
			"[1]tonemap_opencl=tonemap=reinhard:format=nv12:m=bt709:p=bt709:r=tv[2];" +
			"[2]hwmap=derive_device=vaapi:reverse=1[3]",
	}
	out, did := substituteOpenCLTonemap(args, resolveTonemapConfig(nil))
	if !did {
		t.Fatal("expected substitution")
	}
	f := filterComplexOf(out)
	if !strings.Contains(f, "tonemap_opencl=tonemap=reinhard") {
		t.Errorf("Plex's algorithm (reinhard) must be preserved:\n%s", f)
	}
	if !strings.Contains(f, "hwmap=derive_device=opencl,tonemap_opencl") {
		t.Errorf("expected canonical comma-form chain:\n%s", f)
	}
	if !strings.Contains(f, "hwmap=derive_device=vaapi:reverse=1[3]") {
		t.Errorf("end label must be preserved:\n%s", f)
	}
}

// SCALEPLEX_TONEMAP=vaapi collapses Plex's OpenCL chain to the fixed-
// curve tonemap_vaapi filter (algorithm discarded).
func TestSubstituteOpenCLTonemap_VAAPIModeCollapses(t *testing.T) {
	t.Setenv("SCALEPLEX_TONEMAP", "vaapi")
	args := []string{
		"-filter_complex",
		"[0]hwmap=derive_device=opencl[1];" +
			"[1]tonemap_opencl=tonemap=reinhard:format=nv12:m=bt709:p=bt709:r=tv[2];" +
			"[2]hwmap=derive_device=vaapi:reverse=1[3]",
	}
	out, did := substituteOpenCLTonemap(args, resolveTonemapConfig(nil))
	if !did {
		t.Fatal("expected substitution")
	}
	f := filterComplexOf(out)
	if !strings.Contains(f, "tonemap_vaapi=transfer=bt709:matrix=bt709:primaries=bt709:format=nv12") {
		t.Errorf("vaapi mode must collapse to tonemap_vaapi:\n%s", f)
	}
	if strings.Contains(f, "tonemap_opencl") {
		t.Errorf("vaapi mode must not keep tonemap_opencl:\n%s", f)
	}
}

// resolveTonemapConfig: defaults, the Plex on/off pref, and algorithm
// precedence (Plex pref > operator env > built-in hable).
func TestResolveTonemapConfig(t *testing.T) {
	if c := resolveTonemapConfig(nil); !c.enabled || !c.useOpenCL || c.algo != "hable" {
		t.Errorf("default: want enabled+opencl+hable, got %+v", c)
	}
	if c := resolveTonemapConfig(map[string]string{"SCALEPLEX_PLEX_TONEMAP": "0"}); c.enabled {
		t.Errorf("SCALEPLEX_PLEX_TONEMAP=0 must disable, got %+v", c)
	}
	if c := resolveTonemapConfig(map[string]string{"SCALEPLEX_PLEX_TONEMAP_ALGO": "reinhard"}); c.algo != "reinhard" {
		t.Errorf("algo from Plex pref: want reinhard, got %q", c.algo)
	}
	// Plex's pref outranks the operator's SCALEPLEX_TONEMAP_ALGO.
	t.Setenv("SCALEPLEX_TONEMAP_ALGO", "clip")
	if c := resolveTonemapConfig(map[string]string{"SCALEPLEX_PLEX_TONEMAP_ALGO": "mobius"}); c.algo != "mobius" {
		t.Errorf("Plex pref must outrank operator env: want mobius, got %q", c.algo)
	}
	// Operator env is the fallback when Plex sent nothing.
	if c := resolveTonemapConfig(nil); c.algo != "clip" {
		t.Errorf("operator-env fallback: want clip, got %q", c.algo)
	}
}

// Plex's tone-mapping pref off → scaleplex honors it: no tonemap stage
// injected on an HDR source, even though it would otherwise wash out.
func TestRewriter_PlexTonemapDisabled_NoImplicitInjection(t *testing.T) {
	out := Rewrite(hwDecodeHDRArgs(),
		map[string]string{"SCALEPLEX_PLEX_TONEMAP": "0"},
		&RewriteOpts{ProbeVideoColor: func(string) (string, string, string) {
			return "smpte2084", "bt2020", "bt2020nc"
		}})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	f := filterComplexOf(out.Args)
	if strings.Contains(f, "tonemap_opencl") || strings.Contains(f, "tonemap_vaapi") {
		t.Errorf("Plex tonemap pref off — no tonemap stage must be injected:\n%s", f)
	}
	if !containsString(out.Changes, "tonemap:skipped(plex-pref-off)") {
		t.Errorf("expected tonemap:skipped(plex-pref-off) change tag: %v", out.Changes)
	}
}

// Plex's tone-mapping pref on (default) → the injected stage uses the
// algorithm from Plex's TranscoderToneMappingAgorithm pref.
func TestRewriter_PlexTonemapAlgoFromPref(t *testing.T) {
	out := Rewrite(hwDecodeHDRArgs(),
		map[string]string{"SCALEPLEX_PLEX_TONEMAP_ALGO": "reinhard"},
		&RewriteOpts{ProbeVideoColor: func(string) (string, string, string) {
			return "smpte2084", "bt2020", "bt2020nc"
		}})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if f := filterComplexOf(out.Args); !strings.Contains(f, "tonemap_opencl=tonemap=reinhard") {
		t.Errorf("expected Plex's pref algorithm (reinhard):\n%s", f)
	}
}
