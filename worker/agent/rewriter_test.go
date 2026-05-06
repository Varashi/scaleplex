package main

import (
	"strings"
	"testing"
)

// AV1→H264 SW pattern captured from PMS log 2026-05-05 ~12:34Z
// (Superman 2025, GPU-less PMS, mode=remote, libdav1d/libx264).
var swArgsAV1H264 = []string{
	"-codec:0", "libdav1d",
	"-codec:1", "eac3_eae",
	"-eae_prefix:1", "buyrek9xv7rgflpt36lvm1xz_",
	"-analyzeduration", "20000000",
	"-probesize", "20000000",
	"-i", "/media/Movies/Superman.mkv",
	"-start_at_zero",
	"-copyts",
	"-fps_mode", "cfr",
	"-init_hw_device", "vaapi=vaapi:",
	"-filter_hw_device", "vaapi",
	"-y",
	"-nostats",
	"-loglevel", "quiet",
	"-loglevel_plex", "error",
	"-progressurl", "http://127.0.0.1:32400/.../progress",
	"-filter_complex", "[0:0]scale=w=2276:h=1280:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1]",
	"-map", "[1]",
	"-codec:0", "libx264",
	"-crf:0", "16",
	"-maxrate:0", "20000k",
	"-bufsize:0", "40000k",
	"-r:0", "23.975999999999999",
	"-preset:0", "veryfast",
	"-x264opts:0", "subme=0:me_range=4:rc_lookahead=10:me=dia:no_chroma_me:8x8dct=0:partitions=none",
	"-force_key_frames:0", "expr:gte(t,n_forced*3)",
	"-filter_complex", "[0:1] aresample=async=1:ochl='stereo':rematrix_maxval=0.000000dB:osr=48000[2]",
	"-map", "[2]",
	"-metadata:s:1", "language=eng",
	"-codec:1", "aac",
	"-b:1", "256k",
	"-f", "dash",
	"-seg_duration", "3",
	"-dash_segment_type", "mp4",
	"-init_seg_name", "init-stream$RepresentationID$.m4s",
	"-media_seg_name", "chunk-stream$RepresentationID$-$Number%05d$.m4s",
	"-window_size", "5",
	"-delete_removed", "false",
	"-skip_to_segment", "1",
	"-manifest_name", "http://127.0.0.1:32400/.../manifest?X-Plex-Http-Pipeline=infinite",
	"-avoid_negative_ts", "disabled",
	"-map_metadata", "-1",
	"-map_chapters", "-1",
	"dash",
}

// Captured from PMS log 2026-05-05 ~14:30Z when LG WebOS triggered burn
// of an SRT subtitle into a 4K stream while PMS was GPU-less.
var swArgsWithSubs = []string{
	"-codec:0", "libdav1d",
	"-codec:1", "eac3_eae",
	"-eae_prefix:1", "abc_",
	"-i", "/media/Movies/M.mkv",
	"-filter_complex", "[0:0]scale=w=3840:h=2160:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1];[1]inlineass=font_scale=1.000000:font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf:fontconfig_file=/usr/lib/plexmediaserver/Resources/fonts.conf:language=en:overrides=ScaledBorderAndShadow=yes,FontName=Noto Sans Medium,Bold=500,PrimaryColour=&H00FFFFFF,OutlineColour=&H00020713,BackColour=&HCC000000:outline=2.6:shadow=1.7:font_size=54[2]",
	"-map", "[2]",
	"-codec:0", "libx264",
	"-crf:0", "16",
	"-preset:0", "veryfast",
	"-force_key_frames:0", "expr:gte(t,n_forced*3)",
	"-map", "0:3", "-f", "null", "-codec", "ass", "nullfile",
}

