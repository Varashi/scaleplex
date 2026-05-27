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

// activeDialect is the worker's selected backend. Populated in main()
// via selectDialect() before any rewrite occurs. Default vaapi for
// callers that still hold a static reference; once all references go
// through this var the package-level globals become removable.
var activeDialect dialect = vaapiDialect{}

// selectDialect picks the backend at worker startup based on
// WORKER_BACKEND env. Values: "vaapi" (default), "nvidia", "auto"
// (probe /dev/nvidia0 first, fall back to vaapi).
//
// On unknown values: log a WARN and fall back to vaapi (safe default —
// matches every existing prod deployment).
func selectDialect() dialect {
	switch want := strings.ToLower(strings.TrimSpace(os.Getenv("WORKER_BACKEND"))); want {
	case "", "vaapi":
		return vaapiDialect{}
	case "nvidia":
		return nvidiaDialect{}
	case "auto":
		if _, err := os.Stat("/dev/nvidia0"); err == nil {
			return nvidiaDialect{}
		}
		return vaapiDialect{}
	default:
		log.Printf("WORKER_BACKEND=%q unknown; falling back to vaapi", want)
		return vaapiDialect{}
	}
}
