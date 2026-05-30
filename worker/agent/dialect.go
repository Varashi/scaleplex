// dialect — backend-specific HW transcode argv emission.
//
// scaleplex worker today is VAAPI-only (Intel Arc, iHD driver). NVIDIA
// support (skw-d-linuxtest dev box → ghcr nvidia worker image) is being
// added in Phase 1; the rewriter must therefore stop hardcoding `vaapi` /
// `scale_vaapi` / `h264_vaapi` literals and instead route per-backend
// choices through a dialect.
//
// This file defines the interface + the VAAPI implementation. The
// NVIDIA implementation lives in a separate file (added in a later PR).
// Selected at worker startup via WORKER_BACKEND env — default `auto`
// auto-detects `/dev/nvidia0` vs `/dev/dri/renderD*`.
//
// Implementations are STATELESS value types — no per-call allocation,
// no shared state. They're effectively named lookup tables + small
// string builders. Callers hold a `dialect` reference and invoke
// methods anywhere the old code referenced a per-backend global.
//
// **This commit is intentionally additive only** — the interface +
// vaapiDialect{} are defined, but no caller has been switched over
// yet. Tests stay byte-identical. Subsequent commits replace the
// direct global references with dialect method calls one surface at
// a time (encoderMap → filter strings → init_hw_device → ...), each
// commit re-validating the test suite.