var swArgsWithSubsSidecar = []string{
	"-codec:0", "libdav1d",
	"-codec:1", "eac3_eae",
	"-eae_prefix:1", "abc_",
	"-i", "/media/Movies/Superman (2025)/Superman (2025).mkv",
	"-map_inlineass", "0:3",
	"-filter_complex", "[0:0]scale=w=3840:h=2160:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1];[1]inlineass=font_scale=1.000000:font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf:fontconfig_file=/usr/lib/plexmediaserver/Resources/fonts.conf:language=en:overrides=ScaledBorderAndShadow=yes,FontName=Noto Sans Medium,Bold=500,PrimaryColour=&H00FFFFFF,OutlineColour=&H00020713,BackColour=&HCC000000:outline=2.6:shadow=1.7:font_size=54[2]",
	"-map", "[2]",
	"-codec:0", "libx264",
	"-crf:0", "16",
	"-preset:0", "veryfast",
	"-force_key_frames:0", "expr:gte(t,n_forced*3)",
	"-map", "0:3", "-f", "null", "-codec", "ass", "nullfile",
}

func findFilterComplex(args []string, prefix string) int {
	for i := 0; i < len(args); i++ {
		if args[i] == "-filter_complex" && i+1 < len(args) && strings.HasPrefix(args[i+1], prefix) {
			return i + 1
		}
	}
	return -1
}

func TestRewriter_AV1H264_AppliedAndChanges(t *testing.T) {
out := Rewrite(swArgsAV1H264, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("expected applied=true, changes=%v", out.Changes)
	}
	expectChanges := []string{
		"decode:libdav1d->av1",
		"init_hw_device",
		"filter:plain",
		"map-label-update",
		"encode:libx264->h264_vaapi",
		"crf->qp",
		"preset:veryfast->compression_level:6",
		"drop:-x264opts:0",
		"inject:sei+a53_cc",
		"env:LIBVA",
	}
	for _, ch := range expectChanges {
		if !containsString(out.Changes, ch) {
			t.Errorf("missing change %q (got %v)", ch, out.Changes)
		}
	}
}

func TestRewriter_AV1H264_DecoderFollowedByHwaccel(t *testing.T) {
out := Rewrite(swArgsAV1H264, nil, nil)
	decIdx := indexOfArg(out.Args, "-codec:0", 0)
	if out.Args[decIdx+1] != "av1" {
		t.Fatalf("decoder=%s want av1", out.Args[decIdx+1])
	}
	want := []string{
		"-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi",
		"-hwaccel_device:0", "vaapi",
	}
	got := out.Args[decIdx+2 : decIdx+8]
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("hwaccel arg[%d]=%q want %q", i, got[i], w)
		}
	}
}

func TestRewriter_AV1H264_InitHwDeviceGetsRenderDeviceAndDriver(t *testing.T) {
out := Rewrite(swArgsAV1H264, nil, nil)
	i := indexOfArg(out.Args, "-init_hw_device", 0)
	if got := out.Args[i+1]; got != "vaapi=vaapi:/dev/dri/renderD128,driver=iHD" {
		t.Fatalf("init_hw_device=%q", got)
	}
}

func TestRewriter_AV1H264_FilterIsVaapiPlain(t *testing.T) {
out := Rewrite(swArgsAV1H264, nil, nil)
	idx := findFilterComplex(out.Args, "[0:0]")
	want := "[0:0]hwupload[0];[0]scale_vaapi=w=2276:h=1280:format=nv12[1];[1]hwupload[2]"
	if out.Args[idx] != want {
		t.Fatalf("filter=%q want %q", out.Args[idx], want)
	}
}

func TestRewriter_AV1H264_MapLabelUpdated(t *testing.T) {
out := Rewrite(swArgsAV1H264, nil, nil)
	idx := findFilterComplex(out.Args, "[0:0]")
	for i := idx + 1; i < len(out.Args); i++ {
		if out.Args[i] == "-map" {
			if out.Args[i+1] != "[2]" {
				t.Fatalf("map=%q want [2]", out.Args[i+1])
			}
			return
		}
	}
	t.Fatal("no -map after filter")
}

