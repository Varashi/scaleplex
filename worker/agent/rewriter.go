package main

// argRewriter — rewrite Plex SW transcode argv into a stock-ffmpeg VAAPI
// invocation. Ported from clusterplex/orchestrator/argRewriter.js. The Go
// port runs on the worker (where /media is locally mounted) instead of on
// the orchestrator, so sidecar SRT/ASS lookups happen here.
//
// Conservative: only rewrites a known argv shape. Anything unfamiliar is
// returned untouched with applied=false. The caller decides whether to
// spawn ffmpeg with the rewritten or the original arg list.

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type RewriteResult struct {
	Args    []string
	Env     map[string]string
	Applied bool
	Changes []string

	// ProgressURL — when non-empty, the agent must run a worker-side
	// progress reporter against this URL instead of letting ffmpeg
	// drive `-progress`. Captured from Plex's `-progressurl` arg with
	// the loopback rewritten to a worker-reachable address and the
	// per-session X-Plex-Token appended as a query param. Plex's PUT
	// handler parses each request body as a discrete payload — stock
	// ffmpeg's chunked-stream `-progress` confuses it and stalls
	// `/header` for ~120s. The reporter sends one PUT per ffmpeg
	// progress block instead.
	ProgressURL string
	// SeekOffsetSeconds — value captured from Plex's `-ss N` argv on a
	// seek session, used by the renumber watcher to patch each chunk's
	// `tfdt` (track-fragment-decode-time) box. Stock dashenc writes
	// tfdt=0 in seek-session chunks regardless of -ss/-copyts/+cmaf,
	// which makes Plex Web's MSE place the chunks at timeline 0 instead
	// of <off>+. We post-process by adding `SeekOffsetSeconds * tfdt's
	// own timescale` to each tfdt baseMediaDecodeTime + sidx
	// earliest_presentation_time after rename. Zero on initial play.
	SeekOffsetSeconds float64
	// IsMatroskaSegment — true when output shape is `-f segment
	// -segment_format matroska` (Plex Windows desktop, segmented
	// matroska delivered via /transcode/universal/start byte stream).
	// The agent uses this to start `watchAndPatchMatroskaChunks`
	// instead of the DASH-style chunk-renumber watcher.
	IsMatroskaSegment bool
}

// RewriteOpts is for testability; production callers pass nil.
type RewriteOpts struct {
	FSExists func(string) bool
	// SessionDir — Plex's per-session transcode dir (the agent's
	// req.Cwd). Vestigial since the pre-render/extraction path was
	// removed (v1.3.0): subtitles now feed incrementally via the fork's
	// -map_inlineass binding, with no on-disk extraction. Kept for API
	// shape; detectSubtitleSource discards it.
	SessionDir string
	// ProbeSubtitleCodec — when non-nil, the rewriter calls this to
	// learn the codec_name of the subtitle stream Plex's
	// -map_inlineass references. The result picks between two burn-in
	// chains: text (subrip/ass/mov_text/...) → keep Plex's `inlineass=`
	// filter (Phase 2c hardcoded passthrough; fork's vf_inlineass
	// binding renders via libass); bitmap (hdmv_pgs_subtitle/
	// dvb_subtitle/...) → `overlay_vaapi` stream-overlay chain.
	// Args: source file path, stream specifier (e.g. "0:3", "1:s:0").
	// Returns lowercase codec name or "" on probe failure (treated as
	// text by default). Production agent wires this to a synchronous
	// ffprobe; tests inject a fake.
	ProbeSubtitleCodec func(source, streamSpec string) string
	// ProbeVideoColor — when non-nil, returns the source video's color
	// metadata. The rewriter uses `transfer` to detect HDR sources
	// (smpte2084 = HDR10 PQ; arib-std-b67 = HLG) and injects
	// `tonemap_vaapi` into the filter chain when Plex's argv targets
	// SDR but the source is HDR. Without this, HDR sources sent to
	// SDR clients render with washed colors (PQ values get crammed
	// into BT.709 range without tonemapping). Production agent wires
	// to ffprobe; tests inject fakes.
	ProbeVideoColor func(source string) (transfer, primaries, space string)
}

var decoderMap = map[string]string{
	"libdav1d": "av1",
	"libhevc":  "hevc",
	"libx264":  "h264",
}

// hwDecodeShortCodecs is the set of bare codec names PMS emits in the
// `-codec:0` slot when its HW probe succeeded and it wants the worker
// to use VAAPI hwaccel for decode. Distinct from decoderMap (which
// holds Plex software-decoder names like libdav1d) — when PMS sends
// one of these alongside `-hwaccel:0 vaapi`, the rest of the argv
// (filter chain, encoder, CQP) is already VAAPI-shaped and we
// pass it through.
var hwDecodeShortCodecs = map[string]struct{}{
	"av1":  {},
	"hevc": {},
	"h264": {},
	"vp9":  {},
}

// libx264 preset -> VAAPI compression_level (iHD TargetUsage) mapping
// moved into the fork's VAAPI encoder (patch 0118); the rewriter leaves
// `-preset` untouched. The SCALEPLEX_PRESET_MAP env override is retired.

// subRenderHeightDefault — the shipped default render-height cap. At 4K
// it renders the sub band at 1920x1080 and HW-upscales 2× to the output
// band: ~4.25× less pre-render CPU than full-4K libass, glyph edges only
// marginally softer (validated live + A/B against native render). 1080p
// and smaller outputs are unaffected (cap >= output height = native).
const subRenderHeightDefault = 1080

// subRenderHeightCap reads SCALEPLEX_SUB_RENDER_HEIGHT — the maximum
// height (px) at which the subtitle pre-render rasterises libass. The
// pre-render then renders the band at that lower resolution and the main
// graph HW-upscales (scale_vaapi) it back to the output band before
// overlay_vaapi. libass + qtrle cost scale ~linearly with rendered
// pixel area, so capping the render height cuts the pre-render CPU
// proportionally (and the FIFO + main-side hwupload bytes) at the price
// of softer glyph edges after the upscale. Documented tiers: 720, 1080,
// 1440. Unset = the 1080 default; "0" (or any non-positive value) opts
// out to a native full-resolution render. A cap >= the output height is
// a no-op (native) anyway. See project_scaleplex_libass_4k_render_cost.
func subRenderHeightCap() int {
	v := os.Getenv("SCALEPLEX_SUB_RENDER_HEIGHT")
	if v == "" {
		return subRenderHeightDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// encoderMap routes PMS's chosen software encoder to its VAAPI
// equivalent. PMS picks libx264 vs libx265 per session based on its
// own server prefs (TranscoderHEVCEncodingMode) and client capability
// negotiation; the worker tracks that choice rather than forcing one
// codec, so the DASH/HLS manifest's codec_string already matches the
// output stream.
var encoderMap = map[string]string{
	"libx264": "h264_vaapi",
	"libx265": "hevc_vaapi",
}

// All per-shape reFilter* regexes (reFilterAss / reFilterPlain / reFilterHDR /
// reFilterHDRAss for SW-decode + reFilterHWAss / reFilterHWOpenCLAss for
// HW-decode-text) collapsed into extractGraphFacts + composeBurn — see
// rewriteVideoFilter (SW-reshape entrypoint) and the HW-decode-text branch of
// Rewrite(). Plex's HW-decode bitmap (PGS/VobSub/DVDSub) burn graph —
// `[0:N]scale,hwupload; [0:0]hwupload; scale_vaapi; [..][..]overlay_vaapi`
// with an optional tonemap spliced in on the HDR variant — is recognized by
// detectBitmapOverlayBurn (fact extraction, not a rigid shape regex) and
// recomposed onto the unified inlineass burn by composeBurn.

func envOr(k, dflt string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return dflt
}

// envBool reads a boolean env var (1/true/yes/on, case-insensitive).
// Unset/empty/anything else = false.
func envBool(k string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// stripPlexInlineassFilterArgs was removed in patch 0119: the fork's
// vf_inlineass now parses Plex's `overrides`/`outline`/`shadow`/`language`
// keys directly (ass_set_style_overrides), so the rewriter passes Plex's
// inlineass node through verbatim on every path (HW reshape + SW/honor)
// and the user's subtitle styling is preserved instead of dropped.

// assStyleKeys is the subset of ASS [V4+ Styles] field names accepted
// in a `subtitles` filter force_style= value. Plex's inlineass
// `overrides=` list mixes these with script-info keys (e.g.
// ScaledBorderAndShadow) that force_style ignores — keep only the
// real style fields.
var assStyleKeys = map[string]bool{
	"FontName": true, "FontSize": true, "Bold": true, "Italic": true,
	"Underline": true, "StrikeOut": true, "PrimaryColour": true,
	"SecondaryColour": true, "OutlineColour": true, "BackColour": true,
	"BorderStyle": true, "Outline": true, "Shadow": true,
	"Alignment": true, "MarginL": true, "MarginR": true, "MarginV": true,
	"ScaleX": true, "ScaleY": true, "Spacing": true, "Angle": true,
}

// plexInlineassToForceStyle translates the parameters of Plex's
// `inlineass=` filter into a `subtitles` filter force_style= value so
// the pre-rendered overlay reproduces Plex's burn-in styling. Plex's
// top-level keys font_size / outline / shadow map onto the ASS style
// fields FontSize / Outline / Shadow; the `overrides=` sub-list
// already carries ASS style fields and is passed through (style
// fields only). font_scale / font_path / fontconfig_file / language
// have no force_style equivalent and are dropped.
//
// `params` is everything after `inlineass=` — top-level pairs are
// `:`-separated and no value contains a top-level `:` (verified across
// the argv corpus; same assumption as stripPlexInlineassFilterArgs).
// Returns "" when nothing usable was found.
func plexInlineassToForceStyle(params string) string {
	var pairs []string
	for _, p := range strings.Split(params, ":") {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "font_size":
			pairs = append(pairs, "FontSize="+kv[1])
		case "outline":
			pairs = append(pairs, "Outline="+kv[1])
		case "shadow":
			pairs = append(pairs, "Shadow="+kv[1])
		case "overrides":
			for _, ov := range strings.Split(kv[1], ",") {
				okv := strings.SplitN(ov, "=", 2)
				if len(okv) == 2 && assStyleKeys[okv[0]] {
					pairs = append(pairs, okv[0]+"="+okv[1])
				}
			}
		}
	}
	return strings.Join(pairs, ",")
}

func indexOfArg(args []string, key string, from int) int {
	for i := from; i < len(args); i++ {
		if args[i] == key {
			return i
		}
	}
	return -1
}

// streamIDOrdinalMap builds the `#0xNN`-id → ordinal map PMS's stream-by-id
// argv implies: each distinct hex id, in first-occurrence order across the argv
// (a flag suffix like `-codec:#0x01` or a filtergraph ref `[0:#0x01]`), maps to
// the next ordinal (0, 1, 2, …). This mirrors ffmpeg's by-index assignment for
// PMS's video-then-audio declaration order, so `#0x01` (the first id) resolves
// to ordinal 0 (the video stream). Empty when the argv carries no `:#0x` spec
// (the ordinal-form common case — no allocation). Replaces the upfront
// normalizePlexStreamSpecsToOrdinal rewrite (#145): the rewriter now resolves
// stream specs at match time and leaves PMS's argv pristine (ffmpeg accepts the
// `#0xNN` form natively).
func streamIDOrdinalMap(args []string) map[string]int {
	var m map[string]int
	next := 0
	for _, a := range args {
		for _, s := range streamIDSpecRegex.FindAllString(a, -1) {
			id := s[len(":#"):] // ":#0x01" → "0x01"
			if m == nil {
				m = map[string]int{}
			}
			if _, ok := m[id]; !ok {
				m[id] = next
				next++
			}
		}
	}
	return m
}

// streamSpecSelectsOrdinal reports whether a stream-specifier (the text after
// `-flag:`) selects video ordinal `ord`. Handles the three forms PMS emits:
//
//	"0"      ordinal index directly
//	"#0xNN"  stream-by-id hex — resolved via idMap (streamIDOrdinalMap)
//	"v:0"    type+index — video stream `index` (V = attached-pic excluded; we
//	         only match v/V, since the rewriter keys exclusively on video)
func streamSpecSelectsOrdinal(spec string, ord int, idMap map[string]int) bool {
	if n, err := strconv.Atoi(spec); err == nil {
		return n == ord
	}
	if strings.HasPrefix(spec, "#0x") {
		o, ok := idMap[spec[len("#"):]] // "#0x01" → "0x01", matching idMap keys
		return ok && o == ord
	}
	if t, idx, found := strings.Cut(spec, ":"); found && (t == "v" || t == "V") {
		if n, err := strconv.Atoi(idx); err == nil {
			return n == ord
		}
	}
	return false
}

// streamSpecIndex is indexOfArg for a stream-specified flag: it finds the index
// of `-<flagBase>:<spec>` (from `start`) whose specifier selects video ordinal
// `ord`, accepting the ordinal (`:0`), hex-by-id (`:#0xNN`), and type+index
// (`:v:0`) forms alike. The drop-in replacement for the old
// `indexOfArg(args, "-flagBase:0", start)` ordinal-only matching that tripped on
// PMS's `#0xNN` argv (#144 regression chain; #145 fix). flagBase carries no
// trailing colon — `"-hwaccel"` won't false-match `-hwaccel_output_format:` (the
// required `:` separator rules out the `_`-suffixed siblings).
func streamSpecIndex(args []string, flagBase string, ord, start int) int {
	idMap := streamIDOrdinalMap(args)
	prefix := flagBase + ":"
	for i := start; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, prefix) {
			continue
		}
		if streamSpecSelectsOrdinal(a[len(prefix):], ord, idMap) {
			return i
		}
	}
	return -1
}

