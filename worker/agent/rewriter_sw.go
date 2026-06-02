// rewriter_sw — pure-software (CPU) reshape, the swDialect code path
// (scaleplex#77 PR3). A no-GPU worker downgrades ANY incoming argv (foreign HW,
// HW-decode+SW-encode hybrid, or already-SW) to a CPU pipeline, so the
// honor-source path downstream keeps it. The emitted filtergraph matches Plex's
// own captured full-SW shape (plex-test honor-SW sessions) for drop-in parity:
//
//	[0:0]scale=w=W:h=H:force_divisible_by=4[a];[a][swtonemap,]?format=pix_fmts=yuv420p|nv12[b];[b]inlineass=PARAMS?[out]

package main

import (
	"fmt"
	"strconv"
	"strings"
)

// swDecoderForShort maps a bare short codec (Plex's HW-decode hint) to the SW
// decoder Plex actually uses for CPU decode, captured from plex-test full-SW
// sessions: av1→libdav1d (the native "av1" name is libaom/ambiguous; Plex +
// jellyfin-ffmpeg use libdav1d). hevc and h264 are DELIBERATELY absent — their
// bare names ARE the native SW decoders ffmpeg accepts (Plex's full-SW argvs use
// bare `hevc`/`h264`), so converting them (e.g. to a non-existent `libhevc`)
// breaks ffmpeg. So only av1 needs a rename.
var swDecoderForShort = map[string]string{
	"av1": "libdav1d",
}

// swTonemapNode is Plex's stock CPU HDR→SDR tonemap node, captured verbatim
// from a plex-test software session (HardwareAcceleratedCodecs=0 +
// TranscoderToneMapping=1, client forced to SDR): `format=p010,tonemap=<algo>`.
// The algo is the server's TranscoderToneMapAlgorithm (mobius/hable/…), carried
// through from the source argv (facts.algo). composeBurnSW inserts this between
// the main scale and the final pix_fmts format, exactly as Plex does. Only
// emitted when the source argv declared a tonemap (burnSpec.hdr) — Plex omits it
// when the client advertises HDR (passthrough), and so do we.
func swTonemapNode(algo string) string {
	if !validTonemapAlgo(algo) {
		algo = "hable"
	}
	return "format=p010,tonemap=" + algo
}

// swTonemapChain is the swDialect.tonemapFilter return (interface completeness;
// NOT on the composeBurnSW path, which builds the node inline).
func swTonemapChain(algo, pix string) string {
	return swTonemapNode(algo) + ",format=" + pix
}

// composeBurnSW is the pure-software analog of composeBurn, emitting Plex's
// stock CPU filtergraph shapes verbatim (captured from plex-test software
// sessions) so the fork sees stock-Plex args — no SW-specific patch needed.
// [0:0] is a CPU frame (SW decode), so there's no hwupload / HW surface.
//
//	SDR no-sub:  [0:0]scale=W:H:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1]
//	HDR:         …[0];[0]format=p010,tonemap=ALGO[1];[1]format=pix_fmts=…[2]
//	text-sub:    …[fmt];[fmt]inlineass=PARAMS[out]
//	bitmap-sub:  [0:N]scale=W:H[s];[0:0]scale=…[v];[v]…format…[fmt];[fmt][s]overlay[out]
//
// vaResident + animatedTierDown are ignored (CPU; Plex flattens animated ASS to
// an SRT sidecar → the text/inlineass path).
func (tm tonemapConfig) composeBurnSW(s burnSpec) (filter, newLabel string) {
	n := 0
	next := func() string { l := strconv.Itoa(n); n++; return l }
	var b strings.Builder

	// Bitmap sub: the image stream is sub2video-scaled to output size UP FRONT
	// (gets label 0, matching Plex's node ordering), overlaid after the main
	// chain.
	bitmap := s.burnSub && s.subKind == "bitmap"
	subLabel := ""
	if bitmap {
		subLabel = next()
		fmt.Fprintf(&b, "[%s]scale=%s:%s[%s];", s.subSpec, s.w, s.h, subLabel)
	}

	scaled := next()
	fmt.Fprintf(&b, "[0:0]scale=w=%s:h=%s:force_divisible_by=4[%s]", s.w, s.h, scaled)
	cur := scaled

	if s.hdr {
		tmL := next()
		fmt.Fprintf(&b, ";[%s]%s[%s]", cur, swTonemapNode(s.algo), tmL)
		cur = tmL
	}

	fmtL := next()
	fmt.Fprintf(&b, ";[%s]format=pix_fmts=yuv420p|nv12[%s]", cur, fmtL)
	cur = fmtL

	if s.burnSub {
		o := next()
		if bitmap {
			fmt.Fprintf(&b, ";[%s][%s]overlay[%s]", cur, subLabel, o)
		} else {
			// Text: Plex's inlineass with its font params verbatim (no
			// render_height — that's a HW-band optimization).
			fmt.Fprintf(&b, ";[%s]inlineass=%s[%s]", cur, s.subParams, o)
		}
		cur = o
	}
	return b.String(), "[" + cur + "]"
}