func TestRewriter_AV1H264_EncoderEtc(t *testing.T) {
out := Rewrite(swArgsAV1H264, nil, nil)
	if containsString(out.Args, "-preset:0") {
		t.Error("preset:0 not consumed (should translate to -compression_level:v)")
	}
	if containsString(out.Args, "-x264opts:0") {
		t.Error("x264opts:0 not dropped")
	}
	fkfIdx := indexOfArg(out.Args, "-force_key_frames:0", 0)
	if out.Args[fkfIdx-2] != "-sei:0" || out.Args[fkfIdx-1] != "-a53_cc" {
		t.Fatalf("expected -sei:0 -a53_cc before -force_key_frames:0, got %q %q", out.Args[fkfIdx-2], out.Args[fkfIdx-1])
	}
	inputIdx := indexOfArg(out.Args, "-i", 0)
	encIdx := indexOfArg(out.Args, "-codec:0", inputIdx+1)
	if out.Args[encIdx+1] != "h264_vaapi" {
		t.Fatalf("encoder=%q want h264_vaapi", out.Args[encIdx+1])
	}
	// veryfast → compression_level 6
	clIdx := indexOfArg(out.Args, "-compression_level:v", encIdx)
	if clIdx <= 0 {
		t.Fatal("missing -compression_level:v")
	}
	if out.Args[clIdx+1] != "6" {
		t.Fatalf("compression_level=%q want 6 (veryfast)", out.Args[clIdx+1])
	}
	qpIdx := indexOfArg(out.Args, "-qp:0", 0)
	if qpIdx <= 0 {
		t.Fatal("missing -qp:0")
	}
	if out.Args[qpIdx+1] != "16" {
		t.Fatalf("qp=%q want 16", out.Args[qpIdx+1])
	}
}

func TestRewriter_AV1H264_EnvLIBVA_DefaultsAreImageResident(t *testing.T) {
out := Rewrite(swArgsAV1H264, map[string]string{"TZ": "Europe/Brussels"}, nil)
	// Default scaleplex worker doesn't override LIBVA_DRIVERS_PATH —
	// libva auto-discovers iHD under /usr/lib/x86_64-linux-gnu/dri.
	if _, ok := out.Env["LIBVA_DRIVERS_PATH"]; ok {
		t.Fatalf("default should not set LIBVA_DRIVERS_PATH, got %q", out.Env["LIBVA_DRIVERS_PATH"])
	}
	if out.Env["LIBVA_DRIVER_NAME"] != "iHD" {
		t.Fatalf("LIBVA_DRIVER_NAME=%q", out.Env["LIBVA_DRIVER_NAME"])
	}
	if out.Env["TZ"] != "Europe/Brussels" {
		t.Fatalf("TZ stripped: %q", out.Env["TZ"])
	}
}

func TestRewriter_LIBVADriversPath_OverrideHonored(t *testing.T) {
t.Setenv("HW_LIBVA_DRIVERS_PATH", "/opt/some/cache/dri")
	out := Rewrite(swArgsAV1H264, nil, nil)
	if out.Env["LIBVA_DRIVERS_PATH"] != "/opt/some/cache/dri" {
		t.Fatalf("LIBVA_DRIVERS_PATH=%q", out.Env["LIBVA_DRIVERS_PATH"])
	}
}

func TestRewriter_AV1H264_ReturnsCopy(t *testing.T) {
before := strings.Join(swArgsAV1H264, "\x00")
	Rewrite(swArgsAV1H264, nil, nil)
	after := strings.Join(swArgsAV1H264, "\x00")
	if before != after {
		t.Fatal("input args mutated")
	}
}

func TestRewriter_InitHwDevice_Inject(t *testing.T) {
args := []string{
		"-codec:0", "libdav1d",
		"-i", "/media/m.mkv",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264",
		"-crf:0", "16",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "inject:init_hw_device+filter_hw_device") {
		t.Fatalf("missing injection: %v", out.Changes)
	}
	i := indexOfArg(out.Args, "-i", 0)
	want := []string{
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_hw_device", "vaapi",
	}
	for k, w := range want {
		if out.Args[i+2+k] != w {
			t.Fatalf("arg[%d]=%q want %q", i+2+k, out.Args[i+2+k], w)
		}
	}
}

