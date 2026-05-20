package main

// Agent-side band resolution + main-argv patching.
//
// The pre-render's bottom-band height is data-driven (parsed from the
// SRT). For SIDECAR SRT the file is on disk at rewrite time, but for
// EMBEDDED SRT the agent's own extraction step writes the file later.
// To unify both, the rewriter emits a sentinel y-offset in the main
// argv's overlay_vaapi clause and sets SubPrerenderSpec.ResolveBandPostExtract.
// The agent then:
//  1. resolves the subtitle file (sidecar path or extracted .srt),
//  2. calls resolveSRTBand to pick band height + y-offset,
//  3. writes the chosen y back into the main argv via PatchMainArgsBandY.
// Pre-render argv is built post-resolve from the now-final spec.BandHeight,
// so no in-place patch is needed on that side.

import (
	"strconv"
	"strings"
)

// BandYSentinel is the literal token the rewriter writes into the main
// argv where overlay_vaapi's y= value belongs when band resolution is
// deferred to the agent. Format: `overlay_vaapi=x=0:y=__SP_BANDY__:...`.
// Underscore-prefixed double-underscore form keeps it well outside any
// ffmpeg argv token namespace (numbers, expressions, named consts).
const BandYSentinel = "__SP_BANDY__"

// ResolveAgentBand picks the final band height for an SRT-text spec
// whose rewrite-time band was a placeholder. Mutates spec.BandHeight
// when a tighter band is found, leaves it alone (= the static fallback)
// otherwise. Returns the resolved y-offset (Height - BandHeight) that
// the caller patches into the main argv.
//
// Embedded subs use the extracted .srt path; sidecar subs use the spec's
// own SourcePath. Both converge here. Non-SRT codecs / Bitmap path /
// specs without ResolveBandPostExtract are no-ops and the existing
// BandHeight is returned untouched.
func ResolveAgentBand(spec *SubPrerenderSpec, subFile string) int {
	if spec == nil || !spec.ResolveBandPostExtract || spec.Bitmap {
		if spec == nil {
			return 0
		}
		return spec.Height - spec.BandHeight
	}
	if spec.Height <= 0 || subFile == "" {
		return spec.Height - spec.BandHeight
	}
	if tight, ok := resolveSRTBand(subFile, spec.Height, spec.BandHeight); ok {
		spec.BandHeight = tight
	}
	return spec.Height - spec.BandHeight
}

// PatchMainArgsBandY replaces every BandYSentinel in args with the
// supplied integer y value. The rewriter only ever writes the sentinel
// inside the -filter_complex string for the SRT sub-prerender path, so
// in practice exactly one slot is patched. Returns the count of
// substitutions for diagnostic logging.
func PatchMainArgsBandY(args []string, bandY int) int {
	if len(args) == 0 {
		return 0
	}
	repl := strconv.Itoa(bandY)
	n := 0
	for i, a := range args {
		if !strings.Contains(a, BandYSentinel) {
			continue
		}
		c := strings.Count(a, BandYSentinel)
		if c == 0 {
			continue
		}
		args[i] = strings.ReplaceAll(a, BandYSentinel, repl)
		n += c
	}
	return n
}
