// nvidiaDialect — NVIDIA NVENC/NVDEC backend for the worker rewriter.
//
// Counterpart of vaapiDialect in dialect.go. PMS emits NVENC/NVDEC argv
// when its server-side HW probe succeeded against a CUDA device; the
// worker rewrites it for its own local CUDA device index (the PCI form
// PMS embeds is host-local + meaningless on a remote worker).
//
// Phase 1 PR #2: dialect-interface methods only (encoderMap, decoderMap,
// hwDecodeShortCodecs). Filter-emit methods (scale_cuda, tonemap_cuda,
// init_hw_device, etc.) land in subsequent commits on this branch
// alongside the corpus replay test.
//
// Dev box for validation: skw-d-linuxtest (RTX 3080, WSL2 Ubuntu 24.04,
// nvidia-container-toolkit 1.19). 63-clip NVENC argv corpus captured
// 2026-05-27; see project_scaleplex_nvidia_worker_dev +
// reference_plex_nvenc_argv_shapes.

package main

// nvencEncoderMap routes PMS's chosen software encoder to its NVENC
// equivalent. PMS picks libx264 vs libx265 per session based on its
// own prefs + client capability; same logic as VAAPI, different
// encoder name. AV1 encode is Ada (RTX 4000+) only — Ampere RTX 3080
// dev box can't validate libsvtav1→av1_nvenc, so it is intentionally
// absent until a fleet member can encode AV1.
var nvencEncoderMap = map[string]string{
	"libx264": "h264_nvenc",
	"libx265": "hevc_nvenc",
}

// nvidiaDialect — NVIDIA NVENC/NVDEC + CUDA filter chain.
type nvidiaDialect struct{}

func (nvidiaDialect) backendName() string { return "nvidia" }

func (nvidiaDialect) encoderMap() map[string]string {
	return nvencEncoderMap
}

func (nvidiaDialect) decoderMap() map[string]string {
	// Plex's SW decoder library names (libdav1d, libhevc, libx264) are
	// backend-agnostic — the worker's HW backend doesn't change what
	// PMS calls its software decoders. Reuse the package-level map.
	return decoderMap
}

func (nvidiaDialect) hwDecodeShortCodecs() map[string]struct{} {
	// Bare codec names PMS emits in `-codec:0` after a successful HW
	// probe. Same set as VAAPI — Plex's HW-decode hint codec list is
	// the union of formats its NVDEC/VAAPI probe accepts.
	return hwDecodeShortCodecs
}
