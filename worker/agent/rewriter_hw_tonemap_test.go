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

// AMD radeonsi has no tonemap_vaapi — resolveTonemapConfig must force
// useOpenCL=true on the AMD vendor branch even when the operator pinned
// SCALEPLEX_TONEMAP=vaapi (otherwise tm.stage would emit a tonemap_vaapi
// filter that fails to init on radeonsi). #123.
func TestResolveTonemapConfig_AMDForcesOpenCL(t *testing.T) {
	withDialect(t, vaapiDialect{vendor: "amd"})
	// Default (no env set) — opencl by default everywhere; just sanity.
	if c := resolveTonemapConfig(); !c.useOpenCL {
		t.Errorf("AMD default: want opencl, got %+v", c)
	}
	// Operator pin to vaapi must be ignored on AMD.
	t.Setenv("SCALEPLEX_TONEMAP", "vaapi")
	if c := resolveTonemapConfig(); !c.useOpenCL {
		t.Errorf("AMD + SCALEPLEX_TONEMAP=vaapi: must still force opencl (tonemap_vaapi absent on radeonsi), got %+v", c)
	}
	// Intel must still honor the env pin (regression check on the AMD branch).
	withDialect(t, vaapiDialect{vendor: "intel"})
	if c := resolveTonemapConfig(); c.useOpenCL {
		t.Errorf("Intel + SCALEPLEX_TONEMAP=vaapi: want fixed-curve (useOpenCL=false), got %+v", c)
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
