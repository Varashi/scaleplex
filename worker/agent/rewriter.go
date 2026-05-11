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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// SubtitleExtract describes a side ffmpeg invocation the agent must run
// before spawning the main encoder, when Plex requested burn-in of an
// embedded subtitle stream. Pre-extract → file → `subtitles=filename=...`
// because stock ffmpeg's `subtitles=` filter only takes filenames, not
// stream specifiers.
type SubtitleExtract struct {
	SourceFile string // input mkv path (the `-i 0` value)
	StreamSpec string // e.g. "0:3" — what -map_inlineass pointed at
	OutputFile string // path the agent writes the extracted sub to
	Format     string // "srt" or "ass" — codec the agent should muxer to
}

type RewriteResult struct {
	Args    []string
	Env     map[string]string
	Applied bool
	Changes []string

	// SubtitleExtract — non-nil when the rewriter needs the agent to
	// run `ffmpeg -i <SourceFile> -map <StreamSpec> -c:s <Format>
	// <OutputFile>` synchronously before spawning the main transcode.
	// Populated only on burn-in sessions where Plex's -map_inlineass
	// referenced an embedded stream (single `-i`, spec like `0:3`).
	// Sidecar burn-in (Plex pre-stages the file as a second `-i`)
	// sets this to nil — the file is already on disk.
	SubtitleExtract *SubtitleExtract
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
	// ManifestURL — when non-empty, the agent must POST the DASH
	// manifest body to this URL whenever ffmpeg's output `dash` file
	// is updated. Plex's ffmpeg fork drives this from a full-URL
	// `-manifest_name`, but mainline dashenc.c doesn't recognise an
	// HTTP `manifest_name` and writes manifest to a local file
	// instead. Without the POST, PMS waits SegmentedTranscoderTimeout
	// (~125s) on `/header` before falling back to disk-probing
	// init-stream0.m4s. Captured + rewritten the same way as
	// ProgressURL.
	ManifestURL string
	// SkipToSegment — value captured from Plex's `-skip_to_segment N`
	// argv on a seek transcode session. Plex's ffmpeg fork starts the
	// dash muxer's segment index at N so chunk-stream0-NNNNN.m4s aligns
	// with PMS's request URL `.../0/(N-1).m4s`. Stock dashenc has no
	// way to override the starting segment_index — it always counts
	// from 1. The chunk-renumber watcher uses this value to hardlink
	// ffmpeg's 1-indexed chunks to the N-indexed names PMS expects.
	// Zero = initial-play session (no seek, renumber starts at 1).
	SkipToSegment int
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
	// req.Cwd). Used as the staging path for embedded-subtitle
	// extraction. When empty, falls back to /tmp/scaleplex.
	SessionDir string
	// ProbeSubtitleCodec — when non-nil, the rewriter calls this to
	// learn the codec_name of the subtitle stream Plex's
	// -map_inlineass references. The result picks between two burn-in
	// chains: text (subrip/ass/mov_text/...) → `subtitles=filename=`
	// libass chain; bitmap (hdmv_pgs_subtitle/dvb_subtitle/...) →
	// `overlay_vaapi` stream-overlay chain. Args: source file path,
	// stream specifier (e.g. "0:3", "1:s:0"). Returns lowercase codec
	// name or "" on probe failure (treated as text by default,
	// extraction will likely fail loud and surface the unknown
	// codec). Production agent wires this to a synchronous ffprobe;
	// tests inject a fake.
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

func mapX264PresetToVAAPI(preset string) string {
	if v, ok := x264PresetToVAAPI[strings.ToLower(preset)]; ok {
		return v
	}
	// Unknown preset → fastest. Worker only runs when called by orch;
	// playback latency wins over an unfamiliar quality knob.
	return "7"
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
	// We swap `inlineass=...` for stock `subtitles=filename=...` and
	// keep the surrounding hwupload/scale_vaapi/hwdownload chain.
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
	reLanguage = regexp.MustCompile(`(?:^|:)language=([a-zA-Z]{2,3})`)
	// reInitHW accepts both PMS argv shapes for `-init_hw_device`:
	//   `vaapi=vaapi:`                                — SW-decode, PMS
	//   doesn't know the device because the worker chooses it.
	//   `vaapi=vaapi:/dev/dri/renderDNNN[,driver=NAME]` — HW-decode,
	//   PMS reads HardwareDevicePath + iHD driver from its own probe.
	// In either case the rewriter overwrites with HW_RENDER_DEVICE +
	// HW_VAAPI_DRIVER defaults so the worker pod's device wins.
	reInitHW   = regexp.MustCompile(`^vaapi=vaapi:(?:/dev/dri/[A-Za-z0-9_]+(?:,driver=[A-Za-z0-9_]+)?)?$`)
)

func envBool(k string) bool { return os.Getenv(k) == "true" }

func envOr(k, dflt string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return dflt
}

// subtitlesForceStyle returns the `:force_style='...'` suffix for the stock
// `subtitles=` filter. Pinning the font face skips libass's per-frame
// fontselect via fontconfig (the dominant cold-init cost). Override via
// HW_SUBTITLE_FORCE_STYLE; empty value disables. Default mirrors the
// DejaVu Sans face shipped in the slim worker fontsdir.
func subtitlesForceStyle() string {
	v := envOr("HW_SUBTITLE_FORCE_STYLE", "FontName=DejaVu Sans")
	if v == "" {
		return ""
	}
	return ":force_style='" + v + "'"
}

// subtitlesSeekShift returns (pre, post) setpts pieces to bracket a stock
// `subtitles=filename=` filter when -copyts has been stripped (HLS path)
// AND the session is seek-resumed. Without the bracket, frame PTS arrives
// at the libass filter rebased to 0, while SRT cues live at absolute
// time — every cue lookup misses, subs render blank for the entire
// session. The pre setpts offsets PTS by the seek time so libass sees
// absolute time and matches cues; the post setpts undoes the offset so
// the segment muxer emits 0-based chunk PTS (the relay sidecar later
// rewrites CSV start_time to the expected global-timeline window).
//
// Returns ("", "") when no shift is needed (seekOff <= 0). Pre is meant
// to be appended after a comma-separated filter chain segment (leading
// comma included); post is meant to be prepended (trailing comma
// included).
func subtitlesSeekShift(seekOff float64) (pre, post string) {
	if seekOff <= 0 {
		return "", ""
	}
	return fmt.Sprintf(",setpts=PTS+%.3f/TB", seekOff),
		fmt.Sprintf("setpts=PTS-%.3f/TB,", seekOff)
}

// captureSeekSeconds returns the value of the first `-ss N` argv pair as
// seconds, or 0 if none/invalid. Used by the HW + SW sub-burn paths to
// know whether to bracket libass with PTS-shift setpts pieces.
func captureSeekSeconds(args []string) float64 {
	if i := indexOfArg(args, "-ss", 0); i >= 0 && i+1 < len(args) {
		if v, err := strconv.ParseFloat(args[i+1], 64); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

func indexOfArg(args []string, key string, from int) int {
	for i := from; i < len(args); i++ {
		if args[i] == key {
			return i
		}
	}
	return -1
}

// findSidecarSubtitle probes the source media's directory for a sibling
// SRT/ASS subtitle file. Probe order, given lang="en":
//
//   <base>.en.srt
//   <base>.en.ass
//   <base>.en.<flag>.srt   for flag in hi cc sdh forced default
//   <base>.en.<flag>.ass
//   <base>.<flag>.en.srt   (Sonarr alt ordering)
//   <base>.<flag>.en.ass
//   <base>.srt
//   <base>.ass
//
// Sonarr/Radarr writes hearing-impaired tracks as `<base>.en.hi.srt`,
// closed-caption as `.en.cc.srt`, signs/songs forced as `.en.forced.srt`,
// and the default track as `.en.default.srt`. Without those expansions
// the probe misses every file Sonarr just imported and we fall through
// to the hybrid-inlineass bail (which means stock ffmpeg has no usable
// subtitle source and the whole transcode fails when the client asks
// for sub burn-in — exactly what hit LG WebOS on Balls Up
// 2026-05-06: only `.en.hi.srt` and `.nl.srt` existed, the probe looked
// for `.en.srt` and gave up).
func findSidecarSubtitle(mediaPath, lang string, fsExists func(string) bool) string {
	if mediaPath == "" {
		return ""
	}
	dir := filepath.Dir(mediaPath)
	ext := filepath.Ext(mediaPath)
	base := strings.TrimSuffix(filepath.Base(mediaPath), ext)

	flags := []string{"hi", "cc", "sdh", "forced", "default"}
	exts := []string{"srt", "ass"}

	var cands []string
	if lang != "" {
		// <base>.<lang>.<ext>
		for _, e := range exts {
			cands = append(cands, filepath.Join(dir, base+"."+lang+"."+e))
		}
		// <base>.<lang>.<flag>.<ext>
		for _, fl := range flags {
			for _, e := range exts {
				cands = append(cands, filepath.Join(dir, base+"."+lang+"."+fl+"."+e))
			}
		}
		// <base>.<flag>.<lang>.<ext>
		for _, fl := range flags {
			for _, e := range exts {
				cands = append(cands, filepath.Join(dir, base+"."+fl+"."+lang+"."+e))
			}
		}
	}
	// Last-resort: language-less defaults.
	for _, e := range exts {
		cands = append(cands, filepath.Join(dir, base+"."+e))
	}
	for _, c := range cands {
		if fsExists(c) {
			return c
		}
	}
	return ""
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
	// FilePath (text path only): a filesystem path to feed into
	// `subtitles=filename=`. For sidecar text: Plex's pre-staged
	// temp file. For embedded text: the agent's planned extraction
	// target.
	FilePath string
	// Extract (text+embedded only): non-nil when the agent must run
	// a side ffmpeg before spawn to produce FilePath.
	Extract *SubtitleExtract
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
//   1. Embedded text (SRT/ASS in mkv):
//        -i source.mkv
//        -map_inlineass 0:3              (codec=subrip|ass|...)
//      → text path, agent extracts to <sessionDir>/scaleplex-extract.srt
//
//   2. External text sidecar (Plex pre-stages):
//        -i source.mkv
//        -i /transcode/.../temp-0.srt
//        -map_inlineass 1:s:0            (codec=subrip|ass|...)
//      → text path, use staged file directly, drop second -i
//
//   3. Embedded bitmap (PGS/VobSub/DVDSub):
//        -i source.mkv
//        -map_inlineass 0:3              (codec=hdmv_pgs_subtitle|...)
//      → bitmap path, no extraction; filter graph references
//        [0:3] as a stream and overlays it via overlay_vaapi
//
//   4. External bitmap sidecar (rare; .sup files):
//        -i source.mkv
//        -i sidecar.sup
//        -map_inlineass 1:s:0            (codec=hdmv_pgs_subtitle|...)
//      → bitmap path, KEEP second -i (filter pulls the stream from it)
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
		// Embedded.
		if kind == "text" {
			if sessionDir == "" {
				sessionDir = "/tmp/scaleplex"
			}
			outputFile := filepath.Join(sessionDir, "scaleplex-extract.srt")
			res.FilePath = outputFile
			res.Extract = &SubtitleExtract{
				SourceFile: args[inputArgIdxs[0]+1],
				StreamSpec: streamSpec,
				OutputFile: outputFile,
				Format:     "srt",
			}
		}
		// Bitmap: nothing to extract; filter graph references the
		// stream directly via [streamSpec].
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

// rewriteVideoFilter translates Plex's filter graph into a stock-ffmpeg
// equivalent. subSrc, when non-nil, carries the resolved subtitle
// burn-in source (text path or bitmap stream); the function picks the
// matching filter shape. See detectSubtitleSource for source resolution.
//
// sourceIsHDR triggers an implicit tonemap_vaapi injection when the
// matched filter is the SDR-target "plain" pattern. Plex's bundled
// transcoder relied on its own tonemap (opencl/cuda/sw) firing
// implicitly; we have to spell it out for stock ffmpeg or the encoder
// receives PQ-quantized values mapped into BT.709 range without
// tonemapping → washed colors on every HDR-on-SDR-client transcode.
//
// Falls back to the legacy fs-probe (findSidecarSubtitle) when no
// PMS-staged source is present — defensive for older PMS argv shapes
// and tests that don't wire -map_inlineass through.
func rewriteVideoFilter(filterStr, mediaPath string, subSrc *subtitleSource, fsExists func(string) bool, overlayEnabled, sourceIsHDR bool, seekShiftSeconds float64) *filterRewrite {
	if m := reFilterAss.FindStringSubmatch(filterStr); m != nil {
		w, h, assParams := m[1], m[2], m[3]
		_ = assParams
		var lang string
		if lm := reLanguage.FindStringSubmatch(assParams); lm != nil {
			lang = strings.ToLower(lm[1])
		}

		if !overlayEnabled {
			return nil
		}

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
		// HDR-aware scale step. When source is HDR but encoder targets
		// SDR (which sub burn-in always does — both `subtitles=` and
		// `overlay_vaapi` produce BT.709 output), the scale_vaapi must
		// run in p010 → tonemap_vaapi → nv12. Without it the PQ values
		// crash into BT.709 NV12 range with no transfer-function
		// conversion → washed colors. Same root cause as the plain-
		// filter HDR path (see reFilterPlain branch below).
		scaleStep := fmt.Sprintf("scale_vaapi=w=%s:h=%s:format=nv12", w, h)
		if sourceIsHDR {
			scaleStep = fmt.Sprintf(
				"scale_vaapi=w=%s:h=%s:format=p010,tonemap_vaapi=transfer=bt709:format=nv12",
				w, h)
		}

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

		// Text subs (SRT/ASS/MOV_TEXT/...): libass renders the file
		// into a CPU frame, hwupload back to GPU for the encoder.
		sidecar := ""
		if subSrc != nil && subSrc.Kind == "text" {
			sidecar = subSrc.FilePath
		}
		if sidecar == "" {
			// Legacy fs-probe fallback for argvs without -map_inlineass
			// (or test fixtures that don't wire it through). Should be
			// unreachable on real PMS argvs.
			sidecar = findSidecarSubtitle(mediaPath, lang, fsExists)
		}
		if sidecar != "" {
			subPath := escapeFilterPath(sidecar)
			fontsDir := envOr("HW_FONTS_DIR", "/usr/share/fonts/truetype/dejavu")
			mode := "overlay-vaapi"
			if sourceIsHDR {
				mode = "overlay-vaapi-hdr"
			}

			// 1080split path tried (commit 925a7c3 + benched on iHD/Arc
			// A310 2026-05-06): theory was render libass at 1080p +
			// overlay_vaapi composite to save CPU memory bandwidth at
			// 4K targets. Empirically slower at every output resolution
			// — 4K: 1.01x vs native 1.28x; 3072x1280: 1.57x vs 2.32x.
			// overlay_vaapi composition on iHD is more expensive than
			// the libass roundtrip it replaces. Reverted; keeping the
			// native render-at-output-res path for all sizes.
			preShift, postShift := subtitlesSeekShift(seekShiftSeconds)
			return &filterRewrite{
				Filter: fmt.Sprintf(
					"[0:0]hwupload[10];"+
						"[10]%s[11];"+
						"[11]hwdownload[12];"+
						"[12]format=pix_fmts=nv12%s[13];"+
						"[13]subtitles=filename='%s':fontsdir=%s%s[14];"+
						"[14]%shwupload[15]",
					scaleStep, preShift, subPath, fontsDir, subtitlesForceStyle(), postShift),
				OldLabel: "[2]",
				NewLabel: "[15]",
				Mode:     mode,
				Sidecar:  sidecar,
			}
		}

		// No usable subtitle source resolved. Bail loud.
		_ = w
		_ = h
		return nil
	}

	if m := reFilterHDR.FindStringSubmatch(filterStr); m != nil {
		w, h, finalIdx := m[1], m[2], m[3]
		return &filterRewrite{
			Filter: fmt.Sprintf(
				"[0:0]hwupload[0];"+
					"[0]scale_vaapi=w=%s:h=%s:format=p010,"+
					"tonemap_vaapi=transfer=bt709:format=nv12[1];"+
					"[1]hwupload[2]",
				w, h),
			OldLabel: "[" + finalIdx + "]",
			NewLabel: "[2]",
			Mode:     "hdr-tonemap-vaapi",
		}
	}

	if m := reFilterPlain.FindStringSubmatch(filterStr); m != nil {
		w, h := m[1], m[2]
		if sourceIsHDR {
			// Plex's argv targets SDR (`format=pix_fmts=yuv420p|nv12`)
			// but the source video is HDR — inject tonemap_vaapi so
			// the encoder gets BT.709-mapped values instead of raw
			// PQ. Without this the output looks washed and clipped on
			// every HDR remux played to an SDR client (observed on
			// Balls Up + LG WebOS 2026-05-06: filter chain matched
			// "plain", no tonemap, colors visibly off).
			return &filterRewrite{
				Filter: fmt.Sprintf(
					"[0:0]hwupload[0];"+
						"[0]scale_vaapi=w=%s:h=%s:format=p010,"+
						"tonemap_vaapi=transfer=bt709:format=nv12[1];"+
						"[1]hwupload[2]",
					w, h),
				OldLabel: "[1]",
				NewLabel: "[2]",
				Mode:     "hdr-tonemap-vaapi-implicit",
			}
		}
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

// captureManifestName rewrites `-manifest_name <url>` in-place: the
// loopback URL (which workers can't reach) becomes the relay's base
// URL, with X_PLEX_TOKEN appended. ffmpeg then PUTs the manifest body
// to that URL natively via dashenc's HTTP protocol handler — no
// worker-side publisher needed (scaleplex-ffmpeg7 backports Plex's
// -manifest_name extension, libavformat/dashenc.c).
//
// Returns (updated args, empty string, change tags). The empty string
// preserves the prior helper signature (RewriteResult.ManifestURL is
// still wired through call sites and consumed by manifest_publish.go,
// but the publisher is now a no-op when ManifestURL is empty).
//
// If the URL doesn't look like a PMS loopback or no SCALEPLEX_PMS_BASE_URL
// is set we strip the flag entirely — stock dashenc would write the
// manifest into a file literally named `http:` otherwise.
func captureManifestName(args []string, inputEnv map[string]string) ([]string, string, []string) {
	i := indexOfArg(args, "-manifest_name", 0)
	if i < 0 || i+1 >= len(args) {
		return args, "", nil
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
		return args, "", []string{"manifest_name:rewrite-to-relay"}
	}
	args = removeArgs(args, i, 2)
	return args, "", []string{"drop:-manifest_name(no-pms-base-or-non-loopback)"}
}

// capturePMSProgressURL strips `-progressurl <url>` from args, rewrites
// the PMS host to SCALEPLEX_PMS_BASE_URL, and appends the per-session
// X_PLEX_TOKEN as a query param. Returns updated args, the rewritten
// URL (empty when capture failed for any reason), and the change tags
// to record.
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
	return args, rewritten, changes
}

// upgradeLoglevelFromQuiet rewrites `-loglevel quiet|panic|fatal` to
// `info`. PMS's JobRunner expects "Stream mapping:" lines on stderr to
// detect transcoder readiness; quiet stalls /header for ~125s.
func upgradeLoglevelFromQuiet(args []string) ([]string, bool) {
	if i := indexOfArg(args, "-loglevel", 0); i >= 0 && i+1 < len(args) {
		if v := args[i+1]; v == "quiet" || v == "panic" || v == "fatal" {
			args[i+1] = "info"
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

// stripEAEEnvVars removes EAE_ROOT and FFMPEG_EXTERNAL_LIBS — both
// point at Plex Transcoder paths that don't exist on the worker pod.
// X_PLEX_TOKEN is intentionally KEPT for the progress reporter.
func stripEAEEnvVars(env map[string]string) (map[string]string, []string) {
	var changes []string
	for _, k := range []string{"EAE_ROOT", "FFMPEG_EXTERNAL_LIBS"} {
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
//   -codec:STREAMSPEC, -c:STREAMSPEC
//   -c:v / -c:a / -c:s / -c:d / -c:t  (type-only shorthand)
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

// reHWPassthroughSDRChain matches PMS's HW-decode HDR→SDR filter
// chain — the case where PMS naively drops 10-bit HDR (p010) to 8-bit
// SDR (nv12) by setting format=nv12 on scale_vaapi, with no
// transfer-function conversion. The pattern is the canonical 3-step
// shape PMS emits when targeting an 8-bit encoder (h264_vaapi, or
// hevc_vaapi default Main profile):
//
//   [0:0]hwupload[A];[A]scale_vaapi=w=W:h=H:format=nv12[B];[B]hwupload[C]
//
// Backreferences aren't supported in RE2; we capture all six tokens
// and validate label-pair equality in injectHWPassthroughTonemap.
//
// Captures: source-stream label, A, W, H, A-consumer / B-producer,
// B-consumer / C-producer, final label C.
var reHWPassthroughSDRChain = regexp.MustCompile(
	`^\[([0-9:]+)\]hwupload\[(\w+)\];` +
		`\[(\w+)\]scale_vaapi=w=(\d+):h=(\d+):format=nv12\[(\w+)\];` +
		`\[(\w+)\]hwupload\[(\w+)\]$`,
)

// injectHWPassthroughTonemap rewrites the matched chain to route
// through tonemap_vaapi for proper PQ→BT.709 transfer-function
// conversion. The result has the same final output label so PMS's
// `-map [<final>]` keeps resolving:
//
//   [0:0]hwupload[a];[a]scale_vaapi=...:format=p010[b];[b]tonemap_vaapi=transfer=bt709:format=nv12[final]
//
// (Drops the redundant trailing hwupload — tonemap_vaapi outputs to
// a VAAPI surface already.)
//
// Returns (newFilter, true) on match, ("", false) otherwise.
func injectHWPassthroughTonemap(filterStr string) (string, bool) {
	m := reHWPassthroughSDRChain.FindStringSubmatch(filterStr)
	if m == nil {
		return "", false
	}
	in, a1, a2, w, h, b1, b2, final := m[1], m[2], m[3], m[4], m[5], m[6], m[7], m[8]
	// Label pairs must connect: a1 (output of first hwupload) ==
	// a2 (input of scale_vaapi); b1 (output of scale_vaapi) ==
	// b2 (input of trailing hwupload). Otherwise it's not the chain
	// we recognise.
	if a1 != a2 || b1 != b2 {
		return "", false
	}
	out := fmt.Sprintf(
		"[%s]hwupload[%s];[%s]scale_vaapi=w=%s:h=%s:format=p010[%s];[%s]tonemap_vaapi=transfer=bt709:format=nv12[%s]",
		in, a1, a1, w, h, b1, b1, final,
	)
	return out, true
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

	// 1. Strip Plex-private flags. scaleplex-ffmpeg7 natively supports
	// the dashenc additions (`-delete_removed`, `-skip_to_segment`,
	// `-break_non_keyframes`, `-manifest_name`) and segment.c
	// additions (`-segment_list_*`), so those pass through unchanged.
	// Only the truly Plex-runtime-only flags remain on the strip-list.
	//   -strict_ts*: Plex movenc extension (not in our fork)
	//   -loglevel_plex: Plex stderr level alias
	// `-manifest_name <url>` URL gets rewritten in-place below so the
	// HTTP PUT lands on the relay instead of the worker's loopback.
	for _, flag := range []string{
		"-loglevel_plex",
		"-strict_ts:0",
		"-strict_ts",
	} {
		for {
			i := indexOfArg(out, flag, 0)
			if i < 0 || i+1 >= len(out) {
				break
			}
			out = removeArgs(out, i, 2)
			changes = append(changes, "drop:"+flag)
		}
	}

	// Capture -manifest_name URL so manifest_publish.go can POST the
	// .mpd body to the relay on every chunk flush. Without this, PMS
	// 404's on /header chunks (it gates them on the first manifest POST).
	var manifestURL string
	var manifestChanges []string
	out, manifestURL, manifestChanges = captureManifestName(out, inputEnv)
	changes = append(changes, manifestChanges...)

	for {
		i := indexOfArg(out, "-xioerror", 0)
		if i < 0 {
			break
		}
		out = removeArgs(out, i, 1)
		changes = append(changes, "drop:-xioerror")
	}

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
		ManifestURL: manifestURL,
	}, true
}

// dropSidecarInput removes the sidecar `-i` AND any per-input options
// that preceded it (everything between input-0's path and input-1's
// path). Per ffmpeg argv spec those options apply to the next `-i`,
// so removing only the `-i` flag leaves them dangling — they then
// re-bind as positional output options on the LAST `-i`. PMS in
// seek+sub-burn mode places `-ss <T>` in this slot so the SRT input
// seeks to the same timestamp; if it dangles after our drop it
// becomes an output seek, ffmpeg discards every encoded frame whose
// PTS < T, and the segment muxer hangs forever. Returns the modified
// args slice and true if anything was removed.
func dropSidecarInput(args []string, secondInputIdx int) ([]string, bool) {
	if secondInputIdx <= 0 || secondInputIdx+1 >= len(args) {
		return args, false
	}
	firstInputIdx := indexOfArg(args, "-i", 0)
	startDrop := secondInputIdx
	if firstInputIdx >= 0 && firstInputIdx < secondInputIdx {
		startDrop = firstInputIdx + 2 // after input-0's `-i path`
	}
	n := secondInputIdx + 2 - startDrop
	return removeArgs(args, startDrop, n), true
}

// stripNullSubOutput removes Plex's trailing null subtitle output
// declaration: `-map <sub-stream-spec> -f null -codec ass <output_name>`.
// Plex appends this as a second output after the main segment muxer's
// filename, with the -map referring to the sidecar input. Once we drop
// that input, the -map dangles and ffmpeg fails with "stream specifier
// matches no streams". Mutates *args in place; returns true if the
// pattern was found and removed.
func stripNullSubOutput(args *[]string) bool {
	a := *args
	for i := 0; i+6 < len(a); i++ {
		if a[i] != "-map" {
			continue
		}
		if a[i+2] != "-f" || a[i+3] != "null" {
			continue
		}
		if a[i+4] != "-codec" || a[i+5] != "ass" {
			continue
		}
		// a[i+1] is the stream-spec, a[i+6] is the output name.
		*args = removeArgs(a, i, 7)
		return true
	}
	return false
}

func Rewrite(inputArgs []string, inputEnv map[string]string, opts *RewriteOpts) RewriteResult {
	fsExists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	sessionDir := ""
	if opts != nil {
		if opts.FSExists != nil {
			fsExists = opts.FSExists
		}
		sessionDir = opts.SessionDir
	}

	// HW_OVERLAY_VAAPI_ENABLED defaults to true. The mode it gates uses
	// stock ffmpeg's `subtitles=` filter (file-based, libass-rendered,
	// chained through `hwdownload`/`hwupload`) — the only sub-burn path
	// that actually works on stock ffmpeg. The hybrid-inlineass mode it
	// falls back to when this is off relies on Plex's private `inlineass`
	// filter and produces ffmpeg "Filter not found" errors at runtime.
	// Set HW_OVERLAY_VAAPI_ENABLED=false only to deliberately fall back
	// to bail (same effect — playback fails — but with a clearer log).
	overlayEnabled := !envBool("HW_OVERLAY_VAAPI_DISABLED")
	renderDevice := envOr("HW_RENDER_DEVICE", "/dev/dri/renderD128")
	vaapiDriver := envOr("HW_VAAPI_DRIVER", "iHD")
	// Image-resident defaults: Ubuntu's intel-media-va-driver-non-free
	// installs iHD_drv_video.so under /usr/lib/x86_64-linux-gnu/dri and
	// libva auto-discovers it. HW_LIBVA_DRIVERS_PATH only needs to be
	// non-empty when overriding (e.g. talking to a Plex-bundled cache).
	libvaDriversPath := os.Getenv("HW_LIBVA_DRIVERS_PATH")

	changes := []string{}
	// On bail we don't run the full rewriter, but Plex-private flags
	// MUST still come off — stock ffmpeg exits status 8 on the first
	// unrecognised one ("Unrecognized option 'loglevel_plex'"). PMS
	// emits these on EVERY ffmpeg spawn, including audio-only Detection
	// jobs (intro / credits / voice-activity ML pre-pass) that the
	// rewriter bails on with skip:no-decoder. Without this scrub
	// every such session 8'd out, blocking PMS marker generation.
	scrubPlexFlagsOnBail := func(args []string) ([]string, []string) {
		var bailChanges []string
		for _, flag := range []string{"-loglevel_plex", "-progressurl"} {
			for {
				i := indexOfArg(args, flag, 0)
				if i < 0 || i+1 >= len(args) {
					break
				}
				args = removeArgs(args, i, 2)
				bailChanges = append(bailChanges, "drop:"+flag+"(bail)")
			}
		}
		// -xioerror is a Plex-private boolean flag (no value).
		for {
			i := indexOfArg(args, "-xioerror", 0)
			if i < 0 {
				break
			}
			args = removeArgs(args, i, 1)
			bailChanges = append(bailChanges, "drop:-xioerror(bail)")
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

	// 1.5. Pre-emptive sidecar drop (SW-decode-only path). PMS staged
	// a temp-0.srt as a second `-i` for sub-burn sessions. We need to
	// drop it BEFORE phase 2 (-init_hw_device inject) because phase 2
	// injects after the FIRST -i, which lands in input-1's option
	// block — `dropSidecarInput` would then eat the just-injected
	// `-init_hw_device` (between the two -i flags), leaving ffmpeg
	// without a hwdevice ("No VA display found for device vaapi",
	// "No device available for decoder: device type vaapi needed for
	// codec av1"). Live repro 2026-05-09 sessions 7347, 7359, 7418,
	// 7448, 7455, 7475: SW HDR + sub-burn The Accountant on Plex
	// Android, all hit the bug.
	//
	// HW-decode mode: PMS already provides a fully-shaped argv with
	// -init_hw_device set; we don't inject anything in phase 2 (we
	// just patch the device path). Sidecar drop in HW mode happens
	// later inside the isHWDecode branch.
	var earlySubSrc *subtitleSource
	if !isHWDecode {
		var probe func(string, string) string
		if opts != nil && opts.ProbeSubtitleCodec != nil {
			probe = opts.ProbeSubtitleCodec
		}
		earlySubSrc = detectSubtitleSource(args, sessionDir, probe)
		if earlySubSrc != nil && earlySubSrc.SecondInputArgIdx > 0 {
			if newArgs, dropped := dropSidecarInput(args, earlySubSrc.SecondInputArgIdx); dropped {
				args = newArgs
				if removed := stripNullSubOutput(&args); removed {
					_ = removed // logged later via change tags
				}
			}
		}
	}

	// 2. -init_hw_device patch or inject (now safe — second -i and
	// its option block already gone for SW sub-burn sessions).
	initIdx := indexOfArg(args, "-init_hw_device", 0)
	if initIdx >= 0 {
		if !reInitHW.MatchString(args[initIdx+1]) {
			return bail("init_hw_device-pattern:" + args[initIdx+1])
		}
		args[initIdx+1] = "vaapi=vaapi:" + renderDevice + ",driver=" + vaapiDriver
		if indexOfArg(args, "-filter_hw_device", 0) < 0 {
			args = spliceArgs(args, initIdx+2, "-filter_hw_device", "vaapi")
			changes = append(changes, "inject:filter_hw_device")
		}
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
	var subExtract *SubtitleExtract
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
		// bitmap-sidecar) and the filter rewrite below picks the matching
		// stock-ffmpeg chain. Any embedded text extraction needed is
		// signalled to the agent via RewriteResult.SubtitleExtract.
		//
		// Reuse earlySubSrc when phase 1.5 already detected + acted on a
		// text-sidecar drop — the second -i is gone now, so re-detecting
		// here would return nil for the sidecar case and the filter
		// rewrite would fail to find a sidecar path. For bitmap subs and
		// embedded subs (no second -i to drop), earlySubSrc is nil and
		// we still need to detect.
		if earlySubSrc != nil {
			subSrc = earlySubSrc
		} else {
			var probe func(string, string) string
			if opts != nil && opts.ProbeSubtitleCodec != nil {
				probe = opts.ProbeSubtitleCodec
			}
			subSrc = detectSubtitleSource(args, sessionDir, probe)
		}
		if subSrc != nil {
			subExtract = subSrc.Extract
			switch subSrc.Kind {
			case "text":
				if subSrc.Extract != nil {
					changes = append(changes, "subtitle:embedded-extract:"+subSrc.StreamSpec)
				} else {
					changes = append(changes, "subtitle:sidecar-staged")
				}
			case "bitmap":
				label := "subtitle:bitmap:" + subSrc.StreamSpec
				if subSrc.Codec != "" {
					label += "(" + subSrc.Codec + ")"
				}
				changes = append(changes, label)
			}
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

		seekShift := 0.0
		if isHLS {
			seekShift = captureSeekSeconds(args)
		}
		rewritten = rewriteVideoFilter(args[vfIdx], mediaPath, subSrc, fsExists, overlayEnabled, sourceIsHDR, seekShift)
		if rewritten != nil && seekShift > 0 && subSrc != nil && subSrc.Kind == "text" {
			changes = append(changes, fmt.Sprintf("subtitle:pts-shift=%.3fs", seekShift))
		}
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

		// Sidecar drop already happened in phase 1.5 (early, before
		// init_hw_device injection). Just emit the change-tag here so
		// the rewriter changelog still reflects what we did. The
		// sub-source detection ran twice; reconcile via earlySubSrc.
		if earlySubSrc != nil && earlySubSrc.SecondInputArgIdx > 0 {
			changes = append(changes, "drop:-i(sidecar-input)")
			// Did stripNullSubOutput actually remove anything? It runs
			// in phase 1.5; we re-search to confirm before tagging.
			// (We can also infer: if there was a `-map <X> -f null
			// -codec ass <name>` block in the original args, it was
			// removed — but to keep the tag honest, we just check.)
			hasNullF := false
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-f" && args[i+1] == "null" {
					hasNullF = true
					break
				}
			}
			if !hasNullF {
				changes = append(changes, "drop:null-sub-output")
			}
		}

		// Both subtitle-rewrite modes (overlay-vaapi sidecar and the older
		// hybrid-inlineass that uses subtitles= on a hwdownload→hwupload
		// roundtrip) need the Plex-private `-map_inlineass <stream>` argv
		// flag stripped, otherwise stock ffmpeg fails with "Unrecognized
		// option 'map_inlineass'" before the filter graph even runs. The
		// strip used to be gated on overlay-vaapi only, which broke LG
		// WebOS sub-burn the moment the rewriter picked hybrid-inlineass
		// (e.g. when a sidecar SRT is found but VAAPI overlay isn't safe
		// for the source — the fallback path).
		if strings.HasPrefix(rewritten.Mode, "overlay-vaapi") || rewritten.Mode == "hybrid-inlineass" {
			if miaIdx := indexOfArg(args, "-map_inlineass", 0); miaIdx >= 0 {
				args = removeArgs(args, miaIdx, 2)
				changes = append(changes, "drop:-map_inlineass")
			}
			// Fontconfig is opt-in; the worker image ships a system-wide
			// fontconfig (fc-cache built at image-build time) that libass
			// finds without any env nudging.
			if v := os.Getenv("HW_FONTCONFIG_FILE"); v != "" {
				env["FONTCONFIG_FILE"] = v
			}
			if v := os.Getenv("HW_FONTCONFIG_PATH"); v != "" {
				env["FONTCONFIG_PATH"] = v
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

		// HW-decode HDR→SDR tonemap injection. PMS's HW-passthrough
		// filter chain for SDR-encode targets (h264_vaapi, or
		// hevc_vaapi default Main profile) drops 10-bit p010 to 8-bit
		// nv12 via naive `format=nv12` on scale_vaapi — no transfer-
		// function curve, PQ values shoved into BT.709 8-bit range
		// produce washed/clipped colors. Plex's bundled transcoder
		// papered over this with its own opencl/cuda/sw tonemap; we
		// have to spell it out for stock ffmpeg.
		//
		// Live observation 2026-05-10 PM: streaming Big Hero 6 4K HDR
		// with videoCodec=h264 → PMS chain
		//   [0:0]hwupload[0];[0]scale_vaapi=...:format=nv12[1];[1]hwupload[2]
		// ran clean (no error) but output had crushed highlights.
		//
		// Detect via reHWPassthroughSDRChain (matches PMS's exact
		// 3-step shape). Gate on sourceIsHDR so we don't double-tonemap
		// SDR sources. Only fires for the SDR-target shape — if PMS
		// asked for `format=p010` the regex doesn't match and HDR is
		// preserved as-is.
		if opts != nil && opts.ProbeVideoColor != nil && mediaPath != "" {
			if transfer, _, _ := opts.ProbeVideoColor(mediaPath); isHDRTransfer(transfer) {
				sourceIsHDR = true
				for i := 0; i < len(args); i++ {
					if args[i] == "-filter_complex" && i+1 < len(args) {
						if newFilter, ok := injectHWPassthroughTonemap(args[i+1]); ok {
							args[i+1] = newFilter
							changes = append(changes, "video:hdr-source("+strings.ToLower(transfer)+")")
							changes = append(changes, "filter:hw-passthrough-tonemap-injected")
							break
						}
					}
				}
			}
		}

		// Sub burn-in: PMS sends `-map_inlineass` even in HW-decode
		// mode, with a filter graph that runs Plex's private
		// `inlineass` filter on the CPU side of an
		// hwdownload/hwupload sandwich. Stock ffmpeg has no
		// inlineass; we swap it for `subtitles=filename=<staged
		// SRT>:fontsdir=…` (libass) keeping the rest of the chain
		// (and labels [0]–[4]) intact, so PMS's `-map [4]` still
		// resolves and HDR p010 is preserved end-to-end.
		var probe func(string, string) string
		if opts != nil && opts.ProbeSubtitleCodec != nil {
			probe = opts.ProbeSubtitleCodec
		}
		subSrc = detectSubtitleSource(args, sessionDir, probe)
		if subSrc != nil {
			switch subSrc.Kind {
			case "text":
				subExtract = subSrc.Extract
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
				if m == nil {
					return bail("hw-decode-sub:filter-pattern:" + args[vfIdx])
				}
				w, h := m[1], m[2]
				sidecarPath := subSrc.FilePath
				if sidecarPath == "" {
					return bail("hw-decode-sub:no-sidecar")
				}
				subPath := escapeFilterPath(sidecarPath)
				fontsDir := envOr("HW_FONTS_DIR", "/usr/share/fonts/truetype/dejavu")
				// Force nv12 across the libass step. PMS's chain keeps
				// p010 end-to-end (Plex's bundled inlineass reads 10-bit
				// fine), but stock `subtitles=` on p010 has caused the
				// encoder pipeline to stall after the first seek-time
				// fontselect with no further output for 30+s on Plex
				// Android force-burn sessions (live repro 2026-05-08:
				// Plex Android, force-burn subs, seek to 1750s, encoder
				// never opens). Initial play (no -ss) doesn't reproduce
				// because the first 5s of typical content has no
				// subtitle rendering. nv12 throughout the libass step
				// matches the SW path's HDR handling and sidesteps the
				// p010+libass interaction. We lose HDR pass-through on
				// sub-burn sessions specifically; sub burn always
				// composites SDR libass glyphs anyway, so HDR survival
				// past the burn is best-effort. tonemap_vaapi handles
				// HDR-source conversion before the libass step.
				if opts != nil && opts.ProbeVideoColor != nil && mediaPath != "" {
					if transfer, _, _ := opts.ProbeVideoColor(mediaPath); isHDRTransfer(transfer) {
						sourceIsHDR = true
						changes = append(changes, "video:hdr-source("+strings.ToLower(transfer)+")")
					}
				}
				scaleStep := fmt.Sprintf("scale_vaapi=w=%s:h=%s:format=nv12", w, h)
				if sourceIsHDR {
					scaleStep = fmt.Sprintf(
						"scale_vaapi=w=%s:h=%s:format=p010,tonemap_vaapi=transfer=bt709:format=nv12",
						w, h)
				}
				preShift, postShift := "", ""
				if isHLS {
					if so := captureSeekSeconds(args); so > 0 {
						preShift, postShift = subtitlesSeekShift(so)
						changes = append(changes, fmt.Sprintf("subtitle:pts-shift=%.3fs", so))
					}
				}
				args[vfIdx] = fmt.Sprintf(
					"[0:0]hwupload[0];"+
						"[0]%s[1];"+
						"[1]hwdownload,format=nv12%s[2];"+
						"[2]subtitles=filename='%s':fontsdir=%s%s[3];"+
						"[3]%shwupload[4]",
					scaleStep, preShift, subPath, fontsDir, subtitlesForceStyle(),
					postShift,
				)
				changes = append(changes, "hw-decode:filter:inlineass->subtitles")
				changes = append(changes, "subtitle:sidecar-staged")
				changes = append(changes, "sidecar:"+sidecarPath)

				// Drop the sidecar `-i` AND its per-input options. PMS
				// in seek+sub-burn mode puts `-ss <T>` here so ffmpeg
				// seeks the SRT to match the source. dropSidecarInput
				// removes the whole input-1 option block (between the
				// two -i flags) so nothing dangles as output seek.
				if newArgs, dropped := dropSidecarInput(args, subSrc.SecondInputArgIdx); dropped {
					args = newArgs
					changes = append(changes, "drop:-i(sidecar-input)")
				}
				// Strip Plex-only -map_inlineass.
				if miaIdx := indexOfArg(args, "-map_inlineass", 0); miaIdx >= 0 {
					args = removeArgs(args, miaIdx, 2)
					changes = append(changes, "drop:-map_inlineass")
				}
				// Strip Plex's null-sub output (-map <sub-ref> -f null
				// -codec ass <name>) — references the now-dropped
				// sidecar input.
				if removed := stripNullSubOutput(&args); removed {
					changes = append(changes, "drop:null-sub-output")
				}
				// Indices shifted by the splices above; re-locate
				// encCodecIdx for the phases below.
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
		// HDR + sub-burn boost. Default 0: Plex's bundled transcoder on
		// the same hardware (Arc A310) ships qp=15 HEVC for HDR + sub
		// burn-in and gets ~7 Mbps with no buffering issues — the encoder
		// keeps up at near-realtime. Earlier scaleplex bench at 1.3-2×
		// suggested a steady-state shortfall and we added a +6 boost
		// (qp=28); subsequent diff vs Plex showed the boost was over-
		// correcting (Plex achieves the same throughput without it).
		// The buffer events were likely client-side, not encoder-bound.
		// Set HW_QP_HDR_SUB_BOOST=N if HDR-sub bandwidth becomes a real
		// bottleneck (e.g., low-bandwidth WAN client).
		hdrSubBoost := 0
		if v := os.Getenv("HW_QP_HDR_SUB_BOOST"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				hdrSubBoost = n
			}
		}
		appliedBoost := 0
		if sourceIsHDR && rewritten != nil && strings.HasPrefix(rewritten.Mode, "overlay-vaapi") {
			appliedBoost = hdrSubBoost
		}
		if crf, err := strconv.Atoi(args[crfIdx+1]); err == nil {
			qp := crf + offset + appliedBoost
			if qp < 0 {
				qp = 0
			}
			if qp > 51 {
				qp = 51
			}
			args[crfIdx+1] = strconv.Itoa(qp)
			label := fmt.Sprintf("crf%d->qp%d(off=%d", crf, qp, offset)
			if appliedBoost > 0 {
				label += fmt.Sprintf("+hdrsub%d", appliedBoost)
			}
			label += ")"
			changes = append(changes, label)
		} else {
			changes = append(changes, "crf->qp")
		}
	}

	// 7. Translate -preset:0 <x264-name> → -compression_level:v <N>
	// (Plex emits x264 preset names; iHD VAAPI uses a 1-7 TargetUsage
	// scale where 7 = fastest, 1 = highest quality.)
	if i := indexOfArg(args, "-preset:0", encCodecIdx+1); i >= 0 && i+1 < len(args) {
		preset := args[i+1]
		cl := mapX264PresetToVAAPI(preset)
		args = removeArgs(args, i, 2)
		// Inject right after the encoder so the encoder context picks it up.
		args = spliceArgs(args, encCodecIdx+2, "-compression_level:v", cl)
		changes = append(changes, "preset:"+preset+"->compression_level:"+cl)
	} else {
		// No preset emitted (e.g. an x265 path with -x265-params instead);
		// default to fastest. Worker GPU wants throughput, not max quality.
		args = spliceArgs(args, encCodecIdx+2, "-compression_level:v", "7")
		changes = append(changes, "inject:compression_level=7")
	}

	// Drop the remaining SW-encoder-specific flags.
	for _, flag := range []string{"-x264opts:0", "-x265-params:0"} {
		if i := indexOfArg(args, flag, encCodecIdx+1); i >= 0 {
			args = removeArgs(args, i, 2)
			changes = append(changes, "drop:"+flag)
		}
	}

	// 8. -sei:0 -a53_cc before -force_key_frames:0
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

	// Drop Plex-Transcoder-only flags that stock ffmpeg rejects with
	// "Unrecognized option". Each is two-token (`flag value`).
	//   -loglevel_plex <level>   — custom Plex log verbosity, no analog
	//   -delete_removed <bool>   — Plex DASH muxer extension. Plex passes
	//                              `false` to mean "never delete old
	//                              segments". Stock dashenc has no
	//                              equivalent flag; chunk preservation is
	//                              instead controlled by extra_window_size
	//                              (default 5 — segments are unlinked from
	//                              disk after they fall extra_window_size
	//                              past the manifest window). PMS's
	//                              universal handler serves chunks by
	//                              number from disk and 404s the client if
	//                              early chunks were already deleted, so
	//                              we strip Plex's flag and inject a huge
	//                              extra_window_size below to keep
	//                              everything around.
	for _, flag := range []string{"-loglevel_plex"} {
		if i := indexOfArg(args, flag, 0); i >= 0 {
			args = removeArgs(args, i, 2)
			changes = append(changes, "drop:"+flag)
		}
	}

	// Plex-private segment-muxer / fork-only flags. Strip globally —
	// they appear on HLS argv (the primary output) AND on the embedded
	// subtitle pipeline that Plex appends as a second `-f segment` on
	// DASH transcodes when the source has embedded ASS subs. Stock
	// ffmpeg's segment muxer rejects each with "Unrecognized option
	// '<flag>'. Error splitting the argument list" — observed:
	//   - 2026-05-06 LG WebOS HLS Balls Up: -segment_list_unfinished
	//   - 2026-05-06 Chrome DASH Superman: -strict_ts:0,
	//     -segment_list_separate_stream_times (on subtitle output)
	for _, flag := range []string{"-segment_list_separate_stream_times", "-segment_list_unfinished"} {
		for {
			i := indexOfArg(args, flag, 0)
			if i < 0 || i+1 >= len(args) {
				break
			}
			args = removeArgs(args, i, 2)
			changes = append(changes, "drop:"+flag)
		}
	}
	// `-strict_ts*` (any stream specifier suffix) — same family.
	for {
		i := -1
		for j := 0; j < len(args); j++ {
			if strings.HasPrefix(args[j], "-strict_ts") {
				i = j
				break
			}
		}
		if i < 0 {
			break
		}
		args = removeArgs(args, i, 2)
		changes = append(changes, "drop:-strict_ts")
	}

	// `-skip_to_segment N` — Plex DASH muxer extension that starts the
	// dash muxer's segment_index at N. scaleplex-ffmpeg7 backports this
	// natively (libavformat/dashenc.c patch 0095) so the flag flows
	// straight through and ffmpeg emits chunk-stream0-NNNNN.m4s with
	// N matching PMS's URL expectation. Capture the value only so
	// segwatch / checkpoint can record it; do NOT strip the flag.
	skipToSegment := 0
	if i := indexOfArg(args, "-skip_to_segment", 0); i >= 0 && i+1 < len(args) {
		if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
			skipToSegment = n
			changes = append(changes, "skip_to_segment:passthrough="+args[i+1])
		}
	}

	// `-delete_removed false` (Plex DASH extension, also backported)
	// keeps chunks on disk past the sliding manifest window — PMS
	// serves rewind / early-fetch via direct file read. No more need
	// for our previous `-extra_window_size 999999` injection hack.

	// Force CMAF-style segments (moof+mdat only, no per-segment moov).
	//
	// Stock dashenc by default emits self-contained mp4 segments — each
	// chunk-stream<N>-NNNNN.m4s starts with `ftyp+moov` followed by the
	// fragment. MSE source buffers reject the duplicate moov: the first
	// chunk reinitialises the buffer, the second is treated as a new
	// init segment too, and the player oscillates between "playing" and
	// "buffering" without ever advancing past the first ~1s of content
	// (observed live in the seek test, sha-019a335).
	//
	// Plex's ffmpeg fork omits moov from segments; on stock ffmpeg we
	// pass `-format_options` to the inner mp4 muxer to override its
	// movflags so segments contain only moof+mdat.
	//
	// Notable: NO `+frag_keyframe`. With h264_vaapi's default GOP shorter
	// than our 3s segment, +frag_keyframe forces a new moof at every
	// keyframe — chunks ended up with two `moof+mdat` pairs, which Plex
	// Web's MSE could buffer but couldn't seek into cleanly. PT.real
	// emits one fragment per segment; we match that by letting dashenc
	// drive fragmentation (one moof per segment file).
	if !isHLS && indexOfArg(args, "-format_options", 0) < 0 {
		for k := 0; k+1 < len(args); k++ {
			if args[k] == "-f" && args[k+1] == "dash" {
				args = spliceArgs(args, k, "-format_options", "movflags=+empty_moov+default_base_moof+separate_moof+cmaf")
				changes = append(changes, "inject:-format_options=movflags+cmaf-strict")
				break
			}
		}
	}

	// HLS: Plex's `-f ssegment` is its custom stream-segmenter muxer.
	// Stock ffmpeg's `-f segment` covers most of what's needed; translate.
	//
	// Plex argv pattern (captured from PT.real recon, 2026-05-06):
	//   -segment_format matroska     → -segment_format mpegts (matroska
	//                                    inside .ts is Plex's quirk; stock
	//                                    `segment` muxer with mpegts is
	//                                    standards-compliant HLS)
	//   -f ssegment                  → -f segment
	//   -individual_header_trailer 0 → DROP (Plex-only)
	//   -segment_header_filename hdr → DROP (Plex-only; mpegts segments
	//                                    are self-contained)
	//   -segment_list_separate_stream_times 1 → DROP (Plex-only)
	//   -segment_list_unfinished 1   → DROP (Plex-only)
	//   -segment_format_options ...  → DROP (Plex-only inner-format opts)
	//   -segment_time, -segment_start_number, -segment_time_delta,
	//   -segment_list <url>, -segment_list_type csv, -segment_list_size,
	//   "media-%05d.ts" output       → KEEP (stock supports all)
	//
	// Stock segment muxer with -segment_list <http_url> POSTs the listfile
	// to that URL natively (CSV with -segment_list_type csv). PMS reads
	// the CSV and synthesises the m3u8 it serves to clients.
	if isHLS {
		// `-f ssegment` is Plex's name for the stream-segmenter muxer;
		// stock ffmpeg has `-f segment` which is API-compatible for the
		// options Plex actually uses. Verified against `ffmpeg -h
		// muxer=segment` (jellyfin-ffmpeg7): -segment_format,
		// -segment_header_filename, -individual_header_trailer,
		// -segment_format_options, -segment_time, -segment_list,
		// -segment_list_type are all native. Plex's argv keeps using
		// matroska-in-.ts for HLS to multi-channel-audio clients (Plex
		// signals this in the public manifest as container=mkv); the
		// client then fetches /base/header to grab the matroska codec
		// init and treats each .ts as a continuation. Stripping
		// -segment_header_filename caused 4K-HDR + 5.1-audio playback
		// on Plex Android to 404 on /base/header for ~120s before the
		// app gave up.
		if i := indexOfArg(args, "-f", 0); i >= 0 && i+1 < len(args) && args[i+1] == "ssegment" {
			args[i+1] = "segment"
			changes = append(changes, "hls:f=ssegment->segment")
		}
		// (-segment_list_separate_stream_times / -segment_list_unfinished
		// are stripped globally above for both HLS and the DASH+subs
		// embedded-subtitle output. Don't duplicate the strip here.)

		// Strip `-copyts` for **mpegts-based** segmented output (HLS to
		// mobile). With `-ss <off>` + `-copyts` and mpegts segments, stock
		// ssegment fork special-cases it but stock segment muxer with
		// mpegts emits one giant first segment containing the entire
		// remaining runtime (observed: media-00173.ts grew to 222 MB /
		// 23 min on Balls Up). Drop and splits resume at every keyframe.
		//
		// For **matroska-based** segmented output (Plex Windows desktop),
		// KEEP `-copyts`. FFmpeg 7.1.3 has the upstream
		// `reference_stream_first_pts` offset in libavformat/segment.c
		// (lines 904-914) so the split logic accounts for non-zero start
		// PTS. With -copyts kept, the matroska Cluster.Timecode is
		// written directly as absolute (= -ss + local) — no need for our
		// relay-side post-patching. mpv reads correct absolute timeline
		// from byte 0, the audio-track-swap re-spawn passes the right
		// offset back. Verified empirically 2026-05-11 on jellyfin-ffmpeg
		// 7.1.3 with Burn Notice -ss 100: 20 chunks produced in 20s,
		// chunk-0 PTS=100.017, chunk-5 PTS=205.038.
		// Strip `-copyts` for all segmented output. Stock ffmpeg's
		// segment muxer with `-copyts -start_at_zero -ss <off>` is
		// inconsistent across `-ss` values on jellyfin-ffmpeg 7.1.3:
		// for small `-ss` (initial play -ss 4) it splits at keyframes,
		// but for large `-ss` (seek -ss 4801) the encoder processes
		// frames (frame= advancing) yet zero chunks land on disk —
		// verified 2026-05-11 with Big Hero 6 hevc_vaapi. Likely
		// interaction between -start_at_zero's PTS rebase and VAAPI
		// encoder GOP state post deep-seek. Stripping -copyts rebases
		// packet PTS to 0; segment muxer splits reliably, and the
		// relay's Cluster.Timecode patcher restores the absolute
		// timeline for mpv.
		if i := indexOfArg(args, "-copyts", 0); i >= 0 {
			args = removeArgs(args, i, 1)
			changes = append(changes, "hls:drop:-copyts")
		}

		// Rewrite `-segment_format_options live=1` → `live=0` (Plex Windows
		// segmented-matroska shape). Plex's matroska muxer fork patches
		// matroskaenc.c at line 2561 to ALWAYS write the Matroska Duration
		// element from `-metadata duration=` even in live mode (`!is_live
		// || 1`). Stock matroska honours `is_live`: with live=1 the Duration
		// element is skipped entirely, so the segment_header file lands on
		// disk without Duration. The Plex Windows client reads the
		// concatenated header+chunks via `/transcode/universal/start`, sees
		// no Duration in the matroska header, and falls back to inferring
		// duration from received bytes — slider shows 5 min, growing as
		// transcode produces chunks (verified live 2026-05-11 Big Hero 6
		// 8 Mbps 1080p, user couldn't seek past "current produced" mark).
		//
		// Setting live=0 makes stock write Duration from -metadata into
		// the header file. Side effects (SeekHead writing + end-of-stream
		// seek-back to update Duration) are no-ops here: segment muxer
		// captures only header bytes up to first Cluster, so later
		// seek-backs land in chunk files and don't touch the already-
		// written header. Empirically validated 2026-05-11 with testsrc2:
		// live=0 header has 0x4489 Duration = 6112352ms; live=1 doesn't.
		for i := 0; i+1 < len(args); i++ {
			if args[i] != "-segment_format_options" {
				continue
			}
			if args[i+1] == "live=1" {
				// live=0: stock writes matroska Duration from -metadata
				//   into the segment_header file (Plex Windows client uses
				//   it to fill the seekbar with full source duration).
				// cluster_time_limit=1000 + cluster_size_limit=32768:
				//   force per-frame clusters. With live=0, stock's defaults
				//   are 5s/5MB → one cluster per ~5s chunk. Plex Windows
				//   tracks playback position (and the offset it sends to
				//   PMS on audio-track-switch / quality-change / re-spawn)
				//   from Cluster.Timecode. Per-frame clusters match what
				//   Plex-native matroskaenc.c produces under live=1
				//   defaults; verified 2026-05-11 against production PMS
				//   chunk 5 — 25 clusters per 1s chunk at 8 Mbps. Without
				//   per-frame clusters the client falls back to elapsed-
				//   since-stream-start, so audio-swap re-spawns the
				//   transcode at offset=elapsed → playback restarts from
				//   the front.
				args[i+1] = "live=0:cluster_time_limit=1000:cluster_size_limit=32768"
				changes = append(changes, "hls:segment_format_options:live=1->live=0+per-frame-clusters")
			}
			break
		}

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
		if i := indexOfArg(args, "-segment_list", 0); i >= 0 && i+1 < len(args) {
			base := envOr("SCALEPLEX_PMS_BASE_URL", "")
			if envBase, ok := inputEnv["SCALEPLEX_PMS_BASE_URL"]; ok && envBase != "" {
				base = envBase
			}
			origURL := args[i+1]
			if base != "" && strings.HasPrefix(origURL, "http://127.0.0.1:32400") {
				rewritten := strings.Replace(origURL, "http://127.0.0.1:32400", base, 1)
				appendQuery := func(kv string) {
					if strings.Contains(rewritten, "?") {
						rewritten += "&" + kv
					} else {
						rewritten += "?" + kv
					}
				}
				if tok, ok := inputEnv["X_PLEX_TOKEN"]; ok && tok != "" {
					appendQuery("X-Plex-Token=" + tok)
				}
				if segTime != "" {
					appendQuery("scaleplex_seg_time=" + segTime)
				}
				// Pass the session's -ss as scaleplex_mkv_offset_ms so the
				// relay can patch each chunk's Cluster.Timecode in-line
				// with the CSV forward to PMS. Required for matroska-
				// segment output (Plex Windows): -copyts is stripped
				// above so chunks have local 0-based PTS; the relay
				// shifts Cluster.Timecode to absolute before PMS reads.
				if ssIdx := indexOfArg(args, "-ss", 0); ssIdx >= 0 && ssIdx+1 < len(args) {
					if v, err := strconv.ParseFloat(args[ssIdx+1], 64); err == nil && v > 0 {
						appendQuery(fmt.Sprintf("scaleplex_mkv_offset_ms=%d", uint64(v*1000)))
					}
				}
				args[i+1] = rewritten
				changes = append(changes, "hls:segment_list:rewrite-to-relay")
			}
		}
	}

	// Capture `-ss <off>` on seek sessions for the renumber watcher's
	// tfdt patch. Stock dashenc writes tfdt=0 in seek-session chunks
	// regardless of argv (verified: -ss + -copyts + drop -start_at_zero
	// + +cmaf movflag still produces tfdt=0). Plex Web's MSE places
	// such chunks at timeline 0..seg_dur — player's currentTime sits at
	// <off> with no buffered data → BUFFERING_HAVE_NOTHING forever
	// (confirmed via local MSE harness; PT.real seek chunks have
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

	// Seek + force_key_frames expr fix — DASH only.
	//
	// PMS sends `-force_key_frames:0 "expr:gte(t,n_forced*8)"`. The
	// rewrite needed depends on whether `-copyts` is in play:
	//
	//   DASH (we keep `-copyts`): encoder `t` runs in input time, so
	//   it starts at the seek offset (e.g. 2344s). Plex's expr is true
	//   for every frame, ffmpeg fires ~294 forced keyframes back-to-
	//   back, breaks the segment muxer (first segment swallows tens
	//   of minutes). Rewrite to `gte(t-<offset>, n*8)` so keyframes
	//   land at output times 0, 8, 16, ...
	//
	//   HLS (we strip `-copyts` earlier in this function): encoder
	//   `t` already runs in output time, starts at 0. Plex's expr
	//   `gte(t, n*8)` is correct as-is — keyframes fire at t=0, 8,
	//   16. If we apply the (t - seek_offset) rewrite here the expr
	//   never fires (always `false` for output-time t < seek_offset),
	//   the muxer waits forever for a keyframe to close the first
	//   segment, no .ts files are produced, the player times out
	//   with "Connection error" (observed live 2026-05-08 Plex
	//   Android, 2 min wall clock, zero segments, force burn-in +
	//   seek to 3345s).
	if seekOffsetSeconds > 0 && !isHLS {
		for i := 0; i+1 < len(args); i++ {
			if !strings.HasPrefix(args[i], "-force_key_frames") {
				continue
			}
			orig := args[i+1]
			if !strings.HasPrefix(orig, "expr:") {
				continue
			}
			inner := strings.TrimPrefix(orig, "expr:")
			if !strings.Contains(inner, "(t,") && !strings.Contains(inner, "(t ,") {
				continue
			}
			rewritten := strings.Replace(inner, "(t,", fmt.Sprintf("(t-%.3f,", seekOffsetSeconds), 1)
			rewritten = strings.Replace(rewritten, "(t ,", fmt.Sprintf("(t-%.3f ,", seekOffsetSeconds), 1)
			args[i+1] = "expr:" + rewritten
			changes = append(changes, "force_key_frames:offset-by-seek")
		}
	}

	// `-manifest_name <url>` — Plex's ffmpeg fork POSTs the manifest body
	// to this URL whenever the .mpd is regenerated; PMS gates `/header`
	// on the first such POST. Stock ffmpeg's dashenc treats manifest_name
	// as a filename, not a URL, so we strip it from the argv (otherwise
	// it would be written verbatim into a local file) and surface the
	// rewritten URL on RewriteResult so the agent can POST the manifest
	// itself. See manifest_publish.go.
	var manifestURL string
	var manifestChanges []string
	args, manifestURL, manifestChanges = captureManifestName(args, inputEnv)
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
	// timeline (matching what PT.real produces).

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
		ManifestURL:       manifestURL,
		SkipToSegment:     skipToSegment,
		SeekOffsetSeconds: seekOffsetSeconds,
		SubtitleExtract:   subExtract,
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
