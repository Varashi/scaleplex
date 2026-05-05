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
	"strings"
)

type RewriteResult struct {
	Args    []string
	Env     map[string]string
	Applied bool
	Changes []string
}

// RewriteOpts is for testability; production callers pass nil.
type RewriteOpts struct {
	FSExists func(string) bool
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
// SRT/ASS subtitle file. Probe order:
//   <base>.<lang>.srt, <base>.<lang>.ass, <base>.srt, <base>.ass
func findSidecarSubtitle(mediaPath, lang string, fsExists func(string) bool) string {
	if mediaPath == "" {
		return ""
	}
	dir := filepath.Dir(mediaPath)
	ext := filepath.Ext(mediaPath)
	base := strings.TrimSuffix(filepath.Base(mediaPath), ext)
	var cands []string
	if lang != "" {
		cands = append(cands,
			filepath.Join(dir, base+"."+lang+".srt"),
			filepath.Join(dir, base+"."+lang+".ass"),
		)
	}
	cands = append(cands,
		filepath.Join(dir, base+".srt"),
		filepath.Join(dir, base+".ass"),
	)
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

type filterRewrite struct {
	Filter   string
	OldLabel string
	NewLabel string
	Mode     string
	Sidecar  string
}

func rewriteVideoFilter(filterStr, mediaPath string, fsExists func(string) bool, overlayEnabled bool) *filterRewrite {
	if m := reFilterAss.FindStringSubmatch(filterStr); m != nil {
		w, h, assParams := m[1], m[2], m[3]
		var lang string
		if lm := reLanguage.FindStringSubmatch(assParams); lm != nil {
			lang = strings.ToLower(lm[1])
		}
		sidecar := findSidecarSubtitle(mediaPath, lang, fsExists)

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

		return &filterRewrite{
			Filter: fmt.Sprintf(
				"[0:0]hwupload[10];[10]scale_vaapi=w=%s:h=%s:format=nv12[11];"+
					"[11]hwdownload[12];[12]format=pix_fmts=nv12[13];"+
					"[13]inlineass=%s[14];[14]hwupload[15]",
				w, h, assParams),
			OldLabel: "[2]",
			NewLabel: "[15]",
			Mode:     "hybrid-inlineass",
		}
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
	if opts != nil && opts.FSExists != nil {
		fsExists = opts.FSExists
	}

	preferHEVC := envBool("HW_PREFER_HEVC")
	overlayEnabled := envBool("HW_OVERLAY_VAAPI_ENABLED")
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
	rewritten := rewriteVideoFilter(args[vfIdx], mediaPath, fsExists, overlayEnabled)
	if rewritten == nil {
		return bail("filter-pattern:" + args[vfIdx])
	}
	args[vfIdx] = rewritten.Filter
	changes = append(changes, "filter:"+rewritten.Mode)

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

	if rewritten.Mode == "overlay-vaapi" {
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
		changes = append(changes, "sidecar:"+rewritten.Sidecar)
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

	// 6. -crf:0 → -qp:0
	if crfIdx := indexOfArg(args, "-crf:0", encCodecIdx+1); crfIdx >= 0 {
		args[crfIdx] = "-qp:0"
		changes = append(changes, "crf->qp")
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
	//   -delete_removed <bool>   — Plex DASH muxer extension; stock
	//                              ffmpeg keeps segments by default so
	//                              the false case is the implicit behaviour
	//   -skip_to_segment <N>     — Plex DASH muxer extension to start
	//                              segment numbering at N; stock ffmpeg
	//                              starts at 1, which is what Plex sends
	//                              anyway in the cases we've seen
	for _, flag := range []string{"-loglevel_plex", "-delete_removed", "-skip_to_segment"} {
		if i := indexOfArg(args, flag, 0); i >= 0 {
			args = removeArgs(args, i, 2)
			changes = append(changes, "drop:"+flag)
		}
	}

	// Translate `-progressurl <url>` → `-progress <url>`. Stock ffmpeg
	// emits the same key=value progress payload to a destination URL
	// (Plex Transcoder is an ffmpeg fork; the format matches), so PMS
	// keeps tracking transcoder-vs-playback offset and Tautulli's
	// "X seconds ahead" indicator stays accurate.
	if i := indexOfArg(args, "-progressurl", 0); i >= 0 && i+1 < len(args) {
		args[i] = "-progress"
		changes = append(changes, "progressurl->progress")
	}

	// PMS sets `-loglevel quiet`, which silences errors too. Upgrade to
	// `error` so a transcode failure actually leaves a stderr trail
	// (worker captures and logs the tail; orchestrator streams it back
	// to the shim → PMS log). Idempotent if already at error or above.
	if i := indexOfArg(args, "-loglevel", 0); i >= 0 && i+1 < len(args) {
		if args[i+1] == "quiet" || args[i+1] == "panic" || args[i+1] == "fatal" {
			args[i+1] = "error"
			changes = append(changes, "loglevel:->error")
		}
	}

	// 10. Strip env vars that point at Plex-Transcoder-only paths
	// (won't exist on the worker pod and confuse libavcodec init).
	for _, k := range []string{"EAE_ROOT", "FFMPEG_EXTERNAL_LIBS", "X_PLEX_TOKEN"} {
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

	return RewriteResult{Args: args, Env: env, Applied: true, Changes: changes}
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
