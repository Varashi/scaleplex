package main

import (
	"strings"
	"testing"
)

func filterComplexOf(args []string) string {
	for i, a := range args {
		if a == "-filter_complex" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// resolveTonemapConfig: backend choice + fallback algorithm, both from
// the worker-pod env. scaleplex never decides whether to tonemap.
func TestResolveTonemapConfig(t *testing.T) {
	if c := resolveTonemapConfig(); !c.useOpenCL || c.algo != "hable" {
		t.Errorf("default: want opencl+hable, got %+v", c)
	}
	t.Setenv("SCALEPLEX_TONEMAP", "vaapi")
	if c := resolveTonemapConfig(); c.useOpenCL {
		t.Errorf("SCALEPLEX_TONEMAP=vaapi must select the fixed curve, got %+v", c)
	}
	t.Setenv("SCALEPLEX_TONEMAP_ALGO", "mobius")
	if c := resolveTonemapConfig(); c.algo != "mobius" {
		t.Errorf("SCALEPLEX_TONEMAP_ALGO override: want mobius, got %q", c.algo)
	}
}

// When PMS sends its own OpenCL tonemap chain, scaleplex re-emits it in
// canonical comma form, preserving Plex's chosen algorithm — it does not
// discard or override it.
func TestSubstituteOpenCLTonemap_PreservesPlexAlgorithm(t *testing.T) {
	args := []string{
		"-filter_complex",
		"[0]hwmap=derive_device=opencl[1];" +
			"[1]tonemap_opencl=tonemap=reinhard:format=nv12:m=bt709:p=bt709:r=tv[2];" +
			"[2]hwmap=derive_device=vaapi:reverse=1[3]",
	}
	out, did := substituteOpenCLTonemap(args, resolveTonemapConfig())
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

// AMD radeonsi (vendor=amd) has neither tonemap_vaapi nor a working
// tonemap_opencl chain. composeBurn must route HDR sessions to the
// vf_inlineass AMD-Vulkan branch:
//
//   - HDR + no subs → inlineass=tonemap_only=1:hdr_to_sdr=1 (no separate
//     tonemap filter, no -map_inlineass binding expected). #123 v6 +
//     #137 v7 explicit HDR→SDR intent.
//   - HDR + sub-burn → scale_vaapi(...:format=p010) → inlineass=...:hdr_to_sdr=1
//     with no intermediate tonemap stage. The filter absorbs HDR→SDR
//     internally via libplacebo pl_render_image, gated on hdr_to_sdr=1.
//   - SDR → identical to the Intel iHD shape (regression sentinel).
func TestComposeBurn_AMDHDR(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "1080")
	tm := tonemapConfig{d: vaapiDialect{vendor: "amd"}, algo: "hable"}

	t.Run("hdr/no-sub/emits-tonemap_only", func(t *testing.T) {
		got, label := tm.composeBurn(burnSpec{
			vaResident: true, w: "3840", h: "2160", hdr: true, algo: "mobius",
		})
		want := "[0:0]scale_vaapi=w=3840:h=2160:format=p010[0];[0]inlineass=tonemap_only=1:hdr_to_sdr=1[1]"
		if got != want {
			t.Errorf("filter:\n got  %s\n want %s", got, want)
		}
		if label != "[1]" {
			t.Errorf("label: got %s want [1]", label)
		}
		if strings.Contains(got, "tonemap_vaapi") || strings.Contains(got, "tonemap_opencl") {
			t.Errorf("AMD HDR must NOT emit any tonemap_* stage: %s", got)
		}
	})

	t.Run("hdr/sub-burn/absorbs-tonemap", func(t *testing.T) {
		got, label := tm.composeBurn(burnSpec{
			vaResident: true, w: "3840", h: "2160", hdr: true, algo: "mobius",
			burnSub: true,
		})
		want := "[0:0]scale_vaapi=w=3840:h=2160:format=p010[0];[0]inlineass=render_height=1080:hdr_to_sdr=1[1]"
		if got != want {
			t.Errorf("filter:\n got  %s\n want %s", got, want)
		}
		if label != "[1]" {
			t.Errorf("label: got %s want [1]", label)
		}
		if strings.Contains(got, "tonemap_vaapi") || strings.Contains(got, "tonemap_opencl") {
			t.Errorf("AMD HDR+sub must NOT emit any tonemap_* stage: %s", got)
		}
		if strings.Contains(got, "tonemap_only") {
			t.Errorf("AMD HDR+sub must NOT set tonemap_only (only hdr_to_sdr=1): %s", got)
		}
	})

	t.Run("sdr/no-sub/unchanged", func(t *testing.T) {
		got, label := tm.composeBurn(burnSpec{
			vaResident: true, w: "1920", h: "1080",
		})
		want := "[0:0]scale_vaapi=w=1920:h=1080:format=nv12[0]"
		if got != want {
			t.Errorf("AMD SDR must match Intel SDR shape:\n got  %s\n want %s", got, want)
		}
		if label != "[0]" {
			t.Errorf("label: got %s want [0]", label)
		}
		if strings.Contains(got, "hdr_to_sdr") {
			t.Errorf("AMD SDR must NOT carry hdr_to_sdr flag: %s", got)
		}
	})

	t.Run("sdr/sub-burn/unchanged", func(t *testing.T) {
		got, _ := tm.composeBurn(burnSpec{
			vaResident: true, w: "1920", h: "1080", burnSub: true,
		})
		want := "[0:0]scale_vaapi=w=1920:h=1080:format=nv12[0];[0]inlineass=render_height=1080[1]"
		if got != want {
			t.Errorf("AMD SDR+sub must match Intel SDR+sub shape:\n got  %s\n want %s", got, want)
		}
		if strings.Contains(got, "hdr_to_sdr") {
			t.Errorf("AMD SDR+sub must NOT carry hdr_to_sdr flag: %s", got)
		}
	})
}

// HDR-passthrough on AMD: Plex's argv carried NO tonemap stage (HDR client,
// HDR output) so facts.hdr is false → composeBurn must take the Intel-style
// chain, NOT composeBurnAMDHDR, and the inlineass node must NOT carry
// hdr_to_sdr=1. Honors "scaleplex never injects tonemap; Plex's argv decides"
// (project_scaleplex_tonemap_policy) + avoids the libplacebo tonemap dispatch
// cost on passthrough (#137 v6 perf regression root cause).
func TestComposeBurn_AMDHDRPassthrough_NoTonemap(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "1080")
	tm := tonemapConfig{d: vaapiDialect{vendor: "amd"}, algo: "hable"}

	t.Run("passthrough/no-sub", func(t *testing.T) {
		got, _ := tm.composeBurn(burnSpec{
			vaResident: true, w: "3840", h: "2160", // hdr: false (passthrough)
		})
		want := "[0:0]scale_vaapi=w=3840:h=2160:format=nv12[0]"
		if got != want {
			t.Errorf("AMD HDR-passthrough must take Intel-shape chain:\n got  %s\n want %s", got, want)
		}
		if strings.Contains(got, "hdr_to_sdr") {
			t.Errorf("AMD HDR-passthrough must NOT carry hdr_to_sdr (no tonemap intent): %s", got)
		}
		if strings.Contains(got, "tonemap_only") {
			t.Errorf("AMD HDR-passthrough must NOT carry tonemap_only: %s", got)
		}
	})

	t.Run("passthrough/sub-burn", func(t *testing.T) {
		got, _ := tm.composeBurn(burnSpec{
			vaResident: true, w: "3840", h: "2160", burnSub: true, // hdr: false
		})
		want := "[0:0]scale_vaapi=w=3840:h=2160:format=nv12[0];[0]inlineass=render_height=1080[1]"
		if got != want {
			t.Errorf("AMD HDR-passthrough+sub must take Intel-shape chain:\n got  %s\n want %s", got, want)
		}
		if strings.Contains(got, "hdr_to_sdr") {
			t.Errorf("AMD HDR-passthrough+sub must NOT carry hdr_to_sdr: %s", got)
		}
	})
}

// TestComposeBurn_AMDHDRtoSDR_TonemapSet: rewriter intent = HDR→SDR (s.hdr=true)
// must yield the AMD-Vulkan branch with the explicit hdr_to_sdr=1 flag on the
// inlineass node, on both the no-sub and sub-burn shapes. The fork patch 0127
// v7 gates the pl_tgt.color override + intermediate Vulkan pool nv12 swap on
// this flag.
func TestComposeBurn_AMDHDRtoSDR_TonemapSet(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "1080")
	tm := tonemapConfig{d: vaapiDialect{vendor: "amd"}, algo: "hable"}

	t.Run("sub-burn", func(t *testing.T) {
		got, _ := tm.composeBurn(burnSpec{
			vaResident: true, w: "3840", h: "2160", hdr: true, burnSub: true,
		})
		if !strings.Contains(got, "hdr_to_sdr=1") {
			t.Errorf("AMD HDR→SDR+sub must carry hdr_to_sdr=1: %s", got)
		}
	})

	t.Run("no-sub-tonemap-only", func(t *testing.T) {
		got, _ := tm.composeBurn(burnSpec{
			vaResident: true, w: "3840", h: "2160", hdr: true,
		})
		// Both flags expected on the no-sub HDR→SDR shape: tonemap_only=1
		// (libass bypass) + hdr_to_sdr=1 (HDR→SDR pl_tgt override + pool swap).
		if !strings.Contains(got, "tonemap_only=1") {
			t.Errorf("AMD HDR→SDR no-sub must carry tonemap_only=1: %s", got)
		}
		if !strings.Contains(got, "hdr_to_sdr=1") {
			t.Errorf("AMD HDR→SDR no-sub must carry hdr_to_sdr=1: %s", got)
		}
	})
}