package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// deviceExists reports whether a device path is present. A package var so
// tests can simulate host hardware without touching /dev.
var deviceExists = func(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// hasRenderNode reports whether a DRM render node (/dev/dri/renderD*) exists —
// the signal that a VAAPI-capable GPU is present. Used by selectDialect's auto
// mode to distinguish a GPU-less host (→ swDialect) from an Intel/AMD one.
// A package var so tests can override the probe.
var hasRenderNode = func() bool {
	m, _ := filepath.Glob("/dev/dri/renderD*")
	return len(m) > 0
}

// hasNvidiaDevice reports whether the host exposes an NVIDIA GPU to the
// container. /dev/nvidia0 appears on bare-metal / VFIO passthrough; /dev/dxg
// is the WSL2 case — nvidia-container-toolkit synthesizes the NVIDIA
// capability from the DirectX-on-Linux compute interface, and no /dev/nvidia*
// nodes exist there.
func hasNvidiaDevice() bool {
	return deviceExists("/dev/nvidia0") || deviceExists("/dev/dxg")
}

// dialect captures the per-backend specifics of HW transcode argv
// emission. Implementations are stateless — pure value types.
type dialect interface {
	// backendName is the canonical short tag. Reported in /capability
	// and worker logs. "vaapi" or "nvenc".
	backendName() string

	// encoderMap maps PMS-emitted software encoder names to this
	// backend's hardware encoder names. e.g. for VAAPI:
	// {"libx264": "h264_vaapi", "libx265": "hevc_vaapi"}.
	encoderMap() map[string]string

	// decoderMap maps PMS-emitted software decoder names to the bare
	// short codec name used in the HW-decode hint position. e.g.
	// {"libdav1d": "av1", "libhevc": "hevc", "libx264": "h264"}.
	// Same on both VAAPI and NVIDIA — Plex's SW decoder library
	// names don't depend on the worker's HW backend; only the
	// downstream hwaccel + encoder choice does.
	decoderMap() map[string]string

	// hwDecodeShortCodecs is the set of bare codec names PMS emits in
	// the `-codec:0` slot when its HW probe succeeded AND it wants the
	// worker to use this backend's hwaccel. When PMS sends one of
	// these alongside the matching -hwaccel flag, the rest of the
	// argv is already in this backend's shape and we pass through.
	hwDecodeShortCodecs() map[string]struct{}

	// hwaccelName is the value passed to `-hwaccel:N` for HW decode.
	// VAAPI: "vaapi". NVIDIA: "nvdec" (matches Plex's emitted value;
	// CUVID-side decoders also accept "cuda" but Plex picks nvdec).
	hwaccelName() string

	// hwaccelOutputFormat is the value passed to `-hwaccel_output_format:N`
	// — the surface format ffmpeg uses for HW-decoded frames before they
	// enter the filter graph. VAAPI: "vaapi". NVIDIA: "cuda".
	hwaccelOutputFormat() string

	// filterHWDeviceName is the value passed to `-filter_hw_device`,
	// binding HW filters in the graph to this backend's device.
	// VAAPI: "vaapi". NVIDIA: "cuda".
	filterHWDeviceName() string

	// initHWDeviceArg returns the `-init_hw_device` value targeting the
	// worker's local device index `devIdx`.
	//
	// VAAPI: returns "vaapi=vaapi:" — empty path; the scaleplex-ffmpeg
	// fork patch 0116-vaapi-device-env-retarget retargets the VAAPI device
	// at open time from SCALEPLEX_RENDER_DEVICE, so the path content here
	// is intentionally blank. devIdx is ignored on this backend.
	//
	// NVIDIA: returns "cuda=cuda:N". PMS's `-init_hw_device cuda=cuda:pci:BBBB:BB:DD.F`
	// PCI form is host-local and meaningless on a remote worker, so the
	// rewriter normalizes to the worker-local device index always.
	initHWDeviceArg(devIdx int) string

	// scaleFilter emits a HW scale filter targeting pixel format `pix`
	// (typical values: "nv12", "p010"). The format is appended via
	// `:format=PIX`. Filter name is backend-specific:
	//   VAAPI: scale_vaapi=w=W:h=H:format=PIX
	//   NVIDIA: scale_cuda=w=W:h=H:format=PIX
	scaleFilter(w, h, pix string) string

	// tonemapFilter emits a single-stage HW tonemap targeting pixel
	// format `pix` (typical "nv12"). Algo precedence is backend-specific:
	//
	//   VAAPI: tonemap_vaapi=transfer=bt709:format=PIX
	//          (iHD fixed BT.2390 EETF — algo is IGNORED. The OpenCL-derive
	//          alternative used by the existing VAAPI tonemapConfig is
	//          NOT a single-stage filter and is composed elsewhere.)
	//   NVIDIA: tonemap_cuda=ALGO:PIX
	//          (algo is honored; matches Plex's argv shape captured
	//          2026-05-27 from the dev box.)
	tonemapFilter(algo, pix string) string

	// hwUploadFilter is the filter name that uploads CPU frames to this
	// backend's HW surface. Both backends accept the bare "hwupload" name
	// when -filter_hw_device steers the right device; method kept for
	// symmetry + future divergence (e.g. hwupload_cuda explicit form).
	hwUploadFilter() string

	// hwDownloadFilter is the filter name that pulls HW frames back to
	// system memory. Same string ("hwdownload") on both backends today;
	// kept for symmetry.
	hwDownloadFilter() string

	// subBurnDownloadFilter returns the filter(s) to splice between the
	// scaled/tonemapped HW surface and the CPU `inlineass` (libass) burn
	// stage, or "" when none is needed.
	//
	//   VAAPI: "" — the fork's vf_inlineass merged HW branch (patch 0115)
	//          consumes a VAAPI surface directly; no download.
	//   NVIDIA: "" — the fork's vf_inlineass CUDA branch (patch 0126)
	//          consumes a CUDA surface directly; the libass band is rendered
	//          small on the CPU and alpha-blended onto the GPU frame, so the
	//          full frame never leaves the GPU. h264_nvenc takes the result.
	subBurnDownloadFilter() string
}

// vaapiDialect — VAAPI backend, vendor-branched. Intel iHD is the historical
// default (tested against the full ~200-entry argv corpus). AMD radeonsi
// (RX 6800+) shares encode/decode/scale primitives but diverges on tonemap
// (no tonemap_vaapi on radeonsi — libplacebo via Vulkan replaces it) and
// overlay (no overlay_vaapi → sub-burn uses the vf_inlineass AMD-Vulkan
// branch, fork patch 0127). Zero-value preserves Intel iHD behavior so
// callers using `vaapiDialect{}` keep working. #123.
type vaapiDialect struct {
	// vendor: "" / "intel" → iHD (default), "amd" → radeonsi. Selected at
	// startup by selectDialect() from probeVAAPIDriver(). The
	// non-divergent methods (encode/decode/scale/hwaccel/initHWDevice/
	// hwupload/hwdownload) are vendor-agnostic.
	vendor string
}

func (vaapiDialect) backendName() string { return "vaapi" }

// isAMD is the convenience predicate for vendor-branched methods + callers.
func (d vaapiDialect) isAMD() bool { return d.vendor == "amd" }

func (vaapiDialect) encoderMap() map[string]string {
	// Identical content to the package-level encoderMap var (kept for
	// transitional compatibility while call sites migrate). The
	// returned map is the same backing array — do not mutate.
	return encoderMap
}

func (vaapiDialect) decoderMap() map[string]string {
	return decoderMap
}

func (vaapiDialect) hwDecodeShortCodecs() map[string]struct{} {
	return hwDecodeShortCodecs
}

func (vaapiDialect) hwaccelName() string         { return "vaapi" }
func (vaapiDialect) hwaccelOutputFormat() string { return "vaapi" }
func (vaapiDialect) filterHWDeviceName() string  { return "vaapi" }

func (vaapiDialect) initHWDeviceArg(_ int) string {
	// devIdx ignored — the scaleplex-ffmpeg fork patch 0116 retargets
	// the VAAPI device at open time from SCALEPLEX_RENDER_DEVICE.
	return "vaapi=vaapi:"
}

func (vaapiDialect) scaleFilter(w, h, pix string) string {
	return "scale_vaapi=w=" + w + ":h=" + h + ":format=" + pix
}

func (vaapiDialect) tonemapFilter(_ /* algo */, pix string) string {
	// iHD fixed-curve BT.2390 EETF — no algo slot. Algo arg ignored.
	return "tonemap_vaapi=transfer=bt709:format=" + pix
}

func (vaapiDialect) hwUploadFilter() string   { return "hwupload" }
func (vaapiDialect) hwDownloadFilter() string { return "hwdownload" }

// vf_inlineass consumes the VAAPI surface directly (fork patch 0115) — no
// download needed before the burn stage.
func (vaapiDialect) subBurnDownloadFilter() string { return "" }

// swDialect — pure-software (CPU) transcode, for no-GPU fleet members in a
// heterogeneous cluster (scaleplex#77 PR3). It reshapes ANY incoming argv
// (VAAPI/NVENC HW or already-SW) down to a CPU pipeline: SW decode, SW
// `scale=`/zscale tonemap, libx264/libx265 encode, SW (FFDraw) inlineass burn.
// No HW device, no hwaccel, no HW filters.
//
// The HW-shaped interface methods (hwaccelName, initHWDeviceArg, the scale/
// tonemap HW nodes, ...) are NOT on the SW code path — composeBurn early-returns
// to composeBurnSW and reshapeToSoftware strips/rebuilds the HW constructs — so
// they return inert SW-safe values for interface completeness.
type swDialect struct{}

func (swDialect) backendName() string { return "sw" }

// swEncoderMap is identity over the SW encoders: a libx264/libx265 encoder is
// already the SW target, so honor-source detection (plexSWEncoder) reads it as
// "known" and the encoder reshape is a no-op. Package-level (reused, not
// per-call allocated) — same pattern as vaapiDialect.encoderMap. Do not mutate.
var swEncoderMap = map[string]string{"libx264": "libx264", "libx265": "libx265"}

func (swDialect) encoderMap() map[string]string { return swEncoderMap }

// decoderMap is empty — a SW worker never injects a HW-decode hint; Plex's SW
// decoder (libdav1d/…) or the bare codec decodes on the CPU.
func (swDialect) decoderMap() map[string]string { return map[string]string{} }

// hwDecodeShortCodecs is empty — a SW worker never HW-decodes, so no bare short
// codec is treated as a HW-passthrough.
func (swDialect) hwDecodeShortCodecs() map[string]struct{} { return map[string]struct{}{} }

func (swDialect) hwaccelName() string          { return "" }
func (swDialect) hwaccelOutputFormat() string  { return "" }
func (swDialect) filterHWDeviceName() string   { return "" }
func (swDialect) initHWDeviceArg(_ int) string { return "" }

func (swDialect) scaleFilter(w, h, pix string) string {
	return "scale=w=" + w + ":h=" + h + ",format=" + pix
}

func (swDialect) tonemapFilter(algo, pix string) string { return swTonemapChain(algo, pix) }

func (swDialect) hwUploadFilter() string        { return "" }
func (swDialect) hwDownloadFilter() string      { return "" }
func (swDialect) subBurnDownloadFilter() string { return "" }

// activeDialect is the worker's selected backend. Populated in main()
// via selectDialect() before any rewrite occurs. Default vaapi for
// callers that still hold a static reference; once all references go
// through this var the package-level globals become removable.
var activeDialect dialect = vaapiDialect{}

// selectDialect picks the backend at worker startup based on
// WORKER_BACKEND env. Values: "auto" (default — probes for an NVIDIA
// device (/dev/nvidia0 or WSL2's /dev/dxg) first, then a DRM render
// node for VAAPI, else CPU), "vaapi", "nvenc" (alias: "nvidia"), "sw".
//
// The unified worker image carries both runtimes (VAAPI userspace +
// CUDA runtime); the device-probe picks the one matching the host.
// Operators can pin via the env knob; unknown values log a WARN and
// fall back to VAAPI (safe default — matches every pre-PR-#61
// deployment).
func selectDialect() dialect {
	switch want := strings.ToLower(strings.TrimSpace(os.Getenv("WORKER_BACKEND"))); want {
	case "vaapi":
		return pickVaapiDialect()
	case "nvenc", "nvidia": // "nvidia" kept as an operator-facing alias
		return nvencDialect{}
	case "sw", "cpu", "software":
		return swDialect{}
	case "", "auto":
		if hasNvidiaDevice() {
			return nvencDialect{}
		}
		if hasRenderNode() {
			return pickVaapiDialect()
		}
		// No NVIDIA device and no DRM render node → no usable GPU → CPU.
		return swDialect{}
	default:
		log.Printf("WORKER_BACKEND=%q unknown; falling back to vaapi", want)
		return pickVaapiDialect()
	}
}

// probeVAAPIDriverForDialect indirection — production hits the sync.Once'd
// detectVAAPIDriver; tests swap it (the Once + module-level cache make
// detectVAAPIDriver itself awkward to inject directly).
var probeVAAPIDriverForDialect = detectVAAPIDriver

// pickVaapiDialect chooses the VAAPI vendor at startup from the libva driver
// name probed off /sys/class/drm/renderD*/device/vendor (detectVAAPIDriver,
// #124). `radeonsi` → AMD branch (fork patch 0127 vf_inlineass AMD-Vulkan
// sub-burn + libplacebo tonemap), anything else → Intel iHD. #123.
func pickVaapiDialect() dialect {
	if probeVAAPIDriverForDialect() == "radeonsi" {
		return vaapiDialect{vendor: "amd"}
	}
	return vaapiDialect{vendor: "intel"}
}
