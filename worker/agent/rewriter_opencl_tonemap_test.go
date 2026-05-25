package main

import (
	"strings"
	"testing"
)

// GPU-resident OpenCL tonemap fix-up for jellyfin-ffmpeg 7.x. The va→opencl
// hwmap derive fails ENOSYS on 7.x unless: an OpenCL device is created, the
// input is a VA surface (no leading hwupload), and there's no
// reverse-map→hwdownload round-trip. gpuResidentOpenCLTonemap rewrites an
// emitted tonemap_opencl graph to satisfy all three. See
// reference_scaleplex_tonemap_regression_test.

func ocl_filterValue(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-filter_complex" {
			return args[i+1]
		}
	}
	return ""
}

// Full-HW HDR opencl-tonemap + SW-libass tail (the reFilterHDRAss shape):
// leading [0:0]hwupload + reverse-map→hwdownload round-trip. After the pass:
// opencl device injected, output_format forced, no leading hwupload, the
// round-trip collapsed to a direct opencl→sysmem hwdownload.
func TestGPUResidentOpenCLTonemap_HDRAssShape(t *testing.T) {
	args := []string{
		"-codec:0", "av1", "-hwaccel:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/m.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]hwupload[10];[10]scale_vaapi=w=3840:h=2160:format=p010," +
			"hwmap=derive_device=opencl,tonemap_opencl=tonemap=mobius:transfer=bt709:matrix=bt709:primaries=bt709:format=nv12," +
			"hwmap=derive_device=vaapi:reverse=1[11];[11]hwdownload[12];[12]format=pix_fmts=nv12[13];[13]inlineass=x[14];[14]hwupload[15]",
		"-map", "[15]", "-codec:0", "h264_vaapi",
	}
	out, changes := gpuResidentOpenCLTonemap(args)
	g := ocl_filterValue(out)

	if strings.HasPrefix(g, "[0:0]hwupload") {
		t.Errorf("leading hwupload must be dropped: %q", g)
	}
	if !strings.HasPrefix(g, "[0:0]scale_vaapi") {
		t.Errorf("graph must feed scale_vaapi from [0:0] directly: %q", g)
	}
	if strings.Contains(g, "hwmap=derive_device=vaapi:reverse=1") {
		t.Errorf("reverse-map round-trip must be collapsed: %q", g)
	}
	// tonemap_opencl with the algo preserved.
	if !strings.Contains(g, "tonemap_opencl=tonemap=mobius") {
		t.Errorf("opencl tonemap + algo must be kept: %q", g)
	}
	// direct opencl->sysmem hwdownload remains for the SW libass step.
	if !strings.Contains(g, "tonemap_opencl=") || !strings.Contains(g, "hwdownload") {
		t.Errorf("expected opencl tonemap then direct hwdownload: %q", g)
	}
	// OpenCL device injected, derived from the vaapi device, before -i.
	if !hasOpenCLInitDevice(out) {
		t.Errorf("opencl device must be injected: %v", out)
	}
	oclIdx, iIdx := -1, indexOfArg(out, "-i", 0)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-init_hw_device" && strings.HasPrefix(out[i+1], "opencl=") {
			oclIdx = i
			if out[i+1] != "opencl=ocl@vaapi" {
				t.Errorf("opencl device should derive from vaapi: %q", out[i+1])
			}
		}
	}
	if oclIdx < 0 || oclIdx > iIdx {
		t.Errorf("opencl device must be injected before -i (oclIdx=%d iIdx=%d)", oclIdx, iIdx)
	}
	// VA-resident decode forced.
	if indexOfArg(out, "-hwaccel_output_format:0", 0) < 0 {
		t.Errorf("must force -hwaccel_output_format:0 vaapi: %v", out)
	}
	if !containsString(changes, "tonemap:ocl:inject-opencl-device") ||
		!containsString(changes, "tonemap:ocl:drop-lead-hwupload") ||
		!containsString(changes, "tonemap:ocl:collapse-revmap-download") {
		t.Errorf("missing expected change tags: %v", changes)
	}
}

// No-sub HDR opencl tonemap (no leading hwupload, ends back at VA → encode):
// device injected, output_format forced, graph otherwise intact.
func TestGPUResidentOpenCLTonemap_NoSubKeepsVAResident(t *testing.T) {
	args := []string{
		"-codec:0", "hevc", "-hwaccel:0", "vaapi",
		"-i", "/media/m.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]scale_vaapi=w=1920:h=1080:format=p010," +
			"hwmap=derive_device=opencl,tonemap_opencl=tonemap=hable:format=nv12,hwmap=derive_device=vaapi:reverse=1[v]",
		"-map", "[v]", "-codec:0", "h264_vaapi",
	}
	out, _ := gpuResidentOpenCLTonemap(args)
	if !hasOpenCLInitDevice(out) {
		t.Error("opencl device must be injected")
	}
	if indexOfArg(out, "-hwaccel_output_format:0", 0) < 0 {
		t.Error("output_format vaapi must be forced")
	}
	// No download tail here (ends at VA for encode) → reverse-map kept.
	g := ocl_filterValue(out)
	if !strings.Contains(g, "hwmap=derive_device=vaapi:reverse=1") {
		t.Errorf("reverse-map to VA must be kept when encode follows: %q", g)
	}
}

// SW decode (no -hwaccel) → no-op (no VA surface to feed).
func TestGPUResidentOpenCLTonemap_SWDecodeNoop(t *testing.T) {
	args := []string{
		"-codec:0", "libdav1d", "-i", "/media/m.mkv",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=format=p010,hwmap=derive_device=opencl,tonemap_opencl=tonemap=mobius:format=nv12,hwmap=derive_device=vaapi:reverse=1[v]",
		"-map", "[v]", "-codec:0", "h264_vaapi",
	}
	out, changes := gpuResidentOpenCLTonemap(args)
	if hasOpenCLInitDevice(out) {
		t.Error("must not inject opencl device for SW decode")
	}
	if len(changes) != 0 {
		t.Errorf("expected no changes for SW decode, got %v", changes)
	}
}

// tonemap_vaapi (SCALEPLEX_TONEMAP=vaapi, no opencl) → no-op.
func TestGPUResidentOpenCLTonemap_VaapiTonemapNoop(t *testing.T) {
	args := []string{
		"-codec:0", "hevc", "-hwaccel:0", "vaapi", "-i", "/media/m.mkv",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=format=p010,tonemap_vaapi=transfer=bt709:format=nv12[v]",
		"-map", "[v]", "-codec:0", "h264_vaapi",
	}
	out, changes := gpuResidentOpenCLTonemap(args)
	if hasOpenCLInitDevice(out) {
		t.Error("must not inject opencl device when no tonemap_opencl")
	}
	if len(changes) != 0 {
		t.Errorf("expected no changes for tonemap_vaapi, got %v", changes)
	}
}