func TestRewriter_HybridInlineAss(t *testing.T) {
out := Rewrite(swArgsWithSubs, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:hybrid-inlineass") {
		t.Fatalf("missing hybrid-inlineass: %v", out.Changes)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	for _, must := range []string{
		"[0:0]hwupload[10]",
		"scale_vaapi=w=3840:h=2160:format=nv12[11]",
		"[11]hwdownload[12]",
		"[12]format=pix_fmts=nv12[13]",
		"[13]inlineass=",
		"font_scale=1.000000",
		"font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf",
		"font_size=54[14]",
		"[14]hwupload[15]",
	} {
		if !strings.Contains(f, must) {
			t.Errorf("filter missing %q\n%s", must, f)
		}
	}
}

func TestRewriter_HybridInlineAss_MapLabel15(t *testing.T) {
out := Rewrite(swArgsWithSubs, nil, nil)
	idx := findFilterComplex(out.Args, "[0:0]")
	for i := idx + 1; i < len(out.Args); i++ {
		if out.Args[i] == "-map" {
			if out.Args[i+1] != "[15]" {
				t.Fatalf("map=%q want [15]", out.Args[i+1])
			}
			return
		}
	}
	t.Fatal("no -map found")
}

func TestRewriter_PreferHEVC(t *testing.T) {
t.Setenv("HW_PREFER_HEVC", "true")
	args := []string{
		"-codec:0", "libdav1d", "-i", "m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264", "-crf:0", "16", "-preset:0", "veryfast",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "encode:libx264->hevc_vaapi") {
		t.Fatalf("missing hevc encode change: %v", out.Changes)
	}
	inputIdx := indexOfArg(out.Args, "-i", 0)
	encIdx := indexOfArg(out.Args, "-codec:0", inputIdx+1)
	if out.Args[encIdx+1] != "hevc_vaapi" {
		t.Fatalf("encoder=%q want hevc_vaapi", out.Args[encIdx+1])
	}
}

func TestRewriter_PreferHEVCOff(t *testing.T) {
t.Setenv("HW_PREFER_HEVC", "")
	args := []string{
		"-codec:0", "libdav1d", "-i", "m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264", "-crf:0", "16", "-preset:0", "veryfast",
	}
	out := Rewrite(args, nil, nil)
	if !containsString(out.Changes, "encode:libx264->h264_vaapi") {
		t.Fatalf("default-off should map to h264_vaapi: %v", out.Changes)
	}
}

func TestRewriter_OverlayVAAPI_Sidecar(t *testing.T) {
t.Setenv("HW_OVERLAY_VAAPI_ENABLED", "true")
	expected := "/media/Movies/Superman (2025)/Superman (2025).en.srt"
	fsMock := func(p string) bool { return p == expected }
	out := Rewrite(swArgsWithSubsSidecar, nil, &RewriteOpts{FSExists: fsMock})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:overlay-vaapi") {
		t.Fatalf("missing overlay-vaapi: %v", out.Changes)
	}
	if !containsString(out.Changes, "sidecar:"+expected) {
		t.Fatalf("missing sidecar change: %v", out.Changes)
	}
	if !containsString(out.Changes, "drop:-map_inlineass") {
		t.Fatalf("missing drop:-map_inlineass: %v", out.Changes)
	}
	if containsString(out.Args, "-map_inlineass") {
		t.Fatal("-map_inlineass not dropped")
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	for _, must := range []string{
		"[0:0]hwupload[10]",
		"[10]scale_vaapi=w=3840:h=2160:format=nv12[11]",
		"[11]hwdownload[12]",
		"[12]format=pix_fmts=nv12[13]",
		"subtitles=filename='/media/Movies/Superman (2025)/Superman (2025).en.srt'",
		"fontsdir=/usr/share/fonts/truetype/dejavu",
		"[14]hwupload[15]",
	} {
		if !strings.Contains(f, must) {
			t.Errorf("filter missing %q\n%s", must, f)
		}
	}
	for i := idx + 1; i < len(out.Args); i++ {
		if out.Args[i] == "-map" {
			if out.Args[i+1] != "[15]" {
				t.Fatalf("map=%q want [15]", out.Args[i+1])
			}
			break
		}
	}
	// Default: FONTCONFIG_* not injected (image's system fontconfig is used).
	if v, ok := out.Env["FONTCONFIG_FILE"]; ok {
		t.Fatalf("FONTCONFIG_FILE should be unset by default, got %q", v)
	}
	if !containsString(out.Args, "nullfile") {
		t.Fatal("nullfile output mapping must be retained for HLS bookkeeping")
	}
}

func TestRewriter_OverlayVAAPI_FallsBackWhenNoSidecar(t *testing.T) {
t.Setenv("HW_OVERLAY_VAAPI_ENABLED", "true")
	out := Rewrite(swArgsWithSubsSidecar, nil, &RewriteOpts{FSExists: func(string) bool { return false }})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:hybrid-inlineass") {
		t.Fatalf("expected hybrid fallback: %v", out.Changes)
	}
	if !containsString(out.Args, "-map_inlineass") {
		t.Fatal("-map_inlineass should remain in hybrid mode")
	}
}

