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

// Embedded subtitle case — single `-i`, `-map_inlineass 0:3` references
// stream 3 of the source mkv. Captured from PMS log 2026-05-06 (Balls
// Up burn-in to LG WebOS).
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

// External sidecar case — Plex stages the sidecar SRT in the session's
// transcode dir as `temp-0.srt` and adds it as a SECOND `-i`.
// `-map_inlineass 1:s:0` references that input. Captured from PMS log
// 2026-05-06 (Balls Up sidecar SRT burn-in).
var swArgsWithSubsRealSidecar = []string{
	"-codec:0", "libdav1d",
	"-analyzeduration", "20000000", "-probesize", "20000000",
	"-i", "/media/Movies/Balls Up (2026)/Balls Up (2026).mkv",
	"-analyzeduration", "20000000", "-probesize", "20000000",
	"-i", "/transcode/Transcode/Sessions/plex-transcode-q5orqh9o-c7edac0f/temp-0.srt",
	"-start_at_zero", "-copyts", "-fps_mode", "cfr",
	"-map_inlineass", "1:s:0",
	"-filter_complex", "[0:0]scale=w=3840:h=1600:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1];[1]inlineass=font_scale=1.000000:font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf:fontconfig_file=/usr/lib/plexmediaserver/Resources/fonts.conf:language=en:overrides=ScaledBorderAndShadow=yes,FontName=Noto Sans Medium,Bold=500,PrimaryColour=&H00FFFFFF,OutlineColour=&H00020713,BackColour=&HCC000000:outline=2.6:shadow=1.7:font_size=54[2]",
	"-map", "[2]",
	"-codec:0", "libx264",
	"-crf:0", "16",
	"-maxrate:0", "20121k",
	"-bufsize:0", "40242k",
	"-preset:0", "veryfast",
	"-force_key_frames:0", "expr:gte(t,n_forced*1)",
	"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
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
		// CRF=16 → QP=22 with +6 offset (CRF and VAAPI QP scales differ;
		// see rewriter.go for empirical bench).
		"crf16->qp22(off=6)",
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
	// CQP path: -crf:0 16 → -qp:0 22 (CRF + 6 offset). The offset
	// compensates for libx264 CRF being a quality target that floats
	// QP per-frame around the value, while VAAPI's -qp is the literal
	// quantizer. Mapping CRF→QP 1:1 produced near-lossless output and
	// 5× over-budget segments on Balls Up (4K HDR); +6 lands closer to
	// libx264's effective per-frame QP at the same perceptual quality.
	qpIdx := indexOfArg(out.Args, "-qp:0", 0)
	if qpIdx <= 0 {
		t.Fatal("missing -qp:0")
	}
	if out.Args[qpIdx+1] != "22" {
		t.Errorf("qp=%q want 22 (CRF=16 + offset=6)", out.Args[qpIdx+1])
	}
}

