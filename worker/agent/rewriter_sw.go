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

// swTonemapBody is the CPU HDR(bt2020 PQ/HLG)→SDR(bt709) tonemap chain, WITHOUT
// a trailing format node (composeBurnSW appends Plex's `format=pix_fmts=...`).
// Standard jellyfin zscale→tonemap→zscale form.
//
// NOTE: CONSTRUCTED — plex-test never emits a pure-SW HDR chain (its PMS
// HW-decodes HDR even when SW-encoding), so this isn't corpus-matched yet.
// Pending a real capture (flip plex-test PMS HardwareAcceleratedCodecs=0 + play
// a 4K HDR title) to replace with Plex's genuine shape.
func swTonemapBody(algo string) string {
	if !validTonemapAlgo(algo) {
		algo = "hable"
	}
	return "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709," +
		"tonemap=tonemap=" + algo + ":desat=0," +
		"zscale=t=bt709:m=bt709:r=tv"
}

// swTonemapChain is swTonemapBody with the trailing format — the form
// swDialect.tonemapFilter returns for interface completeness (not on the
// composeBurnSW path).
func swTonemapChain(algo, pix string) string {
	return swTonemapBody(algo) + ",format=" + pix
}

// composeBurnSW is the pure-software analog of composeBurn. [0:0] is a CPU frame
// (SW decode), so there's no hwupload / HW surface. Emits Plex's captured SW
// shape: SW scale → [SW tonemap]? → format=pix_fmts=yuv420p|nv12 → [inlineass]?.
// vaResident is ignored (always sysmem). Bitmap subs are NOT handled here (the
// caller defers them, mirroring the HW cross-backend bitmap deferral, #76).
func (tm tonemapConfig) composeBurnSW(s burnSpec) (filter, newLabel string) {
	n := 0
	next := func() string { l := strconv.Itoa(n); n++; return l }
	var b strings.Builder

	scaled := next()
	fmt.Fprintf(&b, "[0:0]scale=w=%s:h=%s:force_divisible_by=4[%s]", s.w, s.h, scaled)

	fmtLabel := next()
	if s.hdr {
		fmt.Fprintf(&b, ";[%s]%s,format=pix_fmts=yuv420p|nv12[%s]", scaled, swTonemapBody(s.algo), fmtLabel)
	} else {
		fmt.Fprintf(&b, ";[%s]format=pix_fmts=yuv420p|nv12[%s]", scaled, fmtLabel)
	}
	out := fmtLabel

	if s.burnSub {
		o := next()
		// Plex's SW inlineass carries only its font params (no render_height —
		// that's a HW-band optimization). Preserve Plex's params verbatim.
		fmt.Fprintf(&b, ";[%s]inlineass=%s[%s]", fmtLabel, s.subParams, o)
		out = o
	}
	return b.String(), "[" + out + "]"
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
//     left untouched (Plex's own SW shape; honor-source keeps it). Bitmap subs
//     are deferred (kept as-is) — SW bitmap burn is out of scope here (#76).
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
	for _, flag := range []string{
		"-hwaccel:0", "-hwaccel_output_format:0", "-hwaccel_device:0",
		"-init_hw_device", "-filter_hw_device",
	} {
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

	// 2. Filter graph → SW (foreign HW graphs only; SW graphs are kept).
	if vfIdx := filterComplexIndex(args); vfIdx >= 0 && !isSWFilterGraph(args[vfIdx]) {
		facts := extractGraphFacts(args[vfIdx], nil)
		if facts.ok && facts.subKind != "bitmap" {
			oldLabel := ""
			if m := reGraphTrailingLabel.FindStringSubmatch(args[vfIdx]); m != nil {
				oldLabel = "[" + m[1] + "]"
			}
			nf, nl := tm.composeBurnSW(burnSpec{
				w:         facts.w,
				h:         facts.h,
				hdr:       facts.hdr,
				algo:      facts.algo,
				burnSub:   facts.subKind == "text",
				subParams: facts.subParams,
			})
			args[vfIdx] = nf
			retargetMapLabel(args, oldLabel, nl)
			changes = append(changes, TagPrefixToSWFilter+composeMode(facts))
		}
	}

	// 3. Encoder → SW (h264_vaapi/h264_nvenc → libx264, hevc_* → libx265).
	if in := indexOfArg(args, "-i", 0); in >= 0 {
		if e := indexOfArg(args, "-codec:0", in+1); e >= 0 && e+1 < len(args) {
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