func escapeFilterPath(p string) string {
	p = strings.ReplaceAll(p, `\`, `\\`)
	p = strings.ReplaceAll(p, `:`, `\:`)
	p = strings.ReplaceAll(p, `'`, `\'`)
	return p
}

// subtitleKind classifies a codec_name returned by ffprobe into "text"
// (libass-renderable via subtitles= filter) vs "bitmap" (image stream,
// must be overlaid via overlay_vaapi or CPU overlay).
//
// Source: ffmpeg's libavcodec/codec_desc.c. Text formats can be muxed
// to .srt (lossy on ASS, fine on most others); bitmap formats must be
// kept as a stream and composited frame-by-frame.
// isHDRTransfer returns true for HDR10 (PQ / SMPTE2084) and HLG
// (ARIB STD-B67) color transfer characteristics. Both require a
// tonemap pass when the encoder targets an SDR (BT.709) output.
func isHDRTransfer(transfer string) bool {
	switch strings.ToLower(transfer) {
	case "smpte2084", "smpte428", "arib-std-b67":
		return true
	}
	return false
}

func subtitleKind(codec string) string {
	switch strings.ToLower(codec) {
	case "subrip", "srt", "ass", "ssa", "mov_text", "tx3g",
		"webvtt", "vtt", "microdvd", "jacosub", "sami",
		"realtext", "stl", "subviewer", "hdmv_text_subtitle":
		return "text"
	case "hdmv_pgs_subtitle", "pgssub", "pgs",
		"dvb_subtitle", "dvbsub",
		"dvd_subtitle", "dvdsub",
		"xsub":
		return "bitmap"
	}
	return "unknown"
}

// assAnimationTags are the ASS override tags whose visual effect
// changes within a single cue's on-screen lifetime: karaoke fills
// (\k family), transforms (\t), movement (\move) and fades (\fad /
// \fade). A cue carrying any of these cannot be pre-rasterized to one
// static bitmap — it must keep the per-frame libass `inlineass` path.
var assAnimationTags = []string{`\k`, `\K`, `\t(`, `\move(`, `\fad`}

// subtitleIsAnimated reports whether a text subtitle needs per-frame
// rendering (animated ASS) rather than the once-per-cue pre-render
// overlay path.
//
//   - SRT/SubRip carries no override tags — never animated.
//   - ASS/SSA is scanned for animation override tags. When the file
//     can't be read (embedded stream, or a read error) the answer is
//     conservatively true, so the session keeps the safe inlineass
//     path instead of silently dropping karaoke / motion.
//   - Other text codecs have no animation model — treated as static.
func subtitleIsAnimated(codec, filePath string, readFile func(string) ([]byte, error)) bool {
	switch strings.ToLower(codec) {
	case "subrip", "srt":
		return false
	case "ass", "ssa":
		if filePath == "" || readFile == nil {
			return true
		}
		data, err := readFile(filePath)
		if err != nil {
			return true
		}
		body := string(data)
		for _, tag := range assAnimationTags {
			if strings.Contains(body, tag) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// SubtitleSource is the result of analysing -map_inlineass + -i argv.
type subtitleSource struct {
	// Kind: "text" or "bitmap" — picked by the codec probe. Empty
	// when the rewriter couldn't detect a burn-in request.
	Kind string
	// Codec: the raw codec_name reported by ffprobe (e.g. "subrip",
	// "hdmv_pgs_subtitle"). Empty if no probe ran or it failed.
	Codec string
	// StreamSpec: the value ffmpeg's -map_inlineass referenced
	// (e.g. "0:3", "1:s:0"). Required for filter-graph stream
	// references on the bitmap path.
	StreamSpec string
	// FilePath (text-sidecar path only): Plex's pre-staged temp file.
	// Empty for embedded text — the fork's scaleplex_inlineass binding
	// owns the stream directly.
	FilePath string
	// SecondInputArgIdx — offset of the second `-i` in args.
	// Caller drops the input pair (-1 when only one -i, or for
	// bitmap-sidecar where we KEEP the input — the filter graph
	// still consumes the stream).
	SecondInputArgIdx int
}

// detectSubtitleSource inspects argv for `-map_inlineass <spec>` and
// resolves the burn-in source.
//
// Plex emits four shapes:
//
//  1. Embedded text (SRT/ASS in mkv):
//     -i source.mkv
//     -map_inlineass 0:3              (codec=subrip|ass|...)
//     → text path, fork's scaleplex_inlineass reads the stream directly.
//
//  2. External text sidecar (Plex pre-stages temp-0.srt):
//     -i source.mkv
//     -i /transcode/.../temp-0.srt
//     -map_inlineass 1:s:0            (codec=subrip|ass|...)
//     → text path, fork's scaleplex_inlineass reads input 1.
//
//  3. Embedded bitmap (PGS/VobSub/DVDSub):
//     -i source.mkv
//     -map_inlineass 0:3              (codec=hdmv_pgs_subtitle|...)
//     → bitmap path, filter graph references [0:3] via overlay_vaapi.
//
//  4. External bitmap sidecar (rare; .sup files):
//     -i source.mkv
//     -i sidecar.sup
//     -map_inlineass 1:s:0            (codec=hdmv_pgs_subtitle|...)
//     → bitmap path, KEEP second -i (filter pulls the stream from it).
//
// When opts.ProbeSubtitleCodec is non-nil, the codec probe runs and
// Kind is populated. Without it, Kind defaults to "text" — the
// production agent always wires up the probe; tests can override.
func detectSubtitleSource(args []string, sessionDir string, probe func(source, streamSpec string) string) *subtitleSource {
	miaIdx := indexOfArg(args, "-map_inlineass", 0)
	if miaIdx < 0 || miaIdx+1 >= len(args) {
		return nil
	}
	streamSpec := args[miaIdx+1]

	var inputArgIdxs []int
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" {
			inputArgIdxs = append(inputArgIdxs, i)
		}
	}
	if len(inputArgIdxs) == 0 {
		return nil
	}

	inputNum := 0
	if colon := strings.Index(streamSpec, ":"); colon > 0 {
		if n, err := strconv.Atoi(streamSpec[:colon]); err == nil {
			inputNum = n
		}
	}
	if inputNum >= len(inputArgIdxs) {
		return nil
	}

	srcForProbe := args[inputArgIdxs[inputNum]+1]
	// streamSpec is in PMS-argv terms (e.g. "1:s:0" — second input file,
	// sub stream 0). For sidecar inputs the probed file is the sidecar
	// itself, which ffprobe sees as the only input. ffprobe REJECTS a
	// file-index prefix in -select_streams when only one input is given
	// ("Invalid stream specifier: 0:s:0") — drop the leading "N:" entirely
	// and pass just the type+index portion (e.g. "s:0").
	probeSpec := streamSpec
	if inputNum > 0 {
		if colon := strings.Index(probeSpec, ":"); colon > 0 {
			probeSpec = probeSpec[colon+1:]
		}
	}
	codec := ""
	if probe != nil {
		codec = strings.ToLower(probe(srcForProbe, probeSpec))
	}
	kind := subtitleKind(codec)
	if kind == "unknown" {
		// No probe (test path) or probe failed. Default to text — the
		// common case; a bitmap mis-detected as text fails loud at the
		// fork's inlineass decode-sink (operator still gets a signal).
		kind = "text"
	}

	res := &subtitleSource{
		Kind:              kind,
		Codec:             codec,
		StreamSpec:        streamSpec,
		SecondInputArgIdx: -1,
	}

	if inputNum == 0 {
		// Embedded — text or bitmap. No file path: text routes through
		// the fork's scaleplex_inlineass binding (reads the stream
		// directly via -map_inlineass); bitmap routes through
		// overlay_vaapi (references [streamSpec] in the filter graph).
		_ = sessionDir
		return res
	}

	// External (second -i).
	res.SecondInputArgIdx = inputArgIdxs[inputNum]
	if kind == "text" {
		// Use the staged temp file directly. Drop the second -i.
		res.FilePath = args[inputArgIdxs[inputNum]+1]
	} else {
		// Bitmap sidecar: KEEP the second -i — overlay_vaapi pulls
		// the stream from it. SecondInputArgIdx stays set so the
		// caller can know about it (e.g. for diagnostics) but the
		// caller skips the drop step.
		res.SecondInputArgIdx = -1
	}
	return res
}

type filterRewrite struct {
	Filter   string
	OldLabel string
	NewLabel string
	Mode     string
}

// rewriteVideoFilter translates Plex's filter graph into a scaleplex-ffmpeg7
// equivalent. subSrc, when non-nil, carries the resolved subtitle
// burn-in source (text path or bitmap stream); the function picks the
// matching filter shape. See detectSubtitleSource for source resolution.
//
// scaleplex never injects a tonemap of its own: HDR→SDR tone mapping
// happens iff Plex's argv carries a tonemap filter (reFilterHDR /
// reFilterHDRAss / the tonemap_opencl chain). sourceIsHDR only labels
// the diagnostic Mode string.

// tonemapConfig is the resolved HDR→SDR tone-mapping backend policy.
// scaleplex does NOT decide whether to tonemap — Plex does: if Plex's
// argv carries a tonemap filter, scaleplex honors it; if it doesn't
// (Plex's "Use hardware-accelerated tone mapping" pref is off — Plex
// then does no tonemapping at all), scaleplex doesn't either. This
// config only picks the backend for the tonemap Plex DID ask for:
//   - SCALEPLEX_TONEMAP (worker-pod env): `opencl` (default) vs `vaapi`
//     fixed-curve.
//   - algo: SCALEPLEX_TONEMAP_ALGO operator override → `hable` — used
//     only by the SW-chain reshapes (reFilterHDR/HDRAss) where the
//     algorithm isn't carried through; substituteOpenCLTonemap keeps
//     the algorithm straight off Plex's own chain.
type tonemapConfig struct {
	useOpenCL bool   // VAAPI only: false = iHD fixed-curve tonemap_vaapi
	algo      string // fallback algorithm for the SW-chain reshapes
	// d is the worker's HW backend. nil = vaapiDialect{} fallback (kept
	// for test ergonomics — tests construct tonemapConfig{...} literals
	// without specifying d and expect VAAPI shapes).
	d dialect
}

// backend returns tm.d with a VAAPI fallback for tests that construct
// tonemapConfig literals without setting d.
func (tm tonemapConfig) backend() dialect {
	if tm.d == nil {
		return vaapiDialect{}
	}
	return tm.d
}

func validTonemapAlgo(a string) bool {
	switch a {
	case "hable", "mobius", "reinhard", "bt2390", "linear", "gamma", "clip":
		return true
	}
	return false
}

// resolveTonemapConfig builds the tone-mapping backend policy from the
// worker-pod env + the active HW dialect. useOpenCL is gated to VAAPI
// — NVIDIA has no OpenCL-derive tonemap path (Plex emits `tonemap_cuda`
// directly and the worker passes it through, see tm.stage).
func resolveTonemapConfig() tonemapConfig {
	cfg := tonemapConfig{
		d:    activeDialect,
		algo: "hable",
	}
	if cfg.d.backendName() == "vaapi" {
		cfg.useOpenCL = !strings.EqualFold(os.Getenv("SCALEPLEX_TONEMAP"), "vaapi")
		// AMD radeonsi: neither backend is usable through the tm.stage()
		// path — tonemap is absorbed into vf_inlineass's AMD-Vulkan branch
		// (fork patch 0127 v6, composeBurnAMDHDR routes around tm.stage).
		// Operators who explicitly opt in to SCALEPLEX_TONEMAP=vaapi on an
		// AMD pod were probably copying Intel runbooks; log a WARN so the
		// "useOpenCL=false but tonemap_vaapi isn't on this driver" mismatch
		// surfaces in logs before they go hunting for a missing stage.
		if vd, ok := cfg.d.(vaapiDialect); ok && vd.isAMD() &&
			strings.EqualFold(os.Getenv("SCALEPLEX_TONEMAP"), "vaapi") {
			log.Printf("WARN: SCALEPLEX_TONEMAP=vaapi has no effect on radeonsi " +
				"(no tonemap_vaapi); HDR→SDR runs through vf_inlineass libplacebo")
		}
	}
	if a := strings.ToLower(os.Getenv("SCALEPLEX_TONEMAP_ALGO")); validTonemapAlgo(a) {
		cfg.algo = a
	}
	return cfg
}

// stage returns the filter fragment that converts an HDR p010 HW
// surface into an SDR (BT.709) nv12 HW surface. It is inserted directly
// after a `scale_*=...:format=p010` step and its output is a same-backend
// surface either way, so it is drop-in wherever a single-stage tonemap
// stood. `algo` overrides the config's algorithm (used when Plex's argv
// carried its own); empty uses cfg.algo. Ignored on VAAPI when useOpenCL
// is false (iHD fixed-curve has no algo slot).
//
//	VAAPI, useOpenCL: hwmap→opencl, tonemap_opencl=<algo>, hwmap→vaapi:reverse=1
//	VAAPI, fixed:     tonemap_vaapi (iHD fixed BT.2390 EETF)
//	NVIDIA:           tonemap_cuda=<algo>:nv12 (algo-driven, no OpenCL derive)
//
// On ffmpeg-7 `hwmap=derive_device=opencl` does NOT self-create the OpenCL
// device inside a -filter_complex (ffmpeg-6 did) — gpuResidentOpenCLTonemap
// injects `-init_hw_device opencl=ocl@vaapi` and makes the surrounding graph
// VA-resident so this fragment's va→opencl derive succeeds.
func (tm tonemapConfig) stage(algo string) string {
	if !tm.useOpenCL {
		return tm.backend().tonemapFilter(algo, "nv12")
	}
	if !validTonemapAlgo(algo) {
		algo = tm.algo
	}
	return "hwmap=derive_device=opencl," +
		"tonemap_opencl=tonemap=" + algo +
		":transfer=bt709:matrix=bt709:primaries=bt709:format=nv12," +
		"hwmap=derive_device=vaapi:reverse=1"
}

// hdrScale returns the combined `scale(p010) + tonemap` chain that
// downscales and tone-maps an HDR source to an SDR nv12 HW surface.
// Used by the reFilterHDR/HDRAss reshapes — i.e. only where Plex's argv
// already declared a tonemap.
func (tm tonemapConfig) hdrScale(w, h string) string {
	return tm.hdrScaleAlgo(w, h, "")
}

// hdrScaleAlgo is hdrScale with an explicit tonemap algo (Plex's captured
// algo on the SW-HDR reshape path). Empty algo falls back to tm.algo.
func (tm tonemapConfig) hdrScaleAlgo(w, h, algo string) string {
	return tm.backend().scaleFilter(w, h, "p010") + "," + tm.stage(algo)
}

// burnSpec captures the orthogonal axes of a HW scale / sub-burn graph. One
// composer (composeBurn) emits every {SDR,HDR}×{none,text,bitmap}×{any
// resolution} shape from these axes, instead of a bespoke target string per
// Plex argv variant. Codec (encoder) and the decode swap stay orthogonal —
// they're handled by encoderMap / the decode reshape, not here.
type burnSpec struct {
	// vaResident: [0:0] is already a VAAPI surface (HW decode with
	// -hwaccel_output_format vaapi). false → the source is in system memory
	// and the composer prepends an hwupload.
	vaResident bool
	w, h       string // target resolution
	hdr        bool   // HDR source → insert the tonemap stage (p010→nv12)
	algo       string // tonemap algo to honor (HDR only; "" = cfg default)
	// tenBit: encoder needs a 10-bit input (HEVC Main10 over HDR-passthrough).
	// Distinct from hdr — hdr means "Plex sent a tonemap node and we render
	// to 8-bit", tenBit means "no tonemap and we keep the 10-bit chain so the
	// encoder gets a p010 surface for Main10". Ignored when hdr is true (the
	// tonemap path is the encoder's bit-depth gate). Caller sets when the
	// source carries an HDR transfer (smpte2084 / arib-std-b67) AND Plex's
	// graph carries no tonemap node AND the encoder is HEVC — i.e. an
	// HDR-passthrough sub-burn. scaleplex#204.
	tenBit bool
	// burnSub + subParams: when burnSub, append an inlineass stage. subParams
	// is the leading inlineass params (Plex's params for text, "" for bitmap);
	// composeBurn appends render_height (the fork's libass/replay_bitmap band
	// knob). The bitmap stream is fed via -map_inlineass by the caller.
	burnSub   bool
	subParams string
	// animatedTierDown: text-sub only. When set, append `animated_tier_down=1`
	// to the inlineass node — the fork's libass renders animated cues
	// (\move/\t/\k/\fad) one resolution tier below render_height. Static cues
	// are unaffected. Caller computes via subtitleIsAnimated(); ignored on
	// !burnSub.
	animatedTierDown bool
	// subKind + subSpec: SW path only (composeBurnSW). subKind distinguishes
	// "text" (inlineass) from "bitmap" (sub2video→overlay, Plex's stock SW PGS
	// shape); subSpec is the bitmap subtitle stream spec (e.g. "0:5") overlaid.
	// The HW composeBurn ignores these (it uses burnSub for text + the unified
	// inlineass bitmap branch).
	subKind string
	subSpec string
}

// composeBurn builds the orthogonal stage chain and returns the filtergraph
// string + its final output label:
//
//	[0:0] → [hwupload]? → scale_vaapi(p010|nv12) → [tonemap]? → [inlineass]?
//
// Each stage is independent: hdr toggles the tonemap insert, burnSub toggles
// inlineass, vaResident toggles the leading hwupload, w/h/algo are params.
// scale_vaapi emits p010 when a tonemap follows and nv12 otherwise, and the
// tonemap stage is a transparent p010→nv12 VAAPI insert, so inlineass always
// receives an nv12 VAAPI surface whether or not HDR ran — the sub stage is
// HDR-agnostic and the tonemap stage is sub-agnostic. gpuResidentOpenCLTonemap
// later injects the OpenCL device + asserts VA-residency for the tonemap stage.
func (tm tonemapConfig) composeBurn(s burnSpec) (filter, newLabel string) {
	d := tm.backend()
	if d.backendName() == "sw" {
		// Software target: no HW surfaces — emit Plex's CPU filtergraph shape.
		return tm.composeBurnSW(s)
	}
	// AMD radeonsi cross-cuts the HDR + sub-burn axes: there is no
	// tonemap_vaapi (Intel-iHD-only) and the tonemap_opencl chain via
	// VAAPI-OpenCL derive is broken on mesa-opencl-icd (PR #134 closed).
	// vf_inlineass's AMD-Vulkan branch (fork patch 0127) runs the HDR→SDR
	// tone-map inside its libplacebo pl_render_image dispatch — see the
	// AMD-Vulkan v6 patch header. Routing on AMD therefore differs from
	// Intel/NVIDIA:
	//
	//   HDR + sub-burn → scale_vaapi(...:format=p010) + inlineass(...)
	//                    (no separate tonemap stage; the burn pass absorbs it)
	//   HDR + no subs  → scale_vaapi(...:format=p010) + inlineass=tonemap_only=1
	//                    (filter runs as a pure HDR→SDR pl_render_image pass)
	//   SDR (any)      → identical to Intel iHD shape
	//
	// Intel iHD + NVIDIA paths fall through to the unchanged emitter below
	// (the AMD branch is gated on `vd.isAMD()`).
	if vd, ok := d.(vaapiDialect); ok && vd.isAMD() && s.hdr {
		return composeBurnAMDHDR(d, s)
	}
	n := 0
	next := func() string { l := strconv.Itoa(n); n++; return l }
	var b strings.Builder
	src := "[0:0]"
	if !s.vaResident {
		u := next()
		fmt.Fprintf(&b, "%s%s[%s];", src, d.hwUploadFilter(), u)
		src = "[" + u + "]"
	}
	scaled := next()
	switch {
	case s.hdr:
		fmt.Fprintf(&b, "%s%s,%s[%s]",
			src, d.scaleFilter(s.w, s.h, "p010"), tm.stage(s.algo), scaled)
	case s.tenBit:
		// HDR-passthrough: preserve 10-bit through the chain. No tonemap
		// stage (Plex's argv didn't carry one — the encoder will emit HDR
		// HEVC Main10). scaleplex#204.
		fmt.Fprintf(&b, "%s%s[%s]",
			src, d.scaleFilter(s.w, s.h, "p010"), scaled)
	default:
		fmt.Fprintf(&b, "%s%s[%s]",
			src, d.scaleFilter(s.w, s.h, "nv12"), scaled)
	}
	out := scaled
	if s.burnSub {
		// inlineass is a CPU/libass stage. On backends whose vf_inlineass
		// can't consume the HW surface directly (NVIDIA: no CUDA branch),
		// splice a download stage so libass gets a system frame. VAAPI
		// returns "" — its merged HW branch takes the VAAPI surface.
		inlineassSrc := scaled
		if dl := d.subBurnDownloadFilter(); dl != "" {
			dlLabel := next()
			fmt.Fprintf(&b, ";[%s]%s[%s]", scaled, dl, dlLabel)
			inlineassSrc = dlLabel
		}
		o := next()
		params := s.subParams
		if params != "" {
			params += ":"
		}
		params += fmt.Sprintf("render_height=%d", subRenderHeightCap())
		if s.animatedTierDown {
			params += ":animated_tier_down=1"
		}
		fmt.Fprintf(&b, ";[%s]inlineass=%s[%s]", inlineassSrc, params, o)
		out = o
	}
	return b.String(), "[" + out + "]"
}

// appendSelectStage re-emits Plex's seek-offset `select=` node at the tail of a
// recomposed graph. composeBurn* drop frame-gating (they only model the
// scale/tonemap/burn axes), so a seeked session's `select=gte(t\,SEEK)` would be
// lost — resetting playback to t=0 — without this. select is a zero-copy
// metadata gate, valid on the VAAPI/CUDA/CPU surface the composer left at `label`
// alike, so it goes last (mirrors Plex, which runs it just before its final
// hwupload). No-op when expr is "".
func appendSelectStage(filter, label, expr string) (string, string) {
	if expr == "" {
		return filter, label
	}
	const out = "vsel"
	return fmt.Sprintf("%s;%sselect=%s[%s]", filter, label, expr, out), "[" + out + "]"
}

// composeBurnAMDHDR emits the AMD-radeonsi-specific HDR shape. The
// vf_inlineass AMD-Vulkan branch (fork patch 0127 v7) absorbs the HDR→SDR
// tonemap into its single pl_render_image dispatch — no separate tonemap
// stage exists or is supported on radeonsi. When there's no sub-burn,
// `tonemap_only=1` lets the filter run for tonemap alone (no libass
// refresh, no -map_inlineass binding required). Always invoked with
// s.hdr == true; caller (composeBurn) gates entry.
//
// hdr_to_sdr=1 is the explicit rewriter-driven tonemap intent (0127 v7,
// issue #137). Honors scaleplex policy "never inject tonemap; Plex's argv
// decides" — composeBurn only routes here when facts.hdr is true (Plex's
// graph carried a tonemap stage), so emitting hdr_to_sdr=1 mirrors that
// intent. HDR-passthrough sessions (Plex sent no tonemap stage → s.hdr
// false → caller stays on the Intel-style composeBurn path) never get this
// flag and the AMD-Vulkan branch leaves source HDR colorimetry intact.
func composeBurnAMDHDR(d dialect, s burnSpec) (filter, newLabel string) {
	n := 0
	next := func() string { l := strconv.Itoa(n); n++; return l }
	var b strings.Builder
	src := "[0:0]"
	if !s.vaResident {
		u := next()
		fmt.Fprintf(&b, "%s%s[%s];", src, d.hwUploadFilter(), u)
		src = "[" + u + "]"
	}
	// Carry HDR through the scale; the inlineass filter applies the HDR→SDR
	// pl_tgt.color = pl_color_space_bt709 override gated on the
	// hdr_to_sdr=1 AVOption emitted below.
	scaled := next()
	fmt.Fprintf(&b, "%s%s[%s]", src, d.scaleFilter(s.w, s.h, "p010"), scaled)
	o := next()
	if s.burnSub {
		params := s.subParams
		if params != "" {
			params += ":"
		}
		params += fmt.Sprintf("render_height=%d", subRenderHeightCap())
		if s.animatedTierDown {
			params += ":animated_tier_down=1"
		}
		// Explicit HDR→SDR intent — see composeBurnAMDHDR doc.
		params += ":hdr_to_sdr=1"
		// VAAPI dialect's subBurnDownloadFilter() returns "" — vf_inlineass
		// AMD-Vulkan consumes the VAAPI surface directly.
		fmt.Fprintf(&b, ";[%s]inlineass=%s[%s]", scaled, params, o)
	} else {
		// No-sub HDR session: tonemap_only=1 + hdr_to_sdr=1. render_height +
		// animated_tier_down are libass-render knobs; meaningless when libass
		// is bypassed, so omitted to keep the argv minimal.
		fmt.Fprintf(&b, ";[%s]inlineass=tonemap_only=1:hdr_to_sdr=1[%s]", scaled, o)
	}
	return b.String(), "[" + o + "]"
}

// reBitmapSubBranch extracts the bitmap subtitle stream spec from Plex's
// sub2video overlay branch: `[0:5]scale=W:H,hwupload[..]`.
var reBitmapSubBranch = regexp.MustCompile(`\[(0:[0-9]+)\]scale=[0-9]+:[0-9]+,hwupload\[`)

// reBitmapMainScale extracts the main-video target W/H from the overlay graph:
// `[0:0]hwupload[..];[..]scale_{vaapi,cuda}=w=W:h=H`. Matches both HW
// backends for parity with reGraphScaleWH — NVIDIA PGS-burn graphs use
// scale_cuda. NOTE: the NVIDIA bitmap-overlay path is not yet
// live-validated (no PGS NVENC capture in the corpus); see scaleplex#66.
// `[0:0]` OR PMS's stream-by-id `[0:#0xNN]` leading video input (#145) — both
// resolve to the video stream, so the bitmap-overlay / OpenCL-tonemap detectors
// accept either form for a pristine (un-normalized) argv.
var reBitmapMainScale = regexp.MustCompile(`\[0:(?:0|#0x[0-9a-fA-F]+)\]hwupload\[\d+\];\[\d+\]scale_(?:vaapi|cuda)=w=([0-9]+):h=([0-9]+)`)

// reVideoInput0 matches a filtergraph's leading video input label in EITHER
// form: ordinal `[0:0]` or PMS's stream-by-id `[0:#0xNN]` (#145, pristine argv —
// no upfront normalize pass). The hex id resolves to the same stream (PMS
// declares video first), so the reshape composers' `[0:0]` output stays the
// correct canonical rewrite.
var reVideoInput0 = regexp.MustCompile(`^\[0:(?:0|#0x[0-9a-fA-F]+)\]`)

// graphLeadsWithVideoInput0 reports whether `graph` opens with the video input
// label (`[0:0]` or `[0:#0xNN]`) immediately followed by `suffix`. The
// stream-spec-agnostic replacement for `strings.HasPrefix(graph, "[0:0]"+suffix)`
// at the reshape entry points, so a `#0xNN`-shaped graph still engages the
// reshape instead of bailing to Plex's SW-inlineass path.
func graphLeadsWithVideoInput0(graph, suffix string) bool {
	loc := reVideoInput0.FindStringIndex(graph)
	if loc == nil {
		return false
	}
	return strings.HasPrefix(graph[loc[1]:], suffix)
}

// reTonemapOpenCLAlgo extracts Plex's tonemap algorithm from an (already
// substituteOpenCLTonemap-normalized) tonemap_opencl node.
var reTonemapOpenCLAlgo = regexp.MustCompile(`tonemap_opencl=tonemap=([A-Za-z0-9]+)`)

// reGraphTrailingLabel captures a filtergraph's final output label (the one
// the video `-map` references), e.g. `…hwupload[7]` → "7".
var reGraphTrailingLabel = regexp.MustCompile(`\[(\d+)\]$`)

// retargetMapLabel points the video `-map oldLabel` at newLabel (quoted or
// not). No-op when oldLabel is empty or absent.
func retargetMapLabel(args []string, oldLabel, newLabel string) {
	if oldLabel == "" {
		return
	}
	for j := 0; j+1 < len(args); j++ {
		if args[j] != "-map" {
			continue
		}
		switch args[j+1] {
		case oldLabel:
			args[j+1] = newLabel
			return
		case `"` + oldLabel + `"`:
			args[j+1] = `"` + newLabel + `"`
			return
		}
	}
}

// graphFacts are the orthogonal axes extracted from ANY recognized Plex video
// filtergraph — the input side of the orthogonal rewriter. composeBurn turns
// them back into a canonical graph, so one extractor + the composer can replace
// the per-shape reFilter* regex zoo. vaResident is NOT here: it's decided by the
// presence of `-hwaccel:0` in the argv, not the graph.
type graphFacts struct {
	w, h      string
	hdr       bool   // a tonemap stage is present
	algo      string // honored tonemap algo ("" = none / fixed-curve)
	subKind    string // "", "text", "bitmap"
	subParams  string // inlineass params (text)
	subSpec    string // sub stream spec (bitmap overlay)
	selectExpr string // seek-offset `select=` expr (e.g. `gte(t\,203.99)`), "" = none
	ok         bool   // recognized AND every node is modeled (safe to recompose)
}

var (
	// reGraphScaleWH: the main-video scale target. Matches the bare SW
	// `scale=w=…:h=…` and both HW backends — `scale_vaapi=` (Intel) and
	// `scale_cuda=` (NVIDIA). Backend identification is via the leading
	// hwupload/hwaccel context, not this regex.
	reGraphScaleWH = regexp.MustCompile(`scale(?:_(?:vaapi|cuda))?=w=(\d+):h=(\d+)`)
	// reGraphTonemapSW: a bare `tonemap=<algo>` filter (Plex's SW HDR chain),
	// excluding tonemap_opencl=/tonemap_vaapi=/tonemap_cuda= (those are
	// matched separately).
	reGraphTonemapSW = regexp.MustCompile(`(?:^|[,;\]])tonemap=([A-Za-z0-9]+)`)
	// reGraphTonemapCUDA: NVIDIA HW tonemap. Matches BOTH:
	//  - Plex's positional form `tonemap_cuda=ALGO:PIX` (Plex-fork
	//    ffmpeg accepts positional 2 = `format`).
	//  - jellyfin-ffmpeg's named form `tonemap_cuda=tonemap=ALGO:format=PIX`
	//    (jellyfin parses positional 2 as `tonemap_mode`, so the named
	//    form is the portable shape — see nvencDialect.tonemapFilter
	//    for why the rewriter emits named-arg form even though Plex's
	//    incoming argv uses positional).
	// Optional `tonemap=` prefix skipped so the captured group is the
	// algo string in either shape.
	reGraphTonemapCUDA = regexp.MustCompile(`tonemap_cuda=(?:tonemap=)?([A-Za-z0-9]+)`)
	reGraphInlineass   = regexp.MustCompile(`inlineass=([^\[]*)`)
	// reGraphSelect: a standalone `select=<expr>` filter node — Plex's
	// seek-offset frame gate on a seeked session (e.g.
	// `[3]select=gte(t\,203.995661)[4]`). Anchored on a node boundary (`;`/`]`)
	// so a `select=` substring inside another node's args can't match, and the
	// expr is captured up to the output label `[`, preserving the escaped comma
	// (`\,`) verbatim — that escaping must survive into the recomposed graph or
	// ffmpeg reparses it as a filterchain separator.
	reGraphSelect = regexp.MustCompile(`[;\]]select=([^\[]+)\[`)
	// reGraphFilterName: a filtergraph node name — an identifier preceded by a
	// chain boundary (start, ';', ',', ']') possibly followed by whitespace,
	// and followed by '=', '[', ',', ';' or end. Arg values (after '=' or ':')
	// are not preceded by a boundary, so they aren't matched. The `\s*` after
	// the boundary lets the matcher catch nodes after whitespace (Plex emits
	// e.g. `; [1]crop=...` in places) — without it, an unmodeled node could
	// bypass graphNodesModeled and silently skip the safety bail.
	reGraphFilterName = regexp.MustCompile(`(?:^|[;,\]])\s*([a-z_][a-z0-9_]*)(?:[=\[,;]|$)`)
)

// modeledFilterNodes is every filtergraph node composeBurn / the rewriter
// understands. extractGraphFacts bails (ok=false) on a graph carrying anything
// else, so an unrecognized shape falls through to the existing bail/SW path
// instead of being mis-recomposed — preserving the strict reFilter* behavior.
var modeledFilterNodes = map[string]bool{
	"scale": true, "scale_vaapi": true, "scale_cuda": true,
	"hwupload": true, "hwdownload": true,
	"hwmap": true, "format": true, "setparams": true, "tonemap": true,
	"tonemap_opencl": true, "tonemap_vaapi": true, "tonemap_cuda": true,
	"zscale":    true,
	"inlineass": true, "overlay_vaapi": true, "overlay_cuda": true,
	// select: Plex's seek-offset frame gate (`select=gte(t\,SEEK)`) on a
	// seeked session. A zero-copy metadata gate (no pixel access), so it's
	// valid on any surface incl. VAAPI; extractGraphFacts lifts the expr and
	// the composers re-emit it at the chain tail (see appendSelectStage).
	"select": true,
}

// graphNodesModeled reports whether every filter node in the graph is in
// modeledFilterNodes.
func graphNodesModeled(graph string) bool {
	for _, m := range reGraphFilterName.FindAllStringSubmatch(graph, -1) {
		if !modeledFilterNodes[m[1]] {
			return false
		}
	}
	return true
}

// extractGraphFacts pulls the orthogonal axes from a Plex filtergraph. It
// recognizes the same shapes the reFilter* regexes do — text/bitmap × SDR/HDR
// × scale — keyed on the semantic nodes rather than one rigid layout, and bails
// (ok=false) on any graph with an unmodeled node. subSrc supplies the
// authoritative sub kind/spec (from -map_inlineass / probe); the graph supplies
// w/h, tonemap algo, and the text inlineass params.
func extractGraphFacts(graph string, subSrc *subtitleSource) graphFacts {
	var f graphFacts
	// Seek-offset `select=` node (a seeked session). Lift the expr up front so
	// both the bitmap and text return paths carry it. If a `select=` node is
	// present but we can't cleanly extract its expr (an unobserved shape), bail
	// (ok stays false) rather than recompose a graph that silently drops the
	// seek gate — the seek would reset to t=0.
	if strings.Contains(graph, "select=") {
		m := reGraphSelect.FindStringSubmatch(graph)
		if m == nil {
			return f
		}
		f.selectExpr = m[1]
	}
	// Bitmap sub2video→overlay_vaapi burn (with/without an intervening
	// tonemap) — already a fact extractor.
	if spec, w, h, algo, hdr, ok := detectBitmapOverlayBurn(graph); ok {
		if !graphNodesModeled(graph) {
			return f
		}
		f.w, f.h, f.algo, f.hdr = w, h, algo, hdr
		f.subKind, f.subSpec, f.ok = "bitmap", spec, true
		return f
	}
	m := reGraphScaleWH.FindStringSubmatch(graph)
	if m == nil {
		return f // no main scale → not a shape we recompose
	}
	f.w, f.h = m[1], m[2]
	switch {
	case reTonemapOpenCLAlgo.MatchString(graph):
		f.hdr, f.algo = true, reTonemapOpenCLAlgo.FindStringSubmatch(graph)[1]
	case reGraphTonemapCUDA.MatchString(graph):
		f.hdr, f.algo = true, reGraphTonemapCUDA.FindStringSubmatch(graph)[1]
	case reGraphTonemapSW.MatchString(graph):
		f.hdr, f.algo = true, reGraphTonemapSW.FindStringSubmatch(graph)[1]
	case strings.Contains(graph, "tonemap_vaapi"):
		f.hdr = true
	}
	switch {
	case subSrc != nil && subSrc.Kind == "bitmap":
		// Sidecar/embedded bitmap reaching us via -map_inlineass with Plex's
		// `inlineass=` node (no overlay_vaapi). subSrc is authoritative — don't
		// misread the inlineass node as text.
		f.subKind, f.subSpec = "bitmap", subSrc.StreamSpec
	case (subSrc != nil && subSrc.Kind == "text") || strings.Contains(graph, "inlineass="):
		f.subKind = "text"
		if im := reGraphInlineass.FindStringSubmatch(graph); im != nil {
			f.subParams = im[1]
		}
	}
	if !graphNodesModeled(graph) {
		return f
	}
	f.ok = true
	return f
}

// detectBitmapOverlayBurn recognizes Plex's bitmap (PGS/VobSub/DVDSub)
// sub2video→overlay_vaapi burn graph — with OR without an intervening
// tonemap_opencl chain — and extracts the orthogonal facts needed to recompose
// it as scale_vaapi→[tonemap]→inlineass (see composeBurn). It deliberately
// does NOT match one rigid shape: it keys off the sub2video branch + the main
// scale + (optionally) a tonemap node, so a tonemap spliced between the scaled
// video and overlay_vaapi (the HDR variant) no longer escapes the optimizer.
// Returns ok=false when the graph isn't a bitmap overlay burn.
func detectBitmapOverlayBurn(graph string) (streamSpec, w, h, algo string, hdr, ok bool) {
	if !strings.Contains(graph, "overlay_vaapi") && !strings.Contains(graph, "overlay_cuda") {
		return
	}
	sb := reBitmapSubBranch.FindStringSubmatch(graph)
	ms := reBitmapMainScale.FindStringSubmatch(graph)
	if sb == nil || ms == nil {
		return
	}
	streamSpec, w, h = sb[1], ms[1], ms[2]
	if m := reTonemapOpenCLAlgo.FindStringSubmatch(graph); m != nil {
		algo, hdr = m[1], true
	}
	ok = true
	return
}

// rewriteVideoFilter reshapes Plex's SW-decode filtergraph (the force-HW/reshape
// path) into scaleplex's canonical HW graph. It is now a thin adapter over the
// orthogonal core: extractGraphFacts pulls the axes (w/h, hdr+algo, sub
// kind/params) and composeBurn emits the one canonical shape
// ([0:0] → hwupload → scale_vaapi(p010|nv12) → [tonemap] → [inlineass]),
// replacing the per-shape reFilterAss/Plain/HDR/HDRAss branches. [0:0] is a
// system-memory frame here (Plex SW-decoded), so vaResident=false. Tonemap is
// driven by facts.hdr (what Plex's graph declared), NOT the source probe — a
// graph with no tonemap stays SDR even on an HDR source (Plex's policy). Bails
// (nil) on any unmodeled graph, exactly like the old strict regexes.
func rewriteVideoFilter(filterStr, mediaPath string, subSrc *subtitleSource, sourceIsHDR bool, tm tonemapConfig) *filterRewrite {
	_ = mediaPath
	_ = sourceIsHDR
	// SW-reshape path only: [0:0] is a system-memory frame (Plex SW-decoded),
	// so the graph opens with a SW `scale=`. HW-shaped graphs (`scale_vaapi` /
	// leading `hwupload`, e.g. the hybrid-force-HW case where [0:0] is already
	// VA) are handled by the HW-decode branch — bail here so they fall through
	// unchanged rather than getting a wrong leading hwupload from
	// composeBurn(vaResident=false). Matches the old reFilter* `^\[0:0\]scale=`
	// anchor.
	if !graphLeadsWithVideoInput0(filterStr, "scale=w=") {
		return nil
	}
	facts := extractGraphFacts(filterStr, subSrc)
	if !facts.ok {
		return nil
	}
	oldLabel := ""
	if m := reGraphTrailingLabel.FindStringSubmatch(filterStr); m != nil {
		oldLabel = "[" + m[1] + "]"
	}
	// Animated-cue tier-down only applies to text subs (libass overrides);
	// bitmap presentations are static per-cue. Embedded ASS with no readable
	// file is conservatively treated as animated (matches HW-decode-text).
	animated := false
	if facts.subKind == "text" && subSrc != nil {
		animated = subtitleIsAnimated(subSrc.Codec, subSrc.FilePath, os.ReadFile)
	}
	f, newLabel := tm.composeBurn(burnSpec{
		vaResident:       false,
		w:                facts.w,
		h:                facts.h,
		hdr:              facts.hdr,
		algo:             facts.algo,
		burnSub:          facts.subKind != "",
		subParams:        facts.subParams,
		animatedTierDown: animated,
	})
	f, newLabel = appendSelectStage(f, newLabel, facts.selectExpr)
	return &filterRewrite{
		Filter:   f,
		OldLabel: oldLabel,
		NewLabel: newLabel,
		Mode:     composeMode(facts),
	}
}

// composeMode names a composeBurn reshape for the change tag + the caller's
// -map_inlineass handling ("bitmap-inlineass-vaapi" makes the caller ensure the
// flag is present for the replay_bitmap feed).
func composeMode(f graphFacts) string {
	switch f.subKind {
	case "bitmap":
		return "bitmap-inlineass-vaapi"
	case "text":
		return "text-inlineass-vaapi"
	}
	if f.hdr {
		return "hdr-tonemap-vaapi"
	}
	return "plain"
}

// substituteOpenCLTonemap normalizes Plex's OpenCL HW tonemap filter
// chain in -filter_complex. PMS with the `Use hardware-accelerated tone
// mapping` pref ON emits a chain that hwmaps VAAPI → OpenCL, runs
// tonemap_opencl with the algorithm from the PMS
// `TranscoderTonemapAlgorithm` pref, hwmaps OpenCL → VAAPI, then
// hwuploads.
//
// Pattern matched (label numbers vary):
//
//	[X]hwmap=derive_device=opencl[A];
//	[A]tonemap_opencl=tonemap=<algo>:format=<pixfmt>:m=<mtx>:p=<prim>:r=<rng>[B];
//	[B]hwmap=derive_device=vaapi:reverse=1[C]
//
// In the default OpenCL tonemap mode the chain is re-emitted in
// scaleplex's canonical comma form, PRESERVING Plex's chosen algorithm
// — the whole point of this path is to honor TranscoderTonemapAlgorithm
// instead of discarding it:
//
//	[X]hwmap=derive_device=opencl,tonemap_opencl=tonemap=<algo>:transfer=bt709:matrix=<mtx>:primaries=<prim>:format=<pixfmt>,hwmap=derive_device=vaapi:reverse=1[C]
//
// With SCALEPLEX_TONEMAP=vaapi it collapses to a single fixed-curve
// tonemap_vaapi instead — the algorithm is then discarded, since iHD's
// VAAPI VPP tonemap uses a fixed BT.2390 EETF curve with no per-
// algorithm tuning. matrix / primaries from Plex's chain are preserved
// in both modes; range has no tonemap_vaapi equivalent.
//
// Returns updated args + true if a substitution was applied.
var openclTonemapRE = regexp.MustCompile(
	`\[(\d+)\]hwmap=derive_device=opencl\[\d+\];` +
		`\[\d+\]tonemap_opencl=(?P<opts>[^[]+)\[\d+\];` +
		`\[\d+\]hwmap=derive_device=vaapi:reverse=1\[(\d+)\]`)
var tonemapOptRE = regexp.MustCompile(`(?:^|:)(tonemap|format|m|p|r|t)=([a-z0-9-]+)`)

func substituteOpenCLTonemap(args []string, tm tonemapConfig) ([]string, bool) {
	// VAAPI-only: the OpenCL-derive tonemap chain (hwmap→opencl→vaapi) is an
	// Intel-pipeline construct. On any other worker backend this is a no-op
	// (the tonemap is reshaped via composeBurn → the dialect's tonemapFilter
	// instead). Belt-and-suspenders for cross-backend (scaleplex#77): a NVIDIA
	// worker receiving a foreign VAAPI tonemap_opencl graph must NOT run this.
	if tm.backend().backendName() != "vaapi" {
		return args, false
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-filter_complex" {
			continue
		}
		orig := args[i+1]
		if !strings.Contains(orig, "tonemap_opencl") {
			continue
		}
		m := openclTonemapRE.FindStringSubmatchIndex(orig)
		if m == nil {
			continue
		}
		startLabel := orig[m[2]:m[3]]
		opts := orig[m[4]:m[5]]
		endLabel := orig[m[6]:m[7]]
		// Parse k=v pairs from the opencl filter opts. format / m
		// (matrix) / p (primaries) are preserved; tonemap (algorithm)
		// is preserved in OpenCL mode, discarded in vaapi mode.
		var pixFmt, matrix, primaries, algo string
		for _, kv := range tonemapOptRE.FindAllStringSubmatch(opts, -1) {
			switch kv[1] {
			case "format":
				pixFmt = kv[2]
			case "m":
				matrix = kv[2]
			case "p":
				primaries = kv[2]
			case "tonemap":
				algo = kv[2]
				// "r" (range) / "t" (transfer) have no tonemap_vaapi
				// equivalent option — VAAPI derives them from the
				// surrounding pipeline; we always target SDR bt709.
			}
		}
		if pixFmt == "" {
			pixFmt = "nv12"
		}
		var replacement string
		if tm.useOpenCL {
			// Re-emit Plex's chain in canonical comma form, keeping its
			// algorithm. matrix / primaries default to bt709 (the SDR
			// target) when Plex's chain did not pin them.
			if !validTonemapAlgo(algo) {
				algo = tm.algo
			}
			if matrix == "" {
				matrix = "bt709"
			}
			if primaries == "" {
				primaries = "bt709"
			}
			replacement = fmt.Sprintf(
				"[%s]hwmap=derive_device=opencl,"+
					"tonemap_opencl=tonemap=%s:transfer=bt709:matrix=%s:primaries=%s:format=%s,"+
					"hwmap=derive_device=vaapi:reverse=1[%s]",
				startLabel, algo, matrix, primaries, pixFmt, endLabel)
		} else {
			// SCALEPLEX_TONEMAP=vaapi: collapse to the fixed-curve
			// VAAPI filter. transfer=bt709 is the standard SDR target.
			vaapiOpts := "transfer=bt709"
			if matrix != "" {
				vaapiOpts += ":matrix=" + matrix
			}
			if primaries != "" {
				vaapiOpts += ":primaries=" + primaries
			}
			vaapiOpts += ":format=" + pixFmt
			replacement = fmt.Sprintf("[%s]tonemap_vaapi=%s[%s]",
				startLabel, vaapiOpts, endLabel)
		}
		args[i+1] = orig[:m[0]] + replacement + orig[m[1]:]
		return args, true
	}
	return args, false
}

// reLeadHwuploadOCL drops a leading `[0:0]hwupload[N];[N]` (or PMS's
// stream-by-id `[0:#0xNN]` form, #145) so scale_vaapi reads the VA decode
// surface directly (see gpuResidentOpenCLTonemap).
var reLeadHwuploadOCL = regexp.MustCompile(`^\[0:(?:0|#0x[0-9a-fA-F]+)\]hwupload\[\d+\];\[\d+\]`)

// reRevmapBeforeDownloadOCL collapses the `hwmap=vaapi:reverse=1[X];[X]hwdownload`
// round-trip (opencl→va→sysmem) into a direct opencl→sysmem hwdownload.
var reRevmapBeforeDownloadOCL = regexp.MustCompile(`hwmap=derive_device=vaapi:reverse=1\[\d+\];\[\d+\]hwdownload`)

// reInitHwDeviceVaapiName extracts the device NAME from `vaapi=<name>:...`.
var reInitHwDeviceVaapiName = regexp.MustCompile(`^vaapi=([A-Za-z0-9_]+):`)

// gpuResidentOpenCLTonemap makes an emitted `tonemap_opencl` filtergraph
// valid + GPU-resident on jellyfin-ffmpeg 7.x. ffmpeg-6 auto-created the
// OpenCL device for `hwmap=derive_device=opencl` and tolerated a leading
// `[0:0]hwupload` / a `hwmap=vaapi:reverse=1→hwdownload→hwupload` round-trip;
// ffmpeg-7 does NOT: in a `-filter_complex` the va→opencl frame derive then
// fails ENOSYS (-38, "hardware pixel format 'opencl' is not supported by the
// device type 'VAAPI'"), which silently broke HDR algo-honoring tonemap on
// the 6→7 bump (latent in prod — full-HW HDR-with-HW-tonemap only). Recipe
// proven on Arc A310 (uid 1000, real decode), no sysmem bounce — see
// reference_scaleplex_tonemap_regression_test:
//  1. inject `-init_hw_device opencl=ocl@<vaapi-dev>` (derive from the VA
//     device → cl_intel_va_api_media_sharing) before the first -i.
//  2. force `-hwaccel_output_format:0 vaapi` so the HW decode hands the
//     filtergraph a real VA surface ([0:0] is VA, not SW).
//  3. drop a leading `[0:0]hwupload[N];[N]` so scale_vaapi reads that VA
//     surface directly (re-uploading an already-VA frame yields a
//     frames-ctx that can't derive opencl).
//  4. collapse `hwmap=vaapi:reverse=1[X];[X]hwdownload` → `hwdownload`
//     (opencl→sysmem direct) when a SW step (libass) follows.
//
// Only fires when the decode is VA-resident (a `-hwaccel:0` is present —
// either Plex's HW decode or the rewriter's libdav1d→VAAPI swap, both of
// which carry `-hwaccel_output_format:0 vaapi` so [0:0] is a VA surface). A
// genuinely SW-decoded HDR source ([0:0] in system memory) has no VA surface
// to feed and is left untouched. No-op when SCALEPLEX_TONEMAP=vaapi (no
// tonemap_opencl is ever emitted).
func gpuResidentOpenCLTonemap(args []string) ([]string, []string) {
	// VAAPI-only — see substituteOpenCLTonemap. The OpenCL device injection +
	// VA-residency assertions are Intel-pipeline constructs; a non-VAAPI
	// worker must never run them (scaleplex#77 cross-backend).
	if activeDialect.backendName() != "vaapi" {
		return args, nil
	}
	if streamSpecIndex(args, "-hwaccel", 0, 0) < 0 {
		return args, nil
	}
	// Locate a filter_complex carrying tonemap_opencl.
	vfIdx := -1
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-filter_complex" && strings.Contains(args[i+1], "tonemap_opencl") {
			vfIdx = i + 1
			break
		}
	}
	if vfIdx < 0 {
		return args, nil
	}
	var changes []string

	// 3 + 4: reshape the graph string.
	g := args[vfIdx]
	if reLeadHwuploadOCL.MatchString(g) {
		g = reLeadHwuploadOCL.ReplaceAllString(g, "[0:0]")
		changes = append(changes, TagTonemapOCLDropLeadHWUpload)
	}
	if reRevmapBeforeDownloadOCL.MatchString(g) {
		g = reRevmapBeforeDownloadOCL.ReplaceAllString(g, "hwdownload")
		changes = append(changes, TagTonemapOCLCollapseRevmapDownload)
	}
	args[vfIdx] = g

	// 1: inject the OpenCL device, derived from the VAAPI device. It MUST be
	// parsed AFTER the `-init_hw_device vaapi=<name>:...` it derives from
	// (`opencl=ocl@<name>` otherwise fails "invalid source device name"), so
	// splice it immediately after that pair — wherever it sits. Plex places
	// its own -init_hw_device AFTER -i; the rewriter's injected one sits
	// before -i. Either way opencl lands right after vaapi.
	if !hasOpenCLInitDevice(args) {
		vaName, vaIdx := "vaapi", -1
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-init_hw_device" {
				if m := reInitHwDeviceVaapiName.FindStringSubmatch(args[i+1]); m != nil {
					vaName, vaIdx = m[1], i
					break
				}
			}
		}
		if vaIdx >= 0 {
			args = spliceArgs(args, vaIdx+2, "-init_hw_device", "opencl=ocl@"+vaName)
			changes = append(changes, TagTonemapOCLInjectOpenCLDevice)
		}
	}

	// 2: force VA-resident decode so [0:0] is a VA surface.
	if hwIdx := streamSpecIndex(args, "-hwaccel", 0, 0); hwIdx >= 0 &&
		streamSpecIndex(args, "-hwaccel_output_format", 0, 0) < 0 {
		args = spliceArgs(args, hwIdx+2, "-hwaccel_output_format:0", "vaapi")
		changes = append(changes, TagTonemapOCLForceOutputFormatVA)
	}
	return args, changes
}