// HW_QP_CRF_OFFSET overrides the +6 default.
func TestRewriter_QPOffset_EnvOverride(t *testing.T) {
	t.Setenv("HW_QP_CRF_OFFSET", "0")
	out := Rewrite(swArgsAV1H264, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	qpIdx := indexOfArg(out.Args, "-qp:0", 0)
	if qpIdx <= 0 {
		t.Fatal("missing -qp:0")
	}
	if out.Args[qpIdx+1] != "16" {
		t.Errorf("qp=%q want 16 (offset 0 → 1:1 mapping)", out.Args[qpIdx+1])
	}
}

// Negative or oversized result clamps to [0, 51].
func TestRewriter_QPOffset_Clamping(t *testing.T) {
	t.Setenv("HW_QP_CRF_OFFSET", "100")
	out := Rewrite(swArgsAV1H264, nil, nil)
	qpIdx := indexOfArg(out.Args, "-qp:0", 0)
	if qpIdx <= 0 {
		t.Fatal("missing -qp:0")
	}
	if out.Args[qpIdx+1] != "51" {
		t.Errorf("qp=%q want 51 (clamped)", out.Args[qpIdx+1])
	}
}

// Sanity test that -crf with no -maxrate also lands in CQP — same as
// the AV1H264 path now, kept around because the encoder runs in CQP
// regardless of whether -maxrate is present.
func TestRewriter_RateControl_CRFOnly_KeepsCQP(t *testing.T) {
	args := append([]string(nil), swArgsAV1H264...)
	// Strip -maxrate:0 and -bufsize:0
	for i := 0; i < len(args); {
		if args[i] == "-maxrate:0" || args[i] == "-bufsize:0" {
			args = append(args[:i], args[i+2:]...)
			continue
		}
		i++
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	hasMapping := false
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "crf16->qp22(off=") || c == "crf->qp" {
			hasMapping = true
			break
		}
	}
	if !hasMapping {
		t.Errorf("expected crf→qp mapping, got %v", out.Changes)
	}
	if !containsString(out.Args, "-qp:0") {
		t.Error("CQP path must keep -qp:0")
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
	// Injection lands BEFORE the first -i (global ffmpeg option scope).
	// Placing it after -i puts it in per-output scope where the av1
	// hwaccel decoder doesn't see the device — ffmpeg fails with
	// "No VA display found for device vaapi" at filter graph build.
	i := indexOfArg(out.Args, "-i", 0)
	if i < 4 {
		t.Fatalf("-i too early; injection should precede it. args=%v", out.Args)
	}
	want := []string{
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_hw_device", "vaapi",
	}
	for k, w := range want {
		if out.Args[i-4+k] != w {
			t.Fatalf("arg[%d]=%q want %q", i-4+k, out.Args[i-4+k], w)
		}
	}
}

// inlineass-style argv with no sidecar on disk: bail. The previous
// behaviour emitted a filter graph using Plex's private `inlineass`
// filter, which stock ffmpeg doesn't have, and ffmpeg failed at runtime
// with "Filter not found" (LG WebOS sub-burn 2026-05-06). Bailing
// surfaces the failure in the rewriter's change list instead of
// pretending success and exploding mid-transcode.
func TestRewriter_InlineAss_NoSidecar_Bails(t *testing.T) {
	out := Rewrite(swArgsWithSubs, nil, &RewriteOpts{FSExists: func(string) bool { return false }})
	if out.Applied {
		t.Fatalf("expected bail when no sidecar; applied=true: %v", out.Changes)
	}
	if !containsString(out.Changes, "skip:filter-pattern:") {
		// Find any change starting with skip:filter-pattern:
		found := false
		for _, c := range out.Changes {
			if strings.HasPrefix(c, "skip:filter-pattern:") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected skip:filter-pattern bail: %v", out.Changes)
		}
	}
}

// PMS picks libx265 when its prefs + client caps both ask for HEVC.
// Worker must follow that signal and emit hevc_vaapi (manifest's
// codec_string is generated from the same PMS-side choice, so they
// stay in sync).
func TestRewriter_PMSHEVCMapsToHEVCVAAPI(t *testing.T) {
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
	if !containsString(out.Changes, "encode:libx265->hevc_vaapi") {
		t.Fatalf("missing hevc encode change: %v", out.Changes)
	}
	inputIdx := indexOfArg(out.Args, "-i", 0)
	encIdx := indexOfArg(out.Args, "-codec:0", inputIdx+1)
	if out.Args[encIdx+1] != "hevc_vaapi" {
		t.Fatalf("encoder=%q want hevc_vaapi", out.Args[encIdx+1])
	}
}

// PMS picks libx264 (default H264 path) → worker emits h264_vaapi.
func TestRewriter_PMSH264MapsToH264VAAPI(t *testing.T) {
	args := []string{
		"-codec:0", "libdav1d", "-i", "m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264", "-crf:0", "16", "-preset:0", "veryfast",
	}
	out := Rewrite(args, nil, nil)
	if !containsString(out.Changes, "encode:libx264->h264_vaapi") {
		t.Fatalf("libx264 should map to h264_vaapi: %v", out.Changes)
	}
}

// containsAnyWithPrefix returns true when any change starts with the
// given prefix — handy for filter modes whose suffix varies (-hdr,
// -1080split, -bitmap, ...).
func containsAnyWithPrefix(slice []string, prefix string) bool {
	for _, s := range slice {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// Update the second sidecar test (already in this file) and the
// other overlay-vaapi assertions to use the prefix matcher.

// External-sidecar burn-in. PMS stages the .srt in the session's
// transcode dir as `temp-0.srt` and references it via a second `-i` +
// `-map_inlineass 1:s:0`. The rewriter must:
//   - point the `subtitles=` filter at the staged temp file
//   - drop the second `-i` (filter reads from disk; stock ffmpeg has
//     no use for the input mapping after we replace the filter)
//   - strip `-map_inlineass`
//   - leave SubtitleExtract == nil (no extraction needed)
func TestRewriter_OverlayVAAPI_Sidecar(t *testing.T) {
	out := Rewrite(swArgsWithSubsRealSidecar, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsAnyWithPrefix(out.Changes, "filter:overlay-vaapi") {
		t.Fatalf("missing overlay-vaapi: %v", out.Changes)
	}
	if !containsString(out.Changes, "subtitle:sidecar-staged") {
		t.Fatalf("missing subtitle:sidecar-staged: %v", out.Changes)
	}
	if !containsString(out.Changes, "drop:-map_inlineass") {
		t.Fatalf("missing drop:-map_inlineass: %v", out.Changes)
	}
	if !containsString(out.Changes, "drop:-i(sidecar-input)") {
		t.Fatalf("expected the second -i to be dropped: %v", out.Changes)
	}
	if containsString(out.Args, "-map_inlineass") {
		t.Fatal("-map_inlineass not dropped")
	}
	if out.SubtitleExtract != nil {
		t.Fatalf("sidecar path should NOT request extraction: %+v", out.SubtitleExtract)
	}
	// Only one -i should remain (the source mkv).
	iCount := 0
	for _, a := range out.Args {
		if a == "-i" {
			iCount++
		}
	}
	if iCount != 1 {
		t.Errorf("expected 1 remaining -i, got %d", iCount)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	// Native libass roundtrip at output resolution (the 1080split
	// experiment was reverted — slower on iHD/Arc at every res).
	for _, must := range []string{
		"[0:0]hwupload[10]",
		"[11]hwdownload[12]",
		"subtitles=filename='/transcode/Transcode/Sessions/plex-transcode-q5orqh9o-c7edac0f/temp-0.srt'",
		"fontsdir=/usr/share/fonts/truetype/dejavu",
		"[14]hwupload[15]",
	} {
		if !strings.Contains(f, must) {
			t.Errorf("filter missing %q\n%s", must, f)
		}
	}
}

// Embedded burn-in. PMS uses `-map_inlineass 0:3` against the source's
// own subtitle stream, no second `-i`. Stock `subtitles=` can't read
// by stream index, so the rewriter must request that the agent extract
// the stream to a file before spawning the main encoder.
func TestRewriter_OverlayVAAPI_EmbeddedExtract(t *testing.T) {
	out := Rewrite(swArgsWithSubsSidecar, nil, &RewriteOpts{SessionDir: "/transcode/Transcode/Sessions/test-sid-job"})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsAnyWithPrefix(out.Changes, "filter:overlay-vaapi") {
		t.Fatalf("missing overlay-vaapi: %v", out.Changes)
	}
	if !containsString(out.Changes, "subtitle:embedded-extract:0:3") {
		t.Fatalf("expected embedded-extract change with stream 0:3: %v", out.Changes)
	}
	if !containsString(out.Changes, "drop:-map_inlineass") {
		t.Fatalf("missing drop:-map_inlineass: %v", out.Changes)
	}
	if out.SubtitleExtract == nil {
		t.Fatal("expected SubtitleExtract to be populated for embedded sub")
	}
	want := SubtitleExtract{
		SourceFile: "/media/Movies/Superman (2025)/Superman (2025).mkv",
		StreamSpec: "0:3",
		OutputFile: "/transcode/Transcode/Sessions/test-sid-job/scaleplex-extract.srt",
		Format:     "srt",
	}
	if *out.SubtitleExtract != want {
		t.Errorf("SubtitleExtract = %+v\nwant %+v", *out.SubtitleExtract, want)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	if !strings.Contains(f, "subtitles=filename='"+want.OutputFile+"'") {
		t.Errorf("filter must point at extraction target:\n%s", f)
	}
}

// SessionDir empty (test/local case) → fall back to a deterministic
// /tmp path so the agent still has a place to extract to.
func TestRewriter_OverlayVAAPI_EmbeddedExtract_DefaultDir(t *testing.T) {
	out := Rewrite(swArgsWithSubsSidecar, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if out.SubtitleExtract == nil {
		t.Fatal("expected SubtitleExtract")
	}
	if out.SubtitleExtract.OutputFile != "/tmp/scaleplex/scaleplex-extract.srt" {
		t.Errorf("OutputFile = %q want /tmp/scaleplex/scaleplex-extract.srt",
			out.SubtitleExtract.OutputFile)
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

// `-skip_to_segment N` must be captured into RewriteResult.SkipToSegment
// and stripped from the argv. The chunk-renumber watcher uses N as the
// starting sequence so chunk-stream0-NNNNN.m4s names align with PMS's
// expected URL `.../0/(N-1).m4s`. PTS-handling flags
// (-copyts/-start_at_zero/-avoid_negative_ts disabled) MUST be left
// alone — stripping them rebased the AAC encoder's PTS to 0 with no
// primer samples and produced empty (199-byte) first audio segments
// after every seek, which DASH players hang on indefinitely.
func TestRewriter_SkipToSegment_Seek(t *testing.T) {
	args := append([]string(nil), swArgsAV1H264...)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-skip_to_segment" {
			args[i+1] = "522"
		}
	}
	args = append([]string{"-ss", "1563"}, args...)

	out := Rewrite(args, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if out.SkipToSegment != 522 {
		t.Fatalf("SkipToSegment=%d want 522", out.SkipToSegment)
	}
	if containsString(out.Args, "-skip_to_segment") {
		t.Fatal("-skip_to_segment must be stripped from argv")
	}
	// On seek, capture the seek offset for the renumber watcher's tfdt
	// patch (stock dashenc writes tfdt=0 regardless of -ss/-copyts).
	// PTS flags survive — they prime the AAC encoder.
	if out.SeekOffsetSeconds != 1563 {
		t.Errorf("SeekOffsetSeconds=%v want 1563", out.SeekOffsetSeconds)
	}
	for _, mustKeep := range []string{"-copyts", "-start_at_zero", "-avoid_negative_ts"} {
		if !containsString(out.Args, mustKeep) {
			t.Errorf("must keep %s on seek", mustKeep)
		}
	}
}

// Initial-play session (`-skip_to_segment 1`, no `-ss`) captures
// SkipToSegment=1 and renumber starts at 1 (no-op rename since names
// match). PTS flags pass through untouched. No -ss → no
// -output_ts_offset injection.
func TestRewriter_SkipToSegment_InitialPlayCaptured(t *testing.T) {
	out := Rewrite(swArgsAV1H264, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if out.SkipToSegment != 1 {
		t.Fatalf("SkipToSegment=%d want 1", out.SkipToSegment)
	}
	if containsString(out.Args, "-skip_to_segment") {
		t.Fatal("-skip_to_segment must be stripped from argv")
	}
	if containsString(out.Args, "-output_ts_offset") {
		t.Errorf("must NOT inject -output_ts_offset on initial play (no -ss)")
	}
}

// On seek, `-force_key_frames "expr:gte(t,n_forced*N)"` must be rewritten
// to subtract the seek offset, otherwise stock ffmpeg evaluates the expr
// against absolute source time `t` (kept by -copyts) and fires hundreds of
// keyframes back-to-back at the start, then waits ~N seconds before the
// next one. The HLS segment muxer needs a keyframe to close a segment, so
// the first segment swallows tens of minutes of content (observed:
// media-00293.ts hit 317 MB / 39 min on Balls Up seek before splitting).
func TestRewriter_ForceKeyFrames_OffsetBySeek(t *testing.T) {
	args := append([]string{"-ss", "2344"}, swArgsAV1H264...)
	out := Rewrite(args, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "force_key_frames:offset-by-seek") {
		t.Fatalf("expected force_key_frames:offset-by-seek, got %v", out.Changes)
	}
	idx := indexOfArg(out.Args, "-force_key_frames:0", 0)
	if idx < 0 || idx+1 >= len(out.Args) {
		t.Fatal("missing -force_key_frames:0")
	}
	want := "expr:gte(t-2344.000,n_forced*3)"
	if out.Args[idx+1] != want {
		t.Errorf("force_key_frames expr=%q want %q", out.Args[idx+1], want)
	}
}

// HLS argv must have `-copyts` stripped. Verified locally that stock
// ffmpeg's segment muxer with `-ss <off> -copyts` never splits — it
// writes one giant first segment containing the entire remaining runtime
// (Balls Up: 222 MB / 23 min in media-00173.ts). DASH path keeps
// `-copyts` because dashenc handles it correctly.
func TestRewriter_HLS_CopytsStripped(t *testing.T) {
	args := []string{
		"-codec:0", "libdav1d",
		"-i", "/media/Movies/M.mkv",
		"-ss", "1384",
		"-copyts", "-start_at_zero", "-avoid_negative_ts", "disabled",
		"-filter_complex", "[0:0]scale=w=1022:h=426:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264",
		"-crf:0", "23",
		"-preset:0", "veryfast",
		"-force_key_frames:0", "expr:gte(t,n_forced*8)",
		"-segment_format", "matroska",
		"-f", "ssegment",
		"-individual_header_trailer", "0",
		"-segment_header_filename", "header",
		"-segment_time", "8",
		"-segment_start_number", "173",
		"media-%05d.ts",
	}
	out := Rewrite(args, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Args, "-copyts") {
		t.Errorf("HLS argv must NOT contain -copyts (segment muxer can't split with it)")
	}
	if !containsString(out.Changes, "hls:drop:-copyts") {
		t.Errorf("expected hls:drop:-copyts in changes, got %v", out.Changes)
	}
	// AAC priming flag must survive — removing it caused 199-byte empty
	// audio chunks on DASH and we don't want the same on HLS.
	if !containsString(out.Args, "-start_at_zero") {
		t.Errorf("HLS must keep -start_at_zero for AAC encoder priming")
	}
}

// HLS+seek must leave the force_key_frames expr untouched. Encoder
// time `t` runs in OUTPUT time on HLS (we strip `-copyts`) — Plex's
// `gte(t, n*8)` is already correct, fires keyframes at output 0,
// 8, 16. If we rewrite to `gte(t-<seek>, n*8)` the expr is always
// false (output t < seek_offset), no keyframes fire, segment muxer
// hangs forever (live regression 2026-05-08, Plex Android force-burn
// + seek to 3345s, zero segments produced in 2 min).
func TestRewriter_HLS_Seek_LeavesForceKeyFramesUntouched(t *testing.T) {
	args := []string{
		"-codec:0", "libdav1d",
		"-i", "/media/Movies/M.mkv",
		"-ss", "1384",
		"-copyts", "-start_at_zero", "-avoid_negative_ts", "disabled",
		"-filter_complex", "[0:0]scale=w=1022:h=426:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264",
		"-crf:0", "23",
		"-preset:0", "veryfast",
		"-force_key_frames:0", "expr:gte(t,n_forced*8)",
		"-segment_format", "matroska",
		"-f", "ssegment",
		"-individual_header_trailer", "0",
		"-segment_header_filename", "header",
		"-segment_time", "8",
		"-segment_start_number", "173",
		"media-%05d.ts",
	}
	out := Rewrite(args, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Changes, "force_key_frames:offset-by-seek") {
		t.Fatal("HLS+seek must NOT rewrite force_key_frames (encoder t already runs in output time)")
	}
	idx := indexOfArg(out.Args, "-force_key_frames:0", 0)
	if idx < 0 || idx+1 >= len(out.Args) {
		t.Fatal("missing -force_key_frames:0")
	}
	if out.Args[idx+1] != "expr:gte(t,n_forced*8)" {
		t.Errorf("force_key_frames expr=%q want unchanged", out.Args[idx+1])
	}
}

// Initial play (no -ss) must leave the force_key_frames expr untouched.
func TestRewriter_ForceKeyFrames_NoSeekUnchanged(t *testing.T) {
	out := Rewrite(swArgsAV1H264, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Changes, "force_key_frames:offset-by-seek") {
		t.Fatal("must NOT rewrite force_key_frames without -ss")
	}
	idx := indexOfArg(out.Args, "-force_key_frames:0", 0)
	if idx < 0 || idx+1 >= len(out.Args) {
		t.Fatal("missing -force_key_frames:0")
	}
	if out.Args[idx+1] != "expr:gte(t,n_forced*3)" {
		t.Errorf("force_key_frames expr=%q want unchanged", out.Args[idx+1])
	}
}

// Sonarr/Radarr name `<base>.en.hi.srt` for hearing-impaired tracks,
// `.en.cc.srt` for closed-caption, etc. The probe must find these
// (Balls Up sidecar 2026-05-06: `.en.hi.srt` was the only English
// track and the original probe missed it, falling through to the
// hybrid bail and breaking LG WebOS sub-burn).
func TestFindSidecarSubtitle_HearingImpaired(t *testing.T) {
	media := "/media/Movies/Balls Up/Balls Up.mkv"
	want := "/media/Movies/Balls Up/Balls Up.en.hi.srt"
	fs := func(p string) bool { return p == want }
	got := findSidecarSubtitle(media, "en", fs)
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

func TestFindSidecarSubtitle_SDH(t *testing.T) {
	media := "/media/Movies/X/X.mkv"
	want := "/media/Movies/X/X.en.sdh.srt"
	fs := func(p string) bool { return p == want }
	if got := findSidecarSubtitle(media, "en", fs); got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

func TestFindSidecarSubtitle_AltOrdering(t *testing.T) {
	// Some renamers emit `<base>.<flag>.<lang>.srt` rather than
	// `<base>.<lang>.<flag>.srt`. Probe both.
	media := "/media/Movies/X/X.mkv"
	want := "/media/Movies/X/X.forced.en.srt"
	fs := func(p string) bool { return p == want }
	if got := findSidecarSubtitle(media, "en", fs); got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

// Plain `<base>.<lang>.srt` (the historical case) must still work.
func TestFindSidecarSubtitle_BasicLang(t *testing.T) {
	media := "/media/Movies/X/X.mkv"
	want := "/media/Movies/X/X.en.srt"
	fs := func(p string) bool { return p == want }
	if got := findSidecarSubtitle(media, "en", fs); got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

// Bitmap embedded burn-in (PGS in a Blu-ray remux). Filter graph must:
//   - reference the subtitle stream by its specifier ([0:3])
//   - convert PGS bitmap → bgra (libavcodec renders the bitmap)
//   - hwupload the rendered surface to GPU
//   - overlay_vaapi composite onto the scaled main video
// And: NO extraction (the stream stays in -i 0).
func TestRewriter_OverlayVAAPI_BitmapEmbedded_PGS(t *testing.T) {
	probe := func(source, spec string) string {
		// Stand-in for ffprobe; report PGS for stream 0:3.
		return "hdmv_pgs_subtitle"
	}
	out := Rewrite(swArgsWithSubsSidecar, nil, &RewriteOpts{
		ProbeSubtitleCodec: probe,
		SessionDir:         "/transcode/Transcode/Sessions/test-sid",
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "subtitle:bitmap:0:3(hdmv_pgs_subtitle)") {
		t.Fatalf("expected subtitle:bitmap label: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:overlay-vaapi-bitmap") {
		t.Fatalf("expected overlay-vaapi-bitmap mode: %v", out.Changes)
	}
	if out.SubtitleExtract != nil {
		t.Errorf("bitmap path must NOT request extraction: %+v", out.SubtitleExtract)
	}
	if !containsString(out.Changes, "drop:-map_inlineass") {
		t.Errorf("missing drop:-map_inlineass: %v", out.Changes)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	for _, must := range []string{
		"[0:0]hwupload[10]",
		"[10]scale_vaapi=w=3840:h=2160:format=nv12[11]",
		"[0:3]format=bgra[12]",
		"[12]hwupload[13]",
		"[11][13]overlay_vaapi=eof_action=pass:repeatlast=1[15]",
	} {
		if !strings.Contains(f, must) {
			t.Errorf("filter missing %q\n%s", must, f)
		}
	}
	if strings.Contains(f, "subtitles=filename=") {
		t.Errorf("bitmap path must NOT use libass `subtitles=`:\n%s", f)
	}
}

// Bitmap sidecar (.sup file as second -i). Rare but possible. The
// rewriter must KEEP the second -i because the overlay_vaapi filter
// pulls the stream from it via [1:s:0].
func TestRewriter_OverlayVAAPI_BitmapSidecar_KeepsInput(t *testing.T) {
	probe := func(source, spec string) string {
		return "hdmv_pgs_subtitle"
	}
	out := Rewrite(swArgsWithSubsRealSidecar, nil, &RewriteOpts{
		ProbeSubtitleCodec: probe,
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:overlay-vaapi-bitmap") {
		t.Fatalf("expected overlay-vaapi-bitmap mode: %v", out.Changes)
	}
	if containsString(out.Changes, "drop:-i(sidecar-input)") {
		t.Fatal("bitmap-sidecar must NOT drop second -i (filter consumes the stream)")
	}
	iCount := 0
	for _, a := range out.Args {
		if a == "-i" {
			iCount++
		}
	}
	if iCount != 2 {
		t.Errorf("expected both -i to remain, got %d", iCount)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	if !strings.Contains(f, "[1:s:0]format=bgra") {
		t.Errorf("filter must reference the sidecar stream:\n%s", f)
	}
}

// subtitleKind classifies common ffprobe codec_name values.
func TestSubtitleKind(t *testing.T) {
	tests := map[string]string{
		"subrip":             "text",
		"ass":                "text",
		"ssa":                "text",
		"mov_text":           "text",
		"webvtt":             "text",
		"hdmv_text_subtitle": "text",
		"hdmv_pgs_subtitle":  "bitmap",
		"pgssub":             "bitmap",
		"dvb_subtitle":       "bitmap",
		"dvd_subtitle":       "bitmap",
		"xsub":               "bitmap",
		"unknown_codec_xxx":  "unknown",
		"":                   "unknown",
	}
	for codec, want := range tests {
		if got := subtitleKind(codec); got != want {
			t.Errorf("subtitleKind(%q) = %q want %q", codec, got, want)
		}
	}
}

// HDR source + SDR-target argv (the "plain" filter pattern, which
// Plex used to autoinject tonemap on its bundled musl ffmpeg). With
// stock ffmpeg we have to inject tonemap_vaapi explicitly or HDR
// values render with washed colors on every SDR client.
func TestRewriter_HDRSource_PlainTarget_InjectsTonemap(t *testing.T) {
	probe := func(source string) (transfer, primaries, space string) {
		return "smpte2084", "bt2020", "bt2020nc"
	}
	out := Rewrite(swArgsAV1H264, nil, &RewriteOpts{ProbeVideoColor: probe})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "video:hdr-source(smpte2084)") {
		t.Fatalf("expected hdr-source label: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:hdr-tonemap-vaapi-implicit") {
		t.Fatalf("expected hdr-tonemap-vaapi-implicit mode: %v", out.Changes)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	if !strings.Contains(f, "tonemap_vaapi=transfer=bt709:format=nv12") {
		t.Errorf("filter must include tonemap_vaapi:\n%s", f)
	}
	if !strings.Contains(f, "scale_vaapi=w=2276:h=1280:format=p010") {
		t.Errorf("scale_vaapi must use p010 input format for tonemap:\n%s", f)
	}
}

// HLG (ARIB STD-B67) is the other HDR transfer; same path.
func TestRewriter_HDRSource_HLG(t *testing.T) {
	probe := func(string) (string, string, string) {
		return "arib-std-b67", "bt2020", "bt2020nc"
	}
	out := Rewrite(swArgsAV1H264, nil, &RewriteOpts{ProbeVideoColor: probe})
	if !containsString(out.Changes, "filter:hdr-tonemap-vaapi-implicit") {
		t.Fatalf("HLG should trigger implicit tonemap: %v", out.Changes)
	}
}

// SDR sources go through the plain (non-tonemap) path unchanged.
func TestRewriter_SDRSource_NoTonemap(t *testing.T) {
	probe := func(string) (string, string, string) {
		return "bt709", "bt709", "bt709"
	}
	out := Rewrite(swArgsAV1H264, nil, &RewriteOpts{ProbeVideoColor: probe})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Changes, "filter:hdr-tonemap-vaapi-implicit") {
		t.Fatal("SDR source must not trigger tonemap")
	}
	if !containsString(out.Changes, "filter:plain") {
		t.Fatalf("expected plain filter mode for SDR: %v", out.Changes)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	if strings.Contains(out.Args[idx], "tonemap_vaapi") {
		t.Errorf("SDR source must not get tonemap_vaapi:\n%s", out.Args[idx])
	}
}

// No probe wired (test path / capability gap) → assume SDR. Default
// behaviour stays as before tonemap injection landed.
func TestRewriter_NoColorProbe_AssumesSDR(t *testing.T) {
	out := Rewrite(swArgsAV1H264, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Changes, "video:hdr-source") {
		t.Fatal("without probe, must not claim HDR source")
	}
	if !containsString(out.Changes, "filter:plain") {
		t.Fatalf("expected plain filter mode: %v", out.Changes)
	}
}

// PMS HW-decode argv pattern, captured live 2026-05-08 from
// `clusterplex-worker-qfltf` with HardwareAcceleratedCodecs=1 +
// TranscoderHEVCEncodingMode=always + Plex for Android client on
// The Accountant (AV1 4K HDR10+ source). Plex's HW probe succeeded
// in PMS so the argv is already VAAPI-shaped: short codec name,
// hwaccel flags, scale_vaapi filter chain, hevc_vaapi encoder with
// -qp:0 directly. Worker only needs to strip Plex-fork-only flags
// and translate the HLS muxer (-f ssegment, -copyts, segment_list
// URL).
var hwDecodeArgsAV1HEVC = []string{
	"-codec:0", "av1",
	"-hwaccel:0", "vaapi",
	"-hwaccel_output_format:0", "vaapi",
	"-hwaccel_device:0", "vaapi",
	"-codec:1", "eac3_eae",
	"-eae_prefix:1", "75ca5833-5cc4-42de-87c9-a730f93350f5_",
	"-analyzeduration", "20000000",
	"-probesize", "20000000",
	"-i", "/media/Movies/The Accountant.mkv",
	"-start_at_zero",
	"-copyts",
	"-fps_mode", "cfr",
	"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
	"-filter_hw_device", "vaapi",
	"-y",
	"-nostats",
	"-loglevel", "quiet",
	"-loglevel_plex", "error",
	"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/sid/job/progress",
	"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1];[1]hwupload[2]",
	"-map", "[2]",
	"-metadata:s:0", "language=eng",
	"-codec:0", "hevc_vaapi",
	"-qp:0", "15",
	"-maxrate:0", "20121k",
	"-bufsize:0", "40242k",
	"-r:0", "23.975999999999999",
	"-sei:0", "-a53_cc",
	"-force_key_frames:0", "expr:gte(t,n_forced*1)",
	"-filter_complex", "[0:1] aresample=async=1:ochl='5.1':rematrix_maxval=0.000000dB:osr=48000[3]",
	"-map", "[3]",
	"-metadata:s:1", "language=eng",
	"-codec:1", "aac",
	"-b:1", "774k",
	"-segment_format", "matroska",
	"-f", "ssegment",
	"-individual_header_trailer", "0",
	"-flags", "+global_header",
	"-segment_header_filename", "header",
	"-segment_time", "1",
	"-segment_start_number", "0",
	"-segment_time_delta", "0.0625",
	"-segment_list", "http://127.0.0.1:32400/video/:/transcode/session/sid/job/manifest?X-Plex-Http-Pipeline=infinite",
	"-segment_list_type", "csv",
	"-segment_list_size", "5",
	"-segment_list_separate_stream_times", "1",
	"-segment_list_unfinished", "1",
	"-segment_format_options", "output_ts_offset=10",
	"-max_delay", "5000000",
	"-avoid_negative_ts", "disabled",
	"-map_metadata", "-1",
	"-map_chapters", "-1",
	"media-%05d.ts",
}

func TestRewriter_HWDecode_PassthroughWithPlexQuirkStrips(t *testing.T) {
	env := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay.svc:32499",
		"X_PLEX_TOKEN":           "tok123",
	}
	out := Rewrite(hwDecodeArgsAV1HEVC, env, nil)
	if !out.Applied {
		t.Fatalf("rewriter NOT applied; changes=%v", out.Changes)
	}

	mustContain := []string{
		"decode:hw-passthrough:av1",
		"encode:hw-passthrough:hevc_vaapi",
		"audio:eac3_eae->eac3",
		"drop:-eae_prefix:1",
		"drop:-loglevel_plex",
		"drop:-segment_list_separate_stream_times",
		"drop:-segment_list_unfinished",
		"hls:f=ssegment->segment",
		"hls:drop:-copyts",
		"hls:segment_list:rewrite-to-relay",
		"progressurl:captured-for-reporter",
		"progress:append-X-Plex-Token",
		"loglevel:->info",
		"drop:-nostats",
		"env:LIBVA",
	}
	for _, want := range mustContain {
		if !containsString(out.Changes, want) {
			t.Errorf("missing change %q; got %v", want, out.Changes)
		}
	}

	mustNotContain := []string{
		"encode:libx265->hevc_vaapi", // never claim a swap; PMS gave it to us
		"encode:libx264->h264_vaapi",
		"filter:plain",
		"filter:hdr-tonemap-vaapi",
	}
	for _, bad := range mustNotContain {
		if containsString(out.Changes, bad) {
			t.Errorf("unexpected change %q in HW-decode mode; got %v", bad, out.Changes)
		}
	}

	// Encoder argv unchanged (still hevc_vaapi)
	newInputIdx := indexOfArg(out.Args, "-i", 0)
	encIdx := indexOfArg(out.Args, "-codec:0", newInputIdx+1)
	if encIdx < 0 || out.Args[encIdx+1] != "hevc_vaapi" {
		t.Fatalf("encoder must remain hevc_vaapi; got args near %d: %v", encIdx, out.Args[encIdx:min(encIdx+4, len(out.Args))])
	}

	// HLS: -f ssegment must be translated to -f segment
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-f" && out.Args[i+1] == "ssegment" {
			t.Fatalf("-f ssegment not translated")
		}
	}
	// -copyts must be gone (HLS path)
	if indexOfArg(out.Args, "-copyts", 0) >= 0 {
		t.Fatalf("-copyts not stripped from HLS argv")
	}
	// -loglevel_plex must be gone
	if indexOfArg(out.Args, "-loglevel_plex", 0) >= 0 {
		t.Fatalf("-loglevel_plex not stripped")
	}
	// -progressurl must be gone (captured into RewriteResult instead)
	if indexOfArg(out.Args, "-progressurl", 0) >= 0 {
		t.Fatalf("-progressurl not stripped from argv")
	}
	if out.ProgressURL == "" {
		t.Fatalf("ProgressURL not captured")
	}
	if !strings.Contains(out.ProgressURL, "relay.svc:32499") {
		t.Fatalf("ProgressURL not rewritten to relay base: %q", out.ProgressURL)
	}
	if !strings.Contains(out.ProgressURL, "X-Plex-Token=tok123") {
		t.Fatalf("ProgressURL missing token: %q", out.ProgressURL)
	}
}

// PMS HW-decode + force-burn subtitle. Captured live from
// clusterplex-worker-5v2zj 2026-05-08 18:10Z, Plex Android playing
// The Accountant with a forced English SDH track and the Plex pref
// "Burn Subtitles: Always" set. PMS extracts the SRT to
// /transcode/<session>/temp-0.srt and adds it as a second -i + a
// `-map_inlineass 1:s:0` directive; the filter graph hwdownloads
// from VAAPI to CPU for libass, then hwuploads back. The rewriter
// must swap inlineass→subtitles=, drop the sidecar input + the
// inlineass map flag + Plex's trailing null sub output.
var hwDecodeArgsAV1HEVCSubBurn = []string{
	"-codec:0", "av1",
	"-hwaccel:0", "vaapi",
	"-hwaccel_output_format:0", "vaapi",
	"-hwaccel_device:0", "vaapi",
	"-codec:1", "eac3_eae",
	"-eae_prefix:1", "bb137193_",
	"-analyzeduration", "20000000",
	"-probesize", "20000000",
	"-i", "/media/Movies/The Accountant.mkv",
	"-analyzeduration", "20000000",
	"-probesize", "20000000",
	"-i", "/transcode/sess/temp-0.srt",
	"-start_at_zero",
	"-copyts",
	"-fps_mode", "cfr",
	"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
	"-filter_hw_device", "vaapi",
	"-y",
	"-nostats",
	"-loglevel", "quiet",
	"-loglevel_plex", "error",
	"-progressurl", "http://127.0.0.1:32400/sess/job/progress",
	"-map_inlineass", "1:s:0",
	"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1024:h=576:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:font_size=54[3];[3]hwupload[4]",
	"-map", "[4]",
	"-metadata:s:0", "language=eng",
	"-codec:0", "hevc_vaapi",
	"-qp:0", "24",
	"-maxrate:0", "1541k",
	"-bufsize:0", "3082k",
	"-r:0", "23.975",
	"-sei:0", "-a53_cc",
	"-force_key_frames:0", "expr:gte(t,n_forced*5)",
	"-filter_complex", "[0:1] aresample=async=1:ochl='5.1':rematrix_maxval=0.000000dB:osr=48000[5]",
	"-map", "[5]",
	"-metadata:s:1", "language=eng",
	"-codec:1", "aac",
	"-b:1", "351k",
	"-segment_format", "matroska",
	"-f", "ssegment",
	"-individual_header_trailer", "0",
	"-flags", "+global_header",
	"-segment_header_filename", "header",
	"-segment_time", "5",
	"-segment_start_number", "0",
	"-segment_time_delta", "0.0625",
	"-segment_list", "http://127.0.0.1:32400/sess/job/manifest?X-Plex-Http-Pipeline=infinite",
	"-segment_list_type", "csv",
	"-segment_list_size", "5",
	"-segment_list_separate_stream_times", "1",
	"-segment_list_unfinished", "1",
	"-segment_format_options", "output_ts_offset=10",
	"-max_delay", "5000000",
	"-avoid_negative_ts", "disabled",
	"-map_metadata", "-1",
	"-map_chapters", "-1",
	"media-%05d.ts",
	"-map", "1:s:0",
	"-f", "null",
	"-codec", "ass",
	"nullfile",
}

func TestRewriter_HWDecode_SubtitleBurnIn(t *testing.T) {
	probe := func(_, _ string) string { return "subrip" }
	out := Rewrite(hwDecodeArgsAV1HEVCSubBurn, map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay.svc:32499",
		"X_PLEX_TOKEN":           "tok123",
	}, &RewriteOpts{
		SessionDir:         "/transcode/sess",
		ProbeSubtitleCodec: probe,
	})
	if !out.Applied {
		t.Fatalf("rewriter NOT applied; changes=%v", out.Changes)
	}

	mustContain := []string{
		"decode:hw-passthrough:av1",
		"encode:hw-passthrough:hevc_vaapi",
		"hw-decode:filter:inlineass->subtitles",
		"subtitle:sidecar-staged",
		"sidecar:/transcode/sess/temp-0.srt",
		"drop:-i(sidecar-input)",
		"drop:-map_inlineass",
		"drop:null-sub-output",
		"audio:eac3_eae->eac3",
		"hls:f=ssegment->segment",
	}
	for _, want := range mustContain {
		if !containsString(out.Changes, want) {
			t.Errorf("missing change %q; got %v", want, out.Changes)
		}
	}

	// Filter chain must mention subtitles= and not inlineass=.
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-filter_complex" && strings.HasPrefix(out.Args[i+1], "[0:0]") {
			f := out.Args[i+1]
			if strings.Contains(f, "inlineass=") {
				t.Fatalf("filter still contains inlineass=: %q", f)
			}
			if !strings.Contains(f, "subtitles=filename='/transcode/sess/temp-0.srt'") {
				t.Fatalf("filter missing subtitles= for staged SRT: %q", f)
			}
			// Output label must remain [4] so the existing -map [4] works
			if !strings.HasSuffix(f, "[3]hwupload[4]") {
				t.Fatalf("filter chain must end at [4]: %q", f)
			}
			break
		}
	}

	// Only one -i should remain (source mkv); sidecar -i dropped
	iCount := 0
	for i := 0; i < len(out.Args); i++ {
		if out.Args[i] == "-i" {
			iCount++
		}
	}
	if iCount != 1 {
		t.Fatalf("want 1 remaining -i, got %d", iCount)
	}

	// `-map_inlineass` must be gone
	if indexOfArg(out.Args, "-map_inlineass", 0) >= 0 {
		t.Fatalf("-map_inlineass not stripped")
	}

	// Null sub output (`-map 1:s:0 -f null -codec ass nullfile`) gone
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-f" && out.Args[i+1] == "null" {
			t.Fatalf("-f null (null sub output) not stripped: %v", out.Args[i:])
		}
	}
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-codec" && out.Args[i+1] == "ass" {
			t.Fatalf("-codec ass (null sub output tail) not stripped")
		}
	}

	// `-map [4]` for the video must still be there (output label
	// preserved).
	foundMap4 := false
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-map" && out.Args[i+1] == "[4]" {
			foundMap4 = true
			break
		}
	}
	if !foundMap4 {
		t.Fatalf("video -map [4] missing after rewrite")
	}
}

// PMS HW-decode + force-burn + SEEK. Captured live 2026-05-08 from
// clusterplex-worker-sjsp4 session 6783, Plex Android, The Accountant
// AV1 4K HDR10+, Original Quality, seek to 1816s. PMS places `-ss 1816`
// before BOTH inputs (source mkv AND staged SRT) so the SRT input
// seeks to match. The rewriter must drop the whole input-1 option
// block when dropping the SRT `-i`; leaving the dangling `-ss 1816`
// makes ffmpeg interpret it as positional output seek and discard
// every encoded frame whose PTS < 1816 (output PTS starts at 0 with
// -copyts stripped on HLS, never reaches 1816), the segment muxer
// waits indefinitely, Plex Android shows "Connection error".
func TestRewriter_HWDecode_SubBurn_SeekDropsSecondInputSs(t *testing.T) {
	probe := func(_, _ string) string { return "subrip" }
	args := []string{
		"-codec:0", "av1",
		"-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi",
		"-hwaccel_device:0", "vaapi",
		"-codec:1", "eac3_eae",
		"-eae_prefix:1", "tok_",
		"-ss", "1816",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/media/Movies/The Accountant.mkv",
		"-ss", "1816",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/transcode/sess/temp-0.srt",
		"-start_at_zero",
		"-copyts",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_hw_device", "vaapi",
		"-y", "-nostats", "-loglevel", "quiet",
		"-loglevel_plex", "error",
		"-progressurl", "http://127.0.0.1:32400/sess/job/progress",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi", "-qp:0", "15",
		"-maxrate:0", "20121k", "-bufsize:0", "40242k",
		"-r:0", "23.975",
		"-sei:0", "-a53_cc",
		"-force_key_frames:0", "expr:gte(t,n_forced*1)",
		"-filter_complex", "[0:1] aresample=async=1:ochl='5.1':osr=48000[5]",
		"-map", "[5]",
		"-codec:1", "aac", "-b:1", "774k",
		"-segment_format", "matroska", "-f", "ssegment",
		"-individual_header_trailer", "0",
		"-segment_header_filename", "header",
		"-segment_time", "1",
		"-segment_start_number", "1816",
		"-segment_time_delta", "0.0625",
		"-segment_list", "http://127.0.0.1:32400/sess/job/manifest?X-Plex-Http-Pipeline=infinite",
		"-segment_list_type", "csv",
		"-segment_list_size", "5",
		"-segment_list_separate_stream_times", "1",
		"-segment_list_unfinished", "1",
		"-segment_format_options", "output_ts_offset=10",
		"-max_delay", "5000000",
		"-avoid_negative_ts", "disabled",
		"-map_metadata", "-1",
		"-map_chapters", "-1",
		"media-%05d.ts",
		"-map", "1:s:0",
		"-f", "null",
		"-codec", "ass",
		"nullfile",
	}
	out := Rewrite(args, map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay.svc:32499",
		"X_PLEX_TOKEN":           "tok123",
	}, &RewriteOpts{
		SessionDir:         "/transcode/sess",
		ProbeSubtitleCodec: probe,
	})
	if !out.Applied {
		t.Fatalf("rewriter NOT applied; changes=%v", out.Changes)
	}

	// Count remaining `-ss` flags. The input-0 -ss must survive (real
	// input seek). The input-1 -ss must be GONE (would dangle as
	// output seek otherwise).
	ssCount := 0
	for i := 0; i < len(out.Args); i++ {
		if out.Args[i] == "-ss" {
			ssCount++
		}
	}
	if ssCount != 1 {
		t.Fatalf("want exactly 1 remaining -ss (the input-0 seek), got %d. args=%v", ssCount, out.Args)
	}

	// And the surviving -ss must be BEFORE the (only) -i — i.e. it's
	// an input option, not a positional output option.
	iIdx := indexOfArg(out.Args, "-i", 0)
	ssIdx := indexOfArg(out.Args, "-ss", 0)
	if ssIdx < 0 {
		t.Fatal("missing -ss after rewrite")
	}
	if ssIdx > iIdx {
		t.Fatalf("-ss landed AFTER -i (would be output seek); ssIdx=%d iIdx=%d", ssIdx, iIdx)
	}

	// Sanity: only one -i remains
	iCount := 0
	for i := 0; i < len(out.Args); i++ {
		if out.Args[i] == "-i" {
			iCount++
		}
	}
	if iCount != 1 {
		t.Fatalf("want 1 remaining -i (source), got %d", iCount)
	}

	// Seek-offset captured for diagnostics
	if !containsString(out.Changes, "seek-offset:captured=1816.000s") {
		t.Errorf("seek-offset not captured: %v", out.Changes)
	}
	// HLS-specific seek + force_key_frames untouched (Plex's expr
	// is correct on HLS without -copyts)
	if containsString(out.Changes, "force_key_frames:offset-by-seek") {
		t.Errorf("HLS+seek must NOT rewrite force_key_frames")
	}
}

// SW-decode + HDR + text-sidecar sub-burn. PMS argv places
// per-input options before each -i, so dropSidecarInput removes a
// chunk of args from BEFORE the filter_complex value, shifting its
// index downward. The map-label-update phase MUST run before that
// drop or it iterates from a stale vfIdx and silently misses the
// `-map [2]` it's supposed to rewrite. Live repro 2026-05-09 session
// 7347, Plex Android, HardwareAcceleratedCodecs=0, The Accountant
// AV1 4K HDR10+ + force-burn English SDH; ffmpeg failed exit 234
// with "Output with label '2' does not exist in any defined filter
// graph". Argv from that session preserved here as the regression
// fixture.
func TestRewriter_SWDecode_HDR_SubBurn_MapLabelUpdated(t *testing.T) {
	probe := func(_, _ string) string { return "subrip" }
	colorProbe := func(_ string) (string, string, string) { return "smpte2084", "bt2020", "bt2020nc" }
	args := []string{
		"-codec:0", "libdav1d",
		"-codec:1", "eac3_eae",
		"-eae_prefix:1", "tok_",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/media/Movies/The Accountant.mkv",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/transcode/sess/temp-0.srt",
		"-start_at_zero",
		"-copyts",
		"-fps_mode", "cfr",
		"-y", "-nostats", "-loglevel", "quiet",
		"-loglevel_plex", "error",
		"-progressurl", "http://127.0.0.1:32400/sess/job/progress",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]scale=w=3840:h=2160:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1];[1]inlineass=font_size=54[2]",
		"-map", "[2]",
		"-codec:0", "libx264",
		"-crf:0", "16",
		"-maxrate:0", "20121k",
		"-bufsize:0", "40242k",
		"-preset:0", "veryfast",
		"-x264opts:0", "subme=0:me_range=4",
		"-force_key_frames:0", "expr:gte(t,n_forced*1)",
		"-filter_complex", "[0:1] aresample=async=1:ochl='5.1':osr=48000[3]",
		"-map", "[3]",
		"-codec:1", "aac",
		"-b:1", "774k",
		"-segment_format", "matroska", "-f", "ssegment",
		"-individual_header_trailer", "0",
		"-segment_header_filename", "header",
		"-segment_time", "1",
		"-segment_start_number", "0",
		"-segment_list", "http://127.0.0.1:32400/sess/job/manifest",
		"-segment_list_type", "csv",
		"-segment_list_size", "5",
		"-segment_list_separate_stream_times", "1",
		"-segment_list_unfinished", "1",
		"-segment_format_options", "output_ts_offset=10",
		"-avoid_negative_ts", "disabled",
		"media-%05d.ts",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay.svc:32499",
		"X_PLEX_TOKEN":           "tok123",
	}, &RewriteOpts{
		SessionDir:         "/transcode/sess",
		ProbeSubtitleCodec: probe,
		ProbeVideoColor:    colorProbe,
	})
	if !out.Applied {
		t.Fatalf("rewriter NOT applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:overlay-vaapi-hdr") {
		t.Fatalf("expected filter:overlay-vaapi-hdr; got %v", out.Changes)
	}
	if !containsString(out.Changes, "map-label-update") {
		t.Fatalf("MAP NOT UPDATED — this is the bug. changes=%v", out.Changes)
	}
	// `-map [2]` (the OldLabel from filter rewrite) must be GONE;
	// in its place must be `-map [15]` (the NewLabel for overlay-vaapi).
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-map" && out.Args[i+1] == "[2]" {
			t.Fatalf("stale -map [2] survived rewrite at idx=%d", i)
		}
	}
	found15 := false
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-map" && out.Args[i+1] == "[15]" {
			found15 = true
			break
		}
	}
	if !found15 {
		t.Fatalf("expected -map [15] (NewLabel for overlay-vaapi); not found. args=%v", out.Args)
	}
	// Sidecar drop applied
	if !containsString(out.Changes, "drop:-i(sidecar-input)") {
		t.Errorf("expected drop:-i(sidecar-input); got %v", out.Changes)
	}
	// `-map_inlineass` Plex-only flag stripped
	if indexOfArg(out.Args, "-map_inlineass", 0) >= 0 {
		t.Errorf("-map_inlineass not stripped")
	}
}