func TestRewriter_OverlayVAAPI_SidecarSpecialChars(t *testing.T) {
t.Setenv("HW_OVERLAY_VAAPI_ENABLED", "true")
	// Real-world filenames: apostrophe, brackets, braces, parentheses.
	// All should make it into the filter without breaking ffmpeg's parser
	// because ' is escaped by escapeFilterPath and the rest are filename-
	// legal inside a single-quoted filename= argument.
	src := "/media/Movies/Pirates of the Caribbean: At World's End (2007)/Pirates {tmdb-285} [Hybrid][Remux-2160p][HDR][AV1].mkv"
	expectedSub := "/media/Movies/Pirates of the Caribbean: At World's End (2007)/Pirates {tmdb-285} [Hybrid][Remux-2160p][HDR][AV1].en.srt"
	args := []string{
		"-codec:0", "libdav1d",
		"-i", src,
		"-map_inlineass", "0:3",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1];[1]inlineass=language=en:font_size=54[2]",
		"-map", "[2]",
		"-codec:0", "libx264",
		"-crf:0", "16",
	}
	fsMock := func(p string) bool { return p == expectedSub }
	out := Rewrite(args, nil, &RewriteOpts{FSExists: fsMock})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:overlay-vaapi") {
		t.Fatalf("expected overlay-vaapi: %v", out.Changes)
	}
	if !containsString(out.Changes, "sidecar:"+expectedSub) {
		t.Fatalf("sidecar change missing: %v", out.Changes)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	// hwdl shape: subtitles= sits in [13] →[14], inside single quotes.
	// `:` and `'` in the path must be backslash-escaped so the filter-
	// graph parser doesn't split on them.
	if !strings.Contains(f, `subtitles=filename='/media/Movies/Pirates of the Caribbean\:`) {
		t.Errorf("`:` in path not escaped:\n%s", f)
	}
	if !strings.Contains(f, `World\'s End`) {
		t.Errorf("`'` in path not escaped:\n%s", f)
	}
}

func TestRewriter_Bail_SubtitlesBurnIn(t *testing.T) {
args := []string{
		"-codec:0", "libdav1d", "-i", "m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[v];[v]subtitles=f.srt[0]",
		"-map", "[0]", "-codec:0", "libx264",
	}
	out := Rewrite(args, nil, nil)
	if out.Applied {
		t.Fatal("should bail")
	}
	if !containsString(out.Changes, "skip:subtitles-burn-in") {
		t.Fatalf("changes=%v", out.Changes)
	}
}

// HDR PMS-shape filter — synthetic mirror of Plex's SW HDR→SDR chain.
// Real captures may differ in label numbering and filter args; the
// matcher is intentionally flexible (zscale + tonemap + final nv12 label).
func TestRewriter_PresetMapping(t *testing.T) {
	cases := []struct {
		x264   string
		vaapi  string
	}{
		{"ultrafast", "7"},
		{"superfast", "7"},
		{"veryfast", "6"},
		{"faster", "5"},
		{"fast", "4"},
		{"medium", "4"},
		{"slow", "3"},
		{"slower", "2"},
		{"veryslow", "1"},
		{"placebo", "1"},
		{"VeryFast", "6"}, // case-insensitive
		{"unknownish", "7"}, // unknown → fastest
	}
	for _, c := range cases {
		got := mapX264PresetToVAAPI(c.x264)
		if got != c.vaapi {
			t.Errorf("preset %q → cl=%q, want %q", c.x264, got, c.vaapi)
		}
	}
}

