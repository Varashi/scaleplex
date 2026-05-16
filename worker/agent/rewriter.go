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
	// SubPrerender — non-nil when the rewriter chose the pre-rendered
	// GPU-overlay path for a text-subtitle burn-in (SRT / static ASS)
	// instead of the per-frame inlineass filter. The agent uses it to
	// spawn the subtitle pre-render alongside the main ffmpeg.
	SubPrerender *SubPrerenderSpec
}

// SubPrerenderSpec, when set on a RewriteResult, tells the agent to
// spawn a subtitle pre-render process alongside the main ffmpeg. The
// pre-render rasterizes the text subtitle into a sparse transparent
// video written to FIFOPath; the rewritten main filter graph reads
// that FIFO as a second input and composites it with overlay_vaapi.
// See project_scaleplex_srt_to_pgs_gpu.
type SubPrerenderSpec struct {
	// FIFOPath — the named pipe the agent creates and the pre-render
	// writes Matroska/ffv1 to. The rewritten main argv already carries
	// `-i FIFOPath`, so the two must agree.
	FIFOPath string
	// SourcePath — the file the pre-render's `subtitles` filter reads:
	// a sidecar .srt path, or the source media container when the
	// subtitle is an embedded stream.
	SourcePath string
	// StreamSpec — the raw `-map_inlineass` stream specifier (e.g.
	// "0:3" for an embedded subtitle, "1:s:0" for a sidecar). The
	// agent resolves it to a `subtitles` filter selector. Empty when
	// SourcePath is a single-stream sidecar file.
	StreamSpec string
	// Embedded — true when SourcePath is the source media container
	// and the subtitle is an embedded stream (the agent extracts it
	// first); false when SourcePath is a standalone sidecar file the
	// `subtitles` filter can read directly.
	Embedded bool
	// Width, Height — overlay canvas size; matches the transcode's
	// post-scale target resolution.
	Width  int
	Height int
	// SeekOffsetSeconds — the pre-render timeline must start here so it
	// aligns with a -ss seek session's main video. 0 on initial play.
	SeekOffsetSeconds float64
	// ForceStyle — optional `subtitles` filter force_style= string
	// carrying Plex's burn-in styling. Empty until the style-mapping
	// phase populates it.
	ForceStyle string
}

