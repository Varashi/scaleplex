// nvencDialect — NVIDIA NVENC/NVDEC backend for the worker rewriter.
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

import "strconv"

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

// nvencDialect — NVIDIA NVENC/NVDEC + CUDA filter chain.
type nvencDialect struct{}

func (nvencDialect) backendName() string { return "nvenc" }

func (nvencDialect) encoderMap() map[string]string {
	return nvencEncoderMap
}

func (nvencDialect) decoderMap() map[string]string {
	// Plex's SW decoder library names (libdav1d, libhevc, libx264) are
	// backend-agnostic — the worker's HW backend doesn't change what
	// PMS calls its software decoders. Reuse the package-level map.
	return decoderMap
}

func (nvencDialect) hwDecodeShortCodecs() map[string]struct{} {
	// Bare codec names PMS emits in `-codec:0` after a successful HW
	// probe. Same set as VAAPI — Plex's HW-decode hint codec list is
	// the union of formats its NVDEC/VAAPI probe accepts.
	return hwDecodeShortCodecs
}

// hwaccelName: Plex emits "-hwaccel:0 nvdec" for NVIDIA HW decode (vs
// "cuda"). Mirror what PMS sends so passthrough is byte-identical.
func (nvencDialect) hwaccelName() string { return "nvdec" }

// hwaccelOutputFormat: surface format the HW decoder writes to.
// PMS emits "-hwaccel_output_format:0 cuda" — frames sit on the
// CUDA device for scale_cuda/tonemap_cuda to consume directly.
func (nvencDialect) hwaccelOutputFormat() string { return "cuda" }

// filterHWDeviceName: -filter_hw_device cuda — binds graph filters
// (scale_cuda, tonemap_cuda, overlay_cuda) to the CUDA device.
func (nvencDialect) filterHWDeviceName() string { return "cuda" }

// initHWDeviceArg normalizes to the worker-local CUDA device index.
// PMS emits `-init_hw_device cuda=cuda:pci:BBBB:BB:DD.F` for HW-probed
// sessions; the PCI ID is host-local on the PMS box and meaningless on
// a remote worker. Worker-local device index `devIdx` is always used.
func (nvencDialect) initHWDeviceArg(devIdx int) string {
	return "cuda=cuda:" + strconv.Itoa(devIdx)
}

func (nvencDialect) scaleFilter(w, h, pix string) string {
	return "scale_cuda=w=" + w + ":h=" + h + ":format=" + pix
}

// tonemapFilter emits `tonemap_cuda=tonemap=ALGO:format=PIX` (named-arg
// syntax). Plex's bundled ffmpeg accepts the positional form
// `tonemap_cuda=ALGO:PIX` (its fork puts `format` as positional 2),
// but jellyfin-ffmpeg (= scaleplex-ffmpeg7's lineage) puts
// `tonemap_mode` at positional 2, so the positional form parses
// `PIX` as a tonemap_mode enum value and bails with "Undefined
// constant". Discovered 2026-05-28 via live deploy on skw-d-linuxtest:
// the 4K HDR HEVC NVDEC chain failed at filter-init until the
// rewriter switched to named-arg emission. Named form is portable
// across both forks. transfer/matrix/primaries kwargs default to
// bt709 which matches Plex's intent for SDR targets.
func (nvencDialect) tonemapFilter(algo, pix string) string {
	return "tonemap_cuda=tonemap=" + algo + ":format=" + pix
}

func (nvencDialect) hwUploadFilter() string   { return "hwupload" }
func (nvencDialect) hwDownloadFilter() string { return "hwdownload" }

// vf_inlineass (CPU/libass) has no CUDA branch, so a scale_cuda/tonemap_cuda
// surface must come down to system nv12 before the burn. Matches Plex's own
// nvenc sub-burn chain (`...tonemap_cuda=...nv12,hwdownload,format=nv12,inlineass`);
// h264_nvenc ingests the resulting sysmem nv12 directly, no re-upload.
func (nvencDialect) subBurnDownloadFilter() string { return "hwdownload,format=nv12" }