func TestRewriter_PresetMapping_FullArgRewrite(t *testing.T) {
	args := []string{
		"-codec:0", "libdav1d", "-i", "m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264", "-crf:0", "16", "-preset:0", "ultrafast",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "preset:ultrafast->compression_level:7") {
		t.Fatalf("missing preset translation change: %v", out.Changes)
	}
	encIdx := indexOfArg(out.Args, "-codec:0", indexOfArg(out.Args, "-i", 0))
	if out.Args[encIdx+2] != "-compression_level:v" || out.Args[encIdx+3] != "7" {
		t.Fatalf("expected -compression_level:v 7 right after encoder swap, got %q %q",
			out.Args[encIdx+2], out.Args[encIdx+3])
	}
	if containsString(out.Args, "-preset:0") {
		t.Fatal("-preset:0 should be consumed")
	}
}

func TestRewriter_NoPresetEmitted_DefaultsFastest(t *testing.T) {
	// libx265 path: PMS emits -x265-params instead of -preset:0. Worker
	// has nothing to translate, so we inject compression_level=7.
	args := []string{
		"-codec:0", "libdav1d", "-i", "m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx265", "-crf:0", "20", "-x265-params", "no-info=1",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "inject:compression_level=7") {
		t.Fatalf("missing default-fastest injection: %v", out.Changes)
	}
}

func TestRewriter_HDR_TonemapVAAPI(t *testing.T) {
args := []string{
		"-codec:0", "libdav1d",
		"-i", "/media/m.mkv",
		"-filter_complex",
		"[0:0]scale=w=1920:h=1080:force_divisible_by=4[0];" +
			"[0]zscale=t=linear:npl=100[1];" +
			"[1]format=gbrpf32le[2];" +
			"[2]zscale=primaries=bt709[3];" +
			"[3]tonemap=tonemap=hable:desat=0[4];" +
			"[4]zscale=t=bt709:m=bt709:r=tv[5];" +
			"[5]format=pix_fmts=yuv420p|nv12[6]",
		"-map", "[6]",
		"-codec:0", "libx264",
		"-crf:0", "16",
		"-preset:0", "veryfast",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:hdr-tonemap-vaapi") {
		t.Fatalf("missing hdr-tonemap-vaapi: %v", out.Changes)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	want := "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=p010,tonemap_vaapi=transfer=bt709:format=nv12[1];[1]hwupload[2]"
	if out.Args[idx] != want {
		t.Fatalf("filter=%q\nwant   %q", out.Args[idx], want)
	}
	for i := idx + 1; i < len(out.Args); i++ {
		if out.Args[i] == "-map" {
			if out.Args[i+1] != "[2]" {
				t.Fatalf("map=%q want [2]", out.Args[i+1])
			}
			return
		}
	}
	t.Fatal("no -map found after filter")
}

func TestRewriter_Bail_UnknownDecoder(t *testing.T) {
args := []string{
		"-codec:0", "librav1e", "-i", "m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]", "-codec:0", "libx264",
	}
	out := Rewrite(args, nil, nil)
	if out.Applied {
		t.Fatal("should bail")
	}
	if !containsString(out.Changes, "skip:unknown-decoder:librav1e") {
		t.Fatalf("changes=%v", out.Changes)
	}
}

func TestRewriter_Bail_FilterMismatch_NoCorruption(t *testing.T) {
args := []string{
		"-codec:0", "libdav1d", "-i", "m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]some_unsupported_filter[1]",
		"-map", "[1]", "-codec:0", "libx264",
	}
	out := Rewrite(args, nil, nil)
	if out.Applied {
		t.Fatal("should bail")
	}
	hasSkip := false
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "skip:filter-pattern:") {
			hasSkip = true
			break
		}
	}
	if !hasSkip {
		t.Fatalf("missing skip:filter-pattern: %v", out.Changes)
	}
	if strings.Join(out.Args, "\x00") != strings.Join(args, "\x00") {
		t.Fatal("args returned not equal to input")
	}
}