// reshapeToSoftware downgrades an incoming argv to a pure-CPU pipeline for a
// swDialect (no-GPU) worker. After it the argv is SW-shaped, so the honor-source
// path keeps it (the SW encoderMap identity makes plexSWEncoder true, and the
// stripped hwaccel makes noHwaccel true). No Plex-Pass gate applies — downgrading
// HW→SW grants no entitlement.
//
// Steps:
//  1. Strip HW decode flags + HW device init.
//  2. Filter graph: a foreign HW graph (scale_vaapi/scale_cuda/hwupload/tonemap_*)
//     → extractGraphFacts → composeBurnSW. An already-SW `[0:0]scale=w=` graph is
//     left untouched (Plex's own SW shape; honor-source keeps it). Both text AND
//     bitmap subs are reshaped: extractGraphFacts derives bitmap subKind/subSpec
//     from the overlay_vaapi|overlay_cuda branch (no probe needed, so the nil
//     subSrc arg is fine), and composeBurnSW emits text→inlineass / bitmap→stock
//     `overlay` (Plex's SW PGS shape). Unlike the HW cross-backend path, bitmap
//     is NOT deferred here.
//  3. Encoder: foreign HW encoder → libx264/libx265.
//
// Decoder stays: a bare short codec or libdav1d decodes on the CPU once the
// hwaccel flags are gone.
func reshapeToSoftware(args []string, tm tonemapConfig) ([]string, []string) {
	src := detectSourceBackend(args) // before stripping, for the tag
	var changes []string

	// 1. Strip HW decode flags + device init (all occurrences). Emit a change
	// whenever anything is stripped so the caller keeps the reshaped args (its
	// len(changes)>0 guard) — a HW-decode hybrid whose filter+encoder are
	// already SW would otherwise produce no change tag, the strip would be
	// discarded, and the HW-decode flags would survive onto a no-GPU worker.
	stripped := false
	// Stream-specified HW-decode flags (`:0` / `:#0xNN` / `:v:0`) — match the
	// video-stream spec in ANY form (#145). PMS emits `-hwaccel:#0xNN` on
	// high-PID containers; an exact "-hwaccel:0" match would miss it, leave the
	// hwaccel flags on the SW argv, and the honor-source path downstream would
	// then see noHwaccel=false and bail unknown-decoder instead of keeping the
	// SW reshape.
	for _, base := range []string{"-hwaccel", "-hwaccel_output_format", "-hwaccel_device"} {
		for {
			i := streamSpecIndex(args, base, 0, 0)
			if i < 0 || i+1 >= len(args) {
				break
			}
			args = removeArgs(args, i, 2)
			stripped = true
		}
	}
	// Device-init flags carry no stream specifier — exact name.
	for _, flag := range []string{"-init_hw_device", "-filter_hw_device"} {
		for {
			i := indexOfArg(args, flag, 0)
			if i < 0 || i+1 >= len(args) {
				break
			}
			args = removeArgs(args, i, 2)
			stripped = true
		}
	}
	if stripped {
		changes = append(changes, TagToSWStripHWDecode)
	}

	// 1b. Decoder: once hwaccel is gone, Plex's bare short-codec HW-decode hint
	// (`hevc`/`av1`/`h264`) must become its SW decoder lib (`libhevc`/`libdav1d`/
	// `libx264`) — both so it decodes on the CPU and so it matches Plex's stock
	// SW decoder shape (the rewriter's decoder phase + the drop-in goal). An
	// already-SW decoder lib (Plex emitted SW) is left untouched. The decoder
	// slot is the first `-codec:0`, before `-i`.
	if dc := streamSpecIndex(args, "-codec", 0, 0); dc >= 0 && dc+1 < len(args) {
		if in := indexOfArg(args, "-i", 0); in < 0 || dc < in {
			if sw, ok := swDecoderForShort[args[dc+1]]; ok {
				changes = append(changes, TagPrefixToSWDecode+args[dc+1]+"->"+sw)
				args[dc+1] = sw
			}
		}
	}

	// 2. Filter graph → SW (foreign HW graphs only; an already-SW `[0:0]scale=w=`
	// graph is Plex's own shape — honor-source keeps it). Text + bitmap both
	// handled (composeBurnSW emits inlineass / sub2video→overlay respectively).
	if vfIdx := filterComplexIndex(args); vfIdx >= 0 && !isSWFilterGraph(args[vfIdx]) {
		facts := extractGraphFacts(args[vfIdx], nil)
		if facts.ok {
			oldLabel := ""
			if m := reGraphTrailingLabel.FindStringSubmatch(args[vfIdx]); m != nil {
				oldLabel = "[" + m[1] + "]"
			}
			nf, nl := tm.composeBurnSW(burnSpec{
				w:         facts.w,
				h:         facts.h,
				hdr:       facts.hdr,
				algo:      facts.algo,
				burnSub:   facts.subKind != "",
				subParams: facts.subParams,
				subKind:   facts.subKind,
				subSpec:   facts.subSpec,
			})
			nf, nl = appendSelectStage(nf, nl, facts.selectExpr)
			args[vfIdx] = nf
			retargetMapLabel(args, oldLabel, nl)
			changes = append(changes, TagPrefixToSWFilter+composeMode(facts))
		}
	}

	// 3. Encoder → SW (h264_vaapi/h264_nvenc → libx264, hevc_* → libx265).
	if in := indexOfArg(args, "-i", 0); in >= 0 {
		if e := streamSpecIndex(args, "-codec", 0, in+1); e >= 0 && e+1 < len(args) {
			if codec, isHW := hwEncoderCodec[args[e+1]]; isHW {
				if lib, ok := codecCanonicalSWEncoder[codec]; ok {
					changes = append(changes, TagPrefixToSWEncode+args[e+1]+"->"+lib)
					args[e+1] = lib
				}
			}
		}
	}

	if len(changes) > 0 {
		changes = append([]string{TagPrefixToSW + string(src)}, changes...)
	}
	return args, changes
}