// resolveTonemapConfig on vendor=amd: useOpenCL still computed (env path),
// but composeBurn routes around tm.stage so the value doesn't reach the
// argv. The WARN-on-SCALEPLEX_TONEMAP=vaapi log is operator-facing only.
func TestResolveTonemapConfig_AMDLeavesEnvAlone(t *testing.T) {
	prev := activeDialect
	defer func() { activeDialect = prev }()
	activeDialect = vaapiDialect{vendor: "amd"}

	c := resolveTonemapConfig()
	if c.d.backendName() != "vaapi" {
		t.Errorf("backend: got %q want vaapi", c.d.backendName())
	}
	// Default (SCALEPLEX_TONEMAP unset): useOpenCL=true. AMD WARNs only on
	// explicit ...=vaapi, not the default; spot-check the WARN guard
	// doesn't crash here.
	if !c.useOpenCL {
		t.Errorf("default useOpenCL must remain true on AMD (env unset): %+v", c)
	}
}

// tonemapFilter on vaapiDialect{vendor:"amd"} returns empty — no
// tonemap_vaapi exists on radeonsi. Anything that bypasses composeBurn and
// asks for an AMD HDR stage gets a visible (loud) hole rather than a
// silent Intel-shaped fragment.
func TestVAAPIDialect_AMDTonemapFilterEmpty(t *testing.T) {
	if got := (vaapiDialect{vendor: "amd"}).tonemapFilter("hable", "nv12"); got != "" {
		t.Errorf("AMD tonemapFilter must be empty (tone-map absorbed into vf_inlineass), got %q", got)
	}
	if got := (vaapiDialect{vendor: "intel"}).tonemapFilter("hable", "nv12"); got == "" {
		t.Errorf("Intel tonemapFilter must still emit tonemap_vaapi")
	}
}

// SCALEPLEX_TONEMAP=vaapi collapses Plex's OpenCL tonemap chain to the
// fixed-curve tonemap_vaapi filter.
func TestSubstituteOpenCLTonemap_VAAPIModeCollapses(t *testing.T) {
	t.Setenv("SCALEPLEX_TONEMAP", "vaapi")
	args := []string{
		"-filter_complex",
		"[0]hwmap=derive_device=opencl[1];" +
			"[1]tonemap_opencl=tonemap=reinhard:format=nv12:m=bt709:p=bt709:r=tv[2];" +
			"[2]hwmap=derive_device=vaapi:reverse=1[3]",
	}
	out, did := substituteOpenCLTonemap(args, resolveTonemapConfig())
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