// HW-decode without -hwaccel flag is treated as unknown SW decoder
// (we don't auto-detect from the codec name alone — Plex has been
// known to send `av1` as a SW probe arg too).
func TestRewriter_HWDecode_RequiresHWAccelFlag(t *testing.T) {
	args := []string{
		"-codec:0", "av1",
		"-i", "/media/m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=p010[1];[1]hwupload[2]",
		"-map", "[2]", "-codec:0", "hevc_vaapi", "-qp:0", "15",
	}
	out := Rewrite(args, nil, nil)
	if out.Applied {
		t.Fatal("must bail without -hwaccel:0 flag — short codec name alone isn't enough signal")
	}
	if !containsString(out.Changes, "skip:unknown-decoder:av1") {
		t.Fatalf("expected skip:unknown-decoder:av1; got %v", out.Changes)
	}
}

// isHDRTransfer classification.
func TestIsHDRTransfer(t *testing.T) {
	cases := map[string]bool{
		"smpte2084":    true,
		"smpte428":     true,
		"arib-std-b67": true,
		"SMPTE2084":    true, // case-insensitive
		"bt709":        false,
		"bt470bg":      false,
		"":             false,
		"unknown":      false,
	}
	for transfer, want := range cases {
		if got := isHDRTransfer(transfer); got != want {
			t.Errorf("isHDRTransfer(%q) = %v want %v", transfer, got, want)
		}
	}
}
