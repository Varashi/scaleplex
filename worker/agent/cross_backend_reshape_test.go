package main

import (
	"strings"
	"testing"
)

// withDialect swaps activeDialect for the duration of a test.
func withDialect(t *testing.T, d dialect) {
	t.Helper()
	prev := activeDialect
	activeDialect = d
	t.Cleanup(func() { activeDialect = prev })
}

func argvHasSeq(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

// A VAAPI-configured PMS dispatches a VAAPI-shaped HW-decode argv to a NVIDIA
// worker. The cross-backend reshape must translate it to native NVENC: nvdec
// decode, cuda surfaces, scale_cuda filter, h264_nvenc encoder — and leave NO
// vaapi/opencl literals anywhere.
func TestCrossBackend_VAAPI_to_NVIDIA_noSub(t *testing.T) {
	withDialect(t, nvencDialect{})
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/x.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_hw_device", "vaapi",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1]",
		"-map", "[1]",
		"-codec:0", "h264_vaapi", "-qp:0", "22",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "cross-backend:vaapi->nvenc") {
		t.Fatalf("missing cross-backend tag: %v", out.Changes)
	}
	joined := strings.Join(out.Args, " ")
	for _, banned := range []string{"vaapi", "renderD128", "iHD", "opencl"} {
		if strings.Contains(joined, banned) {
			t.Errorf("VAAPI literal %q leaked into NVIDIA output: %s", banned, joined)
		}
	}
	// Native NVENC decode + encode + filter.
	if !argvHasSeq(out.Args, "-hwaccel:0", "nvdec") {
		t.Errorf("expected -hwaccel:0 nvdec; args=%v", out.Args)
	}
	if !argvHasSeq(out.Args, "-hwaccel_output_format:0", "cuda") {
		t.Errorf("expected -hwaccel_output_format:0 cuda")
	}
	if !argvHasSeq(out.Args, "-init_hw_device", "cuda=cuda:0") {
		t.Errorf("expected -init_hw_device cuda=cuda:0")
	}
	if !containsString(out.Args, "h264_nvenc") || containsString(out.Args, "h264_vaapi") {
		t.Errorf("encoder not swapped to h264_nvenc: %v", out.Args)
	}
	var fc string
	for i, a := range out.Args {
		if a == "-filter_complex" && i+1 < len(out.Args) {
			fc = out.Args[i+1]
		}
	}
	if !strings.Contains(fc, "scale_cuda") {
		t.Errorf("expected scale_cuda in filter chain: %s", fc)
	}
}

// VAAPI HDR (tonemap_opencl) → NVIDIA: the multi-node OpenCL-derive tonemap
// chain must collapse to a single tonemap_cuda (no hwmap/opencl residue).
func TestCrossBackend_VAAPI_HDR_to_NVIDIA(t *testing.T) {
	withDialect(t, nvencDialect{})
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/x.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=p010,hwmap=derive_device=opencl,tonemap_opencl=tonemap=hable:format=nv12,hwmap=derive_device=vaapi:reverse=1[1]",
		"-map", "[1]",
		"-codec:0", "hevc_vaapi", "-qp:0", "24",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied || !containsString(out.Changes, "cross-backend:vaapi->nvenc") {
		t.Fatalf("not reshaped: applied=%v changes=%v", out.Applied, out.Changes)
	}
	joined := strings.Join(out.Args, " ")
	for _, banned := range []string{"vaapi", "opencl", "hwmap", "derive_device"} {
		if strings.Contains(joined, banned) {
			t.Errorf("VAAPI/OpenCL literal %q leaked: %s", banned, joined)
		}
	}
	var fc string
	for i, a := range out.Args {
		if a == "-filter_complex" && i+1 < len(out.Args) {
			fc = out.Args[i+1]
		}
	}
	if !strings.Contains(fc, "tonemap_cuda=tonemap=hable:format=nv12") {
		t.Errorf("HDR tonemap not collapsed to tonemap_cuda: %s", fc)
	}
	if !containsString(out.Args, "hevc_nvenc") {
		t.Errorf("encoder not swapped to hevc_nvenc: %v", out.Args)
	}
}

// Native argv (source == worker) must NOT be tagged cross-backend — the
// reshape is a no-op, honor-source handles it.
func TestCrossBackend_NativeIsNoOp(t *testing.T) {
	withDialect(t, nvencDialect{})
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "nvdec", "-hwaccel_output_format:0", "cuda", "-hwaccel_device:0", "cuda",
		"-i", "/media/x.mkv",
		"-filter_complex", "[0:0]scale_cuda=w=1280:h=720:format=nv12[1]",
		"-map", "[1]", "-codec:0", "h264_nvenc",
	}
	out := Rewrite(args, nil, nil)
	if containsString(out.Changes, "cross-backend:vaapi->nvenc") || containsString(out.Changes, "cross-backend:nvenc->nvenc") {
		t.Errorf("native NVENC argv wrongly tagged cross-backend: %v", out.Changes)
	}
}

