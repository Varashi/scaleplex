package main

// Agent-side band resolution + main-argv patching.
//
// The pre-render's bottom-band height is data-driven (parsed from the
// SRT cues). For SIDECAR SRT the file is on disk at rewrite time, but for
// EMBEDDED SRT the agent's own extraction step writes the file later. To
// unify both, the rewriter seeds the static-fallback band, sets
// SubPrerenderSpec.ResolveBandPostExtract, and leaves sentinels in the
// main argv where the band-dependent values belong:
//   - overlay_vaapi `y=__SP_BANDY__`        (always, on the SRT path)
//   - scale_vaapi   `h=__SP_BANDH__`        (only under a render-res cap,
//                                            where the upscale target
//                                            height is the band height)
// The agent then, once the SRT file is known (sidecar path or extracted
// .srt):
//  1. calls ResolveAgentBand → resolveSRTBand picks the band height,
//     overwrites spec.BandHeight in place,
//  2. builds the pre-render argv from the now-final spec.BandHeight
//     (so renderBandH / crop are consistent — no patch needed there),
//  3. calls PatchMainArgsBand to substitute both sentinels in the main
//     argv before the main ffmpeg starts.

import (
	"strconv"
	"strings"
)

// Band sentinels the rewriter writes into the main argv when band
// resolution is deferred to the agent. Underscore-wrapped form keeps them
// well outside any ffmpeg argv token namespace (numbers, expressions,
// named consts).
const (
	BandYSentinel = "__SP_BANDY__" // overlay_vaapi y-offset = Height - BandHeight
	BandHSentinel = "__SP_BANDH__" // scale_vaapi upscale target height = BandHeight
)

// ResolveAgentBand picks the final band height for an SRT-text spec whose
// rewrite-time band was a placeholder. Mutates spec.BandHeight when a
// tighter band is found, leaves it alone (= the static fallback)
// otherwise. Returns the resolved y-offset (Height - BandHeight) for the
// caller to patch into the main argv.
//
// Embedded subs use the extracted .srt path; sidecar subs use the spec's
// own SourcePath — both converge here via subFile. Non-SRT codecs, the
// Bitmap path, and specs without ResolveBandPostExtract are no-ops and
// return the existing Height - BandHeight untouched.
func ResolveAgentBand(spec *SubPrerenderSpec, subFile string) int {
	if spec == nil {
		return 0
	}
	if !spec.ResolveBandPostExtract || spec.Bitmap || spec.Height <= 0 || subFile == "" {
		return spec.Height - spec.BandHeight
	}
	if tight, ok := resolveSRTBand(subFile, spec.Height, spec.BandHeight); ok {
		spec.BandHeight = tight
	}
	return spec.Height - spec.BandHeight
}

// PatchMainArgsBand substitutes the band sentinels in the main argv with
// the resolved values: BandYSentinel → bandY, BandHSentinel → bandH. The
// rewriter only ever writes them inside the -filter_complex string, so in
// practice one or two slots in a single arg are patched. Returns the
// total number of substitutions made (for diagnostic logging). Already
// patched / sentinel-free argv is tolerated (returns 0).
func PatchMainArgsBand(args []string, bandY, bandH int) int {
	yStr := strconv.Itoa(bandY)
	hStr := strconv.Itoa(bandH)
	n := 0
	for i, a := range args {
		if !strings.Contains(a, "__SP_BAND") {
			continue
		}
		c := strings.Count(a, BandYSentinel) + strings.Count(a, BandHSentinel)
		if c == 0 {
			continue
		}
		a = strings.ReplaceAll(a, BandYSentinel, yStr)
		a = strings.ReplaceAll(a, BandHSentinel, hStr)
		args[i] = a
		n += c
	}
	return n
}