// hasOpenCLInitDevice reports whether the argv already creates an OpenCL
// hw device (`-init_hw_device opencl=...`).
func hasOpenCLInitDevice(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-init_hw_device" && strings.HasPrefix(args[i+1], "opencl=") {
			return true
		}
	}
	return false
}

func cloneArgs(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// spliceArgs inserts `ins` at position `at` and returns the new slice.
func spliceArgs(s []string, at int, ins ...string) []string {
	out := make([]string, 0, len(s)+len(ins))
	out = append(out, s[:at]...)
	out = append(out, ins...)
	out = append(out, s[at:]...)
	return out
}

// removeArgs removes `n` items starting at `at`.
func removeArgs(s []string, at, n int) []string {
	out := make([]string, 0, len(s)-n)
	out = append(out, s[:at]...)
	out = append(out, s[at+n:]...)
	return out
}

// ensureHEVCMain10 pins `-profile:N main10` on a 10-bit HW HEVC encode.
// Plex's argv never sets a HEVC profile, and a HW HEVC encoder fed p010 may
// default to HEVC Range-Extensions (Rext) rather than Main10 (confirmed on
// VAAPI/iHD) — a profile/pixfmt Apple VideoToolbox (Plex-for-Apple-TV's mpv
// hwdec) cannot decode, so the client crashes on 4K HDR with `hevc (Rext),
// unspecified pixel format`. Forcing main10 yields a stream tvOS plays.
// Backend-agnostic: matches any HW hevc encoder via hwEncoderCodec —
// hevc_vaapi (Intel iHD + AMD radeonsi) and hevc_nvenc (NVIDIA); `-profile
// main10` is valid for both. Only 10-bit (p010) is touched: 8-bit (nv12 →
// encoder default `main`) and h264 are left alone, and an existing
// `-profile` (Plex or a prior pass) is never overwritten. See scaleplex#189.
func ensureHEVCMain10(args, changes []string) ([]string, []string) {
	inputIdx := indexOfArg(args, "-i", 0)
	if inputIdx < 0 {
		return args, changes
	}
	enc := streamSpecIndex(args, "-codec", 0, inputIdx+1) // encoder slot (after -i)
	if enc < 0 || enc+1 >= len(args) || hwEncoderCodec[args[enc+1]] != "hevc" {
		return args, changes
	}
	if !tenBitOutput(args) {
		return args, changes
	}
	// "-codec:0" → "-profile:0"; keep the same stream-spec suffix.
	profileFlag := "-profile" + strings.TrimPrefix(args[enc], "-codec")
	if indexOfArg(args, profileFlag, 0) >= 0 || indexOfArg(args, "-profile:v", 0) >= 0 {
		return args, changes // already set — don't override
	}
	args = spliceArgs(args, enc+2, profileFlag, "main10")
	changes = append(changes, TagEncodeHEVCMain10)
	return args, changes
}

// tenBitOutput reports whether the HW filter chain feeds the encoder a 10-bit
// surface. The pixfmt entering the encoder is the LAST `format=<pixfmt>`
// declaration in the video filter graph: Plex's HDR-tonemap shape declares
// `scale_vaapi=…format=p010` upstream and then `tonemap_opencl=…format=nv12`
// + `hwdownload,format=nv12` downstream, so a naked `p010` substring scan
// would mis-tag an 8-bit hand-off as 10-bit (scaleplex#200). The encoder
// ultimately receives whatever the LAST `format=` token says — track that.
func tenBitOutput(args []string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] != "-filter_complex" {
			continue
		}
		graph := args[i+1]
		lastP010 := lastFormatToken(graph, "p010")
		lastNV12 := lastFormatToken(graph, "nv12")
		if lastP010 < 0 && lastNV12 < 0 {
			continue // not a pixfmt-bearing graph (audio chain etc.)
		}
		if lastP010 > lastNV12 {
			return true
		}
		return false
	}
	return false
}