// Bitmap/PGS: reshapeForeignHWArgv DEFERS the filter-graph reshape for bitmap
// subs (facts.subKind=="bitmap") — but still normalizes decode flags + encoder,
// and the downstream detectBitmapOverlayBurn branch reshapes the overlay graph
// to the worker dialect. End result must be native NVENC, no VAAPI literals.
// (PGS-NVENC is unvalidated live, #76 — this only locks the argv shape.)
func TestCrossBackend_VAAPI_bitmap_to_NVENC(t *testing.T) {
	withDialect(t, nvencDialect{})
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/x.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:5]scale=1920:1080,hwupload[s];[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[v];[v][s]overlay_vaapi[1]",
		"-map", "[1]",
		"-codec:0", "h264_vaapi",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied || !containsString(out.Changes, "cross-backend:vaapi->nvenc") {
		t.Fatalf("not reshaped: applied=%v changes=%v", out.Applied, out.Changes)
	}
	joined := strings.Join(out.Args, " ")
	for _, banned := range []string{"vaapi", "h264_vaapi"} {
		if strings.Contains(joined, banned) {
			t.Errorf("VAAPI literal %q leaked (bitmap cross-backend): %s", banned, joined)
		}
	}
	if !argvHasSeq(out.Args, "-hwaccel:0", "nvdec") || !containsString(out.Args, "h264_nvenc") {
		t.Errorf("decode/encoder not native nvenc: %v", out.Args)
	}
	// Positive: the overlay graph was rewritten to the CUDA filter. (Bitmap
	// burn was unified onto inlineass in v1.6.1, so the downstream branch
	// emits scale_cuda + inlineass, not overlay_cuda.)
	if !strings.Contains(joined, "scale_cuda") {
		t.Errorf("expected scale_cuda in reshaped bitmap graph: %s", joined)
	}
}

// #85 Fix B — VAAPI HW-decode + SW filter graph hybrid (Plex's "HW decode,
// SW scale" shape: -hwaccel vaapi but a bare `[0:0]scale=w=…` graph + libx264).
// The cross-backend translator must NOT reshape the backend-agnostic SW filter
// (doing so produced a scale_cuda graph the main SW→HW path then rejected →
// bail → original vaapi argv leaked). It swaps decode flags only; the main
// force-HW path reshapes the SW filter to scale_cuda + h264_nvenc. No bail, no
// vaapi residue.
func TestCrossBackend_VAAPI_HWDecSWFilter_Hybrid_to_NVENC(t *testing.T) {
	withDialect(t, nvencDialect{})
	t.Setenv("SCALEPLEX_FORCE_HW", "1")
	args := []string{
		"-codec:0", "av1",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/x.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_hw_device", "vaapi",
		"-filter_complex", "[0:0]scale=w=1280:h=720:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264", "-preset:0", "veryfast",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("hybrid bailed (must reshape, not bail): %v", out.Changes)
	}
	joined := strings.Join(out.Args, " ")
	for _, banned := range []string{"vaapi", "renderD128", "iHD", "opencl"} {
		if strings.Contains(joined, banned) {
			t.Errorf("VAAPI literal %q leaked (hybrid cross-backend): %s", banned, joined)
		}
	}
	if !argvHasSeq(out.Args, "-hwaccel:0", "nvdec") {
		t.Errorf("decode not swapped to nvdec: %v", out.Args)
	}
	if !strings.Contains(joined, "scale_cuda") {
		t.Errorf("SW scale not reshaped to scale_cuda by main path: %s", joined)
	}
	if !containsString(out.Args, "h264_nvenc") {
		t.Errorf("encoder not reshaped to h264_nvenc: %v", out.Args)
	}
}

// #85 Fix A — a pure-SW argv (libdav1d decode, SW scale=/tonemap= filter,
// libx264) that carries a STALE foreign -init_hw_device (a `vaapi=vaapi:`
// rewrite artifact from an upstream VAAPI capture). Under FORCE_HW the SW→HW
// reshape converts filters + encoder to CUDA, but the leftover vaapi init must
// be dropped + re-injected for the worker dialect — otherwise ffmpeg fails with
// "Device creation failed … 'vaapi=vaapi:'".
func TestCrossBackend_SW_StaleVAAPIInit_to_NVENC(t *testing.T) {
	withDialect(t, nvencDialect{})
	t.Setenv("SCALEPLEX_FORCE_HW", "1")
	args := []string{
		"-codec:0", "libdav1d",
		"-i", "/media/x.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=3840:h=1608:force_divisible_by=4[0];[0]format=p010,tonemap=mobius[1];[1]format=pix_fmts=yuv420p|nv12[2]",
		"-map", "[2]",
		"-codec:0", "libx264",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "replace:foreign-init_hw_device") {
		t.Errorf("expected foreign-init replace tag: %v", out.Changes)
	}
	joined := strings.Join(out.Args, " ")
	if strings.Contains(joined, "vaapi") {
		t.Errorf("stale vaapi init leaked: %s", joined)
	}
	if !argvHasSeq(out.Args, "-init_hw_device", "cuda=cuda:0") {
		t.Errorf("init not re-injected as cuda=cuda:0: %v", out.Args)
	}
}

// Symmetric: NVENC argv → VAAPI worker.
func TestCrossBackend_NVENC_to_VAAPI(t *testing.T) {
	withDialect(t, vaapiDialect{})
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "nvdec", "-hwaccel_output_format:0", "cuda", "-hwaccel_device:0", "cuda",
		"-i", "/media/x.mkv",
		"-init_hw_device", "cuda=cuda:0", "-filter_hw_device", "cuda",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_cuda=w=1280:h=720:format=nv12[1]",
		"-map", "[1]", "-codec:0", "h264_nvenc",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied || !containsString(out.Changes, "cross-backend:nvenc->vaapi") {
		t.Fatalf("not reshaped: applied=%v changes=%v", out.Applied, out.Changes)
	}
	joined := strings.Join(out.Args, " ")
	for _, banned := range []string{"nvdec", "scale_cuda", "h264_nvenc", "cuda"} {
		if strings.Contains(joined, banned) {
			t.Errorf("NVENC literal %q leaked into VAAPI output: %s", banned, joined)
		}
	}
	if !argvHasSeq(out.Args, "-hwaccel:0", "vaapi") || !containsString(out.Args, "h264_vaapi") {
		t.Errorf("not native VAAPI: %v", out.Args)
	}
}
