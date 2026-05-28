// source_backend — classify the HW backend a PMS-emitted argv is shaped
// for, independent of the worker's own backend.
//
// Phase 1 ("honor source backend") assumed the incoming argv's backend
// matched the worker's. The cross-backend epic (scaleplex#77) drops that
// assumption: a VAAPI-configured PMS can dispatch a VAAPI-shaped argv to a
// NVIDIA worker, which must renormalize it to NVENC/CUDA. The detector here
// is the first step — it answers "what backend is this argv shaped for?" so
// the rewriter can decide whether a cross-backend reshape is needed
// (sourceBackend != activeDialect.backendName()).
//
// This file is additive: the detector + reverse map are defined and unit-
// tested but only wired into the reshape paths in a later PR. No behavior
// change on its own.

package main

import "strings"

// sourceBackend is the HW backend a PMS argv is shaped for.
type sourceBackend string

const (
	srcNone  sourceBackend = "none"  // no video encoder (audio-only / detection pass)
	srcSW    sourceBackend = "sw"    // pure software (libx264/libx265/…, no HW constructs)
	srcVAAPI sourceBackend = "vaapi" // Intel iHD: -hwaccel vaapi / scale_vaapi / h264_vaapi / tonemap_opencl
	srcNVENC sourceBackend = "nvenc" // NVIDIA: -hwaccel nvdec / scale_cuda / h264_nvenc / tonemap_cuda
)

// Drift guard: the cross-backend no-op check compares string(sourceBackend)
// directly against dialect.backendName(), so the source-backend constants MUST
// stay equal to the dialect names. Panic at startup if they ever diverge (e.g.
// a future dialect rename that misses one side) — cheaper to catch here than to
// silently mis-route a cross-backend reshape.
func init() {
	if string(srcVAAPI) != (vaapiDialect{}).backendName() ||
		string(srcNVENC) != (nvencDialect{}).backendName() {
		panic("source-backend/dialect name drift: srcVAAPI=" + string(srcVAAPI) +
			" vaapi=" + (vaapiDialect{}).backendName() +
			"; srcNVENC=" + string(srcNVENC) + " nvenc=" + (nvencDialect{}).backendName())
	}
}

// hwEncoderCodec reverse-maps a hardware encoder name to its bare codec, so a
// foreign HW encoder (e.g. h264_nvenc received by a VAAPI worker) can be
// renormalized to a codec and re-forwarded through the worker dialect's
// encoderMap. The forward direction (codec/libname → worker HW encoder) lives
// in the dialect; this is only the reverse half.
var hwEncoderCodec = map[string]string{
	"h264_vaapi":  "h264",
	"hevc_vaapi":  "hevc",
	"av1_vaapi":   "av1",
	"vp9_vaapi":   "vp9",
	"vp8_vaapi":   "vp8",
	"mjpeg_vaapi": "mjpeg",
	"h264_nvenc":  "h264",
	"hevc_nvenc":  "hevc",
	"av1_nvenc":   "av1",
}

// codecCanonicalSWEncoder maps a bare codec to the canonical PMS software
// encoder name the worker dialect's encoderMap is keyed on. Lets a foreign
// HW encoder be routed: foreign → hwEncoderCodec → codec → this →
// activeDialect.encoderMap()[libname] = worker HW encoder. Only h264/hevc are
// in encoderMap today (the codecs PMS HW-transcodes); others have no HW
// encoder mapping and fall through to a SW/bail decision in the caller.
var codecCanonicalSWEncoder = map[string]string{
	"h264": "libx264",
	"hevc": "libx265",
}

// vaapiFilterTokens / nvencFilterTokens are the backend-distinctive filter
// node names. tonemap_opencl is VAAPI-side (the OpenCL-derive tonemap is only
// used in the VAAPI pipeline). scale/tonemap (bare, SW) are backend-agnostic
// and intentionally absent.
var vaapiFilterTokens = []string{"scale_vaapi", "tonemap_vaapi", "overlay_vaapi", "tonemap_opencl"}
var nvencFilterTokens = []string{"scale_cuda", "tonemap_cuda", "overlay_cuda"}

// detectSourceBackend classifies the HW backend the argv is shaped for. It
// weighs three signals — the `-hwaccel:0` value, the output encoder, and the
// filter-graph node names — and returns the single HW backend referenced
// (PMS never mixes backends in one argv). Falls back to srcSW when a video
// encoder is present but no HW construct is, and srcNone when there's no
// output video encoder at all.
func detectSourceBackend(args []string) sourceBackend {
	hasVAAPI, hasNVENC := false, false

	// 1. -hwaccel:0 value (decode side).
	if i := indexOfArg(args, "-hwaccel:0", 0); i >= 0 && i+1 < len(args) {
		switch args[i+1] {
		case "vaapi":
			hasVAAPI = true
		case "nvdec", "cuda", "cuvid":
			hasNVENC = true
		}
	}

	// 2. Output encoder (after -i). Foreign HW encoder names are decisive.
	if in := indexOfArg(args, "-i", 0); in >= 0 {
		if e := indexOfArg(args, "-codec:0", in+1); e >= 0 && e+1 < len(args) {
			enc := args[e+1]
			if strings.HasSuffix(enc, "_vaapi") {
				hasVAAPI = true
			} else if strings.HasSuffix(enc, "_nvenc") {
				hasNVENC = true
			}
		}
	}

	// 3. Filter-graph node names.
	if fc := filterComplexValue(args); fc != "" {
		for _, t := range vaapiFilterTokens {
			if strings.Contains(fc, t) {
				hasVAAPI = true
				break
			}
		}
		for _, t := range nvencFilterTokens {
			if strings.Contains(fc, t) {
				hasNVENC = true
				break
			}
		}
	}

	switch {
	case hasVAAPI:
		return srcVAAPI
	case hasNVENC:
		return srcNVENC
	}
	// No HW construct. SW if there's a video encoder to transcode, else none.
	if hasVideoEncoder(args) {
		return srcSW
	}
	return srcNone
}