// RewriteOpts is for testability; production callers pass nil.
type RewriteOpts struct {
	FSExists func(string) bool
	// SessionDir — Plex's per-session transcode dir (the agent's
	// req.Cwd). Used as the staging path for embedded-subtitle
	// extraction. When empty, falls back to /tmp/scaleplex.
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

// x264PresetToVAAPI maps Plex's libx264 -preset names onto iHD's VAAPI
// TargetUsage scale (compression_level 1..7, where 7 = fastest /
// "ultrafast" and 1 = highest quality / "veryslow"). The bucketing is
// approximate — VAAPI has 7 levels vs x264's 9 named presets.
//
// Source: Intel iHD driver TargetUsage docs + on-cluster benchmark
// (3× Arc A310, 2026-05-05): cl=7 yielded +30-70% throughput over cl=2
// on no-sub workloads, with no quality difference visible at QP=22.
var x264PresetToVAAPI = map[string]string{
	"ultrafast": "7",
	"superfast": "7",
	"veryfast":  "6",
	"faster":    "5",
	"fast":      "4",
	"medium":    "4",
	"slow":      "3",
	"slower":    "2",
	"veryslow":  "1",
	"placebo":   "1",
}

// mapX264PresetToVAAPI maps Plex's libx264 -preset name onto iHD's
// VAAPI TargetUsage scale (1..7, 7 = fastest). Looks up
// `SCALEPLEX_PRESET_MAP` env first for per-deployment overrides, then
// falls back to the on-cluster-tuned defaults above.
//
// Env format: comma-separated `name=N` pairs (case-insensitive), e.g.
//
//	SCALEPLEX_PRESET_MAP=veryfast=5,fast=3,medium=2
//
// Missing names keep their default mapping; entries with unparsable
// values are silently ignored.
func mapX264PresetToVAAPI(preset string) string {
	key := strings.ToLower(preset)
	if v, ok := parsePresetMapEnv()[key]; ok {
		return v
	}
	if v, ok := x264PresetToVAAPI[key]; ok {
		return v
	}
	// Unknown preset → fastest. Worker only runs when called by orch;
	// playback latency wins over an unfamiliar quality knob.
	return "7"
}

// parsePresetMapEnv reads `SCALEPLEX_PRESET_MAP` and returns the
// overrides as a lowercased map. Parsed per call (env reads are cheap
// and rewriting is rare). Returns nil when env is empty so callers can
// skip lookups.
func parsePresetMapEnv() map[string]string {
	v := os.Getenv("SCALEPLEX_PRESET_MAP")
	if v == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		eq := strings.IndexByte(pair, '=')
		if eq < 1 {
			continue
		}
		name := strings.TrimSpace(strings.ToLower(pair[:eq]))
		val := strings.TrimSpace(pair[eq+1:])
		if name == "" || val == "" {
			continue
		}
		// Validate val is 1..7 — VAAPI TargetUsage range.
		if n, err := strconv.Atoi(val); err != nil || n < 1 || n > 7 {
			continue
		}
		out[name] = val
	}
	return out
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

var (
	reFilterAss = regexp.MustCompile(
		`^\[0:0\]scale=w=(\d+):h=(\d+)(?::force_divisible_by=\d+)?\[0\];` +
			`\[0\]format=pix_fmts=[^\[]*nv12\[1\];` +
			`\[1\]inlineass=([^\[]*)\[2\]$`)
	reFilterPlain = regexp.MustCompile(
		`^\[0:0\]scale=w=(\d+):h=(\d+)(?::force_divisible_by=\d+)?\[0\];` +
			`\[0\]format=pix_fmts=[^\[]*nv12\[1\]$`)
	// HW-decode + inlineass burn-in. PMS sends this when both
	// HardwareAcceleratedCodecs=1 AND a force-burn subtitle target.
	// Filter graph: GPU scale → CPU drop for libass → hwupload back.
	// Phase 2c keeps Plex's `inlineass=` filter intact (fork's
	// vf_inlineass binding renders via libass); rewriter only strips
	// Plex-private AVOption keys vf_inlineass doesn't parse.
	reFilterHWAss = regexp.MustCompile(
		`^\[0:0\]hwupload\[0\];` +
			`\[0\]scale_vaapi=w=(\d+):h=(\d+)(?::format=([A-Za-z0-9]+))?\[1\];` +
			`\[1\]hwdownload,format=([A-Za-z0-9]+)\[2\];` +
			`\[2\]inlineass=([^\[]+)\[3\];` +
			`\[3\]hwupload\[4\]$`)
	// HDR→SDR PMS pattern: scale → zscale(linear) → format(gbrpf32le) →
	// zscale(primaries=bt709) → tonemap → zscale(bt709) → format(nv12).
	// Capture leading w/h and the final output label number; the middle is
	// flexible because Plex tweaks the chain across versions.
	reFilterHDR = regexp.MustCompile(
		`^\[0:0\]scale=w=(\d+):h=(\d+)(?::force_divisible_by=\d+)?\[\d+\];` +
			`.*zscale.*tonemap.*format=pix_fmts=[^\[]*nv12\[(\d+)\]$`)
	// SW HDR + inlineass burn-in PMS pattern. PMS sends this when
	// source is HDR (BT.2020 PQ), target is SDR, AND text-sub burn-in
	// is required. Plex's transcoder collapses the whole pipeline to
	// SW because its HW pipeline can't combine HW tonemap +
	// inlineass(SW). scaleplex CAN: we reshape to
	//   hwupload → scale_vaapi(p010) → tonemap_vaapi(nv12) →
	//   hwdownload → inlineass → hwupload → hevc_vaapi,
	// keeping the libass render step on CPU but everything else on
	// the GPU. Net result: PS4-class clients (HDR source, SDR
	// transcode, text-sub-burn) get HW transcode that Plex's prod
	// transcoder can't produce.
	//
	// Filter shape captured 2026-05-13 on PS4 + FMJ 4K HDR + SRT burn:
	//   [0:0]scale=w=W:h=H:force_divisible_by=4[0];
	//   [0]format=p010,tonemap=hable[1];
	//   [1]format=pix_fmts=yuv420p|nv12[2];
	//   [2]inlineass=...[3]
	reFilterHDRAss = regexp.MustCompile(
		`^\[0:0\]scale=w=(\d+):h=(\d+)(?::force_divisible_by=\d+)?\[0\];` +
			`\[0\]format=p010,tonemap=hable\[1\];` +
			`\[1\]format=pix_fmts=[^\[]*nv12\[2\];` +
			`\[2\]inlineass=([^\[]*)\[3\]$`)
	// HW VAAPI decode + OpenCL tonemap + inlineass burn-in. PMS first-
	// choice when source is HDR AND text-sub burn-in is required AND
	// PMS `Use hardware-accelerated tone mapping` is ON. Plex's argv
	// keeps the GPU pipeline via the OpenCL ICD detour for tonemap,
	// hwdownloads before inlineass, hwuploads after for encode.
	// Captured 2026-05-13 on PS4 retry (PMS first ran this, ffmpeg
	// exited 8 because our rewriter bailed, PMS fell back to the SW
	// shape reFilterHDRAss handles).
	//
	// scaleplex-ffmpeg7 has tonemap_vaapi natively — no OpenCL detour
	// needed. We reshape to the same target shape as reFilterHDRAss:
	//   hwupload → scale_vaapi(p010) → tonemap_vaapi(nv12) →
	//   hwdownload → inlineass → hwupload.
	//
	// Filter shape:
	//   [0:0]hwupload[0];
	//   [0]scale_vaapi=w=W:h=H:format=p010[1];
	//   [1]hwmap=derive_device=opencl[2];
	//   [2]tonemap_opencl=tonemap=...:format=nv12:m=...:p=...:r=...[3];
	//   [3]hwdownload,format=nv12[4];
	//   [4]inlineass=...[5];
	//   [5]hwupload[6]
	reFilterHWOpenCLAss = regexp.MustCompile(
		`^\[0:0\]hwupload\[0\];` +
			`\[0\]scale_vaapi=w=(\d+):h=(\d+)(?::format=[A-Za-z0-9]+)?\[1\];` +
			`\[1\]hwmap=derive_device=opencl\[2\];` +
			`\[2\]tonemap_opencl=[^\[]+\[3\];` +
			`\[3\]hwdownload,format=[A-Za-z0-9]+\[4\];` +
			`\[4\]inlineass=([^\[]+)\[5\];` +
			`\[5\]hwupload\[6\]$`)
	// reInitHW accepts both PMS argv shapes for `-init_hw_device`:
	//   `vaapi=vaapi:`                                — SW-decode, PMS
	//   doesn't know the device because the worker chooses it.
	//   `vaapi=vaapi:/dev/dri/renderDNNN[,driver=NAME]` — HW-decode,
	//   PMS reads HardwareDevicePath + iHD driver from its own probe.
	// In either case the rewriter overwrites with HW_RENDER_DEVICE +
	// HW_VAAPI_DRIVER defaults so the worker pod's device wins.
	reInitHW = regexp.MustCompile(`^vaapi=vaapi:(?:/dev/dri/[A-Za-z0-9_]+(?:,driver=[A-Za-z0-9_]+)?)?$`)
)

func envOr(k, dflt string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return dflt
}

// stripPlexInlineassFilterArgs removes the 4 AVOption keys that Plex's
// argv emits on its `inlineass=` filter but scaleplex-ffmpeg7's
// vf_inlineass does not parse: `language`, `overrides`, `outline`,
// `shadow`. Operates on a full -filter_complex graph string and is
// idempotent + leaves graphs without `inlineass=` untouched.
//
// Without this strip, the fork's AVOption parser rejects Plex's filter
// argv at filter init time. We omitted the stub options in patch 0100
// to sidestep an unexplained LTO/AVFILTER_DEFINE_CLASS table-truncation
// (see project_scaleplex_inlineass_port.md). The strip is the
// rewriter's half of that bargain.
//
// Plex's argv shape inside the filter graph:
//
//	[1]inlineass=font_scale=1.0:font_path=...:fontconfig_file=...:
//	  language=en:overrides=ScaledBorderAndShadow=yes,FontName=...,...:
//	  outline=2.6:shadow=1.7:font_size=54[2]
//
// Top-level pairs are `:`-separated. The `overrides=` value contains
// `,` and `=` but never a top-level `:` (verified across argv corpus
// 2026-05-12 — 6 PMS-emitted inlineass= argv samples, none have
// embedded `:` inside any of the 4 keys).
func stripPlexInlineassFilterArgs(filterStr string) string {
	if !strings.Contains(filterStr, "inlineass=") {
		return filterStr
	}
	stripKeys := map[string]bool{
		"language":  true,
		"overrides": true,
		"outline":   true,
		"shadow":    true,
	}
	var out strings.Builder
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
		// Segment ends at next graph terminator: `[` (label), `;`, or
		// end of string. Output labels like [2] always follow the
		// filter args.
		end := strings.IndexAny(filterStr[k:], "[;")
		segEnd := len(filterStr)
		if end >= 0 {
			segEnd = k + end
		}
		segment := filterStr[k:segEnd]
		pairs := strings.Split(segment, ":")
		kept := pairs[:0]
		for _, p := range pairs {
			kv := strings.SplitN(p, "=", 2)
			if len(kv) == 2 && stripKeys[kv[0]] {
				continue
			}
			kept = append(kept, p)
		}
		out.WriteString(strings.Join(kept, ":"))
		i = segEnd
	}
	return out.String()
}

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
	codec := ""
	if probe != nil {
		codec = strings.ToLower(probe(srcForProbe, streamSpec))
	}
	kind := subtitleKind(codec)
	if kind == "unknown" {
		// No probe (test path) or probe failed. Default to text since
		// it's the common case and the agent's extraction step will
		// fail loud on bitmap inputs (so the operator still gets a
		// signal, just without the cleaner overlay_vaapi path).
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
	Sidecar  string
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
	useOpenCL bool   // false = iHD fixed-curve tonemap_vaapi
	algo      string // fallback algorithm for the SW-chain reshapes
}

