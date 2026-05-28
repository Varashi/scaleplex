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
