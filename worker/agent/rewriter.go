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
}

// RewriteOpts is for testability; production callers pass nil.
type RewriteOpts struct {
	FSExists func(string) bool
	// SessionDir — Plex's per-session transcode dir (the agent's
	// req.Cwd). Used as the staging path for embedded-subtitle
	// extraction. When empty, falls back to /tmp/scaleplex.
	SessionDir string
}

var decoderMap = map[string]string{
	"libdav1d": "av1",
	"libhevc":  "hevc",
	"libx264":  "h264",
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

func encoderMap(preferHEVC bool) map[string]string {
	if preferHEVC {
		return map[string]string{"libx264": "hevc_vaapi", "libx265": "hevc_vaapi"}
	}
	return map[string]string{"libx264": "h264_vaapi", "libx265": "hevc_vaapi"}
}

var (
	reFilterAss = regexp.MustCompile(
		`^\[0:0\]scale=w=(\d+):h=(\d+)(?::force_divisible_by=\d+)?\[0\];` +
			`\[0\]format=pix_fmts=[^\[]*nv12\[1\];` +
			`\[1\]inlineass=([^\[]*)\[2\]$`)
	reFilterPlain = regexp.MustCompile(
		`^\[0:0\]scale=w=(\d+):h=(\d+)(?::force_divisible_by=\d+)?\[0\];` +
			`\[0\]format=pix_fmts=[^\[]*nv12\[1\]$`)
	// HDR→SDR PMS pattern: scale → zscale(linear) → format(gbrpf32le) →
	// zscale(primaries=bt709) → tonemap → zscale(bt709) → format(nv12).
	// Capture leading w/h and the final output label number; the middle is
	// flexible because Plex tweaks the chain across versions.
	reFilterHDR = regexp.MustCompile(
		`^\[0:0\]scale=w=(\d+):h=(\d+)(?::force_divisible_by=\d+)?\[\d+\];` +
			`.*zscale.*tonemap.*format=pix_fmts=[^\[]*nv12\[(\d+)\]$`)
	reLanguage = regexp.MustCompile(`(?:^|:)language=([a-zA-Z]{2,3})`)
	reInitHW   = regexp.MustCompile(`^vaapi=vaapi:?$`)
)

func envBool(k string) bool { return os.Getenv(k) == "true" }

func envOr(k, dflt string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return dflt
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

// detectSubtitleSource inspects argv for `-map_inlineass <spec>` and
// returns the filesystem path to use with stock ffmpeg's `subtitles=`
// filter, plus an extraction request when needed.
//
// Plex emits two distinct shapes for sub burn-in:
//
//   1. Embedded subtitle in the source .mkv:
//        -i /media/.../source.mkv
//        -map_inlineass 0:3
//      Single `-i`, spec `0:N`. Stock `subtitles=` can't read by stream
//      index — we request an extraction. Agent runs a side ffmpeg
//      before the main spawn.
//
//   2. External sidecar (Plex pre-stages a copy in the session dir):
//        -i /media/.../source.mkv
//        -i /transcode/Transcode/Sessions/<sid>-<job>/temp-0.srt
//        -map_inlineass 1:s:0
//      Two `-i`s, spec `1:s:0`. The temp file is already on disk and
//      shared via NFS; we use it directly. The second `-i` is dropped
//      from the rewritten argv since we replace the filter graph.
//
// secondInputArgIdx is the offset of the second `-i` in `args` (so the
// caller can drop both `-i` and the path that follows). Returns -1 when
// only one `-i` is present.
func detectSubtitleSource(args []string, sessionDir string) (subPath string, extract *SubtitleExtract, secondInputArgIdx int) {
	miaIdx := indexOfArg(args, "-map_inlineass", 0)
	if miaIdx < 0 || miaIdx+1 >= len(args) {
		return "", nil, -1
	}
	streamSpec := args[miaIdx+1]

	var inputArgIdxs []int
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" {
			inputArgIdxs = append(inputArgIdxs, i)
		}
	}
	if len(inputArgIdxs) == 0 {
		return "", nil, -1
	}

	// Stream spec like "0:3", "1:s:0", "1:0". First numeric token
	// before the first colon is the input index.
	inputNum := 0
	if colon := strings.Index(streamSpec, ":"); colon > 0 {
		if n, err := strconv.Atoi(streamSpec[:colon]); err == nil {
			inputNum = n
		}
	}

	if inputNum == 0 {
		// Embedded path. Schedule extraction.
		if sessionDir == "" {
			sessionDir = "/tmp/scaleplex"
		}
		sourceFile := args[inputArgIdxs[0]+1]
		// Default to srt — covers SRT/SubRip/MOV_TEXT/TX3G inputs that
		// muxer can convert via -c:s srt. ASS sources should ideally
		// stay ASS to preserve styling; agent ffprobes the source
		// before extraction and overrides Format if needed (TODO).
		outputFile := filepath.Join(sessionDir, "scaleplex-extract.srt")
		return outputFile, &SubtitleExtract{
			SourceFile: sourceFile,
			StreamSpec: streamSpec,
			OutputFile: outputFile,
			Format:     "srt",
		}, -1
	}

	// Sidecar path. Use the file Plex already staged.
	if inputNum >= len(inputArgIdxs) {
		return "", nil, -1
	}
	return args[inputArgIdxs[inputNum]+1], nil, inputArgIdxs[inputNum]
}

