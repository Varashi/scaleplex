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
	"strings"
)

// dialect captures the per-backend specifics of HW transcode argv
// emission. Implementations are stateless — pure value types.
type dialect interface {
	// backendName is the canonical short tag. Reported in /capability
	// and worker logs. "vaapi" or "nvidia".
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
}

// vaapiDialect — Intel iHD / VAAPI. The historical default; tested
// against the full ~200-entry argv corpus.
type vaapiDialect struct{}

func (vaapiDialect) backendName() string { return "vaapi" }

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

// activeDialect is the worker's selected backend. Populated in main()
// via selectDialect() before any rewrite occurs. Default vaapi for
// callers that still hold a static reference; once all references go
// through this var the package-level globals become removable.
var activeDialect dialect = vaapiDialect{}

// selectDialect picks the backend at worker startup based on
// WORKER_BACKEND env. Values: "auto" (default — probes /dev/nvidia0
// first, falls back to VAAPI), "vaapi", "nvidia".
//
// The unified worker image carries both runtimes (VAAPI userspace +
// CUDA runtime); the device-probe picks the one matching the host.
// Operators can pin via the env knob; unknown values log a WARN and
// fall back to VAAPI (safe default — matches every pre-PR-#61
// deployment).
func selectDialect() dialect {
	switch want := strings.ToLower(strings.TrimSpace(os.Getenv("WORKER_BACKEND"))); want {
	case "vaapi":
		return vaapiDialect{}
	case "nvidia":
		return nvidiaDialect{}
	case "", "auto":
		if _, err := os.Stat("/dev/nvidia0"); err == nil {
			return nvidiaDialect{}
		}
		return vaapiDialect{}
	default:
		log.Printf("WORKER_BACKEND=%q unknown; falling back to vaapi", want)
		return vaapiDialect{}
	}
}