// -progressurl must be stripped entirely (not translated to -progress)
// and the worker-reachable URL surfaced on RewriteResult.ProgressURL,
// with the per-session X-Plex-Token appended as a query param.
func TestRewriter_ProgressURL_CapturedAndStripped(t *testing.T) {
	in := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay.svc:32499",
		"X_PLEX_TOKEN":           "secret",
	}
	out := Rewrite(swArgsAV1H264, in, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Args, "-progressurl") {
		t.Fatal("-progressurl must be stripped from argv")
	}
	if containsString(out.Args, "-progress") {
		t.Fatal("-progress must NOT be inserted; reporter handles HTTP")
	}
	wantURL := "http://relay.svc:32499/.../progress?X-Plex-Token=secret"
	if out.ProgressURL != wantURL {
		t.Fatalf("ProgressURL=%q want %q", out.ProgressURL, wantURL)
	}
	if !containsString(out.Changes, "progressurl:captured-for-reporter") {
		t.Fatalf("missing capture change: %v", out.Changes)
	}
	if !containsString(out.Changes, "progress:append-X-Plex-Token") {
		t.Fatalf("missing token-append change: %v", out.Changes)
	}
}

// Without a base URL we drop -progressurl entirely and ProgressURL is
// empty (reporter no-ops). Avoids workers POSTing to PMS's internal
// 127.0.0.1:32400 — which they can't reach anyway.
func TestRewriter_ProgressURL_NoBase_Drops(t *testing.T) {
	out := Rewrite(swArgsAV1H264, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Args, "-progressurl") {
		t.Fatal("-progressurl must be stripped from argv")
	}
	if out.ProgressURL != "" {
		t.Fatalf("ProgressURL=%q want empty when no base", out.ProgressURL)
	}
	if !containsString(out.Changes, "drop:-progressurl(no-pms-base)") {
		t.Fatalf("missing drop change: %v", out.Changes)
	}
}

// -manifest_name must be stripped from ffmpeg argv (stock dashenc treats
// the value as a filename, not an HTTP URL) and the rewritten URL
// surfaced on RewriteResult.ManifestURL with X-Plex-Token appended. The
// publisher uses this URL to POST the manifest body — without that POST,
// PMS's /header stalls ~125s.
func TestRewriter_ManifestURL_CapturedAndStripped(t *testing.T) {
	in := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay.svc:32499",
		"X_PLEX_TOKEN":           "secret",
	}
	out := Rewrite(swArgsAV1H264, in, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Args, "-manifest_name") {
		t.Fatal("-manifest_name must be stripped from argv")
	}
	wantURL := "http://relay.svc:32499/.../manifest?X-Plex-Http-Pipeline=infinite&X-Plex-Token=secret"
	if out.ManifestURL != wantURL {
		t.Fatalf("ManifestURL=%q want %q", out.ManifestURL, wantURL)
	}
	if !containsString(out.Changes, "manifest_name:captured-for-publisher") {
		t.Fatalf("missing capture change: %v", out.Changes)
	}
}

// Without a base URL the manifest URL is dropped entirely (publisher
// no-ops). PMS's /header stays slow but the alternative is workers
// POSTing to an unreachable 127.0.0.1.
func TestRewriter_ManifestURL_NoBase_Drops(t *testing.T) {
	out := Rewrite(swArgsAV1H264, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Args, "-manifest_name") {
		t.Fatal("-manifest_name must be stripped from argv")
	}
	if out.ManifestURL != "" {
		t.Fatalf("ManifestURL=%q want empty when no base", out.ManifestURL)
	}
	if !containsString(out.Changes, "drop:-manifest_name(no-pms-base-or-non-loopback)") {
		t.Fatalf("missing drop change: %v", out.Changes)
	}
}