func validTonemapAlgo(a string) bool {
	switch a {
	case "hable", "mobius", "reinhard", "bt2390", "linear", "gamma", "clip":
		return true
	}
	return false
}

// resolveTonemapConfig builds the tone-mapping backend policy from the
// worker-pod env.
func resolveTonemapConfig() tonemapConfig {
	cfg := tonemapConfig{
		useOpenCL: !strings.EqualFold(os.Getenv("SCALEPLEX_TONEMAP"), "vaapi"),
		algo:      "hable",
	}
	if a := strings.ToLower(os.Getenv("SCALEPLEX_TONEMAP_ALGO")); validTonemapAlgo(a) {
		cfg.algo = a
	}
	return cfg
}

// stage returns the filter fragment that converts an HDR p010 VAAPI
// surface into an SDR (BT.709) nv12 VAAPI surface. It is inserted
// directly after a `scale_vaapi=...:format=p010` step and its output is
// a VAAPI surface either way, so it is drop-in wherever `tonemap_vaapi`
// stood. `algo` overrides the config's algorithm (used when Plex's argv
// carried its own); empty uses cfg.algo. Ignored in vaapi mode.
//
//	opencl: hwmap→opencl, tonemap_opencl=<algo>, hwmap→vaapi:reverse=1
//	vaapi:  tonemap_vaapi (iHD fixed BT.2390 EETF)
//
// The OpenCL device is self-derived by hwmap from the input frame's
// VAAPI device — no `-init_hw_device opencl` is needed.
func (tm tonemapConfig) stage(algo string) string {
	if !tm.useOpenCL {
		return "tonemap_vaapi=transfer=bt709:format=nv12"
	}
	if !validTonemapAlgo(algo) {
		algo = tm.algo
	}
	return "hwmap=derive_device=opencl," +
		"tonemap_opencl=tonemap=" + algo +
		":transfer=bt709:matrix=bt709:primaries=bt709:format=nv12," +
		"hwmap=derive_device=vaapi:reverse=1"
}

// hdrScale returns the combined `scale_vaapi(p010) + tonemap` chain that
// downscales and tone-maps an HDR source to an SDR nv12 VAAPI surface.
// Used by the reFilterHDR/HDRAss reshapes — i.e. only where Plex's argv
// already declared a tonemap.
func (tm tonemapConfig) hdrScale(w, h string) string {
	return "scale_vaapi=w=" + w + ":h=" + h + ":format=p010," + tm.stage("")
}