// filterComplexValue returns the first -filter_complex value, or "".
func filterComplexValue(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-filter_complex" {
			return args[i+1]
		}
	}
	return ""
}

// hasVideoEncoder reports whether an output video encoder is present (a
// -codec:0 after -i that names an encoder, not "copy"). Distinguishes a real
// transcode from an audio-only / detection pass.
func hasVideoEncoder(args []string) bool {
	in := indexOfArg(args, "-i", 0)
	if in < 0 {
		return false
	}
	e := indexOfArg(args, "-codec:0", in+1)
	if e < 0 || e+1 >= len(args) {
		return false
	}
	enc := args[e+1]
	return enc != "copy" && enc != ""
}

// filterComplexIndex returns the index of the first -filter_complex VALUE
// (the token after the flag), or -1.
func filterComplexIndex(args []string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-filter_complex" {
			return i + 1
		}
	}
	return -1
}

// setArgValue replaces the value token following `flag` with `val`, in place.
// No-op when the flag is absent. Used by the cross-backend reshape to REPLACE
// foreign decode-flag values (vs the honor-source paths which inject-when-
// missing).
func setArgValue(args []string, flag, val string) bool {
	if i := indexOfArg(args, flag, 0); i >= 0 && i+1 < len(args) {
		args[i+1] = val
		return true
	}
	return false
}

// isForeignHWSource reports whether the argv is shaped for a HW backend other
// than the worker's — i.e. a cross-backend reshape would apply. Used to decide
// whether the Plex-Pass gate query is needed (re-accel only).
func isForeignHWSource(args []string) bool {
	src := detectSourceBackend(args)
	return (src == srcVAAPI || src == srcNVENC) && string(src) != activeDialect.backendName()
}

// reshapeForeignHWArgv translates an argv shaped for a HW backend DIFFERENT
// from the worker's into the worker's native backend, so the honor-source
// logic downstream runs on it unchanged. No-op when source == worker, or
// source is SW/none (SW is reshaped by the backend-agnostic rewriteVideoFilter
// path; none has no video to transcode).
//
// Three normalizations:
//  1. Decode flags — REPLACE -hwaccel:0 / -hwaccel_output_format:0 /
//     -hwaccel_device:0 / -init_hw_device / -filter_hw_device with the worker
//     dialect's. The decoder codec (hevc/av1/h264) is backend-agnostic, kept.
//  2. Filter graph — extractGraphFacts (backend-agnostic) → composeBurn (worker
//     dialect). vaResident=true: with -hwaccel_output_format now the worker's,
//     [0:0] lands as the worker's HW surface. Covers no-sub + text-sub; bitmap
//     is left for the downstream bitmap branch (PGS cross-backend unvalidated,
//     scaleplex#76). animatedTierDown is added by the downstream pass which has
//     the authoritative subSrc — the composeBurn here is idempotent under it.
//  3. Encoder — foreign HW encoder → hwEncoderCodec → codecCanonicalSWEncoder
//     → activeDialect.encoderMap() → worker HW encoder.
//
// This re-accelerates onto the worker's HW; the caller gates it on the
// Plex-Pass check (scaleplex#78).
func reshapeForeignHWArgv(args []string, tm tonemapConfig) ([]string, []string) {
	src := detectSourceBackend(args)
	if src != srcVAAPI && src != srcNVENC {
		return args, nil
	}
	if string(src) == activeDialect.backendName() {
		return args, nil // already native (srcVAAPI/srcNVENC match the dialect names directly)
	}
	d := activeDialect

	// 1. Decode flags → worker's.
	setArgValue(args, "-hwaccel:0", d.hwaccelName())
	setArgValue(args, "-hwaccel_output_format:0", d.hwaccelOutputFormat())
	setArgValue(args, "-hwaccel_device:0", d.filterHWDeviceName())
	setArgValue(args, "-init_hw_device", d.initHWDeviceArg(0))
	setArgValue(args, "-filter_hw_device", d.filterHWDeviceName())

	// 2. Filter graph → worker dialect (no-sub + text-sub; bitmap deferred).
	if vfIdx := filterComplexIndex(args); vfIdx >= 0 {
		facts := extractGraphFacts(args[vfIdx], nil)
		if facts.ok && facts.subKind != "bitmap" {
			oldLabel := ""
			if m := reGraphTrailingLabel.FindStringSubmatch(args[vfIdx]); m != nil {
				oldLabel = "[" + m[1] + "]"
			}
			newFilter, newLabel := tm.composeBurn(burnSpec{
				vaResident: true,
				w:          facts.w,
				h:          facts.h,
				hdr:        facts.hdr,
				algo:       facts.algo,
				burnSub:    facts.subKind == "text",
				subParams:  facts.subParams,
			})
			args[vfIdx] = newFilter
			retargetMapLabel(args, oldLabel, newLabel)
		}
	}

	// 3. Encoder → worker's HW encoder.
	if in := indexOfArg(args, "-i", 0); in >= 0 {
		if e := indexOfArg(args, "-codec:0", in+1); e >= 0 && e+1 < len(args) {
			if codec, isHW := hwEncoderCodec[args[e+1]]; isHW {
				if lib, ok := codecCanonicalSWEncoder[codec]; ok {
					if hwEnc, ok := d.encoderMap()[lib]; ok {
						args[e+1] = hwEnc
					}
				}
			}
		}
	}

	return args, []string{"cross-backend:" + string(src) + "->" + d.backendName()}
}