// lastFormatToken returns the byte offset of the last `format=<pixfmt>` token
// in graph whose pixfmt identifier matches `pixfmt` exactly (no partial
// matches like `nv12` inside `nv12_something`), or -1 if none.
func lastFormatToken(graph, pixfmt string) int {
	needle := "format=" + pixfmt
	off, last := 0, -1
	for {
		pos := strings.Index(graph[off:], needle)
		if pos < 0 {
			break
		}
		pos += off
		end := pos + len(needle)
		if end < len(graph) {
			c := graph[end]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '_' {
				off = end
				continue // identifier continues — partial match, skip
			}
		}
		last = pos
		off = end
	}
	return last
}

// stripInlineassDecodeSink removes Plex's subtitle decode-sink output —
// the trailing `-map <spec> -f null -codec ass|dvdsub nullfile` (7 tokens).
// Plex emits it only to force the subtitle stream to decode so its private
// inlineass filter gets fed. Fork patch 0120 makes the `-map_inlineass`
// binding self-decode the stream (a sink-less decoder, paced by the demux's
// video-read backpressure), so the separate null-mux output is redundant —
// it only adds an unthrottled reader/encoder/muxer that competes for the
// (NFS) input read during the pre-throttle buffer fill, the embedded-sub
// startup skips. Gated on `-map_inlineass` still being present: if a path
// dropped the binding (e.g. overlay_vaapi), the sink is the only thing
// decoding the sub and must stay.
//
// **Sidecar exception (validated 2026-05-26)**: patch 0120's sink-less
// decoder relies on the SHARED demuxer being pumped by the video pipeline
// — when the `-map_inlineass` stream lives in a separate input file
// (sidecar SRT: `-i sub.srt -map_inlineass 1:s:0`), the sidecar's own
// demuxer has no other consumer and the scheduler only emits the first
// packet (~7 bytes) before stalling. The decoder thread fires zero
// times → libass track stays empty → blank subs. Keep the decode-sink
// for sidecar bindings (file_idx >= 1) so the sidecar demuxer has a
// real downstream consumer; the sink-less decoder co-exists (ist_use
// is idempotent), but the real-sink one is what actually pumps the
// stream. Returns (args, stripped?).
func stripInlineassDecodeSink(args []string) ([]string, bool) {
	mi := indexOfArg(args, "-map_inlineass", 0)
	if mi < 0 || mi+1 >= len(args) {
		return args, false
	}
	// `-map_inlineass <file_idx>:<stream_spec>` — keep the sink when the
	// binding points at a non-main (sidecar) input. See header comment.
	spec := args[mi+1]
	colon := strings.IndexByte(spec, ':')
	if colon > 0 && spec[:colon] != "0" {
		return args, false
	}
	for i := 0; i+6 < len(args); i++ {
		if args[i] == "-map" && args[i+2] == "-f" && args[i+3] == "null" &&
			args[i+4] == "-codec" && (args[i+5] == "ass" || args[i+5] == "dvdsub") &&
			args[i+6] == "nullfile" {
			return removeArgs(args, i, 7), true
		}
	}
	return args, false
}

// ─── shared rewriter helpers ─────────────────────────────────────────
//
// The transcode and Optimize-remux paths share the same "strip
// Plex-private cruft + rewrite EAE audio + capture progressurl +
// adjust env" tail. Extracted here so the tail in Rewrite() runs once
// and serves both whether or not the transcode body executed.

// eaeBaseFor maps a `*_eae` codec name to the stock equivalent.
// **Direction-sensitive** because the answer differs:
//
//   - decode: every base codec (eac3, ac3, truehd, mlp) has a working
//     stock decoder. Strip the _eae suffix and let ffmpeg run.
//   - encode: stock has eac3 + ac3 encoders; truehd/mlp encoders are
//     experimental and run sub-realtime under jellyfin-ffmpeg7. Fall
//     back to eac3 for those (lossy but functional).
//
// Pre-2026-05-10 PM: a single direction-blind helper applied the
// encode-side fallback to both positions. That broke real TrueHD
// sources at the input decoder slot — ffmpeg was told to decode
// TrueHD bytes with the eac3 decoder and failed on the bitstream
// parse (live-validated 2026-05-10 PM with a fresh TrueHD download).
func eaeBaseFor(eaeName string, direction eaeDirection) string {
	base, ok := strings.CutSuffix(eaeName, "_eae")
	if !ok {
		return ""
	}
	switch direction {
	case eaeDecode:
		// All base codecs decodable in stock ffmpeg.
		return base
	default: // eaeEncode
		switch base {
		case "eac3", "ac3":
			return base
		default:
			return "eac3"
		}
	}
}

type eaeDirection int

const (
	eaeDecode eaeDirection = iota
	eaeEncode
)

// audioCodecFlag returns true when arg s is an audio-track codec
// option ("-c:a", "-codec:N", "-c:a:N" with any digit N). Stream :0
// is video in PMS argv; suffix-test on the value (e.g. "_eae") rules
// out video-stream collisions when callers gate further.
func audioCodecFlag(s string) bool {
	if s == "-c:a" {
		return true
	}
	if rest, ok := strings.CutPrefix(s, "-codec:"); ok {
		if _, err := strconv.Atoi(rest); err == nil {
			return true
		}
	}
	if rest, ok := strings.CutPrefix(s, "-c:a:"); ok {
		if _, err := strconv.Atoi(rest); err == nil {
			return true
		}
	}
	return false
}

// swapEAEAudioDecoders replaces every `<audio-codec-flag> X_eae` value
// pair with the stock equivalent returned by eaeBaseFor. Position-aware:
// pairs that sit BEFORE the first `-i` are input decoder hints (use
// the real base codec — stock ffmpeg has decoders for all of them);
// pairs AFTER `-i` are output encoder selections (eac3/ac3 survive,
// truehd/mlp fall back to eac3 because stock has no working encoder
// for those). Mutates args in place; returns it for fluent use plus
// a deduplicated list of (from→to) swap pairs the caller emits as
// change tags. Pairs aren't deduped *per direction* because the same
// source codec name can land at two different targets (truehd_eae
// at input → truehd, at output → eac3 — both must surface as tags).
func swapEAEAudioDecoders(args []string) ([]string, [][2]string) {
	iIdx := indexOfArg(args, "-i", 0)
	seen := map[[2]string]struct{}{}
	var swapped [][2]string
	for i := 0; i < len(args); i++ {
		if !audioCodecFlag(args[i]) || i+1 >= len(args) {
			continue
		}
		dir := eaeEncode
		if iIdx >= 0 && i < iIdx {
			dir = eaeDecode
		}
		from := args[i+1]
		to := eaeBaseFor(from, dir)
		if to == "" {
			continue
		}
		args[i+1] = to
		key := [2]string{from, to}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			swapped = append(swapped, key)
		}
	}
	return args, swapped
}

// dropEAEPrefixFlags walks args and removes every `-eae_prefix:N
// <token>` pair (PMS adds one per *_eae codec; sessions can have 1-3).
// Returns updated args and the flag names dropped (for change tags).
func dropEAEPrefixFlags(args []string) ([]string, []string) {
	var dropped []string
	for {
		removed := false
		for i := 0; i < len(args); i++ {
			if strings.HasPrefix(args[i], "-eae_prefix") {
				dropped = append(dropped, args[i])
				args = removeArgs(args, i, 2)
				removed = true
				break
			}
		}
		if !removed {
			break
		}
	}
	return args, dropped
}

// dropFramedropBSF walks args and removes every `-bsf:N framedrop=*`
// pair. PMS emits this BSF on the audio output post-seek to drop the
// first N AAC frames for A/V alignment; `framedrop` is a
// Plex-Transcoder-only bitstream filter (scaleplex-ffmpeg7's
// jellyfin-ffmpeg7 base has no such BSF). Without this drop, ffmpeg
// fails at open-output time with "Bitstream filter not found
// 'framedrop'" → exit status 8 before any chunks are written. Plex's
// session-handler retries on exit-8 with different argv so the user
// sees a brief stutter rather than a stuck session, but the
// recoverable failure is still ugly in logs.
//
// Match is anchored on BOTH halves of the pair: the flag starts with
// `-bsf:` (covers `-bsf:0`, `-bsf:1`, `-bsf:a:0`, etc.) and the value
// starts with `framedrop=`. Other `-bsf:N <chain>` pairs (e.g.
// `-bsf:0 dovi_rpu=strip=1` — used by Dolby Vision passthrough — or
// `-bsf:v h264_metadata=...`) MUST survive untouched, so the value
// prefix check is mandatory. Returns updated args and the flag names
// dropped (for change tags).
func dropFramedropBSF(args []string) ([]string, []string) {
	var dropped []string
	for {
		removed := false
		for i := 0; i < len(args)-1; i++ {
			if strings.HasPrefix(args[i], "-bsf:") &&
				strings.HasPrefix(args[i+1], "framedrop=") {
				dropped = append(dropped, args[i])
				args = removeArgs(args, i, 2)
				removed = true
				break
			}
		}
		if !removed {
			break
		}
	}
	return args, dropped
}

// scrubPlexInlineassFilesystemPaths removes PMS-install-only AVOption
// keys (`font_path`, `fontconfig_file`) from every `inlineass=` node in
// a -filter_complex graph. PMS emits absolute paths that exist only on
// the Plex Media Server install (`/usr/lib/plexmediaserver/Resources/
// Fonts/NotoSans-Medium.otf`, `/usr/lib/plexmediaserver/Resources/
// fonts.conf`); the worker image has neither file, so vf_inlineass +
// libass fontconfig init fails and ffmpeg exits 145 on every force-burn
// session. Without those keys, libass falls back to the worker's
// FONTCONFIG_FILE env (/opt/scaleplex/fonts.conf) and the default font
// name "DejaVu Sans" hardcoded in fork patch 0099 — visually similar
// sans-serif coverage, no broken renders.
//
// Top-level `inlineass=` pairs are `:`-separated. `overrides=` is the
// only value containing `,` and `=`; verified across the argv corpus
// (2026-05-12) that no top-level `:` appears inside any inlineass key
// value — same assumption as plexInlineassToForceStyle and the retired
// stripPlexInlineassFilterArgs (deleted in d82374e; that one stripped
// styling keys the fork couldn't parse pre-0119, but never touched the
// filesystem-path keys, leaving the latent libass bug to surface once
// uid:1000 made the fontconfig cache write unauthorized too).
//
// Idempotent. Returns the (possibly rewritten) graph + true if any pair
// was stripped.
func scrubPlexInlineassFilesystemPaths(filterStr string) (string, bool) {
	if !strings.Contains(filterStr, "inlineass=") {
		return filterStr, false
	}
	stripKeys := map[string]bool{
		"font_path":       true,
		"fontconfig_file": true,
	}
	var out strings.Builder
	out.Grow(len(filterStr))
	changed := false
	i := 0
	for {
		j := strings.Index(filterStr[i:], "inlineass=")
		if j < 0 {
			out.WriteString(filterStr[i:])
			break
		}
		j += i
		out.WriteString(filterStr[i : j+len("inlineass=")])
		k := j + len("inlineass=")
		segEnd := len(filterStr)
		if end := strings.IndexAny(filterStr[k:], "[;"); end >= 0 {
			segEnd = k + end
		}
		segment := filterStr[k:segEnd]
		pairs := strings.Split(segment, ":")
		kept := pairs[:0]
		for _, p := range pairs {
			kv := strings.SplitN(p, "=", 2)
			if len(kv) == 2 && stripKeys[kv[0]] {
				changed = true
				continue
			}
			kept = append(kept, p)
		}
		out.WriteString(strings.Join(kept, ":"))
		i = segEnd
	}
	return out.String(), changed
}

// streamIDSpecRegex matches a `:#0xNN` stream-by-id suffix wherever it appears
// — a flag (`-codec:#0x01`) or inside a `-filter_complex` value
// (`[0:#0x01]hwupload`). streamIDOrdinalMap scans it to resolve ids to ordinals
// at match time (#145); the rewriter no longer rewrites these to ordinal form,
// so PMS's argv reaches ffmpeg (which accepts the `#0xNN` form) unchanged.
var streamIDSpecRegex = regexp.MustCompile(`:#0x[0-9a-fA-F]+`)

// scrubPlexInlineassFilesystemPathsInArgs runs
// scrubPlexInlineassFilesystemPaths over every `-filter_complex` value
// in args. Returns the new slice and true if any value was scrubbed.
// Safe on the input slice — only clones on first write so a no-op pass
// returns the original.
func scrubPlexInlineassFilesystemPathsInArgs(args []string) ([]string, bool) {
	out := args
	cloned := false
	didAny := false
	for i := 0; i+1 < len(out); i++ {
		if out[i] != "-filter_complex" {
			continue
		}
		newVal, did := scrubPlexInlineassFilesystemPaths(out[i+1])
		if !did {
			continue
		}
		if !cloned {
			out = append([]string(nil), args...)
			cloned = true
		}
		out[i+1] = newVal
		didAny = true
	}
	return out, didAny
}