func rewriteVideoFilter(filterStr, mediaPath string, subSrc *subtitleSource, sourceIsHDR bool, tm tonemapConfig) *filterRewrite {
	_ = mediaPath
	if m := reFilterAss.FindStringSubmatch(filterStr); m != nil {
		w, h, assParams := m[1], m[2], m[3]

		// Bitmap subs (PGS / VobSub / DVDSub): overlay_vaapi the
		// stream onto the scaled main video. The subtitle stream
		// stays in the input(s); the filter graph references it via
		// [streamSpec] (e.g. [0:3] for embedded, [1:s:0] for sidecar
		// .sup) without going through any intermediate file.
		//
		//   [0:0]                                          source video
		//     ↓ hwupload + scale_vaapi → nv12 surface     [main]
		//   [streamSpec]                                  PGS stream
		//     ↓ format=bgra (libavcodec renders bitmap)
		//     ↓ hwupload                                   [sub]
		//   [main][sub]overlay_vaapi
		//
		// Output stays NV12 throughout so the encoder gets what it
		// expects. eof_action=pass keeps the stream open after the
		// last subtitle event; repeatlast=1 holds the final caption
		// until video ends (matches Plex's UX).
		//
		// No tonemap is injected here. If Plex wanted HDR→SDR tone
		// mapping it would carry a tonemap filter in the argv (handled
		// by reFilterHDRAss / reFilterHWOpenCLAss); a plain reFilterAss
		// match means Plex emitted none, so scaleplex emits none either.
		scaleStep := fmt.Sprintf("scale_vaapi=w=%s:h=%s:format=nv12", w, h)

		if subSrc != nil && subSrc.Kind == "bitmap" && subSrc.StreamSpec != "" {
			mode := "overlay-vaapi-bitmap"
			if sourceIsHDR {
				mode = "overlay-vaapi-bitmap-hdr"
			}
			return &filterRewrite{
				Filter: fmt.Sprintf(
					"[0:0]hwupload[10];"+
						"[10]%s[11];"+
						"[%s]format=bgra[12];"+
						"[12]hwupload[13];"+
						"[11][13]overlay_vaapi=eof_action=pass:repeatlast=1[15]",
					scaleStep, subSrc.StreamSpec),
				OldLabel: "[2]",
				NewLabel: "[15]",
				Mode:     mode,
				Sidecar:  subSrc.Codec,
			}
		}

		// Text subs (SRT/ASS/MOV_TEXT/...): keep Plex's inlineass=
		// filter (strip the 4 unknown AVOption keys) and let the
		// scaleplex-ffmpeg7 fork's vf_inlineass + scaleplex_inlineass
		// binding render. Sub packets arrive via the -map_inlineass
		// side-channel; the filter graph runs libass on CPU nv12
		// frames between hwdownload and hwupload.
		if subSrc != nil && subSrc.Kind == "text" {
			mode := "passthrough-inlineass"
			if sourceIsHDR {
				mode = "passthrough-inlineass-hdr"
			}
			strippedAss := stripPlexInlineassFilterArgs("inlineass=" + assParams)
			strippedAss = strings.TrimPrefix(strippedAss, "inlineass=")
			return &filterRewrite{
				Filter: fmt.Sprintf(
					"[0:0]hwupload[10];"+
						"[10]%s[11];"+
						"[11]hwdownload[12];"+
						"[12]format=pix_fmts=nv12[13];"+
						"[13]inlineass=%s[14];"+
						"[14]hwupload[15]",
					scaleStep, strippedAss),
				OldLabel: "[2]",
				NewLabel: "[15]",
				Mode:     mode,
			}
		}

		// No usable subtitle source resolved. Bail loud.
		_ = w
		_ = h
		_ = assParams
		return nil
	}

	if m := reFilterHDR.FindStringSubmatch(filterStr); m != nil {
		w, h, finalIdx := m[1], m[2], m[3]
		return &filterRewrite{
			Filter: fmt.Sprintf(
				"[0:0]hwupload[0];[0]%s[1];[1]hwupload[2]",
				tm.hdrScale(w, h)),
			OldLabel: "[" + finalIdx + "]",
			NewLabel: "[2]",
			Mode:     "hdr-tonemap-vaapi",
		}
	}

	if m := reFilterHDRAss.FindStringSubmatch(filterStr); m != nil {
		// HDR-source + SDR-target + text-sub burn-in. PMS sent a full SW
		// chain; we force HW reshape with libass on CPU between
		// hwdownload/hwupload brackets. Mirrors reFilterAss text branch
		// but with the tonemap step PMS's SW pattern declared inline.
		w, h, assParams := m[1], m[2], m[3]
		if subSrc == nil || subSrc.Kind != "text" {
			// Bitmap subs reach us via reFilterAss (PGS uses
			// `format=nv12 + inlineass` without the separate tonemap
			// step); only the text path lands here. Bail loud if we
			// see anything else.
			return nil
		}
		strippedAss := stripPlexInlineassFilterArgs("inlineass=" + assParams)
		strippedAss = strings.TrimPrefix(strippedAss, "inlineass=")
		return &filterRewrite{
			Filter: fmt.Sprintf(
				"[0:0]hwupload[10];[10]%s[11];"+
					"[11]hwdownload[12];"+
					"[12]format=pix_fmts=nv12[13];"+
					"[13]inlineass=%s[14];"+
					"[14]hwupload[15]",
				tm.hdrScale(w, h), strippedAss),
			OldLabel: "[3]",
			NewLabel: "[15]",
			Mode:     "hdr-tonemap-vaapi-passthrough-inlineass",
		}
	}

	if m := reFilterPlain.FindStringSubmatch(filterStr); m != nil {
		w, h := m[1], m[2]
		// No implicit tonemap: a plain SDR-target chain with no tonemap
		// filter is exactly what Plex emits when HW tone mapping is off
		// (Plex then does no tonemapping at all). scaleplex matches that
		// — it does not second-guess Plex with an injected tonemap.
		return &filterRewrite{
			Filter: fmt.Sprintf(
				"[0:0]hwupload[0];[0]scale_vaapi=w=%s:h=%s:format=nv12[1];[1]hwupload[2]",
				w, h),
			OldLabel: "[1]",
			NewLabel: "[2]",
			Mode:     "plain",
		}
	}
	return nil
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

// ─── shared rewriter helpers ─────────────────────────────────────────
//
// Both the main rewriter and tryOptimizeRemux need to do the same
// "strip Plex-private cruft + rewrite EAE audio + capture progressurl
// + adjust env" work. Extracted here so both call paths stay in sync
// when a flag is added or a base codec needs new handling.

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
		tag := "hls:segment_list:rewrite-to-relay"
		if variant == "side-channel" {
			tag = "subs:side-channel-segment_list:rewrite-to-relay"
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
		changes = append(changes, "progress:append-X-Plex-Token")
	}
	changes = append(changes, "progressurl:captured-for-reporter")
	// Inject -canthrottleurl pointing at the same relay endpoint.
	// Splice at index 0 so it lands in global-scope (before -i),
	// matching ffmpeg's option-context rules. Skip if the option is
	// somehow already present (shouldn't happen, but be defensive).
	if indexOfArg(args, "-canthrottleurl", 0) < 0 {
		args = spliceArgs(args, 0, "-canthrottleurl", rewritten)
		changes = append(changes, "inject:-canthrottleurl(scaleplex-ffmpeg7-canThrottle)")
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
			changes = append(changes, "env:strip:"+k)
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
				changes = append(changes, "drop:-eae_prefix(bail)")
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

// tryOptimizeRemux detects + handles Plex Optimize argv shapes where
// video output is `-codec:0 copy` (no transcode). Returns (result,
// true) on match. See callsite for rationale.
func tryOptimizeRemux(args []string, env map[string]string, inputEnv map[string]string) (RewriteResult, bool) {
	iIdx := indexOfArg(args, "-i", 0)
	if iIdx < 0 {
		return RewriteResult{}, false
	}
	// Decoder side: bare stock decoder, no hwaccel.
	dIdx := indexOfArg(args, "-codec:0", 0)
	if dIdx < 0 || dIdx >= iIdx || dIdx+1 >= len(args) {
		return RewriteResult{}, false
	}
	dec := args[dIdx+1]
	if _, ok := hwDecodeShortCodecs[dec]; !ok {
		return RewriteResult{}, false
	}
	if indexOfArg(args, "-hwaccel:0", 0) >= 0 {
		return RewriteResult{}, false
	}
	// Encoder side: first `-codec:0` after -i must be `copy`.
	encIdx := indexOfArg(args, "-codec:0", iIdx+1)
	if encIdx < 0 || encIdx+1 >= len(args) || args[encIdx+1] != "copy" {
		return RewriteResult{}, false
	}

	out := cloneArgs(args)
	changes := []string{"decode:remux:" + dec, "encode:copy(passthrough)"}

	// Plex-private flags pass through natively: `-loglevel_plex` +
	// `-strict_ts:N` (patches 0098/0107 stubs); dashenc additions
	// (`-delete_removed`, `-skip_to_segment`, `-break_non_keyframes`,
	// `-manifest_name`) and segment.c additions (`-segment_list_*`)
	// land via patches 0095/0096. `-xioerror` was never observed in
	// the argv corpus; if it ever surfaces, ffmpeg rejection will
	// flag it loud and we add a strip then.

	// Rewrite -manifest_name URL to point at the relay; ffmpeg's
	// dashenc PUTs the manifest body there natively (scaleplex-ffmpeg7
	// patch 0095). No worker-side publisher.
	var manifestChanges []string
	out, manifestChanges = rewriteManifestName(out, inputEnv)
	changes = append(changes, manifestChanges...)

	// Plex Web Chrome DASH sessions have a SECOND output: subtitle
	// side-channel running `-f segment -segment_format ass` with its
	// own `-segment_list http://127.0.0.1:32400/...?stream=subtitles`
	// loopback URL. Worker pod has no PMS on loopback; rewrite to the
	// relay (variant="side-channel" matches the stream=subtitles
	// fingerprint). Without this the sub side-channel ECONNREFUSED's
	// and ffmpeg exits 145 in the remux fast-path. Observed
	// 2026-05-14 on Plex Web Chrome FMJ DASH playback.
	rewriteSegmentList(out, inputEnv, "", &changes, "side-channel")

	// 2-6. Audio EAE swap, eae_prefix drop, progressurl capture,
	// loglevel + nostats fix-ups, env strips. All shared with the
	// main rewriter — see helpers above for rationale.
	var swapped [][2]string
	out, swapped = swapEAEAudioDecoders(out)
	for _, p := range swapped {
		changes = append(changes, "audio:"+p[0]+"->"+p[1])
	}
	var droppedPrefixes []string
	out, droppedPrefixes = dropEAEPrefixFlags(out)
	for _, d := range droppedPrefixes {
		changes = append(changes, "drop:"+d)
	}
	var progressURL string
	var progressChanges []string
	out, progressURL, progressChanges = capturePMSProgressURL(out, inputEnv)
	changes = append(changes, progressChanges...)
	if newOut, ok := upgradeLoglevelFromQuiet(out); ok {
		out = newOut
		changes = append(changes, "loglevel:->info")
	}
	if newOut, ok := dropNostatsFlag(out); ok {
		out = newOut
		changes = append(changes, "drop:-nostats")
	}
	var envChanges []string
	env, envChanges = stripEAEEnvVars(env)
	changes = append(changes, envChanges...)
	env = setWorkerHomeEnv(env)
	changes = append(changes, "env:HOME")

	return RewriteResult{
		Args:        out,
		Env:         env,
		Applied:     true,
		Changes:     changes,
		ProgressURL: progressURL,
	}, true
}

func Rewrite(inputArgs []string, inputEnv map[string]string, opts *RewriteOpts) RewriteResult {
	sessionDir := ""
	if opts != nil {
		sessionDir = opts.SessionDir
	}

	renderDevice := envOr("HW_RENDER_DEVICE", "/dev/dri/renderD128")
	vaapiDriver := envOr("HW_VAAPI_DRIVER", "iHD")
	// Image-resident defaults: Ubuntu's intel-media-va-driver-non-free
	// installs iHD_drv_video.so under /usr/lib/x86_64-linux-gnu/dri and
	// libva auto-discovers it. HW_LIBVA_DRIVERS_PATH only needs to be
	// non-empty when overriding (e.g. talking to a Plex-bundled cache).
	libvaDriversPath := os.Getenv("HW_LIBVA_DRIVERS_PATH")

	changes := []string{}

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
			bailChanges = append(bailChanges, "drop:-progressurl(bail)")
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
				bailChanges = append(bailChanges, "bail:segment_list:rewrite-to-relay")
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
		merged = append(merged, "skip:"+reason)
		// Applied=true whenever we mutated argv (scrub OR hint drops),
		// so the worker uses our rewritten copy instead of the input.
		applied := len(scrub) > 0 || len(hintChanges) > 0
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
			changes = append(changes, "filter:tonemap_opencl-normalized")
		} else {
			changes = append(changes, "filter:tonemap_opencl->tonemap_vaapi")
		}
	}

	// Plex Optimize remux fast-path. PMS emits a bare `-codec:0 h264`
	// (or hevc/av1/vp9) input decoder — no `-hwaccel:0` — paired with
	// `-codec:0 copy` on the first video output, when the Optimize
	// target preset already matches the source resolution / bitrate
	// and video can be passed through. Worker has nothing to do
	// video-side; the full rewriter pipeline doesn't apply (no
	// init_hw_device, no encoder swap, no filter chain). But the
	// argv still carries Plex-private flags (-loglevel_plex,
	// -progressurl) and EAE audio decoders (-codec:N eac3_eae)
	// that stock ffmpeg can't handle.
	//
	// This branch handles those minimal fixes and returns. Without it
	// the main rewriter bails with "unknown-decoder:h264" (decoder
	// allowlist requires a paired hwaccel) and Optimize never works
	// on non-AV1 sources — observed 2026-05-10 with Pat & Mat
	// (h264) and All Creatures (hevc) → ffmpeg exit 8 on
	// "Unknown decoder 'eac3_eae'". Live test confirmed.
	if remux, ok := tryOptimizeRemux(args, env, inputEnv); ok {
		return remux
	}

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

	for i := 0; i < len(args); i++ {
		if args[i] != "-filter_complex" {
			continue
		}
		f := ""
		if i+1 < len(args) {
			f = args[i+1]
		}
		if strings.Contains(f, "subtitles=") {
			return bail("subtitles-burn-in")
		}
		// HDR (zscale/tonemap) is rewritten to tonemap_vaapi by
		// rewriteVideoFilter; only video chains starting with [0:0] are
		// in-scope. Bail kept off — phase 1 smoke proved tonemap_vaapi
		// works on real HDR10.
	}

	inputIdx := indexOfArg(args, "-i", 0)
	if inputIdx < 0 {
		return bail("no-input")
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
	decCodecIdx := indexOfArg(args, "-codec:0", 0)
	if decCodecIdx < 0 || decCodecIdx >= inputIdx {
		return bail("no-decoder")
	}
	swDecoder := args[decCodecIdx+1]

	isHWDecode := false
	if _, isShort := hwDecodeShortCodecs[swDecoder]; isShort {
		if indexOfArg(args, "-hwaccel:0", 0) >= 0 {
			isHWDecode = true
			changes = append(changes, "decode:hw-passthrough:"+swDecoder)
		} else {
			// Bare short codec name (hevc/h264/av1/vp9) without -hwaccel:0.
			// Only safe to auto-upgrade when the encoder side is genuinely
			// SW-shaped (libx264/libx265) — that's the case where PMS
			// staged a SW pipeline but happened to emit the canonical
			// codec name in the decoder slot. If the encoder is already
			// HW-shaped (e.g. h264_vaapi), the argv is malformed in a way
			// we can't safely reshape; bail rather than guess.
			if peer := indexOfArg(args, "-codec:0", inputIdx+1); peer > 0 && peer+1 < len(args) {
				if _, isSW := encoderMap[args[peer+1]]; !isSW {
					return bail("unknown-decoder:" + swDecoder)
				}
			} else {
				return bail("unknown-decoder:" + swDecoder)
			}
			args = spliceArgs(args, decCodecIdx+2,
				"-hwaccel:0", "vaapi",
				"-hwaccel_output_format:0", "vaapi",
				"-hwaccel_device:0", "vaapi",
			)
			changes = append(changes, "decode:bare-hw-upgrade:"+swDecoder)
		}
	} else if hwDecoder, ok := decoderMap[swDecoder]; ok {
		args[decCodecIdx+1] = hwDecoder
		args = spliceArgs(args, decCodecIdx+2,
			"-hwaccel:0", "vaapi",
			"-hwaccel_output_format:0", "vaapi",
			"-hwaccel_device:0", "vaapi",
		)
		changes = append(changes, "decode:"+swDecoder+"->"+hwDecoder)
	} else {
		return bail("unknown-decoder:" + swDecoder)
	}

	// Detect subtitle source up-front so later phases can act on it.
	// Pass-through is hardcoded: the fork's scaleplex_inlineass binding
	// owns the sidecar -i + null-sub output, so we never drop them.
	var earlySubSrc *subtitleSource
	if !isHWDecode {
		var probe func(string, string) string
		if opts != nil && opts.ProbeSubtitleCodec != nil {
			probe = opts.ProbeSubtitleCodec
		}
		earlySubSrc = detectSubtitleSource(args, sessionDir, probe)
	}

	// 2. -init_hw_device patch or inject (now safe — second -i and
	// its option block already gone for SW sub-burn sessions).
	initIdx := indexOfArg(args, "-init_hw_device", 0)
	if initIdx >= 0 {
		if !reInitHW.MatchString(args[initIdx+1]) {
			return bail("init_hw_device-pattern:" + args[initIdx+1])
		}
		args[initIdx+1] = "vaapi=vaapi:" + renderDevice + ",driver=" + vaapiDriver
		// PMS always pairs -init_hw_device with -filter_hw_device in
		// the 286-entry argv corpus (0 mismatches). No defensive
		// inject needed; we only patch the device path.
		changes = append(changes, "init_hw_device")
	} else {
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
			"-init_hw_device", "vaapi=vaapi:"+renderDevice+",driver="+vaapiDriver,
			"-filter_hw_device", "vaapi",
		)
		changes = append(changes, "inject:init_hw_device+filter_hw_device")
	}

	// Locate output -codec:0 (after -i) up-front; both SW and HW paths
	// reference it for later phases (CRF→QP, preset→cl, sei inject).
	newInputIdx := indexOfArg(args, "-i", 0)
	encCodecIdx := indexOfArg(args, "-codec:0", newInputIdx+1)
	if encCodecIdx < 0 {
		return bail("no-encoder")
	}

	mediaPath := ""
	if i := indexOfArg(args, "-i", 0); i >= 0 && i+1 < len(args) {
		mediaPath = args[i+1]
	}

	// SW-decode-only artefacts. In HW-decode mode PMS already shaped
	// the filter chain, encoder, and map labels for VAAPI; we leave
	// them untouched. Subtitle burn-in for HW-decode mode is not yet
	// supported (PMS likely doesn't request it when HW probe matches).
	var rewritten *filterRewrite
	var subSrc *subtitleSource
	var subPrerender *SubPrerenderSpec
	sourceIsHDR := false

	if !isHWDecode {
		// 3. Video -filter_complex rewrite
		vfIdx := -1
		for i := 0; i < len(args); i++ {
			if args[i] == "-filter_complex" && i+1 < len(args) && strings.HasPrefix(args[i+1], "[0:0]") {
				vfIdx = i + 1
				break
			}
		}
		if vfIdx < 0 {
			return bail("no-video-filter")
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
			label := "subtitle:bitmap:" + subSrc.StreamSpec
			if subSrc.Codec != "" {
				label += "(" + subSrc.Codec + ")"
			}
			changes = append(changes, label)
		}

		// Detect HDR source — Plex's bundled transcoder used to autoinject
		// tonemap (musl-bound opencl, sw fallback); we can't, so we ask the
		// agent to ffprobe color metadata and pass through here. Skipped
		// when no probe is wired (tests treat all sources as SDR by default).
		if opts != nil && opts.ProbeVideoColor != nil && mediaPath != "" {
			transfer, _, _ := opts.ProbeVideoColor(mediaPath)
			if isHDRTransfer(transfer) {
				sourceIsHDR = true
				changes = append(changes, "video:hdr-source("+strings.ToLower(transfer)+")")
			}
		}

		rewritten = rewriteVideoFilter(args[vfIdx], mediaPath, subSrc, sourceIsHDR, tm)
		if rewritten == nil {
			return bail("filter-pattern:" + args[vfIdx])
		}
		args[vfIdx] = rewritten.Filter
		changes = append(changes, "filter:"+rewritten.Mode)

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
				changes = append(changes, "map-label-update")
				break
			}
		}

		// Bitmap subs (overlay_vaapi) need the Plex-private
		// `-map_inlineass <stream>` argv flag stripped. The text path is
		// pass-through and keeps the flag (the fork's binding consumes
		// it). Bitmap sidecar still strips because overlay_vaapi pulls
		// the stream via its own [streamSpec] reference, not via
		// -map_inlineass.
		if strings.HasPrefix(rewritten.Mode, "overlay-vaapi") {
			if miaIdx := indexOfArg(args, "-map_inlineass", 0); miaIdx >= 0 {
				args = removeArgs(args, miaIdx, 2)
				changes = append(changes, "drop:-map_inlineass")
			}
			if rewritten.Sidecar != "" {
				changes = append(changes, "sidecar:"+rewritten.Sidecar)
			}
		}

		// 5. Encoder swap (libx264 → h264_vaapi etc.)
		// Re-locate encCodecIdx because the splices above may have
		// shifted indices.
		newInputIdx = indexOfArg(args, "-i", 0)
		encCodecIdx = indexOfArg(args, "-codec:0", newInputIdx+1)
		if encCodecIdx < 0 {
			return bail("no-encoder")
		}
		swEncoder := args[encCodecIdx+1]
		hwEncoder, ok := encoderMap[swEncoder]
		if !ok {
			return bail("unknown-encoder:" + swEncoder)
		}
		args[encCodecIdx+1] = hwEncoder
		changes = append(changes, "encode:"+swEncoder+"->"+hwEncoder)
	} else {
		// HW-decode mode: PMS already emitted a VAAPI encoder. Validate
		// that, but leave the filter chain, map labels, and encoder
		// argument intact.
		swEncoder := args[encCodecIdx+1]
		switch swEncoder {
		case "h264_vaapi", "hevc_vaapi":
			// expected
		default:
			return bail("hw-decode:unexpected-encoder:" + swEncoder)
		}
		changes = append(changes, "encode:hw-passthrough:"+swEncoder)

		// HDR source detection (diagnostic only). scaleplex does NOT
		// inject a tonemap: when Plex's HW-decode chain is the plain
		// `scale_vaapi=...:format=nv12` shape with no tonemap filter,
		// that means Plex's "Use hardware-accelerated tone mapping" is
		// off and Plex itself does no tonemapping — scaleplex matches
		// that behavior rather than second-guessing it.
		if opts != nil && opts.ProbeVideoColor != nil && mediaPath != "" {
			if transfer, _, _ := opts.ProbeVideoColor(mediaPath); isHDRTransfer(transfer) {
				sourceIsHDR = true
				changes = append(changes, "video:hdr-source("+strings.ToLower(transfer)+")")
			}
		}

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
						strings.HasPrefix(args[i+1], "[0:0]") {
						vfIdx = i + 1
						break
					}
				}
				if vfIdx < 0 {
					return bail("hw-decode-sub:no-inlineass-filter")
				}
				m := reFilterHWAss.FindStringSubmatch(args[vfIdx])
				openclMode := false
				assGroup := 5
				if m == nil {
					// HW VAAPI + OpenCL tonemap variant. PMS emits this
					// when HW tonemap pref is ON. We can swap the OpenCL
					// detour for tonemap_vaapi and keep the inlineass
					// passthrough.
					m = reFilterHWOpenCLAss.FindStringSubmatch(args[vfIdx])
					if m == nil {
						return bail("hw-decode-sub:filter-pattern:" + args[vfIdx])
					}
					openclMode = true
					assGroup = 3
				}
				w, h := m[1], m[2]
				// Force nv12 across the libass step (matches the SW
				// pass-through path). HDR-source detection is diagnostic
				// only — no tonemap is injected here; if Plex wanted one
				// it would carry a tonemap_opencl chain (reFilterHWOpenCLAss).
				if opts != nil && opts.ProbeVideoColor != nil && mediaPath != "" {
					if transfer, _, _ := opts.ProbeVideoColor(mediaPath); isHDRTransfer(transfer) {
						sourceIsHDR = true
						changes = append(changes, "video:hdr-source("+strings.ToLower(transfer)+")")
					}
				}
				scaleStep := fmt.Sprintf("scale_vaapi=w=%s:h=%s:format=nv12", w, h)
				// Animated ASS (karaoke / transform / move / fade) can't
				// be pre-rasterized once per cue — keep the per-frame
				// inlineass path. SRT and static ASS route to the GPU
				// overlay pre-render path in the else branch.
				if subtitleIsAnimated(subSrc.Codec, subSrc.FilePath, os.ReadFile) {
					// Keep Plex's inlineass= filter; strip the 4 Plex-only
					// AVOption keys vf_inlineass doesn't parse. Sidecar -i,
					// -map_inlineass, and null-sub output all stay — the
					// fork's scaleplex_inlineass binding owns those.
					assParams := m[assGroup]
					strippedAss := stripPlexInlineassFilterArgs("inlineass=" + assParams)
					strippedAss = strings.TrimPrefix(strippedAss, "inlineass=")
					args[vfIdx] = fmt.Sprintf(
						"[0:0]hwupload[0];"+
							"[0]%s[1];"+
							"[1]hwdownload,format=nv12[2];"+
							"[2]inlineass=%s[3];"+
							"[3]hwupload[4]",
						scaleStep, strippedAss,
					)
					modeTag := "hw-decode:filter:inlineass-passthrough"
					oldMapLabel := "[4]"
					if openclMode {
						modeTag = "hw-decode:filter:opencl-tonemap->vaapi:inlineass-passthrough"
						// reFilterHWOpenCLAss matches a graph that ends at
						// label [6] (extra hwmap→opencl + tonemap_opencl +
						// hwdownload + inlineass + hwupload). Our rewrite
						// collapses to label [4]; PMS's `-map [6]` must
						// retarget or ffmpeg bails with "Output with label
						// '6' does not exist".
						oldMapLabel = "[6]"
					}
					changes = append(changes, modeTag)
					if oldMapLabel != "[4]" {
						for i := vfIdx + 1; i < len(args)-1; i++ {
							if args[i] != "-map" {
								continue
							}
							v := args[i+1]
							if v == oldMapLabel || v == `"`+oldMapLabel+`"` {
								if strings.HasPrefix(v, `"`) {
									args[i+1] = `"[4]"`
								} else {
									args[i+1] = "[4]"
								}
								changes = append(changes, "hw-decode:map-label-update")
								break
							}
						}
					}
				} else {
					// SRT / static ASS → pre-render the subtitle once per
					// cue into a sparse transparent video and composite it
					// on the GPU with overlay_vaapi, replacing the per-frame
					// CPU inlineass bracket. The agent spawns the pre-render
					// (writing to FIFOPath) from SubPrerenderSpec; the graph
					// reads that FIFO as a second video input.
					// See project_scaleplex_srt_to_pgs_gpu.
					fifoDir := sessionDir
					if fifoDir == "" {
						fifoDir = "/tmp/scaleplex"
					}
					fifoPath := fifoDir + "/scaleplex-sub-overlay.fifo"
					fifoInput := 0
					for _, a := range args {
						if a == "-i" {
							fifoInput++
						}
					}
					// On a seek session the main video reaches the
					// filtergraph at the seek offset (PTS N), but the
					// overlay reaches overlay_vaapi's framesync at ~0 —
					// framesync then drains the overlay 0→N hunting for a
					// frame to pair, and the pre-render grinds out N
					// seconds of overlay (startup latency scales with
					// seek distance). Fix: rebase BOTH branches to 0 with
					// setpts=PTS-STARTPTS so framesync sees a 0-based
					// pair (identical to initial-play, which works), then
					// rebase the composite back to +offset so dashenc and
					// the seek-chunk/tfdt machinery see the unchanged
					// source timeline. Initial play (offset 0) keeps the
					// plain graph untouched.
					seekOff := 0.0
					if si := indexOfArg(args, "-ss", 0); si >= 0 && si+1 < len(args) {
						if v, err := strconv.ParseFloat(args[si+1], 64); err == nil && v > 0 {
							seekOff = v
						}
					}
					if seekOff > 0 {
						args[vfIdx] = fmt.Sprintf(
							"[0:0]hwupload[10];"+
								"[10]%s,setpts=PTS-STARTPTS[11];"+
								"[%d:v]setpts=PTS-STARTPTS,format=bgra,hwupload[12];"+
								"[11][12]overlay_vaapi=eof_action=pass:repeatlast=1[13];"+
								"[13]setpts=PTS+%s/TB[4]",
							scaleStep, fifoInput,
							strconv.FormatFloat(seekOff, 'f', 3, 64),
						)
					} else {
						args[vfIdx] = fmt.Sprintf(
							"[0:0]hwupload[10];"+
								"[10]%s[11];"+
								"[%d:v]format=bgra,hwupload[12];"+
								"[11][12]overlay_vaapi=eof_action=pass:repeatlast=1[4]",
							scaleStep, fifoInput,
						)
					}
					// Drop `-map_inlineass <spec>` — no inlineass filter
					// consumes it on this path; the pre-render reads the
					// subtitle itself.
					if mi := indexOfArg(args, "-map_inlineass", 0); mi >= 0 && mi+1 < len(args) {
						args = removeArgs(args, mi, 2)
					}
					// Append the overlay FIFO immediately after the last
					// existing input's path. It must NOT go just before
					// -filter_complex: Plex puts output-side options
					// (-start_at_zero, -copyts, -fps_mode) between the last
					// input and the filtergraph, and a new -i there makes
					// ffmpeg mis-parse those as input options for the FIFO.
					//
					// `-copyts` on the FIFO input is required: ffmpeg
					// rebases a plain input's timestamps to start at zero,
					// but on a seek session the main video keeps its real
					// (non-zero) PTS via its own -copyts. Without -copyts
					// here the overlay would rebase to 0 while the main
					// video stays at the seek offset — overlay_vaapi
					// framesync never aligns and the transcode stalls.
					//
					// `-probesize 32 -analyzeduration 0`: without them
					// find_stream_info reads up to probesize (5 MB) of the
					// FIFO before returning, and since the overlay is
					// sparse that forces the pre-render to grind seconds
					// of timeline ahead while the main ffmpeg blocks —
					// ~5 s of startup latency. The Matroska header alone
					// gives the codec and dimensions, so a minimal probe
					// returns in ~0.8 s (measured).
					lastInput := -1
					for i := 0; i+1 < len(args); i++ {
						if args[i] == "-i" {
							lastInput = i
						}
					}
					if lastInput >= 0 {
						args = spliceArgs(args, lastInput+2,
							"-copyts", "-probesize", "32", "-analyzeduration", "0",
							"-i", fifoPath)
					}
					// reFilterHWOpenCLAss argv ends at label [6] with
					// `-map [6]`; our overlay graph outputs [4]. Retarget.
					if openclMode {
						for i := 0; i+1 < len(args); i++ {
							if args[i] != "-map" {
								continue
							}
							if args[i+1] == "[6]" {
								args[i+1] = "[4]"
								break
							}
							if args[i+1] == `"[6]"` {
								args[i+1] = `"[4]"`
								break
							}
						}
					}
					srcPath := subSrc.FilePath
					if srcPath == "" {
						if ii := indexOfArg(args, "-i", 0); ii >= 0 && ii+1 < len(args) {
							srcPath = args[ii+1]
						}
					}
					wInt, _ := strconv.Atoi(w)
					hInt, _ := strconv.Atoi(h)
					subPrerender = &SubPrerenderSpec{
						FIFOPath:   fifoPath,
						SourcePath: srcPath,
						StreamSpec: subSrc.StreamSpec,
						Embedded:   subSrc.FilePath == "",
						Width:      wInt,
						Height:     hInt,
						ForceStyle: plexInlineassToForceStyle(m[assGroup]),
					}
					changes = append(changes, "hw-decode:filter:sub-prerender-overlay")
				}
				newInputIdx = indexOfArg(args, "-i", 0)
				encCodecIdx = indexOfArg(args, "-codec:0", newInputIdx+1)
			case "bitmap":
				return bail("hw-decode-sub:bitmap-unsupported")
			}
		}
	}

	// 6. -crf:0 <Q> → -qp:0 <Q + HW_QP_CRF_OFFSET> (CQP mode).
	//
	// Plex emits `-crf:0 <Q> -maxrate:0 <R> -bufsize:0 <B>`. With libx264
	// this is "VBR with quality target Q, bitrate capped at R". h264_vaapi
	// has no clean equivalent — when `-qp:0` is set the encoder runs in
	// CQP and ignores -maxrate. We accept that compromise (over budget
	// on complex 4K HDR scenes) because the alternative (force VBR via
	// `-rc_mode VBR -b:v <R>`) breaks rate control on -ss seek: live
	// bench 2026-05-06 showed iHD producing 100 Mbps segments after a
	// seek even when -rc_mode VBR + -b:v 20Mbps + -bufsize 40Mb were
	// all explicit on the encoder context.
	//
	// CRF and QP aren't the same scale even though both are 0-51:
	// libx264 CRF=16 averages ~QP 20-22 in practice (CRF adjusts QP per
	// frame around its target), while VAAPI's `-qp` is the literal
	// quantizer. Mapping CRF=16 → QP=16 produces near-lossless output —
	// 4K HDR segments came in at 14 MB / 1 second on Balls Up
	// (~110 Mbps, 5× over Plex's 20 Mbps target). Add an offset so the
	// VAAPI QP lands closer to libx264's effective QP:
	//
	//   target_qp = clamp(crf + offset, 0, 51)
	//
	// Default offset is 6: empirically lines QP up with x264's average
	// at the same perceptual quality level (Balls Up isolated bench
	// 2026-05-06: QP=22 produced 5 MB + 3 MB / 1 second segments,
	// ~40 Mbps — closer to budget while still better than Plex's
	// 20 Mbps target nominal). Override via HW_QP_CRF_OFFSET if the
	// quality/bitrate trade-off needs tuning.
	if crfIdx := indexOfArg(args, "-crf:0", encCodecIdx+1); crfIdx >= 0 && crfIdx+1 < len(args) {
		args[crfIdx] = "-qp:0"
		offset := 6
		if v := os.Getenv("HW_QP_CRF_OFFSET"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				offset = n
			}
		}
		if crf, err := strconv.Atoi(args[crfIdx+1]); err == nil {
			qp := crf + offset
			if qp < 0 {
				qp = 0
			}
			if qp > 51 {
				qp = 51
			}
			args[crfIdx+1] = strconv.Itoa(qp)
			changes = append(changes, fmt.Sprintf("crf%d->qp%d(off=%d)", crf, qp, offset))
		} else {
			changes = append(changes, "crf->qp")
		}
	}

	// 7. Translate -preset:0 <x264-name> → -compression_level:v <N>
	// (Plex emits x264 preset names; iHD VAAPI uses a 1-7 TargetUsage
	// scale where 7 = fastest, 1 = highest quality.)
	//
	// When PMS doesn't emit a preset (e.g. x265 path with
	// `-x265-params` instead), do nothing — let stock vaapi_encode
	// leave `compression_level == FF_COMPRESSION_DEFAULT`, which
	// dispatches to the iHD driver's intrinsic default (~TU=4
	// balanced). Matches Plex Transcoder's prod behaviour (their
	// `-quality` AVOption defaults to 0 → driver-vendor-default).
	// We previously injected `cl=7` (max speed) here, which was
	// +30-70% throughput vs cl=2 on no-sub workloads but more
	// aggressive than Plex on the quality axis. Bandaid B5 retired
	// 2026-05-15.
	if i := indexOfArg(args, "-preset:0", encCodecIdx+1); i >= 0 && i+1 < len(args) {
		preset := args[i+1]
		cl := mapX264PresetToVAAPI(preset)
		args = removeArgs(args, i, 2)
		// Inject right after the encoder so the encoder context picks it up.
		args = spliceArgs(args, encCodecIdx+2, "-compression_level:v", cl)
		changes = append(changes, "preset:"+preset+"->compression_level:"+cl)
	}

	// Drop the remaining SW-encoder-specific flags.
	for _, flag := range []string{"-x264opts:0", "-x265-params:0"} {
		if i := indexOfArg(args, flag, encCodecIdx+1); i >= 0 {
			args = removeArgs(args, i, 2)
			changes = append(changes, "drop:"+flag)
		}
	}

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
	if indexOfArg(args, "-sei:0", 0) < 0 {
		if fkfIdx := indexOfArg(args, "-force_key_frames:0", encCodecIdx+1); fkfIdx >= 0 {
			args = spliceArgs(args, fkfIdx, "-sei:0", "-a53_cc")
			changes = append(changes, "inject:sei+a53_cc")
		}
	}

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
			changes = append(changes, "audio:"+p[0]+"->"+p[1])
		}
	}
	{
		var dropped []string
		args, dropped = dropEAEPrefixFlags(args)
		for _, d := range dropped {
			changes = append(changes, "drop:"+d)
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
			changes = append(changes, "skip_to_segment:passthrough="+args[i+1])
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
			changes = append(changes, fmt.Sprintf("seek-offset:captured=%.3fs", v))
		}
	}
	// The subtitle pre-render timeline must start at the same offset as
	// a seek session's main video, or overlay_vaapi framesync places
	// the burned text at the wrong time.
	if subPrerender != nil {
		subPrerender.SeekOffsetSeconds = seekOffsetSeconds
	}

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
		changes = append(changes, "loglevel:->info")
	}
	if newArgs, ok := dropNostatsFlag(args); ok {
		args = newArgs
		changes = append(changes, "drop:-nostats")
	}

	{
		var echanges []string
		env, echanges = stripEAEEnvVars(env)
		changes = append(changes, echanges...)
	}

	// VAAPI driver discovery env. Only override if explicitly set;
	// libva otherwise auto-discovers iHD on the worker image.
	env["LIBVA_DRIVER_NAME"] = vaapiDriver
	if libvaDriversPath != "" {
		env["LIBVA_DRIVERS_PATH"] = libvaDriversPath
	}
	changes = append(changes, "env:LIBVA")

	env = setWorkerHomeEnv(env)
	changes = append(changes, "env:HOME")

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
		SubPrerender:      subPrerender,
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