type filterRewrite struct {
	Filter   string
	OldLabel string
	NewLabel string
	Mode     string
	Sidecar  string
}

// rewriteVideoFilter translates Plex's filter graph into a stock-ffmpeg
// equivalent. resolvedSubPath, when non-empty, is the path the burn-in
// chain should pass to `subtitles=filename=...` (resolved by the caller
// from -map_inlineass + -i argv shape; see detectSubtitleSource).
// Falls back to the legacy fs-probe (findSidecarSubtitle) when no
// PMS-staged path is present, mostly for robustness against PMS
// versions that change the argv shape.
func rewriteVideoFilter(filterStr, mediaPath, resolvedSubPath string, fsExists func(string) bool, overlayEnabled bool) *filterRewrite {
	if m := reFilterAss.FindStringSubmatch(filterStr); m != nil {
		w, h, assParams := m[1], m[2], m[3]
		var lang string
		if lm := reLanguage.FindStringSubmatch(assParams); lm != nil {
			lang = strings.ToLower(lm[1])
		}
		// Prefer the path PMS already gave us via -map_inlineass + -i
		// argv. Fall back to the fs-probe sidecar lookup if not
		// resolved (older PMS shapes; or unit tests that pass argvs
		// without -map_inlineass set up).
		sidecar := resolvedSubPath
		if sidecar == "" {
			sidecar = findSidecarSubtitle(mediaPath, lang, fsExists)
		}

		if sidecar != "" && overlayEnabled {
			// hwdownload + libass + hwupload on jellyfin-ffmpeg.
			// Bench 2026-05-05 (3× Arc A310): vs stock-ffmpeg same shape
			// gained +25-50% per stream, mostly from jellyfin's tonemapx
			// SIMD CPU tonemap and scale_vaapi=fast default.
			//
			// TODO(scaleplex): sub2video=1 + hwmap zero-copy chain to
			// skip the 12 MB nv12 main-stream hwdownload. Needs a
			// `[0:v]split=2[main_in][timing]; [timing]hwmap=mode=read...`
			// wiring; first attempt failed because subtitles=...:sub2video=1
			// requires an unlabeled video input pad for timing.
			subPath := escapeFilterPath(sidecar)
			fontsDir := envOr("HW_FONTS_DIR", "/usr/share/fonts/truetype/dejavu")
			return &filterRewrite{
				Filter: fmt.Sprintf(
					"[0:0]hwupload[10];"+
						"[10]scale_vaapi=w=%s:h=%s:format=nv12[11];"+
						"[11]hwdownload[12];"+
						"[12]format=pix_fmts=nv12[13];"+
						"[13]subtitles=filename='%s':fontsdir=%s[14];"+
						"[14]hwupload[15]",
					w, h, subPath, fontsDir),
				OldLabel: "[2]",
				NewLabel: "[15]",
				Mode:     "overlay-vaapi",
				Sidecar:  sidecar,
			}
		}

		// No sidecar found OR overlay disabled. The previous fallback
		// emitted an `inlineass=` filter (a Plex-private filter not in
		// stock ffmpeg) which silently produced broken filter graphs
		// that ffmpeg failed at runtime. Bail instead so the failure
		// is loud and the operator can either drop the right sidecar
		// next to the source or accept that embedded-only-subs sources
		// can't be burned in by stock ffmpeg without a pre-extract step
		// (TODO: extract embedded subs to a tmp file and feed
		// `subtitles=filename=...` against that — defer until needed).
		_, _, _ = w, h, assParams
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

func Rewrite(inputArgs []string, inputEnv map[string]string, opts *RewriteOpts) RewriteResult {
	fsExists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	sessionDir := ""
	if opts != nil {
		if opts.FSExists != nil {
			fsExists = opts.FSExists
		}
		sessionDir = opts.SessionDir
	}

	preferHEVC := envBool("HW_PREFER_HEVC")
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
	bail := func(reason string) RewriteResult {
		return RewriteResult{
			Args:    cloneArgs(inputArgs),
			Env:     cloneEnv(inputEnv),
			Applied: false,
			Changes: append(changes, "skip:"+reason),
		}
	}

	args := cloneArgs(inputArgs)
	env := cloneEnv(inputEnv)

	// Detect output format up-front so format-specific rewrites can branch.
	// Plex's argv ends with one of:
	//   -f dash           — DASH (Plex Web, Chrome on desktop, DASH-capable apps)
	//   -f ssegment       — Plex's stream-segmenter, used for HLS-style output
	//                       to mobile clients (iOS/Android). Stock ffmpeg
	//                       doesn't have ssegment; we translate to `-f segment`
	//                       with `-segment_format mpegts`.
	outputFormat := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-f" {
			outputFormat = args[i+1]
			break
		}
	}
	isHLS := outputFormat == "ssegment"

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

	// 1. Decoder swap (FIRST -codec:0 must precede -i)
	decCodecIdx := indexOfArg(args, "-codec:0", 0)
	if decCodecIdx < 0 || decCodecIdx >= inputIdx {
		return bail("no-decoder")
	}
	swDecoder := args[decCodecIdx+1]
	hwDecoder, ok := decoderMap[swDecoder]
	if !ok {
		return bail("unknown-decoder:" + swDecoder)
	}
	args[decCodecIdx+1] = hwDecoder
	args = spliceArgs(args, decCodecIdx+2,
		"-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi",
		"-hwaccel_device:0", "vaapi",
	)
	changes = append(changes, "decode:"+swDecoder+"->"+hwDecoder)

	// 2. -init_hw_device patch or inject
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
		newInputIdx := indexOfArg(args, "-i", 0)
		injectAt := newInputIdx + 2
		args = spliceArgs(args, injectAt,
			"-init_hw_device", "vaapi=vaapi:"+renderDevice+",driver="+vaapiDriver,
			"-filter_hw_device", "vaapi",
		)
		changes = append(changes, "inject:init_hw_device+filter_hw_device")
	}

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
	mediaPath := ""
	if i := indexOfArg(args, "-i", 0); i >= 0 && i+1 < len(args) {
		mediaPath = args[i+1]
	}

	// Subtitle source detection. PMS hands us the subtitle file/stream
	// via -map_inlineass <spec> + -i shape; the rewriter resolves which
	// case we're in (sidecar vs embedded) so the filter rewrite below
	// can swap Plex's private `inlineass=` for stock `subtitles=`. Any
	// embedded extraction needed is signalled to the agent via
	// RewriteResult.SubtitleExtract.
	subPath, subExtract, secondInputArgIdx := detectSubtitleSource(args, sessionDir)
	if subExtract != nil {
		changes = append(changes, "subtitle:embedded-extract:"+subExtract.StreamSpec)
	} else if subPath != "" {
		changes = append(changes, "subtitle:sidecar-staged")
	}

	rewritten := rewriteVideoFilter(args[vfIdx], mediaPath, subPath, fsExists, overlayEnabled)
	if rewritten == nil {
		return bail("filter-pattern:" + args[vfIdx])
	}
	args[vfIdx] = rewritten.Filter
	changes = append(changes, "filter:"+rewritten.Mode)

	// Drop the second `-i` for sidecar burn-in. The rewritten filter
	// graph references the file directly via `subtitles=filename=...`,
	// so stock ffmpeg has no use for it as an input — keeping it would
	// just trip the "stream specifier '1:s:0' matches no streams"
	// validator on muxers that don't see the unmapped stream. Compute
	// the offset against the post-filter args (no shifts to that point).
	if secondInputArgIdx > 0 && secondInputArgIdx+1 < len(args) {
		args = removeArgs(args, secondInputArgIdx, 2)
		changes = append(changes, "drop:-i(sidecar-input)")
	}

	// 4. Update -map output label following the video filter
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

	// Both subtitle-rewrite modes (overlay-vaapi sidecar and the older
	// hybrid-inlineass that uses subtitles= on a hwdownload→hwupload
	// roundtrip) need the Plex-private `-map_inlineass <stream>` argv
	// flag stripped, otherwise stock ffmpeg fails with "Unrecognized
	// option 'map_inlineass'" before the filter graph even runs. The
	// strip used to be gated on overlay-vaapi only, which broke LG
	// WebOS sub-burn the moment the rewriter picked hybrid-inlineass
	// (e.g. when a sidecar SRT is found but VAAPI overlay isn't safe
	// for the source — the fallback path).
	if rewritten.Mode == "overlay-vaapi" || rewritten.Mode == "hybrid-inlineass" {
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

	// 5. Encoder swap (next -codec:0 after -i)
	newInputIdx := indexOfArg(args, "-i", 0)
	encCodecIdx := indexOfArg(args, "-codec:0", newInputIdx+1)
	if encCodecIdx < 0 {
		return bail("no-encoder")
	}
	swEncoder := args[encCodecIdx+1]
	hwEncoder, ok := encoderMap(preferHEVC)[swEncoder]
	if !ok {
		return bail("unknown-encoder:" + swEncoder)
	}
	args[encCodecIdx+1] = hwEncoder
	changes = append(changes, "encode:"+swEncoder+"->"+hwEncoder)

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
			// crf wasn't numeric (shouldn't happen with PMS argv); pass
			// through unchanged but flip the flag name.
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

	// 9. Audio: Plex emits `-codec:1 eac3_eae -eae_prefix:1 <token>`
	// (eac3_eae is Plex's custom encoder backed by EasyAudioEncoder over
	// a localhost socket — only present in Plex Transcoder, not in
	// stock/jellyfin ffmpeg). Walk the whole arg list and replace every
	// `<codec-flag> eac3_eae` with `<codec-flag> eac3`. PMS sometimes
	// repeats the codec flag (early near-input and again after video
	// codec block).
	{
		audioCodecFlag := func(s string) bool {
			switch s {
			case "-codec:0", "-codec:1", "-c:a", "-c:a:0", "-c:a:1":
				return true
			}
			return false
		}
		swapped := false
		for i := 0; i < len(args); i++ {
			if audioCodecFlag(args[i]) && i+1 < len(args) && args[i+1] == "eac3_eae" {
				args[i+1] = "eac3"
				swapped = true
			}
		}
		if swapped {
			changes = append(changes, "audio:eac3_eae->eac3")
		}
	}
	// Drop -eae_prefix:N (any stream spec). Walk because there may be
	// multiple, and removeArgs shifts indexes.
	for {
		removed := false
		for i := 0; i < len(args); i++ {
			if strings.HasPrefix(args[i], "-eae_prefix") {
				dropped := args[i]
				args = removeArgs(args, i, 2)
				changes = append(changes, "drop:"+dropped)
				removed = true
				break
			}
		}
		if !removed {
			break
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
	for _, flag := range []string{"-loglevel_plex", "-delete_removed"} {
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
	// dash muxer's segment_index at N. Used on seek transcode sessions
	// (with a matching `-ss <offset>`) so chunk-stream0-NNNNN.m4s
	// aligns with PMS's expected URL `.../0/(N-1).m4s`. Stock dashenc
	// always counts from 1, so we capture N for the chunk-renumber
	// watcher (segwatch.go) to hardlink ffmpeg's 1-indexed output to
	// the N-indexed names PMS expects. Zero = initial-play session.
	skipToSegment := 0
	if i := indexOfArg(args, "-skip_to_segment", 0); i >= 0 && i+1 < len(args) {
		if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
			skipToSegment = n
			changes = append(changes, "skip_to_segment:captured="+args[i+1])
		}
		args = removeArgs(args, i, 2)
	}

	// Keep every chunk on disk for the lifetime of the session. Stock
	// dashenc deletes segments once they fall `extra_window_size` past
	// the manifest's sliding window (default 5 + 5 = 10 newest); PMS's
	// universal serve-chunk handler 404s the client when it asks for
	// chunk 3 and ffmpeg already deleted it. Inject a very large
	// extra_window_size so segments stick around. (We can't use 0 —
	// that means "extra zero", same as deletion-on-rotate.) DASH-only:
	// stock segment muxer for HLS retains all .ts files by default.
	if !isHLS && indexOfArg(args, "-extra_window_size", 0) < 0 {
		for k := 0; k+1 < len(args); k++ {
			if args[k] == "-f" && args[k+1] == "dash" {
				args = spliceArgs(args, k, "-extra_window_size", "999999")
				changes = append(changes, "inject:-extra_window_size=999999")
				break
			}
		}
	}

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

		// Strip `-copyts` for HLS. Verified locally: with `-ss <off>` and
		// `-copyts`, stock ffmpeg's segment muxer never emits a split — it
		// writes one giant first segment containing the entire remaining
		// runtime (observed: media-00173.ts grew to 222 MB / 23 min before
		// finally splitting on Balls Up). Drop only `-copyts` and the splits
		// resume at every keyframe past `-segment_time`. `-start_at_zero`
		// stays in for AAC encoder priming (removing it caused 199-byte
		// empty audio chunks earlier on DASH); `-avoid_negative_ts disabled`
		// is also harmless on its own. Plex's ssegment fork apparently
		// special-cases the copyts+seek path so they ship all three together.
		if i := indexOfArg(args, "-copyts", 0); i >= 0 {
			args = removeArgs(args, i, 1)
			changes = append(changes, "hls:drop:-copyts")
		}

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

	// Seek + force_key_frames expr fix.
	//
	// PMS sends `-force_key_frames:0 "expr:gte(t,n_forced*8)"`. With
	// `-copyts`, the encoder's `t` starts at the seek offset (e.g. 2344s)
	// rather than 0, so the expression is true for every frame whose
	// `n_forced*8 <= t`. ffmpeg fires a forced keyframe on every such
	// frame: ~294 keyframes back-to-back at the start, then ~8s of
	// silence, repeating. The HLS segment muxer needs a keyframe to
	// close a segment; with the run of forced keyframes followed by an
	// 8s gap, the first segment swallows tens of minutes of content
	// (observed: media-00293.ts reached 317 MB / 39 min before the next
	// keyframe-aligned split landed, breaking Android Plex on seek).
	// Plex's fork either resets `t` to 0 on seek or special-cases the
	// expr; stock ffmpeg does neither.
	//
	// Rewrite the expression to evaluate against output time
	// (t - seek_offset). Keyframe cadence then matches what PMS intended
	// (kf at output 0, 8, 16, ...) and splits land every 8s.
	if seekOffsetSeconds > 0 {
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
	manifestURL := ""
	if i := indexOfArg(args, "-manifest_name", 0); i >= 0 && i+1 < len(args) {
		base := envOr("SCALEPLEX_PMS_BASE_URL", "")
		if envBase, ok := inputEnv["SCALEPLEX_PMS_BASE_URL"]; ok && envBase != "" {
			base = envBase
		}
		origURL := args[i+1]
		args = removeArgs(args, i, 2)
		if base != "" && strings.HasPrefix(origURL, "http://127.0.0.1:32400") {
			rewritten := strings.Replace(origURL, "http://127.0.0.1:32400", base, 1)
			if tok, ok := inputEnv["X_PLEX_TOKEN"]; ok && tok != "" {
				if strings.Contains(rewritten, "?") {
					rewritten += "&X-Plex-Token=" + tok
				} else {
					rewritten += "?X-Plex-Token=" + tok
				}
			}
			manifestURL = rewritten
			changes = append(changes, "manifest_name:captured-for-publisher")
		} else {
			changes = append(changes, "drop:-manifest_name(no-pms-base-or-non-loopback)")
		}
	}

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
	progressURL := ""
	if i := indexOfArg(args, "-progressurl", 0); i >= 0 && i+1 < len(args) {
		base := envOr("SCALEPLEX_PMS_BASE_URL", "")
		if envBase, ok := inputEnv["SCALEPLEX_PMS_BASE_URL"]; ok && envBase != "" {
			base = envBase
		}
		origURL := args[i+1]
		args = removeArgs(args, i, 2)
		if base != "" {
			rewritten := strings.Replace(origURL, "http://127.0.0.1:32400", base, 1)
			// Plex's progress endpoint is auth-gated by the per-session
			// X_PLEX_TOKEN that PMS pipes into the spawn env. The PUT
			// has no headers we control on this side, so embed the
			// token in the URL query.
			if tok, ok := inputEnv["X_PLEX_TOKEN"]; ok && tok != "" {
				if strings.Contains(rewritten, "?") {
					rewritten += "&X-Plex-Token=" + tok
				} else {
					rewritten += "?X-Plex-Token=" + tok
				}
				changes = append(changes, "progress:append-X-Plex-Token")
			}
			progressURL = rewritten
			changes = append(changes, "progressurl:captured-for-reporter")
		} else {
			changes = append(changes, "drop:-progressurl(no-pms-base)")
		}
	}

	// PMS sets `-loglevel quiet`, which silences everything ffmpeg
	// would normally write to stderr. PMS's JobRunner reads the
	// child's stderr to detect transcoder readiness — without
	// "Stream mapping:" / "[dash @ ...] Representation N init segment
	// will be written to" lines, /header sits ~125s waiting for a
	// signal it never gets. Upgrade to `info` so those lines emit;
	// they ride the worker→orchestrator→shim stream back to PMS via
	// the shim's stderr.
	if i := indexOfArg(args, "-loglevel", 0); i >= 0 && i+1 < len(args) {
		if args[i+1] == "quiet" || args[i+1] == "panic" || args[i+1] == "fatal" {
			args[i+1] = "info"
			changes = append(changes, "loglevel:->info")
		}
	}
	// Also drop -nostats so ffmpeg emits its periodic
	// "size= time= bitrate= speed=" stderr line that PMS's
	// transcoder-statistics parser hooks into.
	if i := indexOfArg(args, "-nostats", 0); i >= 0 {
		args = removeArgs(args, i, 1)
		changes = append(changes, "drop:-nostats")
	}

	// 10. Strip env vars that point at Plex-Transcoder-only paths
	// (won't exist on the worker pod and confuse libavcodec init).
	// X_PLEX_TOKEN is INTENTIONALLY kept — it's the per-session auth
	// the progress endpoint expects (PMS routes PUT
	// /video/:/transcode/session/<token>/<uuid>/progress with that
	// token as X-Plex-Token); a future POST→PUT relay can use it.
	for _, k := range []string{"EAE_ROOT", "FFMPEG_EXTERNAL_LIBS"} {
		if _, ok := env[k]; ok {
			delete(env, k)
			changes = append(changes, "env:strip:"+k)
		}
	}

	// 11. VAAPI driver discovery env. Only override if explicitly set;
	// libva otherwise auto-discovers iHD on the worker image.
	env["LIBVA_DRIVER_NAME"] = vaapiDriver
	if libvaDriversPath != "" {
		env["LIBVA_DRIVERS_PATH"] = libvaDriversPath
	}
	changes = append(changes, "env:LIBVA")

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