// rewriteManifestName rewrites `-manifest_name <url>` in-place: the
// loopback URL (which workers can't reach) becomes the relay's base
// URL, with X_PLEX_TOKEN appended. ffmpeg then PUTs the manifest body
// to that URL natively via dashenc's HTTP protocol handler
// (scaleplex-ffmpeg7 backports Plex's -manifest_name extension —
// libavformat/dashenc.c patch 0095).
//
// If the URL doesn't look like a PMS loopback or no
// SCALEPLEX_PMS_BASE_URL is set, the flag is stripped — stock dashenc
// would otherwise write the manifest into a file literally named
// `http:`.
func rewriteManifestName(args []string, inputEnv map[string]string) ([]string, []string) {
	i := indexOfArg(args, "-manifest_name", 0)
	if i < 0 || i+1 >= len(args) {
		return args, nil
	}
	base := envOr("SCALEPLEX_PMS_BASE_URL", "")
	if envBase, ok := inputEnv["SCALEPLEX_PMS_BASE_URL"]; ok && envBase != "" {
		base = envBase
	}
	origURL := args[i+1]
	if base != "" && strings.HasPrefix(origURL, "http://127.0.0.1:32400") {
		rewritten := strings.Replace(origURL, "http://127.0.0.1:32400", base, 1)
		if tok, ok := inputEnv["X_PLEX_TOKEN"]; ok && tok != "" {
			if strings.Contains(rewritten, "?") {
				rewritten += "&X-Plex-Token=" + tok
			} else {
				rewritten += "?X-Plex-Token=" + tok
			}
		}
		args[i+1] = rewritten
		return args, []string{"manifest_name:rewrite-to-relay"}
	}
	args = removeArgs(args, i, 2)
	return args, []string{"drop:-manifest_name(no-pms-base-or-non-loopback)"}
}

// rewriteSegmentList rewrites every `-segment_list <url>` occurrence
// in args whose URL targets PMS loopback (http://127.0.0.1:32400).
// Each is rewritten to point at SCALEPLEX_PMS_BASE_URL with
// X-Plex-Token appended. On the HLS variant the relay also needs
// scaleplex_seg_time (so CSV row times can be rewritten to global)
// and scaleplex_mkv_offset_ms (so per-chunk Cluster.Timecode can be
// patched on matroska side-output for Plex Windows seek).
//
// DASH sessions with text subs also use a `-segment_list` for the
// side-channel ASS sub stream. That URL contains `?stream=subtitles`;
// it gets the same loopback rewrite (relay proxies the CSV POST
// to PMS, which then knows the sub-chunk-* filenames). No
// scaleplex_seg_time / mkv_offset on the side-channel variant —
// relay treats sub CSV rows as pass-through.
func rewriteSegmentList(args []string, inputEnv map[string]string, segTime string, changes *[]string, variant string) {
	base := envOr("SCALEPLEX_PMS_BASE_URL", "")
	if envBase, ok := inputEnv["SCALEPLEX_PMS_BASE_URL"]; ok && envBase != "" {
		base = envBase
	}
	if base == "" {
		return
	}
	tok := inputEnv["X_PLEX_TOKEN"]

	from := 0
	for {
		i := indexOfArg(args, "-segment_list", from)
		if i < 0 || i+1 >= len(args) {
			return
		}
		from = i + 2
		origURL := args[i+1]
		if !strings.HasPrefix(origURL, "http://127.0.0.1:32400") {
			continue
		}
		isSubChannel := strings.Contains(origURL, "stream=subtitles")
		// HLS path only handles its native sub-less segment_list; the
		// sub-channel pass picks up the stream=subtitles URL.
		if variant == "hls" && isSubChannel {
			continue
		}
		if variant == "side-channel" && !isSubChannel {
			continue
		}
		rewritten := strings.Replace(origURL, "http://127.0.0.1:32400", base, 1)
		appendQuery := func(kv string) {
			if strings.Contains(rewritten, "?") {
				rewritten += "&" + kv
			} else {
				rewritten += "?" + kv
			}
		}
		if tok != "" {
			appendQuery("X-Plex-Token=" + tok)
		}
		if variant == "hls" {
			if segTime != "" {
				appendQuery("scaleplex_seg_time=" + segTime)
			}
			// scaleplex_mkv_offset_ms retired 2026-05-13. With
			// scaleplex-ffmpeg7 patch 0103 the segment muxer no longer
			// rebases end_pts by reference_stream_first_pts, so
			// `-copyts` works on matroska seek sessions. Rewriter keeps
			// -copyts (see -copyts handling block below). Cluster.Timecode
			// already reflects absolute source PTS — relay has nothing to
			// patch. patchSessionMatroskaChunks stays in relay as
			// defensive no-op for any in-flight session that lingers
			// without the query param.
		}
		args[i+1] = rewritten
		tag := TagHLSSegmentListRewriteToRelay
		if variant == "side-channel" {
			tag = TagSubsSideChannelSegListToRelay
		}
		*changes = append(*changes, tag)
		// -segment_list_size left untouched. scaleplex-ffmpeg7 patch 0106
		// detects URL-handler outputs in segment_end and force-buffers
		// the full chunk history regardless of list_size. Plex's
		// `-segment_list_size 5` becomes inert for URL listfiles, which
		// is the only sane semantic when PMS aggregates from every PUT
		// body.
	}
}

// capturePMSProgressURL strips `-progressurl <url>` from args, rewrites
// the PMS host to SCALEPLEX_PMS_BASE_URL, and appends the per-session
// X_PLEX_TOKEN as a query param. Returns updated args, the rewritten
// URL (empty when capture failed for any reason), and the change tags
// to record.
//
// Also injects `-canthrottleurl <rewritten-url>` so scaleplex-ffmpeg7's
// in-binary canThrottle handler (patch 0097) can do its own one-shot
// PUT and apply per-input-packet `av_usleep` natively. Worker still
// reads progress via `-progress pipe:N` (substituted by main.go after
// spawn) for metrics + checkpoint; both consume the same response
// body, the per-packet sleep just becomes ffmpeg-internal.
func capturePMSProgressURL(args []string, inputEnv map[string]string) ([]string, string, []string) {
	i := indexOfArg(args, "-progressurl", 0)
	if i < 0 || i+1 >= len(args) {
		return args, "", nil
	}
	base := envOr("SCALEPLEX_PMS_BASE_URL", "")
	if envBase, ok := inputEnv["SCALEPLEX_PMS_BASE_URL"]; ok && envBase != "" {
		base = envBase
	}
	origURL := args[i+1]
	args = removeArgs(args, i, 2)
	if base == "" {
		return args, "", []string{"drop:-progressurl(no-pms-base)"}
	}
	rewritten := strings.Replace(origURL, "http://127.0.0.1:32400", base, 1)
	changes := []string{}
	if tok, ok := inputEnv["X_PLEX_TOKEN"]; ok && tok != "" {
		if strings.Contains(rewritten, "?") {
			rewritten += "&X-Plex-Token=" + tok
		} else {
			rewritten += "?X-Plex-Token=" + tok
		}
		changes = append(changes, TagProgressAppendXPlexToken)
	}
	changes = append(changes, TagProgressURLCapturedForReporter)
	// Inject -canthrottleurl pointing at the same relay endpoint.
	// Splice at index 0 so it lands in global-scope (before -i),
	// matching ffmpeg's option-context rules. Skip if the option is
	// somehow already present (shouldn't happen, but be defensive).
	//
	// SCALEPLEX_DISABLE_CANTHROTTLE (worker-pod env) suppresses the
	// injection entirely — a diagnostic escape hatch to A/B whether
	// canThrottle is responsible for live-session throughput collapse
	// on heavy (sub-burn) transcodes. With it set, ffmpeg runs
	// unthrottled; progress is still read via `-progress pipe:N`.
	if indexOfArg(args, "-canthrottleurl", 0) < 0 {
		if os.Getenv("SCALEPLEX_DISABLE_CANTHROTTLE") != "" {
			changes = append(changes, TagCanThrottleDisabledByEnv)
		} else {
			args = spliceArgs(args, 0, "-canthrottleurl", rewritten)
			changes = append(changes, TagInjectCanThrottleURL)
		}
	}
	return args, rewritten, changes
}

// upgradeLoglevelFromQuiet rewrites `-loglevel quiet|panic|fatal` to
// the value of `SCALEPLEX_FFMPEG_LOGLEVEL` env (default "info").
// PMS's JobRunner expects "Stream mapping:" lines on stderr to detect
// transcoder readiness; quiet stalls /header for ~125s.
//
// SCALEPLEX_FFMPEG_LOGLEVEL=debug exposes the per-cycle
// `scaleplex/ct: PUT/avio_read/body` diagnostics emitted by patch
// 0097's canThrottle handler. The state-transition `throttle ON|OFF`
// lines are AV_LOG_INFO so they show at the default level.
func upgradeLoglevelFromQuiet(args []string) ([]string, bool) {
	if i := indexOfArg(args, "-loglevel", 0); i >= 0 && i+1 < len(args) {
		if v := args[i+1]; v == "quiet" || v == "panic" || v == "fatal" {
			args[i+1] = envOr("SCALEPLEX_FFMPEG_LOGLEVEL", "info")
			return args, true
		}
	}
	return args, false
}

// dropNostatsFlag removes `-nostats` so ffmpeg emits its periodic
// `size= time= bitrate= speed=` line for PMS's stats parser.
func dropNostatsFlag(args []string) ([]string, bool) {
	if i := indexOfArg(args, "-nostats", 0); i >= 0 {
		return removeArgs(args, i, 1), true
	}
	return args, false
}

// stripEAEEnvVars removes Plex-Transcoder-private env vars that break
// stock ffmpeg on the worker:
//   - EAE_ROOT / FFMPEG_EXTERNAL_LIBS point at Plex Transcoder paths
//     that don't exist on the worker pod.
//   - OCL_ICD_VENDORS is set to "0" by PMS to disable OpenCL ICD
//     discovery in its bundled ffmpeg. Inherited by a worker ffmpeg it
//     makes the OpenCL loader scan a bogus path → zero platforms →
//     clGetPlatformIDs returns -1001, which kills any tonemap_opencl
//     HDR transcode. Stripping it lets ocl-icd fall back to its
//     default /etc/OpenCL/vendors, where the Intel ICD lives.
//
// X_PLEX_TOKEN is intentionally KEPT for the progress reporter.
func stripEAEEnvVars(env map[string]string) (map[string]string, []string) {
	var changes []string
	for _, k := range []string{"EAE_ROOT", "FFMPEG_EXTERNAL_LIBS", "OCL_ICD_VENDORS"} {
		if _, ok := env[k]; ok {
			delete(env, k)
			changes = append(changes, TagPrefixEnvStrip+k)
		}
	}
	return env, changes
}

// inputDecoderHintFlag returns true when arg s is an input-side
// decoder-codec flag. Matches all ffmpeg stream-specifier shapes
// per ffmpeg-all(1):
//
//	-codec:STREAMSPEC, -c:STREAMSPEC
//	-c:v / -c:a / -c:s / -c:d / -c:t  (type-only shorthand)
//
// where STREAMSPEC is anything: digit ("1"), type+index ("a:0"),
// stream-id ("#0x02" — Plex Transcoder's preferred form), program,
// metadata pattern, etc. We don't validate; whatever PMS sends, the
// flag pair gets dropped on bail.
func inputDecoderHintFlag(s string) bool {
	if s == "-c:v" || s == "-c:a" || s == "-c:s" || s == "-c:d" || s == "-c:t" {
		return true
	}
	for _, prefix := range []string{"-codec:", "-c:"} {
		if rest, ok := strings.CutPrefix(s, prefix); ok && rest != "" {
			return true
		}
	}
	return false
}

// dropInputAudioDecoderHints removes every input-side `-codec:* <value>`
// pair (any stream-spec form) plus any orphaned `-eae_prefix:N`. Stock
// ffmpeg auto-detects each stream's decoder from codec_id when no hint
// is set; PMS often pipes a wrong hint (e.g. `-codec:#0x02 aac` for an
// EAC3 audio stream) expecting Plex's bundled EAE engine to bridge it.
// Without this drop, stock ffmpeg fails on the bitstream parse with
// empty stderr (because `-loglevel quiet`) and exit status 8. Used in
// the no-decoder bail path — caller has already decided the rewriter
// can't reason about this argv shape, so deferring to ffmpeg auto-
// detection is the safest forward path.
func dropInputAudioDecoderHints(args []string) ([]string, []string) {
	var changes []string
	for {
		removed := false
		iIdx := indexOfArg(args, "-i", 0)
		if iIdx < 0 {
			break
		}
		for i := 0; i < iIdx && i+1 < len(args); i++ {
			if !inputDecoderHintFlag(args[i]) {
				continue
			}
			tag := "drop:" + args[i] + "=" + args[i+1] + "(bail)"
			args = removeArgs(args, i, 2)
			changes = append(changes, tag)
			removed = true
			break
		}
		if !removed {
			break
		}
	}
	// Also drop -eae_prefix:N pairs — orphaned now that the codec
	// hint they referenced is gone.
	for {
		removed := false
		for i := 0; i < len(args); i++ {
			if strings.HasPrefix(args[i], "-eae_prefix") && i+1 < len(args) {
				args = removeArgs(args, i, 2)
				changes = append(changes, TagDropEAEPrefixBail)
				removed = true
				break
			}
		}
		if !removed {
			break
		}
	}
	return args, changes
}

// setWorkerHomeEnv repoints HOME at the worker image's pre-populated
// /home/ubuntu (with fontconfig cache) so libass fontselect lands ms-
// fast instead of blocking 2+ minutes on a fresh corpus scan.
func setWorkerHomeEnv(env map[string]string) map[string]string {
	env["HOME"] = envOr("HW_RUNTIME_HOME", "/home/ubuntu")
	return env
}

// isOptimizeRemux fingerprints the Plex Optimize remux fast-path: bare
// stock decoder (`-codec:0 <short>` before `-i`, no `-hwaccel:0`)
// paired with `-codec:0 copy` on the first video output. PMS emits
// this when the Optimize target preset matches the source resolution/
// bitrate and video can be passed through. Used by Rewrite() to skip
// the transcode-side pipeline (HW init, decoder upgrade, filter graph,
// encoder swap, SEI inject) and run only the transport/audio/env tail.
// Without this branch the main rewriter would bail with
// "unknown-decoder:<short>" (decoder allowlist requires a paired
// hwaccel) and Optimize never works on non-AV1 sources — observed
// 2026-05-10 with Pat & Mat (h264) and All Creatures (hevc) → ffmpeg
// exit 8 on "Unknown decoder 'eac3_eae'".
func isOptimizeRemux(args []string) bool {
	iIdx := indexOfArg(args, "-i", 0)
	if iIdx < 0 {
		return false
	}
	dIdx := streamSpecIndex(args, "-codec", 0, 0)
	if dIdx < 0 || dIdx >= iIdx || dIdx+1 >= len(args) {
		return false
	}
	if _, ok := activeDialect.hwDecodeShortCodecs()[args[dIdx+1]]; !ok {
		return false
	}
	if streamSpecIndex(args, "-hwaccel", 0, 0) >= 0 {
		return false
	}
	encIdx := streamSpecIndex(args, "-codec", 0, iIdx+1)
	if encIdx < 0 || encIdx+1 >= len(args) || args[encIdx+1] != "copy" {
		return false
	}
	return true
}

func Rewrite(inputArgs []string, inputEnv map[string]string, opts *RewriteOpts) RewriteResult {
	sessionDir := ""
	if opts != nil {
		sessionDir = opts.SessionDir
	}

	// Default the VAAPI driver to whatever the local GPU's PCI vendor maps to
	// (Intel=iHD, AMD=radeonsi, ...) — #124. Operator can pin with
	// HW_VAAPI_DRIVER. Falls back to "iHD" on unknown vendor / probe failure
	// (back-compat for the historical Intel fleet).
	vaapiDriver := envOr("HW_VAAPI_DRIVER", detectVAAPIDriver())
	// Image-resident defaults: Ubuntu's intel-media-va-driver-non-free
	// installs iHD_drv_video.so under /usr/lib/x86_64-linux-gnu/dri and
	// libva auto-discovers it. HW_LIBVA_DRIVERS_PATH only needs to be
	// non-empty when overriding (e.g. talking to a Plex-bundled cache).
	libvaDriversPath := os.Getenv("HW_LIBVA_DRIVERS_PATH")

	changes := []string{}

	// Scrub Plex's PMS-install-only AVOption keys (`font_path`,
	// `fontconfig_file`) from every inlineass= filter node. PMS's argv
	// embeds absolute paths under /usr/lib/plexmediaserver/Resources/...
	// that don't exist on the worker image; libass fails fontconfig init
	// and ffmpeg exits 145 on ALL force-burn sessions
	// ([[project_scaleplex_sub_burn_prod_bug_2026_05_30]]). Runs before
	// the bail closure is defined so both the bail path (which re-clones
	// inputArgs) and the main reshape path see the cleaned graph — fixes
	// the SW-passthrough case (rewriter applies skip:no-decoder bail but
	// leaves Plex's libx264+inlineass argv otherwise untouched) and the
	// HW-reshape case (extractGraphFacts now captures scrubbed params)
	// from the single call site.
	inlineassPathsScrubbed := false
	if scrubbed, did := scrubPlexInlineassFilesystemPathsInArgs(inputArgs); did {
		inputArgs = scrubbed
		inlineassPathsScrubbed = true
		changes = append(changes, TagFilterInlineassScrubPlexFontPaths)
	}

	// Plex's stream-id-by-id specifier syntax (`-codec:#0xNN`, `[0:#0xNN]`),
	// emitted on files where the container's first video stream isn't at index 0
	// (Plex Versions / Optimized for TV outputs, high-PID m2ts), is no longer
	// rewritten to ordinal form. Every `-flag:0` detector is now a streamSpecIndex
	// call that resolves `#0xNN` → ordinal at match time (streamIDOrdinalMap), so
	// PMS's argv reaches ffmpeg (which accepts the `#0xNN` form natively) pristine.
	// This replaces the upfront normalizePlexStreamSpecsToOrdinal pass (#145); the
	// regression it guarded against (HW-decode detector misses `-hwaccel:#0x01` →
	// bail skip:no-decoder → exit 145 on dash, live repro 2026-05-31 Ghosts S2E1)
	// is now prevented by the polymorphic matchers themselves.

	// Tone-mapping backend (opencl vs vaapi) for the tonemap Plex's
	// argv asked for. scaleplex never decides whether to tonemap.
	tm := resolveTonemapConfig()

	// On bail we don't run the full rewriter, but `-progressurl` must
	// still come off — its URL is loopback-only, ffmpeg would try to
	// PUT and fail. PMS emits it on every spawn, including audio-only
	// Detection jobs that bail with skip:no-decoder.
	// `-loglevel_plex` + `-strict_ts:N` pass through natively (fork
	// patches 0098/0107). `-xioerror` was never observed in corpus.
	scrubPlexFlagsOnBail := func(args []string) ([]string, []string) {
		var bailChanges []string
		for {
			i := indexOfArg(args, "-progressurl", 0)
			if i < 0 || i+1 >= len(args) {
				break
			}
			args = removeArgs(args, i, 2)
			bailChanges = append(bailChanges, TagDropProgressurlBail)
		}
		// `-segment_list http://127.0.0.1:32400/...` — PMS loopback URL that
		// the segment muxer PUTs the CSV manifest to. Worker pod's loopback
		// has no PMS; ffmpeg gets ECONNREFUSED and exits with the muxer's
		// task-error status (~145). Rewrite to SCALEPLEX_PMS_BASE_URL +
		// X-Plex-Token, same shape as the full rewriter's rewriteSegmentList.
		// Observed 2026-05-14 on LG webOS sidecar SRT side-channel.
		base := envOr("SCALEPLEX_PMS_BASE_URL", "")
		if envBase, ok := inputEnv["SCALEPLEX_PMS_BASE_URL"]; ok && envBase != "" {
			base = envBase
		}
		if base != "" {
			tok := inputEnv["X_PLEX_TOKEN"]
			from := 0
			for {
				i := indexOfArg(args, "-segment_list", from)
				if i < 0 || i+1 >= len(args) {
					break
				}
				from = i + 2
				orig := args[i+1]
				if !strings.HasPrefix(orig, "http://127.0.0.1:32400") {
					continue
				}
				rewritten := strings.Replace(orig, "http://127.0.0.1:32400", base, 1)
				if tok != "" {
					if strings.Contains(rewritten, "?") {
						rewritten += "&X-Plex-Token=" + tok
					} else {
						rewritten += "?X-Plex-Token=" + tok
					}
				}
				args[i+1] = rewritten
				bailChanges = append(bailChanges, TagBailSegmentListRewriteToRelay)
			}
		}
		// `-manifest_name http://127.0.0.1:32400/...` — DASH equivalent
		// of the segment_list block above. dashenc fork patch 0095 POSTs
		// the .mpd body to that URL on each rewrite; loopback unreachable
		// from the worker pod → ECONNREFUSED → ffmpeg exit 145 with a
		// `-loglevel quiet` argv emitting no fatal to stderr (only
		// fontconfig warnings if libass also opened). Live regression
		// repro 2026-05-31 on Ghosts S2E1 force-burn before the
		// stream-spec normalizer engaged → bail path stayed live for the
		// dash class. Reuses the main-path helper so loopback rewrites
		// stay identical between bail and full reshape.
		if mnArgs, mnTags := rewriteManifestName(args, inputEnv); len(mnTags) > 0 {
			args = mnArgs
			for _, t := range mnTags {
				switch t {
				case "manifest_name:rewrite-to-relay":
					bailChanges = append(bailChanges, TagBailManifestNameRewriteToRelay)
				case "drop:-manifest_name(no-pms-base-or-non-loopback)":
					bailChanges = append(bailChanges, TagBailManifestNameDrop)
				default:
					bailChanges = append(bailChanges, t+"(bail)")
				}
			}
		}
		return args, bailChanges
	}
	bail := func(reason string) RewriteResult {
		args := cloneArgs(inputArgs)
		args, scrub := scrubPlexFlagsOnBail(args)
		merged := append([]string{}, changes...)
		merged = append(merged, scrub...)
		// Detection / audio-only ML pre-pass argv (output to
		// /transcode/Transcode/Detection/<uuid>) bails with no-decoder
		// because there's no `-codec:0` for video. Those argvs ALSO
		// carry input-side audio decoder hints — `-codec:N <hint>`
		// before `-i` — that PMS pipes through expecting Plex's EAE
		// engine to decode the bytes regardless of source codec.
		// Stock ffmpeg honours the hint literally: when PMS sends
		// `-codec:1 aac` for a stream that's actually EAC3/AC3/DTS
		// (very common — PMS uses one canonical hint per probe call),
		// the AAC decoder fails on the bitstream parse and ffmpeg
		// exits 8.
		//
		// Drop those input-side hints in the no-decoder bail. ffmpeg
		// auto-detects the decoder from each stream's codec_id when
		// no hint is set, which always picks correctly.
		var hintChanges []string
		if reason == "no-decoder" {
			args, hintChanges = dropInputAudioDecoderHints(args)
			merged = append(merged, hintChanges...)
		}
		// EAE safety net on EVERY bail. A bail returns before the main
		// path's swapEAEAudioDecoders (step 9), so any `-codec:N <X>_eae`
		// PMS emitted survives into the bailed argv. The fork has no
		// `*_eae` decoder/encoder (Plex's EasyAudioEncoder is a
		// Plex-Transcoder-only socket engine) → ffmpeg exits 8 "Unknown
		// decoder 'eac3_eae'" → client transcoder error. Most exposed: a
		// FORCE_HW=1 hybrid bail (`hw-decode:unexpected-encoder:libx264`)
		// on HDR-remux content (~all EAC3/TrueHD/Atmos). Swap to stock
		// codecs + drop the orphaned `-eae_prefix:N` so the bailed session
		// degrades to "plays" instead of erroring. For no-decoder the
		// input hints (incl. eae) are already gone above; this then only
		// catches any output-side eae and is otherwise a no-op.
		var eaeSwapped [][2]string
		args, eaeSwapped = swapEAEAudioDecoders(args)
		for _, p := range eaeSwapped {
			merged = append(merged, TagPrefixAudio+p[0]+"->"+p[1]+"(bail)")
		}
		var eaeDropped []string
		args, eaeDropped = dropEAEPrefixFlags(args)
		for _, d := range eaeDropped {
			merged = append(merged, TagPrefixDrop+d+"(bail)")
		}
		// Same safety-net rationale as the EAE swap above: a bail
		// returns before the main path's dropFramedropBSF (step 9
		// neighbour), so any `-bsf:N framedrop=*` PMS emitted survives
		// into the bailed argv. scaleplex-ffmpeg7 has no `framedrop`
		// BSF → ffmpeg exits 8 "Bitstream filter not found" before any
		// chunks are written. Strip on every bail so the bailed
		// session at least reaches open-output cleanly.
		var fdDropped []string
		args, fdDropped = dropFramedropBSF(args)
		for _, d := range fdDropped {
			merged = append(merged, TagPrefixDrop+d+"(framedrop)(bail)")
		}
		merged = append(merged, TagPrefixSkip+reason)
		// Applied=true whenever we mutated argv (scrub, hint drops,
		// EAE swap/prefix-drop, framedrop-BSF drop, OR the top-of-
		// Rewrite inlineass font-path scrub that ran on inputArgs
		// before bail() was invoked — without that bit the caller
		// would see Applied=false on a scrub-only bail and execute
		// the unsanitized original argv, re-triggering the very
		// libass fontconfig exit-145 the scrub exists to prevent).
		applied := inlineassPathsScrubbed ||
			len(scrub) > 0 || len(hintChanges) > 0 ||
			len(eaeSwapped) > 0 || len(eaeDropped) > 0 ||
			len(fdDropped) > 0
		return RewriteResult{
			Args:    args,
			Env:     cloneEnv(inputEnv),
			Applied: applied,
			Changes: merged,
		}
	}

	args := cloneArgs(inputArgs)
	env := cloneEnv(inputEnv)

	// Normalize Plex's OpenCL HW-tonemap filter chain. PMS with `Use
	// hardware-accelerated tone mapping` ON emits a hwmap=opencl →
	// tonemap_opencl → hwmap=vaapi-reverse chain. By default we re-emit
	// it in canonical comma form keeping Plex's chosen algorithm;
	// SCALEPLEX_TONEMAP=vaapi collapses it to fixed-curve tonemap_vaapi.
	// Runs before phase 1 so any later filter-chain inspection sees the
	// already-substituted form.
	if newArgs, did := substituteOpenCLTonemap(args, tm); did {
		args = newArgs
		if tm.useOpenCL {
			changes = append(changes, TagFilterTonemapOpenCLNormalized)
		} else {
			changes = append(changes, TagFilterTonemapOpenCLToVAAPI)
		}
	}

	// Plex Optimize remux fast-path detection. PMS emits a bare
	// `-codec:0 h264` (or hevc/av1/vp9) input decoder — no
	// `-hwaccel:0` — paired with `-codec:0 copy` on the first video
	// output, when the Optimize target preset already matches the
	// source resolution / bitrate and video can be passed through.
	// Worker has nothing to do video-side; the transcode pipeline
	// (init_hw_device, decoder upgrade, filter chain, encoder swap,
	// SEI inject) does not apply. The argv still carries Plex-private
	// flags (-loglevel_plex, -progressurl) and EAE audio decoders
	// (-codec:N eac3_eae) that stock ffmpeg can't handle — those run
	// through the common tail below regardless of isRemux. Without
	// gating the transcode block off, the decoder allowlist would
	// bail with "unknown-decoder:<short>" (no paired hwaccel) and
	// Optimize would never work on non-AV1 sources — observed
	// 2026-05-10 with Pat & Mat (h264) and All Creatures (hevc) →
	// ffmpeg exit 8 on "Unknown decoder 'eac3_eae'".
	isRemux := isOptimizeRemux(args)

	// Detect output format up-front so format-specific rewrites can branch.
	// Plex's argv ends with one of:
	//   -f dash           — DASH (Plex Web Chrome via dashenc, mp4 chunks)
	//   -f ssegment       — Plex's stream-segmenter, used for HLS-style output
	//                       to mobile clients (iOS/Android). Stock ffmpeg
	//                       doesn't have ssegment; we translate to `-f segment`.
	//   -f segment        — Plex Windows desktop (segmented matroska, live=1).
	//                       Already-stock muxer; same chunk-list shape as HLS
	//                       so we treat them identically below: rewrite
	//                       -segment_list URL to relay, drop -copyts so
	//                       splits actually fire.
	outputFormat := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-f" {
			outputFormat = args[i+1]
			break
		}
	}
	isHLS := outputFormat == "ssegment" || outputFormat == "segment"

	// Transcode-side block. Gated off on Optimize remux (no video work
	// — bare decoder → -codec:0 copy). The common tail below (EAE
	// swap, manifest_name / segment_list / progressurl rewrites,
	// loglevel / env scrubs) runs unconditionally.
	if !isRemux {

		for i := 0; i < len(args); i++ {
			if args[i] != "-filter_complex" {
				continue
			}
			f := ""
			if i+1 < len(args) {
				f = args[i+1]
			}
			if strings.Contains(f, "subtitles=") {
				return bail(TagBailReasonSubtitlesBurnIn)
			}
			// HDR (zscale/tonemap) is rewritten to tonemap_vaapi by
			// rewriteVideoFilter; only video chains starting with [0:0] are
			// in-scope. Bail kept off — phase 1 smoke proved tonemap_vaapi
			// works on real HDR10.
		}

		inputIdx := indexOfArg(args, "-i", 0)
		if inputIdx < 0 {
			return bail(TagBailReasonNoInput)
		}

		// 0. Plex-Pass gate (scaleplex#78, L3, fail-closed). Gate ONLY the path
		// that genuinely GRANTS HW the user may not be entitled to:
		// SCALEPLEX_FORCE_HW=1 forcing HW onto an argv Plex emitted as SOFTWARE
		// video (Plex chose SW → the user may have no Pass). ANY HW source —
		// foreign (cross-backend, #77) OR same-backend passthrough — is NOT
		// gated: Plex only ever emits a HW argv for an active Pass (it gates HW
		// transcode itself), so the HW argv IS proof of entitlement and
		// reshaping/forcing it grants nothing Plex didn't already grant. Gating
		// HW sources was both wrong (over-gates a Pass user) and fragile: an
		// EXTERNAL worker can't reach the in-cluster SCALEPLEX_PMS_BASE_URL for
		// the L3 probe → it fail-closed every cross-backend session into an
		// un-runnable foreign-HW passthrough (#99). Query PMS only for the
		// FORCE_HW-on-SW case, so honor-source + cross-backend pay no network cost.
		forceHWEnv := envBool("SCALEPLEX_FORCE_HW")
		hwReaccelOK := true
		if forceHWEnv && detectSourceBackend(args) == srcSW {
			hwReaccelOK = hwAccelAllowed(inputEnv)
			if !hwReaccelOK {
				changes = append(changes, TagPassGateDenied)
			}
		}

		// 0b. Cross-backend reshape (scaleplex#77). If PMS shaped this argv for
		// a HW backend other than the worker's (e.g. a VAAPI-configured PMS
		// dispatching to a NVIDIA worker), translate decode flags + filter
		// graph + encoder to the worker's native backend FIRST, so the
		// honor-source logic below runs on a native argv. No-op when source ==
		// worker or source is SW/none. In-place value swaps only — no length
		// change, so inputIdx stays valid. NOT Pass-gated: a foreign HW source
		// is itself proof of Pass (see gate note above), so hwReaccelOK is
		// always true here for a foreign-HW source.
		if activeDialect.backendName() == "sw" {
			// No-GPU worker: downgrade ANY incoming argv (foreign HW / hybrid)
			// to a pure-CPU pipeline, so the honor-source path below keeps it.
			// Not Pass-gated — downgrading HW→SW grants no entitlement; the gate
			// above only probes for FORCE_HW on a SW source.
			if reshaped, swChanges := reshapeToSoftware(args, tm); len(swChanges) > 0 {
				args = reshaped
				changes = append(changes, swChanges...)
			}
		} else if hwReaccelOK {
			if reshaped, ccChanges := reshapeForeignHWArgv(args, tm); len(ccChanges) > 0 {
				args = reshaped
				changes = append(changes, ccChanges...)
			}
		}
		// reshapeToSoftware REMOVES args (strips HW decode flags), changing the
		// arg length — unlike reshapeForeignHWArgv's in-place value swaps. Recompute
		// inputIdx so the decoder/encoder phases below index the right -i.
		inputIdx = indexOfArg(args, "-i", 0)
		if inputIdx < 0 {
			return bail(TagBailReasonNoInput)
		}

		// 1. Decoder.
		//
		// Two argv shapes:
		//
		//   SW-decode (Plex's `HardwareAcceleratedCodecs=0` or no HW probe):
		//     -codec:0 libdav1d -i ...                 (libhevc / libx264)
		//   PMS lets the worker handle HW decode by rewriting decoder to
		//   the native codec name + injecting -hwaccel:0 vaapi flags below.
		//
		//   HW-decode (`HardwareAcceleratedCodecs=1` and HW probe succeeded):
		//     -codec:0 av1 -hwaccel:0 vaapi -hwaccel_output_format:0 vaapi
		//     -hwaccel_device:0 vaapi -i ...
		//   PMS already produced the full VAAPI argv: short codec name,
		//   hwaccel flags, scale_vaapi filter chain, h264_vaapi / hevc_vaapi
		//   encoder with -qp:0 directly. We pass that through and only do
		//   the Plex-quirk strips (phases 9-24).
		decCodecIdx := streamSpecIndex(args, "-codec", 0, 0)
		if decCodecIdx < 0 || decCodecIdx >= inputIdx {
			return bail(TagBailReasonNoDecoder)
		}
		swDecoder := args[decCodecIdx+1]

		// Honor Plex's HW/SW decision PER AXIS (docs/HW_PROFILE.md). Plex picks
		// decode and encode independently; we honor both unless SCALEPLEX_FORCE_HW
		// is set (the homelab's all-GPU fleet sets it → always re-accelerate, so
		// honor is a no-op there). Backend targeting (which GPU) stays node-local
		// regardless (patch 0116) — these flags are only the HW-vs-SW axis.
		//   - honorSW     = no `-hwaccel:0` + SW encoder → full SW on the worker.
		//   - honorHybrid = `-hwaccel:0` (HW decode) + SW encoder → keep HW decode
		//     (+ device) and SW-encode on CPU. The smart CPU-offload mode: the
		//     heavy decode stays on the GPU, only the encode is CPU (realtime even
		//     for 4K sources, unlike full SW which is decode-bound).
		// forceHW honors the Plex-Pass gate computed above (#78): the env asks
		// for re-accel, but without a confirmed Pass we fall back to honoring
		// Plex's SW pipeline (fail-closed).
		// A SW worker has no HW to force onto — FORCE_HW is meaningless there;
		// keep honor-SW so reshapeToSoftware's output is kept as-is.
		forceHW := forceHWEnv && hwReaccelOK && activeDialect.backendName() != "sw"
		plexSWEncoder := false
		if peer := streamSpecIndex(args, "-codec", 0, inputIdx+1); peer > 0 && peer+1 < len(args) {
			_, plexSWEncoder = activeDialect.encoderMap()[args[peer+1]]
		}
		noHwaccel := streamSpecIndex(args, "-hwaccel", 0, 0) < 0
		honorSW := plexSWEncoder && noHwaccel && !forceHW
		honorHybrid := plexSWEncoder && !noHwaccel && !forceHW

		// Counterfactual logging: when FORCE_HW=1 masks a session we WOULD
		// have honored (Plex staged a SW encoder), emit a diagnostic tag so
		// prod logs quantify real SW exposure before flipping FORCE_HW off
		// (docs/HW_PROFILE.md). No behaviour change — purely observational.
		// The session still re-accelerates to HW below.
		if forceHW && plexSWEncoder {
			if noHwaccel {
				changes = append(changes, TagForceHWWouldHonorSW)
			} else {
				changes = append(changes, TagForceHWWouldHonorHWDecSWEnc)
			}
		}

		isHWDecode := false
		if honorSW {
			// Honoring Plex's SW pipeline: leave the decoder as PMS emitted it
			// (no VAAPI swap, no -hwaccel inject). isHWDecode stays false.
		} else if _, isShort := activeDialect.hwDecodeShortCodecs()[swDecoder]; isShort {
			if streamSpecIndex(args, "-hwaccel", 0, 0) >= 0 {
				isHWDecode = true
				changes = append(changes, TagPrefixDecodeHWPassthrough+swDecoder)
			} else {
				// Bare short codec name (hevc/h264/av1/vp9) without -hwaccel:0.
				// Only safe to auto-upgrade when the encoder side is genuinely
				// SW-shaped (libx264/libx265) — that's the case where PMS
				// staged a SW pipeline but happened to emit the canonical
				// codec name in the decoder slot. If the encoder is already
				// HW-shaped (e.g. h264_vaapi), the argv is malformed in a way
				// we can't safely reshape; bail rather than guess.
				if peer := streamSpecIndex(args, "-codec", 0, inputIdx+1); peer > 0 && peer+1 < len(args) {
					if _, isSW := activeDialect.encoderMap()[args[peer+1]]; !isSW {
						return bail(TagPrefixBailUnknownDecoder + swDecoder)
					}
				} else {
					return bail(TagPrefixBailUnknownDecoder + swDecoder)
				}
				args = spliceArgs(args, decCodecIdx+2,
					"-hwaccel:0", activeDialect.hwaccelName(),
					"-hwaccel_output_format:0", activeDialect.hwaccelOutputFormat(),
					"-hwaccel_device:0", activeDialect.filterHWDeviceName(),
				)
				changes = append(changes, TagPrefixDecodeBareHWUpgrade+swDecoder)
			}
		} else if hwDecoder, ok := activeDialect.decoderMap()[swDecoder]; ok {
			args[decCodecIdx+1] = hwDecoder
			args = spliceArgs(args, decCodecIdx+2,
				"-hwaccel:0", activeDialect.hwaccelName(),
				"-hwaccel_output_format:0", activeDialect.hwaccelOutputFormat(),
				"-hwaccel_device:0", activeDialect.filterHWDeviceName(),
			)
			changes = append(changes, TagPrefixDecode+swDecoder+"->"+hwDecoder)
		} else {
			return bail(TagPrefixBailUnknownDecoder + swDecoder)
		}

		// FORCE_HW=1 + Plex hybrid (HW decode + SW encode): under the homelab's
		// force-HW intent we honor neither (honorHybrid is off when forceHW) nor
		// bail — we reshape the SW filter+encode tail to VAAPI. Plex's hybrid
		// argv HW-decodes but runs the filter chain AND encoder in software
		// (`[0:0]scale=...`, `tonemap=...`, `inlineass=...`, libx264 — captured
		// live 2026-05-24, Avatar 4K HDR with PMS "HW encoding" off). That tail
		// is shape-identical to a SW-decode session's, so it reshapes through the
		// same path (rewriteVideoFilter + encoder swap) while keeping the
		// existing HW decode. Removes the `hw-decode:unexpected-encoder:libx264`
		// bail landmine and honors force-HW (GPU encode, not CPU). A hybrid whose
		// graph is already partly-VAAPI (no matching SW regex) falls through to
		// the bail, which now degrades to "plays" via the EAE safety net.
		// NOTE: zero argv-corpus coverage (homelab PMS never emits hybrid) —
		// validate on plex-test with PMS "HW encoding" unchecked before prod.
		hybridForceHW := isHWDecode && forceHW && plexSWEncoder

		// Detect subtitle source up-front so later phases can act on it.
		// Pass-through is hardcoded: the fork's scaleplex_inlineass binding
		// owns the sidecar -i + null-sub output, so we never drop them.
		var earlySubSrc *subtitleSource
		if (!isHWDecode || hybridForceHW) && !honorSW {
			var probe func(string, string) string
			if opts != nil && opts.ProbeSubtitleCodec != nil {
				probe = opts.ProbeSubtitleCodec
			}
			earlySubSrc = detectSubtitleSource(args, sessionDir, probe)
		}

		// 2. -init_hw_device. The scaleplex-ffmpeg fork retargets the VAAPI
		// device at open time from SCALEPLEX_RENDER_DEVICE (patch
		// 0116-vaapi-device-env-retarget); the driver comes from
		// LIBVA_DRIVER_NAME, which the rewriter injects into the subprocess
		// env below. So the device path Plex baked in (its own host's
		// HardwareDevicePath) is irrelevant on the worker — we leave Plex's
		// -init_hw_device untouched and only inject the option when Plex
		// emitted none (a pure SW session being HW-accelerated). On VAAPI
		// the injected `vaapi=vaapi:` is filled by the fork's env override
		// at device-open; on NVIDIA the dialect emits `cuda=cuda:N` against
		// the worker-local device index. Skipped entirely when honoring a
		// SW session — no HW device needed.
		// Cross-backend (#85): if a -init_hw_device for a DIFFERENT backend is
		// present (e.g. a VAAPI-shaped argv — or a SW argv carrying a stale
		// `vaapi=vaapi:` rewrite artifact — landing on a nvenc worker), the
		// fork's same-backend device retarget (patch 0116) does NOT apply, so
		// leaving it causes `Device creation failed … 'vaapi=vaapi:'`. Drop the
		// foreign pair so the inject-below recreates them for the worker dialect
		// in correct global (pre -i) position. A matching-backend init is left
		// untouched (the fork env-retargets it).
		if !honorSW {
			if ihd := indexOfArg(args, "-init_hw_device", 0); ihd >= 0 && ihd+1 < len(args) {
				if hwDeviceBackend(args[ihd+1]) != hwDeviceBackend(activeDialect.initHWDeviceArg(0)) {
					for _, flag := range []string{"-init_hw_device", "-filter_hw_device"} {
						if i := indexOfArg(args, flag, 0); i >= 0 && i+1 < len(args) {
							args = removeArgs(args, i, 2)
						}
					}
					changes = append(changes, TagReplaceForeignInitHWDevice)
				}
			}
		}
		// Strip Plex's hardcoded `,driver=iHD` from a same-backend VAAPI
		// init_hw_device — its presence forces libva to load iHD regardless of
		// LIBVA_DRIVER_NAME env, breaking AMD radeonsi (or any non-Intel) hosts.
		// With it stripped, libva falls back to LIBVA_DRIVER_NAME (set by the
		// rewriter at the spawn-env section below) — vendor-aware default from
		// detectVAAPIDriver(). #124.
		if !honorSW && activeDialect.backendName() == "vaapi" {
			if ihd := indexOfArg(args, "-init_hw_device", 0); ihd >= 0 && ihd+1 < len(args) {
				if stripped := stripVAAPIDriverParam(args[ihd+1]); stripped != args[ihd+1] {
					args[ihd+1] = stripped
					changes = append(changes, TagStripVAAPIDriverParam)
				}
			}
		}
		if !honorSW && indexOfArg(args, "-init_hw_device", 0) < 0 {
			// Inject -init_hw_device + -filter_hw_device BEFORE the first
			// -i so they're parsed as global options. Placing them after
			// -i puts them in ffmpeg's per-output option scope, where
			// -init_hw_device's per-input dispatch silently fails to bind
			// the hwaccel device to the input stream — ffmpeg parses the
			// option but the av1 hwaccel decoder doesn't see it,
			// resulting in "No VA display found for device vaapi" /
			// "No device available for decoder" at filter graph build.
			// Live repro 2026-05-09 session 7561 (SW HDR + sub-burn The
			// Accountant on Plex Android): rewriter applied with both
			// drop:-i(sidecar-input) and inject:init_hw_device, but
			// ffmpeg still failed to bind vaapi until injection moved
			// to global position.
			newInputIdx := indexOfArg(args, "-i", 0)
			args = spliceArgs(args, newInputIdx,
				"-init_hw_device", activeDialect.initHWDeviceArg(0),
				"-filter_hw_device", activeDialect.filterHWDeviceName(),
			)
			changes = append(changes, TagInjectInitHWDevice)
		}

		// Locate output -codec:0 (after -i) up-front; both SW and HW paths
		// reference it for later phases (CRF→QP, preset→cl, sei inject).
		newInputIdx := indexOfArg(args, "-i", 0)
		encCodecIdx := streamSpecIndex(args, "-codec", 0, newInputIdx+1)
		if encCodecIdx < 0 {
			return bail(TagBailReasonNoEncoder)
		}

		mediaPath := ""
		if i := indexOfArg(args, "-i", 0); i >= 0 && i+1 < len(args) {
			mediaPath = args[i+1]
		}

		// HDR-source detection — session-level, run once. Previously each
		// downstream branch re-probed (SW-reshape, HW-decode-passthrough,
		// HW-decode-text-sub-burn) and each emitted its own
		// `video:hdr-source(<transfer>)` change tag, surfacing the tag
		// 2× on sessions that hit both the HW-decode-passthrough block
		// AND the HW-decode-text-sub-burn sub-branch (HEVC/AV1 HDR +
		// text SRT/ASS burn, ~10% of HDR transcodes). Cosmetic but
		// confusing in logs and grep tooling. Hoist the probe + emit
		// here so every downstream consumer reads `sourceIsHDR` from
		// the same fact.
		var rewritten *filterRewrite
		var subSrc *subtitleSource
		sourceIsHDR := false
		if opts != nil && opts.ProbeVideoColor != nil && mediaPath != "" {
			if transfer, _, _ := opts.ProbeVideoColor(mediaPath); isHDRTransfer(transfer) {
				sourceIsHDR = true
				changes = append(changes, TagPrefixVideoHDRSource+strings.ToLower(transfer)+")")
			}
		}

		if honorSW {
			// Honor Plex's full software pipeline: decoder, filter chain, and
			// encoder stay exactly as PMS emitted them — SW decode, SW filters
			// (scale=/tonemap=/format=), and libx264/libx265 run on the worker
			// CPU. The fork's merged inlineass SW (FFDraw nv12) branch renders
			// any `-inlineass` burn-in. Plex's native `-crf`/`-preset`/`-x264opts`
			// are honoured by libx264 directly. Only the transport/audio/flag
			// scrubs below apply. See docs/HW_PROFILE.md.
			//
			// A fully-SW pipeline uses no VAAPI device, so drop the HW device
			// init PMS pairs with every spawn — otherwise a GPU-less /
			// CPU-fallback node would fail trying to open VAAPI.
			for _, flag := range []string{"-init_hw_device", "-filter_hw_device"} {
				if i := indexOfArg(args, flag, 0); i >= 0 && i+1 < len(args) {
					args = removeArgs(args, i, 2)
				}
			}
			changes = append(changes, TagHonorPlexSW)
		} else if !isHWDecode || hybridForceHW {
			if hybridForceHW {
				// HW decode already in place (kept); only the SW filter+encode
				// tail is reshaped to VAAPI below. See hybridForceHW comment.
				changes = append(changes, TagPrefixForceHWReshapeHybrid+swDecoder)
			}
			// 3. Video -filter_complex rewrite
			vfIdx := -1
			for i := 0; i < len(args); i++ {
				if args[i] == "-filter_complex" && i+1 < len(args) && reVideoInput0.MatchString(args[i+1]) {
					vfIdx = i + 1
					break
				}
			}
			if vfIdx < 0 {
				return bail(TagBailReasonNoVideoFilter)
			}

			// Subtitle source detection. PMS hands us the subtitle file/stream
			// via -map_inlineass <spec> + -i shape; the rewriter resolves which
			// case we're in (text-sidecar / text-embedded / bitmap-embedded /
			// bitmap-sidecar) and the filter rewrite picks the matching shape.
			// Text routes through the fork's scaleplex_inlineass binding;
			// bitmap routes through overlay_vaapi.
			if earlySubSrc != nil {
				subSrc = earlySubSrc
			} else {
				var probe func(string, string) string
				if opts != nil && opts.ProbeSubtitleCodec != nil {
					probe = opts.ProbeSubtitleCodec
				}
				subSrc = detectSubtitleSource(args, sessionDir, probe)
			}
			if subSrc != nil && subSrc.Kind == "bitmap" {
				label := TagPrefixSubtitleBitmap + subSrc.StreamSpec
				if subSrc.Codec != "" {
					label += "(" + subSrc.Codec + ")"
				}
				changes = append(changes, label)
			}

			// HDR-source already detected + emitted once at the session level
			// (hoisted above the branch split); `sourceIsHDR` carries the
			// decision into rewriteVideoFilter so it picks the tonemap shape.

			rewritten = rewriteVideoFilter(args[vfIdx], mediaPath, subSrc, sourceIsHDR, tm)
			if rewritten == nil {
				return bail(TagPrefixBailFilterPattern + args[vfIdx])
			}
			args[vfIdx] = rewritten.Filter
			changes = append(changes, TagPrefixFilter+rewritten.Mode)

			// 4. Update -map output label following the video filter.
			// MUST run BEFORE dropSidecarInput: that drop removes args
			// from BEFORE vfIdx (the input-1 option block), which shifts
			// vfIdx downward. Iterating from the old vfIdx+1 then misses
			// the `-map <oldlabel>` that's now closer to the filter, and
			// the rewriter silently leaves a stale label → ffmpeg fails
			// with "Output with label '<old>' does not exist in any
			// defined filter graph" (live repro 2026-05-09 session 7347:
			// SW HDR + text-sidecar sub-burn, exit status 234).
			for i := vfIdx + 1; i < len(args); i++ {
				if args[i] != "-map" {
					continue
				}
				v := args[i+1]
				if v == rewritten.OldLabel || v == `"`+rewritten.OldLabel+`"` {
					if strings.HasPrefix(v, `"`) {
						args[i+1] = `"` + rewritten.NewLabel + `"`
					} else {
						args[i+1] = rewritten.NewLabel
					}
					changes = append(changes, TagMapLabelUpdate)
					break
				}
			}

			// Bitmap subs now burn through the SAME inlineass binding as text
			// (composeBurn, mode "bitmap-inlineass-vaapi") — no overlay_vaapi. The
			// fork's `-map_inlineass <stream>` routes the bitmap codec to
			// replay_bitmap; ensure the flag is present (Plex's overlay argv may
			// not have carried it). The sidecar input is KEPT (SecondInputArgIdx
			// stays -1 for bitmap sidecar) so the binding can read the .sup stream.
			// The decode-sink is stripped later (stripInlineassDecodeSink).
			if rewritten.Mode == "bitmap-inlineass-vaapi" {
				if subSrc != nil && subSrc.StreamSpec != "" &&
					indexOfArg(args, "-map_inlineass", 0) < 0 {
					args = spliceArgs(args, indexOfArg(args, "-filter_complex", 0),
						"-map_inlineass", subSrc.StreamSpec)
					changes = append(changes, TagAddMapInlineass)
				}
			}

			// 5. Encoder swap (libx264 → h264_vaapi etc.)
			// Re-locate encCodecIdx because the splices above may have
			// shifted indices.
			newInputIdx = indexOfArg(args, "-i", 0)
			encCodecIdx = streamSpecIndex(args, "-codec", 0, newInputIdx+1)
			if encCodecIdx < 0 {
				return bail(TagBailReasonNoEncoder)
			}
			swEncoder := args[encCodecIdx+1]
			hwEncoder, ok := activeDialect.encoderMap()[swEncoder]
			if !ok {
				return bail(TagPrefixBailUnknownEncoder + swEncoder)
			}
			args[encCodecIdx+1] = hwEncoder
			changes = append(changes, TagPrefixEncode+swEncoder+"->"+hwEncoder)
			args, changes = ensureHEVCMain10(args, changes)
		} else if honorHybrid {
			// HW decode + SW encode (per-axis honor, docs/HW_PROFILE.md). PMS
			// emitted `-hwaccel:0 vaapi` decode + a libx264/libx265 encode (its
			// "HW accel on, HW encoding off" config). Keep the pipeline exactly:
			// HW decode (device retargeted by the fork, patch 0116), Plex's SW
			// filter chain incl. its `inlineass` node (fork parses the keys,
			// patch 0119), and the SW encoder on CPU. No reshape, no VAAPI
			// encoder validation. The smart CPU-offload mode — the heavy decode
			// stays on the GPU, only the encode is CPU (realtime even at 4K,
			// unlike full SW which is decode-bound). Only the transport/audio
			// scrubs below apply.
			changes = append(changes, TagHonorPlexHWDecSWEnc)
		} else {
			// HW-decode mode: PMS already emitted a HW encoder for the
			// active backend. Validate that, but leave the filter chain,
			// map labels, and encoder argument intact. The accepted set
			// is whatever encoderMap maps to — VAAPI:
			// {h264_vaapi, hevc_vaapi}; NVIDIA: {h264_nvenc, hevc_nvenc}.
			swEncoder := args[encCodecIdx+1]
			expected := false
			for _, hw := range activeDialect.encoderMap() {
				if hw == swEncoder {
					expected = true
					break
				}
			}
			if !expected {
				return bail(TagPrefixBailUnexpectedEncoder + swEncoder)
			}
			changes = append(changes, TagPrefixEncodeHWPassthrough+swEncoder)
			args, changes = ensureHEVCMain10(args, changes)

			// HDR-source already detected + emitted at the session level
			// (hoisted above the branch split); `sourceIsHDR` flows through
			// for diagnostic-only consumers. scaleplex does NOT inject a
			// tonemap here — when Plex's HW-decode chain is the plain
			// `scale_vaapi=...:format=nv12` shape with no tonemap filter,
			// that means Plex's "Use hardware-accelerated tone mapping" is
			// off and Plex itself does no tonemapping — scaleplex matches
			// that behavior rather than second-guessing it.

			// Sub burn-in: PMS sends `-map_inlineass` even in HW-decode
			// mode, with a filter graph that runs Plex's private
			// `inlineass` filter on the CPU side of an
			// hwdownload/hwupload sandwich. Phase 2c keeps the
			// `inlineass=` filter (fork's vf_inlineass binding renders
			// via libass natively); rewriter strips the four Plex-private
			// AVOption keys vf_inlineass doesn't parse and reshapes the
			// surrounding chain (e.g. tonemap_vaapi for HDR sources)
			// while preserving the [0]–[4] label sequence so PMS's
			// `-map [4]` still resolves.
			var probe func(string, string) string
			if opts != nil && opts.ProbeSubtitleCodec != nil {
				probe = opts.ProbeSubtitleCodec
			}
			subSrc = detectSubtitleSource(args, sessionDir, probe)
			if subSrc != nil {
				switch subSrc.Kind {
				case "text":
					vfIdx := -1
					for i := 0; i < len(args); i++ {
						if args[i] == "-filter_complex" && i+1 < len(args) &&
							strings.Contains(args[i+1], "inlineass=") &&
							reVideoInput0.MatchString(args[i+1]) {
							vfIdx = i + 1
							break
						}
					}
					if vfIdx < 0 {
						return bail(TagBailReasonHWDecodeSubNoInlineass)
					}
					// Orthogonal core: extractGraphFacts + composeBurn — same
					// path the SW-reshape and HW-decode-bitmap branches already
					// take. Plex's HW-text argv shape (with OR without an
					// intervening tonemap_opencl chain) is a modeled graph;
					// extractGraphFacts lifts {w/h, hdr+algo, subKind=text,
					// subParams} and the composer emits the merged-inlineass
					// VAAPI graph in one shape — Plex's redundant leading
					// `[0:0]hwupload[0]` is dropped (vaResident=true), and the
					// hwdownload→inlineass(SW)→hwupload bracket / the OpenCL
					// detour are absent by construction.
					facts := extractGraphFacts(args[vfIdx], subSrc)
					if !facts.ok || facts.subKind != "text" {
						return bail(TagPrefixBailHWDecodeSubUnmodeled + args[vfIdx])
					}
					// Two HDR signals merge into the session-level `sourceIsHDR`:
					// `facts.hdr` (graph-derived — Plex's argv carried a tonemap
					// stage, indicating it saw HDR) and the hoisted ProbeVideoColor
					// emit above. Keep the graph-derived OR here so a probe that
					// returns "" (file unreadable / non-PathMedia source) still
					// triggers the right tonemap shape downstream. The
					// `video:hdr-source(<transfer>)` tag is NOT re-emitted —
					// hoisted to a single session-level site to fix the
					// double-emit on HW-decode + text-sub-burn + HDR (the issue
					// `[KNOWN: DupHDRTag]` documented).
					if facts.hdr {
						sourceIsHDR = true
					}
					oldLabel := ""
					if m := reGraphTrailingLabel.FindStringSubmatch(args[vfIdx]); m != nil {
						oldLabel = "[" + m[1] + "]"
					}
					// vaResident=true: Plex's argv carries the HW decode flags
					// (`-hwaccel:0 <vaapi|nvdec> -hwaccel_output_format:0
					// <vaapi|cuda>`), so [0:0] is already a backend surface —
					// composeBurn skips the leading hwupload (Plex's own leading
					// `[0:0]hwupload[0]` was a redundant passthrough on a
					// HW-tagged frame). Force -hwaccel_output_format:0 to the
					// backend's surface format defensively (parity with the
					// HW-decode-bitmap branch).
					if ofIdx := streamSpecIndex(args, "-hwaccel_output_format", 0, 0); ofIdx >= 0 {
						args[ofIdx+1] = activeDialect.hwaccelOutputFormat()
					} else if hwIdx := streamSpecIndex(args, "-hwaccel", 0, 0); hwIdx >= 0 {
						args = spliceArgs(args, hwIdx+2, "-hwaccel_output_format:0", activeDialect.hwaccelOutputFormat())
						vfIdx = indexOfArg(args, "-filter_complex", 0) + 1
					}
					animated := subtitleIsAnimated(subSrc.Codec, subSrc.FilePath, os.ReadFile)
					// HDR-passthrough sub-burn: Plex carried no tonemap node
					// (facts.hdr=false) but the source is HDR (sourceIsHDR) and
					// the HEVC encoder will run Main10 — keep the chain 10-bit
					// so the encoder gets a p010 surface (scaleplex#204).
					tenBit := sourceIsHDR && !facts.hdr && hwEncoderCodec[args[encCodecIdx+1]] == "hevc"
					newFilter, newLabel := tm.composeBurn(burnSpec{
						vaResident:       true,
						w:                facts.w,
						h:                facts.h,
						hdr:              facts.hdr,
						algo:             facts.algo,
						tenBit:           tenBit,
						burnSub:          true,
						subParams:        facts.subParams,
						animatedTierDown: animated,
					})
					newFilter, newLabel = appendSelectStage(newFilter, newLabel, facts.selectExpr)
					args[vfIdx] = newFilter
					retargetMapLabel(args, oldLabel, newLabel)
					if facts.hdr {
						// Plex's HW-tonemap-ON shape carries a tonemap_opencl chain;
						// the rewrite preserves the algorithm (composeBurn routes it
						// through tm.stage, which keeps the OpenCL chain by default
						// or collapses to tonemap_vaapi under SCALEPLEX_TONEMAP=vaapi).
						// Without this preservation HDR renders washed.
						changes = append(changes, TagPrefixHWDecodeSubTonemapPreserved+facts.algo+")")
						changes = append(changes, TagHWDecodeFilterOCLToVAAPIIA)
					} else {
						changes = append(changes, TagHWDecodeFilterInlineassVA)
					}
					if oldLabel != "" && oldLabel != newLabel {
						changes = append(changes, TagHWDecodeMapLabelUpdate)
					}
					newInputIdx = indexOfArg(args, "-i", 0)
					encCodecIdx = streamSpecIndex(args, "-codec", 0, newInputIdx+1)
				case "bitmap":
					return bail(TagBailReasonHWDecodeSubBitmapUnsupported)
				}
			}

			// PGS / bitmap burn-in in HW-decode mode. PMS emits its own
			// overlay_vaapi graph that sub2video-bridges the bitmap subtitle and
			// SW-upscales it to the full output resolution — no `-map_inlineass`,
			// so the detectSubtitleSource switch above never sees it. That
			// full-frame overlay (and, on the HDR variant, a decode→sysmem→
			// re-upload round-trip from Plex's leading `[0:0]hwupload`) runs the
			// transcode sub-realtime (measured 0.37x at 4K HDR; the buffer Frank
			// hit 2026-05-25). Unify it onto the inlineass burn like every other
			// sub path: detectBitmapOverlayBurn extracts the stream spec + target
			// W/H + (optional) tonemap algo regardless of an intervening tonemap,
			// and composeBurn re-emits VA-resident scale_vaapi → [tonemap] →
			// inlineass(render_height) — the fork's replay_bitmap renders the
			// bitmap at render_height (band), seek is native. See composeBurn /
			// project_scaleplex_perf_tuning.
			for i := 0; i+1 < len(args); i++ {
				if args[i] != "-filter_complex" {
					continue
				}
				streamSpec, w, h, algo, hdr, ok := detectBitmapOverlayBurn(args[i+1])
				if !ok {
					break
				}
				oldLabel := ""
				if m := reGraphTrailingLabel.FindStringSubmatch(args[i+1]); m != nil {
					oldLabel = "[" + m[1] + "]"
				}
				// VA-resident only when Plex's argv actually HW-decodes; otherwise
				// composeBurn prepends the hwupload itself.
				vaResident := streamSpecIndex(args, "-hwaccel", 0, 0) >= 0
				// HDR-passthrough bitmap burn (scaleplex#204) — mirrors the
				// text branch: keep the chain 10-bit when source is HDR and
				// Plex didn't tonemap so the HEVC encoder gets p010 for Main10.
				tenBit := sourceIsHDR && !hdr && hwEncoderCodec[args[encCodecIdx+1]] == "hevc"
				newFilter, newLabel := tm.composeBurn(burnSpec{
					vaResident: vaResident, w: w, h: h, hdr: hdr, algo: algo,
					tenBit:  tenBit,
					burnSub: true,
				})
				args[i+1] = newFilter
				// Make [0:0] a real HW surface (Plex's bitmap argv decodes to
				// sysmem) so the no-hwupload composer is valid + the round-trip is
				// gone. Idempotent with gpuResidentOpenCLTonemap's own force.
				if vaResident {
					if ofIdx := streamSpecIndex(args, "-hwaccel_output_format", 0, 0); ofIdx >= 0 {
						args[ofIdx+1] = activeDialect.hwaccelOutputFormat()
					} else if hwIdx := streamSpecIndex(args, "-hwaccel", 0, 0); hwIdx >= 0 {
						args = spliceArgs(args, hwIdx+2, "-hwaccel_output_format:0", activeDialect.hwaccelOutputFormat())
					}
				}
				retargetMapLabel(args, oldLabel, newLabel)
				// Plex's bitmap argv carries no -map_inlineass; add it (before
				// -filter_complex, matching Plex's text placement) so the fork feeds
				// the decoded presentation to replay_bitmap. No decode-sink: fork
				// patch 0120's binding self-decodes the stream, paced by the demux.
				args = spliceArgs(args, indexOfArg(args, "-filter_complex", 0), "-map_inlineass", streamSpec)
				if hdr {
					changes = append(changes, TagPrefixHWDecodeFilterBitmapHDRTM+algo+")")
				} else {
					changes = append(changes, TagHWDecodeFilterBitmapInlineassVA)
				}
				// The splices shifted indices; relocate the encoder.
				newInputIdx = indexOfArg(args, "-i", 0)
				encCodecIdx = streamSpecIndex(args, "-codec", 0, newInputIdx+1)
				break
			}
		}

		// 5b. GPU-resident OpenCL tonemap fix-up (jellyfin-ffmpeg 7.x). Any
		// emitted `tonemap_opencl` graph needs an explicit OpenCL device + a VA
		// surface input + no hwupload/round-trip cruft, else the va→opencl
		// derive fails ENOSYS on 7.x. Runs after all filter reshaping so it
		// sees the final graph. See gpuResidentOpenCLTonemap.
		{
			var oclChanges []string
			args, oclChanges = gpuResidentOpenCLTonemap(args)
			changes = append(changes, oclChanges...)
			newInputIdx = indexOfArg(args, "-i", 0)
			encCodecIdx = streamSpecIndex(args, "-codec", 0, newInputIdx+1)
		}

		// 6. -crf:0 is left untouched. The scaleplex-ffmpeg fork's VAAPI
		// encoder accepts libx264-style `-crf:N` directly and maps it to
		// QP = crf + crf_qp_offset (default 6) before rate-control
		// selection (patch 0117-vaapi-encode-accept-crf); patch 0105 then
		// routes the resulting QP + `-maxrate` to QVBR. So Plex's
		// `-crf:0 <Q> -maxrate:0 <R> -bufsize:0 <B>` argv reaches the
		// encoder verbatim — no crf->qp translation here. The
		// HW_QP_CRF_OFFSET env knob is retired; override via the encoder's
		// `-crf_qp_offset` argv option.

		// 7. -preset:0, -x264opts:0 and -x265-params:0 are left untouched.
		// The fork's VAAPI encoder accepts a libx264-style `-preset NAME`
		// and maps it to compression_level (iHD TargetUsage) at init, and
		// swallows the `-x264opts` / `-x265-params` private blobs as no-ops
		// (patch 0118-vaapi-encode-accept-preset-and-stubs). So Plex's
		// libx264 encoder argv reaches the VAAPI encoder verbatim — no
		// preset translation or opt-blob dropping here. When PMS omits a
		// preset, the encoder leaves compression_level at the driver default
		// (~TU=4). The SCALEPLEX_PRESET_MAP env knob is retired; override by
		// passing -compression_level directly.

		// 8. Match Plex prod's "no a53_cc SEI in HW-encoded output" convention.
		//
		// PMS emits `-sei:0 -a53_cc` on every HEVC/libx265 session (201/287
		// corpus entries) but omits it on libx264 sessions (10 entries).
		// In stock AV_OPT_TYPE_FLAGS parser semantics `-X` REMOVES flag X,
		// not adds it. Plex's bundled libavcodec.so.60 has `a53_cc` as a
		// SEI flag (verified via `strings`), but their published 1.12.3
		// source omits it — private patch. Net Plex-prod intent: strip
		// a53_cc SEI on every HW-encode session so their separate CC
		// pipeline (PGS-style overlay) drives caption rendering, not SEI
		// passthrough.
		//
		// Without our inject, jellyfin-ffmpeg's h264_vaapi default
		// (`IDENTIFIER|TIMING|RECOVERY_POINT|A53_CC`, a53_cc default-ON
		// per vaapi_encode_h264.c L1089) would emit a53_cc SEI on the 10
		// libx264-swapped-to-h264_vaapi sessions — diverging from Plex
		// prod. Inject keeps libx264 sessions consistent with the libx265
		// majority. Tag stays for replay-corpus diagnostics.
		//
		// Skipped when honoring a SW session: the encoder stays libx264, where
		// a53_cc isn't default-injected, so PMS's own omission already matches.
		if !honorSW && !honorHybrid && streamSpecIndex(args, "-sei", 0, 0) < 0 {
			if fkfIdx := streamSpecIndex(args, "-force_key_frames", 0, encCodecIdx+1); fkfIdx >= 0 {
				args = spliceArgs(args, fkfIdx, "-sei:0", "-a53_cc")
				changes = append(changes, TagInjectSEIA53CC)
			}
		}

	} // end if !isRemux

	// 9. Audio: Plex emits `-codec:N <codec>_eae -eae_prefix:N <token>`
	// (the `*_eae` family is Plex's EasyAudioEncoder over a localhost
	// socket — only present in Plex Transcoder, not in stock/jellyfin
	// ffmpeg). Walk the whole arg list and replace every
	// `<audio-codec-flag> X_eae` with the safe stock equivalent.
	// Mapping: eac3_eae → eac3 (clean stock encoder); other *_eae
	// (truehd_eae seen 4× in corpus on Atmos remuxes) → eac3 fallback
	// because stock truehd encoder is flagged experimental and
	// jellyfin-ffmpeg7 still requires `-strict experimental` plus
	// produces sub-realtime; client loses bitstream passthrough but
	// keeps working audio. Add explicit cases here when a base codec
	// becomes worth preserving (e.g. ac3_eae→ac3 if it ever surfaces).
	{
		var swapped [][2]string
		args, swapped = swapEAEAudioDecoders(args)
		for _, p := range swapped {
			changes = append(changes, TagPrefixAudio+p[0]+"->"+p[1])
		}
	}
	{
		var dropped []string
		args, dropped = dropEAEPrefixFlags(args)
		for _, d := range dropped {
			changes = append(changes, TagPrefixDrop+d)
		}
	}

	// `-bsf:N framedrop=count=*` — Plex emits this audio-output BSF
	// post-seek to drop the first N AAC frames for A/V alignment.
	// `framedrop` is Plex-Transcoder-only; scaleplex-ffmpeg7 lacks the
	// BSF → exit 8 "Bitstream filter not found" at open-output.
	// Recoverable (PMS retries with different argv) but ugly in logs.
	// Mirror the EAE-prefix drop pattern.
	{
		var dropped []string
		args, dropped = dropFramedropBSF(args)
		for _, d := range dropped {
			changes = append(changes, TagPrefixDrop+d+"(framedrop)")
		}
	}

	// `-loglevel_plex`, `-strict_ts:N` passthrough: scaleplex-ffmpeg7
	// patches 0098/0107 register both as OPT_TYPE_STRING sinks (accepted
	// + discarded). `-segment_list_separate_stream_times` +
	// `-segment_list_unfinished` pass through natively (patch 0096
	// no-op AVOptions; full per-stream end-time tracking + CSV
	// unfinished prefix scheduled for Phase 2b).

	// `-skip_to_segment N` — Plex DASH muxer extension that starts the
	// dash muxer's segment_index at N. scaleplex-ffmpeg7 backports this
	// natively (libavformat/dashenc.c patch 0095) so the flag flows
	// straight through and ffmpeg emits chunk-stream0-NNNNN.m4s with
	// N matching PMS's URL expectation. Diagnostic tag only; do NOT
	// strip the flag.
	if i := indexOfArg(args, "-skip_to_segment", 0); i >= 0 && i+1 < len(args) {
		if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
			changes = append(changes, TagPrefixSkipToSegmentPassthrough+args[i+1])
		}
	}

	// `-delete_removed false` (Plex DASH extension, also backported)
	// keeps chunks on disk past the sliding manifest window — PMS
	// serves rewind / early-fetch via direct file read. No more need
	// for our previous `-extra_window_size 999999` injection hack.

	// CMAF-strict movflags now applied natively by scaleplex-ffmpeg7
	// dashenc init (patch 0104). The inner mp4 muxer gets
	// +empty_moov+default_base_moof+separate_moof+cmaf appended to
	// movflags by dash_init, so each chunk emits moof+mdat only —
	// Plex Web's Chromium MSE accepts the stream without the
	// per-segment moov re-init that confused it pre-patch.

	// HLS: Plex's `-f ssegment` (custom stream-segmenter muxer) and
	// related segment.c options pass through to scaleplex-ffmpeg7
	// natively:
	//   -f ssegment                  — patch 0098 adds AVFMT_GLOBALHEADER
	//                                  to ff_stream_segment_muxer (shared
	//                                  code with ff_segment_muxer via
	//                                  `.priv_class = &seg_class`)
	//   -segment_list_separate_stream_times / -segment_list_unfinished
	//                                — patch 0096 registers as no-op
	//                                  AVOptions (stripped globally
	//                                  above; full per-stream end-time
	//                                  emit scheduled for Phase 2b)
	//   -segment_list_size           — left untouched; patch 0106
	//                                  force-buffers full chunk history
	//                                  on URL-handler outputs regardless
	//                                  of list_size, making the value
	//                                  inert for our PMS-aggregation case
	//   -copyts                       — kept on matroska + ssegment seek
	//                                  sessions. Patch 0103 dropped the
	//                                  jellyfin `reference_stream_first_pts`
	//                                  end_pts adjustment that broke
	//                                  split cadence on `-ss + -copyts`,
	//                                  restoring Plex-fork split semantics.
	//                                  Cluster.Timecode in each chunk now
	//                                  reflects absolute source PTS — relay
	//                                  has nothing to patch.
	//
	// Stock segment muxer with -segment_list <http_url> POSTs the listfile
	// to that URL natively (CSV with -segment_list_type csv). PMS reads
	// the CSV and synthesises the m3u8 it serves to clients.
	if isHLS {
		// `-segment_format_options live=1` passes through. scaleplex-
		// ffmpeg7 patch 0094 makes matroskaenc.c always write Duration
		// regardless of is_live, and jellyfin 7.x's `IS_SEEKABLE = pb
		// seekable && !is_live` naturally falls to the cluster-defaults
		// else-branch at live=1 → 1000 ms / 32 KB defaults ≈ per-frame
		// clusters at typical bitrates. Both behaviours the previous
		// rewrite forced (Duration in header + per-frame clusters) are
		// now patch-0094-plus-stock-defaults.
		//
		// Previous rewrite (deleted 2026-05-12 audit pass) replaced
		// `live=1` with `live=0:cluster_time_limit=1000:cluster_size_limit=32768`.
		// Real PMS argvs for Plex Android use `output_ts_offset=10` and
		// never trigger the live=1 match, so the rewrite was dead code
		// for current production traffic. Plex Windows shape would emit
		// live=1 — needs revalidation when hw becomes available, but
		// patch 0094 + IS_SEEKABLE semantics give the same end state.

		// Stage-rename pattern attempted (rewriter swap to `chunk-%05d
		// .tmp` + relay-side patch-and-rename to final chunk-N) —
		// reverted 2026-05-11. ffmpeg's segment muxer occasionally
		// produces 0-byte chunk files (verified live: chunks 00000,
		// 00006, 00050 all 0 bytes amid populated neighbours); before
		// stage-rename PMS tolerated these (CSV row + 0-byte file →
		// player skipped). With stage-rename, the patcher dutifully
		// renamed 0-byte .tmp files to final chunk-N names, making
		// them indistinguishable from valid chunks. mpv read empty
		// chunks → video corruption + audio desync. Reverted to keep
		// chunks named chunk-N from ffmpeg's pen; relay patches in
		// place (audio-track-swap playhead-reset remains a known
		// minor UX issue).

		// Matroska seek playhead-reset: known cosmetic issue 2026-05-11.
		// On Plex Windows seek, the new transcode session produces chunks
		// with locally-rebased PTS (-copyts stripped so segment muxer
		// can split). Matroska Cluster Timecode starts at 0, Plex
		// Windows client interprets that as playback position 0 and
		// resets the slider thumb to the front — even though `Duration`
		// in the header is correct and seeking continues to function.
		//
		// Tried injecting `-output_ts_offset <seek>` to shift output
		// PTS back to global. Verified live 2026-05-11 Big Hero 6: it
		// breaks segment splits exactly like `-copyts` does (single
		// ~90s chunk instead of 90 1s chunks → playback freezes).
		// Stock segment muxer doesn't support non-zero start PTS for
		// its split logic. Plex's fork patches libavformat/segment.c
		// to offset `end_pts` by `reference_stream_first_pts` so the
		// threshold tracks correctly; we can't replicate without a
		// custom ffmpeg build. Park as accepted cosmetic issue.

		// Rewrite -segment_list URL to the relay address. Plex points it
		// at 127.0.0.1:32400 (PMS's loopback) which the worker pod can't
		// reach. Same pattern as -progressurl / -manifest_name. Stock
		// ffmpeg's segment muxer POSTs to this URL natively when it's
		// HTTP, so once routed through the relay it just works.
		//
		// Also append `&scaleplex_seg_time=<segment_time>` so the relay
		// can rewrite each CSV row's start/end to global-timeline values
		// (chunk N → N*seg_time .. (N+1)*seg_time). PMS returns 0-byte
		// bodies whenever a CSV row's start_time disagrees with the
		// chunk's playlist window, so for seek sessions we MUST send
		// global times — but stock ffmpeg only produces global times
		// with `-copyts`, which we just stripped because it blocks
		// splits. Relay rewrite is the cleanest place to fix this.
		segTime := ""
		if i := indexOfArg(args, "-segment_time", 0); i >= 0 && i+1 < len(args) {
			segTime = args[i+1]
		}
		rewriteSegmentList(args, inputEnv, segTime, &changes, "hls")
	}

	// DASH sessions with text subs use a SECOND output `-f segment
	// -segment_format ass` to deliver subs as a side-channel DASH
	// track. That output has its own `-segment_list` URL pointing
	// at PMS loopback. Without rewriting, CSV POSTs fail silently
	// and Plex Web's subtitle layer gets no chunks → subs never
	// surface. Same loopback-to-relay rewrite, no scaleplex_seg_time
	// rewrite (relay treats sub CSV rows as pass-through).
	rewriteSegmentList(args, inputEnv, "", &changes, "side-channel")

	// Capture `-ss <off>` on seek sessions for the renumber watcher's
	// tfdt patch. Stock dashenc writes tfdt=0 in seek-session chunks
	// regardless of argv (verified: -ss + -copyts + drop -start_at_zero
	// + +cmaf movflag still produces tfdt=0). Plex Web's MSE places
	// such chunks at timeline 0..seg_dur — player's currentTime sits at
	// <off> with no buffered data → BUFFERING_HAVE_NOTHING forever
	// (confirmed via local MSE harness; Plex Transcoder seek chunks have
	// tfdt=5000s for an offset-5000 seek, scaleplex had tfdt=0).
	//
	// Don't drop -start_at_zero — it primes the AAC encoder; removing
	// it caused 199-byte empty audio chunks earlier. Instead, surface
	// the seek offset so the renumber watcher can patch tfdt and
	// sidx.ept after the rename.
	seekOffsetSeconds := 0.0
	if ssIdx := indexOfArg(args, "-ss", 0); ssIdx >= 0 && ssIdx+1 < len(args) {
		if v, err := strconv.ParseFloat(args[ssIdx+1], 64); err == nil && v > 0 {
			seekOffsetSeconds = v
			changes = append(changes, fmt.Sprintf(TagPrefixSeekOffsetCaptured, v))
		}
	}
	// The subtitle pre-render timeline must start at the same offset as
	// a seek session's main video, or overlay_vaapi framesync places
	// the burned text at the wrong time.

	// `-manifest_name <url>` — Plex's ffmpeg fork POSTs the manifest
	// body to this URL whenever the .mpd is regenerated; PMS gates
	// `/header` on the first such POST. scaleplex-ffmpeg7 backports
	// the URL-aware `-manifest_name` (patch 0095), so we only need to
	// rewrite the loopback host to the relay base; ffmpeg PUTs the
	// body natively via dashenc's HTTP protocol handler.
	var manifestChanges []string
	args, manifestChanges = rewriteManifestName(args, inputEnv)
	changes = append(changes, manifestChanges...)

	// PTS handling — keep Plex's `-copyts -start_at_zero
	// -avoid_negative_ts disabled` exactly as PMS sends them.
	//
	// Earlier rewriter versions stripped these and injected
	// `-output_ts_offset 0` to force chunks to start at PTS=0. That
	// "worked" for video but broke audio on every seek: with -ss <off>
	// + 0-rebased output, the AAC encoder gets no primer samples and
	// emits a 199-byte empty first segment (just an mp4 box, no
	// frames). DASH players then hang on initial-audio-buffer-fill
	// even though video chunks decode fine.
	//
	// Stock dashenc's segment_index always counts from 1 regardless
	// of input PTS, so file numbering stays predictable for the
	// renumber watcher. Chunk-internal mp4 PTS lands on the global
	// timeline (matching what Plex Transcoder produces).

	// Plex's `-progressurl <url>` points at 127.0.0.1:32400 — PMS's own
	// loopback, unreachable from the worker. Earlier we translated to
	// stock `-progress <url>` (ffmpeg's HTTP progress sink). That fails
	// because ffmpeg streams updates over a single chunked-encoded PUT
	// body and Plex's progress handler parses each PUT body as a
	// complete discrete payload — `/header` then waits ~120s for a
	// "first" report it never sees. So we strip `-progressurl` from the
	// argv entirely and surface the rewritten URL on RewriteResult so
	// the agent can run its own reporter (one PUT per progress block).
	var progressURL string
	{
		var pchanges []string
		args, progressURL, pchanges = capturePMSProgressURL(args, inputEnv)
		changes = append(changes, pchanges...)
	}

	if newArgs, ok := upgradeLoglevelFromQuiet(args); ok {
		args = newArgs
		changes = append(changes, TagLoglevelInfo)
	}
	if newArgs, ok := dropNostatsFlag(args); ok {
		args = newArgs
		changes = append(changes, TagDropNostats)
	}

	{
		var echanges []string
		env, echanges = stripEAEEnvVars(env)
		changes = append(changes, echanges...)
	}

	// VAAPI driver discovery env. Only override if explicitly set;
	// libva otherwise auto-discovers iHD on the worker image. Skipped
	// on Optimize remux — pure passthrough, no VAAPI device touched.
	if !isRemux {
		env["LIBVA_DRIVER_NAME"] = vaapiDriver
		if libvaDriversPath != "" {
			env["LIBVA_DRIVERS_PATH"] = libvaDriversPath
		}
		changes = append(changes, TagEnvLIBVA)
	}

	env = setWorkerHomeEnv(env)
	changes = append(changes, TagEnvHOME)

	// scaleplex (0120): drop Plex's subtitle decode-sink now that the
	// -map_inlineass binding self-decodes. See stripInlineassDecodeSink.
	// Skipped on Optimize remux — no -map_inlineass / no decode sink in
	// the argv anyway, but gate it explicitly to keep the remux path's
	// changes list free of no-op tags.
	if !isRemux {
		if stripped, ok := stripInlineassDecodeSink(args); ok {
			args = stripped
			changes = append(changes, TagDropInlineassDecodeSink)
		}
	}

	isMatroskaSegment := false
	if outputFormat == "segment" {
		if i := indexOfArg(args, "-segment_format", 0); i >= 0 && i+1 < len(args) && args[i+1] == "matroska" {
			isMatroskaSegment = true
		}
	}

	return RewriteResult{
		Args:              args,
		Env:               env,
		Applied:           true,
		Changes:           changes,
		ProgressURL:       progressURL,
		SeekOffsetSeconds: seekOffsetSeconds,
		IsMatroskaSegment: isMatroskaSegment,
	}
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
