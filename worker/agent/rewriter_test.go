package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The homelab's all-GPU worker fleet runs SCALEPLEX_FORCE_HW=1
// (docs/HW_PROFILE.md): always re-accelerate, never honor a SW argv. Most
// tests here validate that reshape behaviour, so model the fleet default
// for the whole package. Honor-SW tests opt out with t.Setenv(...,"0").
func TestMain(m *testing.M) {
	os.Setenv("SCALEPLEX_FORCE_HW", "1")
	// Rewriter tests assume an active Plex Pass (they exercise the HW-accel
	// paths, not the gate). Stub the gate's PMS query so it never does real
	// HTTP; pass_gate_test.go overrides this per-test to exercise denial.
	passCheck = func(base, tok string) (bool, error) { return true, nil }
	os.Exit(m.Run())
}

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

// Honor-Plex-SW (docs/HW_PROFILE.md phase 1): with SCALEPLEX_FORCE_HW off,
// a fully-SW PMS argv (no -hwaccel + SW encoder) runs SW on the worker —
// no decoder swap, no VAAPI device, no encoder swap, no filter reshape —
// while the transport/audio scrubs still apply.
func TestRewriter_HonorSW_FullSoftwarePassthrough(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "0")
	out := Rewrite(swArgsAV1H264, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "honor:plex-sw") {
		t.Fatalf("expected honor:plex-sw tag, got %v", out.Changes)
	}
	// Decoder untouched (SW).
	dIdx := indexOfArg(out.Args, "-codec:0", 0)
	if out.Args[dIdx+1] != "libdav1d" {
		t.Errorf("decoder=%q want libdav1d (untouched)", out.Args[dIdx+1])
	}
	// No HW reshape.
	if containsString(out.Args, "-hwaccel:0") {
		t.Error("must not inject -hwaccel:0 when honoring SW")
	}
	if containsString(out.Args, "-init_hw_device") || containsString(out.Args, "-filter_hw_device") {
		t.Error("must strip/skip HW device init when honoring SW")
	}
	if containsString(out.Args, "scale_vaapi") || containsString(out.Args, "h264_vaapi") {
		t.Errorf("must not reshape to VAAPI when honoring SW: %v", out.Args)
	}
	// Encoder stays libx264.
	encIdx := indexOfArg(out.Args, "-codec:0", indexOfArg(out.Args, "-i", 0)+1)
	if out.Args[encIdx+1] != "libx264" {
		t.Errorf("encoder=%q want libx264 (untouched)", out.Args[encIdx+1])
	}
	// Transport/audio scrubs still apply.
	if !containsString(out.Changes, "audio:eac3_eae->eac3") {
		t.Errorf("audio scrub should still apply under honor-SW: %v", out.Changes)
	}
}

// With SCALEPLEX_FORCE_HW=1 the same SW argv is re-accelerated (the homelab
// fleet mode, and TestMain's package default).
func TestRewriter_HonorSW_ForceHWReaccelerates(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "1")
	out := Rewrite(swArgsAV1H264, nil, nil)
	if containsString(out.Changes, "honor:plex-sw") {
		t.Fatalf("FORCE_HW=1 must re-accelerate, not honor: %v", out.Changes)
	}
	if !containsString(out.Changes, "encode:libx264->h264_vaapi") {
		t.Errorf("expected encoder reshape under FORCE_HW: %v", out.Changes)
	}
}

// Honor only applies to fully-SW argv. A HW argv (PMS emitted -hwaccel +
// vaapi encoder) is never "honored as SW" even with FORCE_HW off — it takes
// the HW-passthrough path.
func TestRewriter_HonorSW_HWArgvNotHonored(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "0")
	args := []string{
		"-codec:0", "hevc", "-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/Movies/HEVCSource.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1];[1]hwupload[2]",
		"-map", "[2]", "-codec:0", "h264_vaapi", "-qp:0", "22",
	}
	out := Rewrite(args, nil, nil)
	if containsString(out.Changes, "honor:plex-sw") {
		t.Fatalf("HW argv must not be honored as SW: %v", out.Changes)
	}
	encIdx := indexOfArg(out.Args, "-codec:0", indexOfArg(out.Args, "-i", 0)+1)
	if out.Args[encIdx+1] != "h264_vaapi" {
		t.Errorf("HW encoder should pass through: %q", out.Args[encIdx+1])
	}
}

// Per-axis honor (docs/HW_PROFILE.md): HW-decode + SW-encode (PMS "HW accel
// on, encoding off") is honored under FORCE_HW=0 — keep HW decode + device +
// Plex's SW filtergraph (incl. inlineass keys, fork 0119), keep the SW
// encoder, no reshape, no bail. Live repro 2026-05-24 (Avatar, encode off).
func TestRewriter_HonorHybrid_HWDecodeSWEncode(t *testing.T) {
	t.Setenv("SCALEPLEX_FORCE_HW", "0")
	args := []string{
		"-codec:0", "av1", "-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/m.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-map_inlineass", "0:3",
		"-filter_complex", "[0:0]scale_vaapi=w=1920:h=1080:format=p010[1];[1]hwdownload,format=p010[2];" +
			"[2]inlineass=font_scale=1.0:language=en:overrides=foo:outline=2.6:shadow=1.7:font_size=54[3]",
		"-map", "[3]", "-codec:0", "libx264", "-crf:0", "16", "-preset:0", "veryfast",
		"-map", "0:3", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied || !containsString(out.Changes, "honor:plex-hwdec-swenc") {
		t.Fatalf("expected honor:plex-hwdec-swenc, got applied=%v changes=%v", out.Applied, out.Changes)
	}
	if !containsString(out.Args, "-hwaccel:0") {
		t.Error("HW decode (-hwaccel:0) must be kept under hybrid honor")
	}
	if !containsString(out.Args, "-init_hw_device") {
		t.Error("HW device must be kept (fork 0116 retargets it)")
	}
	encIdx := indexOfArg(out.Args, "-codec:0", indexOfArg(out.Args, "-i", 0)+1)
	if out.Args[encIdx+1] != "libx264" {
		t.Errorf("encoder must stay libx264 (hybrid SW encode), got %q", out.Args[encIdx+1])
	}
	if containsString(out.Args, "h264_vaapi") {
		t.Errorf("must not reshape encoder to vaapi under hybrid honor: %v", out.Args)
	}
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-filter_complex" && strings.Contains(out.Args[i+1], "inlineass=") {
			if !strings.Contains(out.Args[i+1], "overrides=") {
				t.Errorf("inlineass keys must pass through under hybrid honor: %s", out.Args[i+1])
			}
		}
	}
}

func TestRewriter_AV1H264_AppliedAndChanges(t *testing.T) {
	out := Rewrite(swArgsAV1H264, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("expected applied=true, changes=%v", out.Changes)
	}
	expectChanges := []string{
		"decode:libdav1d->av1",
		"filter:plain",
		"map-label-update",
		"encode:libx264->h264_vaapi",
		// -crf:0 / -preset:0 / -x264opts:0 are left untouched — the fork's
		// VAAPI encoder maps crf->qp (0117) and preset->compression_level
		// + swallows x264opts (0118), so no crf/preset/x264opts changes
		// here anymore.
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

// The rewriter no longer rewrites the device path Plex emitted — the
// scaleplex-ffmpeg fork retargets it from SCALEPLEX_RENDER_DEVICE at
// device-open (patch 0116). Plex's `-init_hw_device vaapi=vaapi:` is left
// verbatim; an empty path is filled by the fork's env override.
func TestRewriter_AV1H264_InitHwDeviceLeftForForkRetarget(t *testing.T) {
	out := Rewrite(swArgsAV1H264, nil, nil)
	i := indexOfArg(out.Args, "-init_hw_device", 0)
	if got := out.Args[i+1]; got != "vaapi=vaapi:" {
		t.Fatalf("init_hw_device=%q want untouched vaapi=vaapi:", got)
	}
}

func TestRewriter_AV1H264_FilterIsVaapiPlain(t *testing.T) {
	out := Rewrite(swArgsAV1H264, nil, nil)
	idx := findFilterComplex(out.Args, "[0:0]")
	// composeBurn: scale_vaapi output is already a VA surface for the encoder —
	// no trailing hwupload (the old emit re-uploaded a VA frame redundantly).
	want := "[0:0]hwupload[0];[0]scale_vaapi=w=2276:h=1280:format=nv12[1]"
	if out.Args[idx] != want {
		t.Fatalf("filter=%q want %q", out.Args[idx], want)
	}
}

func TestRewriter_AV1H264_MapLabelUpdated(t *testing.T) {
	out := Rewrite(swArgsAV1H264, nil, nil)
	idx := findFilterComplex(out.Args, "[0:0]")
	for i := idx + 1; i < len(out.Args); i++ {
		if out.Args[i] == "-map" {
			if out.Args[i+1] != "[1]" {
				t.Fatalf("map=%q want [1]", out.Args[i+1])
			}
			return
		}
	}
	t.Fatal("no -map after filter")
}

func TestRewriter_AV1H264_EncoderEtc(t *testing.T) {
	out := Rewrite(swArgsAV1H264, nil, nil)
	// -preset:0 and -x264opts:0 are left untouched — the fork's VAAPI
	// encoder maps the preset to compression_level and swallows the
	// x264opts blob (patch 0118). The rewriter no longer translates or
	// drops them, and never injects -compression_level:v.
	if !containsString(out.Args, "-preset:0") {
		t.Error("-preset:0 should survive verbatim (fork patch 0118 maps it)")
	}
	if !containsString(out.Args, "-x264opts:0") {
		t.Error("-x264opts:0 should survive verbatim (fork patch 0118 swallows it)")
	}
	if containsString(out.Args, "-compression_level:v") {
		t.Error("rewriter must not inject -compression_level:v (fork derives it from -preset)")
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
	// -crf:0 is left untouched: the fork's VAAPI encoder accepts it and
	// maps crf->qp internally (patch 0117). The rewriter no longer emits
	// -qp:0 on the SW->HW path.
	crfIdx := indexOfArg(out.Args, "-crf:0", 0)
	if crfIdx <= 0 || out.Args[crfIdx+1] != "16" {
		t.Fatalf("expected -crf:0 16 to survive verbatim, got args=%v", out.Args)
	}
	if containsString(out.Args, "-qp:0") {
		t.Errorf("rewriter must not convert -crf:0 to -qp:0 (fork patch 0117 maps it), got: %v", out.Args)
	}
}

// crf->qp translation (and its HW_QP_CRF_OFFSET / clamp knobs) moved into
// the fork's VAAPI encoder (patch 0117); the rewriter no longer touches
// -crf. Offset override is now the encoder's -crf_qp_offset argv option.

// Rewriter no longer converts CQP→VBR/CBR. scaleplex-ffmpeg7 patch 0105
// makes vaapi_encode auto-select QVBR (preserving PMS's quality target
// via -qp) when both -qp and -maxrate are present. PMS argv passes
// through unchanged; the encoder picks the right mode.
func TestRewriter_RC_PassthroughCQPandMaxrate(t *testing.T) {
	out := Rewrite(swArgsAV1H264, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Args, "-crf:0") {
		t.Errorf("expected -crf:0 to pass through to encoder (fork 0117 maps crf->qp, 0105 handles RC mode), got: %v", out.Args)
	}
	if !containsString(out.Args, "-maxrate:0") {
		t.Error("expected -maxrate:0 to pass through")
	}
	if containsString(out.Args, "-rc_mode") {
		t.Errorf("rewriter must not inject -rc_mode (fork picks QVBR)")
	}
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "rc:CQP(") || strings.HasPrefix(c, "rc:") {
			t.Errorf("unexpected rate-control rewrite tag (should have been retired with patch 0105): %q", c)
		}
	}
}

// Without -maxrate the fork encoder runs CQP off the crf-derived QP
// (patch 0105 only flips to QVBR when both -qp/-crf AND -maxrate are
// present). The rewriter leaves -crf:0 untouched either way.
func TestRewriter_RateControl_CRFOnly_PassesThrough(t *testing.T) {
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
	if !containsString(out.Args, "-crf:0") {
		t.Errorf("expected -crf:0 to survive, got %v", out.Args)
	}
	if containsString(out.Args, "-qp:0") {
		t.Errorf("rewriter must not synthesize -qp:0 (fork maps crf), got %v", out.Args)
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
	// Injected device path is empty; the fork fills it from
	// SCALEPLEX_RENDER_DEVICE (patch 0116) at device-open.
	want := []string{
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
	}
	for k, w := range want {
		if out.Args[i-4+k] != w {
			t.Fatalf("arg[%d]=%q want %q", i-4+k, out.Args[i-4+k], w)
		}
	}
}

// NVIDIA sister test of TestRewriter_InitHwDevice_Inject — same input
// args, activeDialect swapped to nvencDialect, expects cuda=cuda:0
// and -filter_hw_device cuda. Locks the call-site migration in Rewrite().
func TestRewriter_InitHwDevice_Inject_NVIDIA(t *testing.T) {
	prev := activeDialect
	activeDialect = nvencDialect{}
	defer func() { activeDialect = prev }()

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
	if i < 4 {
		t.Fatalf("-i too early; injection should precede it. args=%v", out.Args)
	}
	want := []string{
		"-init_hw_device", "cuda=cuda:0",
		"-filter_hw_device", "cuda",
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

// preset->compression_level mapping moved into the fork's VAAPI encoder
// (patch 0118); there is no longer a Go map to unit-test. The rewriter
// just leaves -preset / -x264opts / -x265-params untouched.
func TestRewriter_PresetAndOptBlobs_PassThrough(t *testing.T) {
	args := []string{
		"-codec:0", "libdav1d", "-i", "m.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264", "-crf:0", "16", "-preset:0", "ultrafast",
		"-x264opts:0", "subme=0:me=dia",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	// Encoder swapped, but preset + x264opts survive for the fork to consume.
	if !containsString(out.Args, "-preset:0") {
		t.Error("-preset:0 should pass through (fork patch 0118 maps it)")
	}
	if !containsString(out.Args, "-x264opts:0") {
		t.Error("-x264opts:0 should pass through (fork patch 0118 swallows it)")
	}
	if containsString(out.Args, "-compression_level:v") {
		t.Errorf("rewriter must not inject -compression_level:v: %v", out.Args)
	}
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "preset:") || strings.HasPrefix(c, "drop:-x264opts") {
			t.Errorf("unexpected preset/x264opts rewrite tag (moved to fork): %q", c)
		}
	}
}

func TestRewriter_NoPresetEmitted_NoCompressionLevelInject(t *testing.T) {
	// libx265 path: PMS emits -x265-params instead of -preset:0. Worker
	// must NOT inject -compression_level:v — let vaapi_encode dispatch
	// to the iHD driver's intrinsic default (~TU=4 balanced), which
	// matches Plex Transcoder's prod behaviour. B5 bandaid retired
	// 2026-05-15.
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
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "inject:compression_level") ||
			strings.HasPrefix(c, "preset:") {
			t.Errorf("no compression_level change expected when PMS omits -preset:0, got %q: %v", c, out.Changes)
		}
	}
	if containsString(out.Args, "-compression_level:v") {
		t.Errorf("-compression_level:v must not be injected: %v", out.Args)
	}
}

func TestRewriter_HDR_TonemapVAAPI(t *testing.T) {
	t.Setenv("SCALEPLEX_TONEMAP", "vaapi") // assert the fixed-curve fallback shape
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
	// composeBurn: tonemap output is a VA surface for the encoder — no trailing hwupload.
	want := "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=p010,tonemap_vaapi=transfer=bt709:format=nv12[1]"
	if out.Args[idx] != want {
		t.Fatalf("filter=%q\nwant   %q", out.Args[idx], want)
	}
	for i := idx + 1; i < len(out.Args); i++ {
		if out.Args[i] == "-map" {
			if out.Args[i+1] != "[1]" {
				t.Fatalf("map=%q want [1]", out.Args[i+1])
			}
			return
		}
	}
	t.Fatal("no -map found after filter")
}

// PS4 + 4K HDR + SRT-burn case. PMS chose a full SW pipeline because
// its HW pipeline can't combine HW tonemap + inlineass(SW). The
// rewriter reshapes to HW hybrid: hwupload + scale_vaapi(p010) +
// tonemap_vaapi(nv12) + hwdownload + inlineass + hwupload. Reclaims
// HW for every stage except the libass render step. Live argv
// captured 2026-05-13 23:19Z on session ee6xs0g12mq5k6bcdom62ju9-
// ed6ea7eb-03b4-4f81-b640-5aa9580b3978.
// Dolby Vision RPU round-trip: PMS emits `-bsf:0 dovi_rpu=strip=1` on DoVi
// HEVC sources (strips the RPU from the encoded stream). The rewriter has no
// bsf/dovi handling, so it must pass the bitstream filter through untouched —
// flag + value preserved and adjacent. Closes the argv-corpus dovi gap
// (~6 entries, e.g. Legend (2015) Remux DoVi).
func TestRewriter_DoviRPU_BSF_PreservedRoundTrip(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/Movies/Legend.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1];[1]hwupload[2]",
		"-map", "[2]",
		"-codec:0", "hevc_vaapi", "-qp:0", "22",
		"-bsf:0", "dovi_rpu=strip=1",
		"-force_key_frames:0", "expr:gte(t,n_forced*1)",
		"-segment_format", "mpegts", "-f", "ssegment", "-segment_time", "1",
		"-segment_start_number", "0", "media-%05d.ts",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	i := indexOfArg(out.Args, "-bsf:0", 0)
	if i < 0 {
		t.Fatalf("-bsf:0 dropped — DoVi RPU strip lost: %v", out.Args)
	}
	if i+1 >= len(out.Args) {
		t.Fatalf("-bsf:0 has no value (truncated): %v", out.Args)
	}
	if out.Args[i+1] != "dovi_rpu=strip=1" {
		t.Fatalf("-bsf:0 value mangled, got %q", out.Args[i+1])
	}
}

// reFilterHDRAss must match ANY tonemap algo, not just `hable` — a non-hable
// SW-HDR sub-burn session (e.g. tonemap=mobius) used to miss the regex and
// bail (exit 8). The generalized capture reshapes it like the hable case.
func TestRewriter_SWHDR_InlineAss_Reshape_NonHable(t *testing.T) {
	t.Setenv("SCALEPLEX_TONEMAP", "vaapi")
	args := []string{
		"-codec:0", "hevc",
		"-codec:1", "dca",
		"-i", "/media/m.mkv",
		"-i", "/transcode/sess/temp-0.srt",
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-map_inlineass", "1:s:0",
		"-filter_complex",
		"[0:0]scale=w=1920:h=1080:force_divisible_by=4[0];" +
			"[0]format=p010,tonemap=mobius[1];" +
			"[1]format=pix_fmts=yuv420p|nv12[2];" +
			"[2]inlineass=font_scale=1.000000:font_size=54[3]",
		"-map", "[3]",
		"-codec:0", "libx264", "-crf:0", "21", "-preset:0", "veryfast",
		"-segment_format", "mpegts", "-f", "ssegment", "-segment_time", "1",
		"-segment_start_number", "0", "media-%05d.ts",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, map[string]string{}, &RewriteOpts{
		ProbeVideoColor: func(string) (string, string, string) {
			return "smpte2084", "bt2020nc", "bt2020"
		},
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:text-inlineass-vaapi") {
		t.Fatalf("non-hable SW-HDR must reshape (not bail on tonemap algo): %v", out.Changes)
	}
}

func TestRewriter_SWHDR_InlineAss_Reshape(t *testing.T) {
	t.Setenv("SCALEPLEX_TONEMAP", "vaapi") // assert the fixed-curve fallback shape
	args := []string{
		"-codec:0", "hevc",
		"-codec:1", "dca",
		"-i", "/media/m.mkv",
		"-i", "/transcode/sess/temp-0.srt",
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-map_inlineass", "1:s:0",
		"-filter_complex",
		"[0:0]scale=w=1920:h=1080:force_divisible_by=4[0];" +
			"[0]format=p010,tonemap=hable[1];" +
			"[1]format=pix_fmts=yuv420p|nv12[2];" +
			"[2]inlineass=font_scale=1.000000:font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf:fontconfig_file=/usr/lib/plexmediaserver/Resources/fonts.conf:language=en:overrides=ScaledBorderAndShadow\\\\\\=yes:outline=2.6:shadow=1.7:font_size=54[3]",
		"-map", "[3]",
		"-codec:0", "libx264",
		"-crf:0", "21",
		"-preset:0", "veryfast",
		"-segment_format", "mpegts", "-f", "ssegment",
		"-segment_time", "1",
		"-segment_start_number", "0",
		"media-%05d.ts",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, map[string]string{}, &RewriteOpts{
		ProbeVideoColor: func(string) (string, string, string) {
			return "smpte2084", "bt2020nc", "bt2020"
		},
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:text-inlineass-vaapi") {
		t.Fatalf("missing hdr-tonemap-vaapi-passthrough-inlineass tag: %v", out.Changes)
	}
	if !containsString(out.Changes, "encode:libx264->h264_vaapi") {
		t.Fatalf("expected libx264->h264_vaapi swap: %v", out.Changes)
	}
	if !containsString(out.Changes, "decode:bare-hw-upgrade:hevc") {
		t.Fatalf("expected bare-hw-upgrade decode injection: %v", out.Changes)
	}
	if !containsString(out.Changes, "map-label-update") {
		t.Fatalf("expected map-label-update to retarget -map [3]->[15]: %v", out.Changes)
	}
	// Unified composeBurn HW reshape — scale_vaapi + tonemap_vaapi → inlineass
	// ON the VA surface (no hwdownload/hwupload bracket), render_height band.
	idx := findFilterComplex(out.Args, "[0:0]")
	got := out.Args[idx]
	for _, mustHave := range []string{
		"[0:0]hwupload[0]",
		"scale_vaapi=w=1920:h=1080:format=p010",
		"tonemap_vaapi=transfer=bt709:format=nv12",
		"inlineass=",
		"render_height=",
	} {
		if !strings.Contains(got, mustHave) {
			t.Errorf("filter chain missing %q\n  got: %s", mustHave, got)
		}
	}
	if strings.Contains(got, "hwdownload") {
		t.Errorf("inlineass-on-VA: no hwdownload bracket expected\n  got: %s", got)
	}
	// Plex's 4 styling keys (language/overrides/outline/shadow) are now
	// passed through verbatim — the fork's vf_inlineass parses them
	// (patch 0119); the rewriter no longer strips, preserving the user's
	// subtitle styling.
	for _, mustKeep := range []string{"language=", "overrides=", "outline=", "shadow="} {
		if !strings.Contains(got, mustKeep) {
			t.Errorf("Plex styling key %q must pass through inlineass= (fork 0119 parses it)", mustKeep)
		}
	}
	// -map [3] should be rewritten to -map [15].
	for i := idx + 1; i < len(out.Args)-1; i++ {
		if out.Args[i] == "-map" && out.Args[i+1] == "[3]" {
			t.Errorf("found stale -map [3] post-reshape — map-label-update did not retarget")
		}
	}
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

// Plex Optimize remux argv for a genuine h264 source — bare decoder,
// no -hwaccel, video output is -codec:0 copy. Worker has no video
// work to do but must still strip Plex-private flags AND swap
// -codec:1 eac3_eae for stock eac3, or ffmpeg fails on
// "Unknown decoder 'eac3_eae'". Reproduces Pat & Mat S01E04 capture
// 2026-05-10 (clusterplex source) before the fast-path landed.
func TestRewriter_OptimizeRemux_h264_EAE(t *testing.T) {
	args := []string{
		"-codec:0", "h264",
		"-codec:1", "eac3_eae",
		"-eae_prefix:1", "abcdef-prefix_",
		"-noaccurate_seek",
		"-analyzeduration", "20000000", "-probesize", "20000000",
		"-i", "/media/SeriesNL/Show/S01E01.mkv",
		"-y", "-nostats", "-loglevel", "quiet", "-loglevel_plex", "error",
		"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/sid/job/progress",
		"-map", "0:0", "-codec:0", "copy",
		"-filter_complex", "[0:1] aresample=async=1:ochl='stereo':osr=48000[0]",
		"-map", "[0]",
		"-codec:1", "aac", "-b:1", "256k",
		"-f", "mp4", "-movflags", "+faststart",
		"/media/SeriesNL/Show/Plex Versions/Optimized for TV/.inProgress/S01E01.mp4.99",
	}
	out := Rewrite(args, map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://pms.local:32400",
		"X_PLEX_TOKEN":           "tok",
	}, nil)
	if !out.Applied {
		t.Fatalf("remux fast-path should fire: %v", out.Changes)
	}
	// Post-fold: the decode:remux:* / encode:copy(passthrough) markers
	// are gone (the transcode block is gated off via isRemux; the
	// common tail's helper-emitted tags suffice for observability).
	if !containsString(out.Changes, "audio:eac3_eae->eac3") {
		t.Errorf("missing eae swap: %v", out.Changes)
	}
	// Decoder + encoder shape preserved.
	dec := indexOfArg(out.Args, "-codec:0", 0)
	if dec < 0 || out.Args[dec+1] != "h264" {
		t.Errorf("decoder slot mangled: %v", out.Args)
	}
	// -loglevel_plex passes through (scaleplex-ffmpeg7 patch 0098
	// makes it a no-op stub); we deliberately stopped stripping it.
	if containsString(out.Args, "-progressurl") {
		t.Errorf("-progressurl should be captured + stripped: %v", out.Args)
	}
	if containsString(out.Args, "eac3_eae") {
		t.Errorf("eac3_eae should be swapped: %v", out.Args)
	}
	if containsString(out.Args, "-eae_prefix:1") {
		t.Errorf("-eae_prefix should be dropped: %v", out.Args)
	}
	if out.ProgressURL == "" {
		t.Errorf("progressURL not captured")
	}
	// Crucially, MUST NOT inject -init_hw_device — there's no GPU work.
	if containsString(out.Args, "-init_hw_device") {
		t.Errorf("init_hw_device must NOT be injected for remux: %v", out.Args)
	}
}

// hevc + sidecar SRT + multiple sub-copy outputs (the All Creatures
// shape). Sidecar inputs MUST survive — they're referenced by
// -map 1:s:0 / -map 2:s:0 sub copy outputs. The dropSidecarInput
// helper from the SW-decode path would break this; the remux
// fast-path bypasses that helper entirely.
func TestRewriter_OptimizeRemux_hevc_PreservesSidecars(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-codec:1", "eac3_eae",
		"-eae_prefix:1", "abc_",
		"-noaccurate_seek",
		"-analyzeduration", "20000000", "-probesize", "20000000",
		"-i", "/media/Series/Show/S04E04.mkv",
		"-noaccurate_seek",
		"-analyzeduration", "20000000", "-probesize", "20000000",
		"-i", "/transcode/Transcode/Sessions/sess/temp-0.srt",
		"-noaccurate_seek",
		"-analyzeduration", "20000000", "-probesize", "20000000",
		"-i", "/transcode/Transcode/Sessions/sess/temp-1.srt",
		"-y", "-nostats", "-loglevel", "quiet", "-loglevel_plex", "error",
		"-map", "0:0", "-codec:0", "copy",
		"-filter_complex", "[0:1] aresample=async=1:ochl='stereo':osr=48000[0]",
		"-map", "[0]",
		"-codec:1", "aac", "-b:1", "256k",
		"-f", "mp4",
		"/media/Series/Show/Plex Versions/Optimized for TV/.inProgress/S04E04.mp4.99",
		"-map", "1:s:0", "-codec:0", "copy", "-strict_ts:0", "0", "-f", "srt",
		"/media/Series/Show/Plex Versions/Optimized for TV/.inProgress/S04E04.mp4.99.111.sidecar",
		"-map", "2:s:0", "-codec:0", "copy", "-strict_ts:0", "0", "-f", "srt",
		"/media/Series/Show/Plex Versions/Optimized for TV/.inProgress/S04E04.mp4.99.222.sidecar",
	}
	out := Rewrite(args, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("remux fast-path should fire: %v", out.Changes)
	}
	// Three -i must survive (source + 2 sidecars).
	count := 0
	for _, a := range out.Args {
		if a == "-i" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("expected 3 -i, got %d: %v", count, out.Args)
	}
	// -map 1:s:0 / 2:s:0 must still be present (point at sidecar inputs).
	if !containsString(out.Args, "1:s:0") || !containsString(out.Args, "2:s:0") {
		t.Errorf("sidecar -map references lost: %v", out.Args)
	}
}

// Plex Web Chrome DASH playback with text subs: video uses `-codec:0
// copy` (remux fast-path) PLUS a subtitle side-channel output running
// `-f segment -segment_format ass` with `-segment_list
// http://127.0.0.1:32400/...?stream=subtitles` loopback URL. Worker
// pod has no PMS on loopback → ECONNREFUSED → ffmpeg exits 145 in the
// remux fast-path. Rewriter must rewrite the side-channel URL to
// SCALEPLEX_PMS_BASE_URL with X-Plex-Token (same shape as the main
// rewriter's side-channel handling). Observed 2026-05-14 on FMJ
// Plex Web Chrome.
func TestRewriter_OptimizeRemux_PlexWebDASH_SubSideChannel(t *testing.T) {
	env := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://clusterplex-pms.clusterplex.svc:32499",
		"X_PLEX_TOKEN":           "tok123",
	}
	args := []string{
		"-codec:0", "hevc", "-codec:1", "dca",
		"-noaccurate_seek", "-analyzeduration", "20000000", "-probesize", "20000000",
		"-i", "/media/Movies/FMJ/FMJ.mkv",
		"-noaccurate_seek", "-analyzeduration", "20000000", "-probesize", "20000000",
		"-i", "/transcode/Transcode/Sessions/abc/temp-0.srt",
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-y", "-nostats", "-loglevel", "quiet", "-loglevel_plex", "error",
		"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/abc/def/progress",
		"-map", "0:0", "-codec:0", "copy",
		"-filter_complex", "[0:1] aresample=async=1:ochl='stereo':osr=48000[0]",
		"-map", "[0]", "-metadata:s:1", "language=eng",
		"-codec:1", "aac", "-b:1", "256k",
		"-f", "dash", "-seg_duration", "5", "-dash_segment_type", "mp4",
		"-init_seg_name", "init-stream$RepresentationID$.m4s",
		"-media_seg_name", "chunk-stream$RepresentationID$-$Number%05d$.m4s",
		"-window_size", "5", "-delete_removed", "false", "-skip_to_segment", "1",
		"-manifest_name", "http://127.0.0.1:32400/video/:/transcode/session/abc/def/manifest?X-Plex-Http-Pipeline=infinite",
		"-avoid_negative_ts", "disabled", "-map_metadata", "-1", "-map_chapters", "-1", "dash",
		"-map", "1:s:0", "-metadata:s:0", "language=eng",
		"-codec:0", "ass", "-strict_ts:0", "0",
		"-f", "segment", "-segment_format", "ass", "-segment_time", "1",
		"-segment_header_filename", "sub-header", "-segment_start_number", "0",
		"-segment_list", "http://127.0.0.1:32400/video/:/transcode/session/abc/def/manifest?stream=subtitles&X-Plex-Http-Pipeline=infinite",
		"-segment_list_type", "csv", "-segment_list_size", "5",
		"-segment_list_separate_stream_times", "1",
		"-segment_format_options", "ignore_readorder=1",
		"-segment_list_unfinished", "1", "-fflags", "+flush_packets",
		"sub-chunk-%05d",
	}
	out := Rewrite(args, env, nil)
	if !out.Applied {
		t.Fatalf("remux fast-path should fire: %v", out.Changes)
	}
	// Side-channel -segment_list must point at relay, not loopback.
	idx := indexOfArg(out.Args, "-segment_list", 0)
	if idx < 0 || idx+1 >= len(out.Args) {
		t.Fatalf("-segment_list missing: %v", out.Args)
	}
	got := out.Args[idx+1]
	if strings.HasPrefix(got, "http://127.0.0.1:32400") {
		t.Errorf("-segment_list still loopback: %s", got)
	}
	if !strings.Contains(got, "clusterplex-pms.clusterplex.svc:32499") {
		t.Errorf("-segment_list not rewritten to relay: %s", got)
	}
	if !strings.Contains(got, "X-Plex-Token=tok123") {
		t.Errorf("X-Plex-Token not appended: %s", got)
	}
	hasTag := false
	for _, c := range out.Changes {
		if c == "subs:side-channel-segment_list:rewrite-to-relay" {
			hasTag = true
			break
		}
	}
	if !hasTag {
		t.Errorf("expected subs:side-channel-segment_list:rewrite-to-relay tag: %v", out.Changes)
	}
}

// Detection-shape argv carries `-codec:1 aac` even when the source
// audio is actually EAC3/AC3/DTS. PMS expects Plex's bundled EAE to
// bridge any source codec; stock ffmpeg honours the hint literally
// and the AAC decoder fails on the EAC3 bitstream with exit 8. The
// no-decoder bail path drops audio-side input decoder hints so
// ffmpeg auto-detects from each stream's codec_id (always picks
// correctly).
func TestRewriter_Bail_NoDecoder_DropsAudioInputHints(t *testing.T) {
	args := []string{
		"-codec:1", "aac",
		"-eae_prefix:1", "abc-prefix_",
		"-analyzeduration", "20000000", "-probesize", "20000000",
		"-i", "/media/Series/Show/S01E01.mkv",
		"-y", "-nostats", "-loglevel", "quiet",
		"-loglevel_plex", "error",
		"-filter_complex", "[0:1] aresample=async=1:ochl='stereo':osr=48000[0]",
		"-map", "[0]",
		"-codec:0", "flac", "-b:0", "4096k",
		"-f", "flac", "-t", "30",
		"/transcode/Transcode/Detection/some-uuid",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("scrub should mark Applied=true: %v", out.Changes)
	}
	// -codec:1 (input audio decoder hint) gone.
	for i, a := range out.Args {
		if a == "-codec:1" && i+1 < len(out.Args) && out.Args[i+1] == "aac" {
			// must NOT be before -i
			iIdx := indexOfArg(out.Args, "-i", 0)
			if i < iIdx {
				t.Fatalf("input-side -codec:1 aac still present pre--i: %v", out.Args)
			}
		}
	}
	// -eae_prefix:1 also gone (orphaned without the codec hint).
	for _, a := range out.Args {
		if strings.HasPrefix(a, "-eae_prefix") {
			t.Fatalf("-eae_prefix should be dropped: %v", out.Args)
		}
	}
	// Output-side -codec:0 flac (the encode hint) MUST survive — it's
	// AFTER -i, post-input-stream output codec, not a decode hint.
	hasFLAC := false
	for i, a := range out.Args {
		if a == "-codec:0" && i+1 < len(out.Args) && out.Args[i+1] == "flac" {
			iIdx := indexOfArg(out.Args, "-i", 0)
			if i > iIdx {
				hasFLAC = true
			}
		}
	}
	if !hasFLAC {
		t.Errorf("output -codec:0 flac was dropped (must survive): %v", out.Args)
	}
}

// PMS spawns audio-only Detection ffmpeg jobs (intro / credits / voice
// activity ML pre-pass) for every video item. They have no video
// decoder so the rewriter bails with skip:no-decoder, but they ALSO
// carry Plex-private flags (-loglevel_plex, -progressurl) that stock
// ffmpeg rejects with exit 8. The bail path must still scrub those
// flags or every Detection job fails on this cluster — observed
// 2026-05-10 as a flood of exit-8 sessions blocking PMS marker
// generation. Reproduces the captured-from-clusterplex argv shape.
func TestRewriter_Bail_StripsPlexPrivateFlags(t *testing.T) {
	args := []string{
		"-codec:1", "aac", "-analyzeduration", "20000000", "-probesize", "20000000",
		"-i", "/media/Series/Show/S01E01.mkv",
		"-y", "-nostats", "-loglevel", "quiet",
		"-loglevel_plex", "error",
		"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/abc/def/progress",
		"-filter_complex", "[0:1] aresample=async=1:ochl='stereo':osr=48000[0]",
		"-map", "[0]",
		"-codec:0", "flac", "-b:0", "4096k",
		"-f", "flac", "-t", "30",
		"/transcode/Transcode/Detection/some-uuid",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("scrub should mark Applied=true so worker uses cleaned args; changes=%v", out.Changes)
	}
	// -loglevel_plex passes through (fork patch 0098 makes it a stub).
	if containsString(out.Args, "-progressurl") {
		t.Errorf("-progressurl still present: %v", out.Args)
	}
	// Should still mark a bail reason.
	hasBail := false
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "skip:") {
			hasBail = true
			break
		}
	}
	if !hasBail {
		t.Errorf("expected skip: change still present alongside scrub: %v", out.Changes)
	}
}

// PMS side-channel SRT extractor argv: subtitle pipe only, output `-codec:0
// copy`, no decoder before `-i`. Rewriter must hit no-decoder bail AND
// rewrite `-segment_list` loopback URL to the relay (worker pod has no PMS
// on 127.0.0.1:32400 → ECONNREFUSED → ffmpeg muxer task-error → exit 145).
// `-strict_ts:0` passes through unchanged via fork patch 0107 stub.
// Observed 2026-05-14 on LG webOS sidecar SRT.
func TestRewriter_Bail_SubtitleSideChannel_StripsAndRewrites(t *testing.T) {
	env := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://clusterplex-pms.clusterplex.svc:32499",
		"X_PLEX_TOKEN":           "tok123",
	}
	args := []string{
		"-ss", "112", "-analyzeduration", "20000000", "-probesize", "20000000",
		"-i", "/transcode/Transcode/Sessions/plex-transcode-abc/temp-0.srt",
		"-start_at_zero", "-copyts", "-y", "-nostats", "-loglevel", "quiet",
		"-loglevel_plex", "error",
		"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/abc/def/progress",
		"-map", "0:s:0", "-metadata:s:0", "language=eng",
		"-codec:0", "copy", "-strict_ts:0", "0",
		"-f", "segment", "-segment_format", "srt", "-segment_time", "1",
		"-segment_start_number", "0",
		"-segment_list", "http://127.0.0.1:32400/video/:/transcode/session/abc/def/manifest?X-Plex-Http-Pipeline=infinite",
		"-segment_list_type", "csv",
		"-avoid_negative_ts", "disabled",
		"chunk-%05d",
	}
	out := Rewrite(args, env, nil)
	if !out.Applied {
		t.Fatalf("expected Applied=true, got changes=%v", out.Changes)
	}
	stIdx := indexOfArg(out.Args, "-strict_ts:0", 0)
	if stIdx < 0 || stIdx+1 >= len(out.Args) || out.Args[stIdx+1] != "0" {
		t.Errorf("-strict_ts:0 0 should pass through unchanged: %v", out.Args)
	}
	idx := indexOfArg(out.Args, "-segment_list", 0)
	if idx < 0 || idx+1 >= len(out.Args) {
		t.Fatalf("-segment_list missing: %v", out.Args)
	}
	got := out.Args[idx+1]
	if strings.HasPrefix(got, "http://127.0.0.1:32400") {
		t.Errorf("-segment_list still loopback: %s", got)
	}
	if !strings.Contains(got, "clusterplex-pms.clusterplex.svc:32499") {
		t.Errorf("-segment_list not rewritten to relay: %s", got)
	}
	if !strings.Contains(got, "X-Plex-Token=tok123") {
		t.Errorf("X-Plex-Token not appended: %s", got)
	}
	hasBail, hasRewrite := false, false
	for _, c := range out.Changes {
		switch c {
		case "skip:no-decoder":
			hasBail = true
		case "bail:segment_list:rewrite-to-relay":
			hasRewrite = true
		}
		if strings.HasPrefix(c, "drop:-strict_ts") {
			t.Errorf("strict_ts should pass through, got change %q: %v", c, out.Changes)
		}
	}
	if !hasBail || !hasRewrite {
		t.Errorf("missing change tags (bail=%v rewrite=%v): %v", hasBail, hasRewrite, out.Changes)
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
	// scaleplex-ffmpeg7 honours -manifest_name natively; the rewriter
	// rewrites the URL in-place (loopback → relay base) instead of
	// stripping and surfacing on ManifestURL.
	wantURL := "http://relay.svc:32499/.../manifest?X-Plex-Http-Pipeline=infinite&X-Plex-Token=secret"
	mnIdx := indexOfArg(out.Args, "-manifest_name", 0)
	if mnIdx < 0 || mnIdx+1 >= len(out.Args) {
		t.Fatal("-manifest_name must remain in argv (rewritten in-place)")
	}
	if out.Args[mnIdx+1] != wantURL {
		t.Fatalf("-manifest_name value=%q want %q", out.Args[mnIdx+1], wantURL)
	}
	if !containsString(out.Changes, "manifest_name:rewrite-to-relay") {
		t.Fatalf("missing rewrite change: %v", out.Changes)
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
	// Without SCALEPLEX_PMS_BASE_URL we still must strip the flag — ffmpeg's
	// HTTP protocol handler cannot reach 127.0.0.1:32400 from the worker.
	if containsString(out.Args, "-manifest_name") {
		t.Fatal("-manifest_name must be stripped from argv when no relay base")
	}
	if !containsString(out.Changes, "drop:-manifest_name(no-pms-base-or-non-loopback)") {
		t.Fatalf("missing drop change: %v", out.Changes)
	}
}

// `-skip_to_segment N` passes through to ffmpeg untouched (dashenc
// fork patch 0095 honours it natively). Diagnostic change-tag is
// emitted. PTS-handling flags (-copyts/-start_at_zero/-avoid_negative_ts
// disabled) MUST be left alone — stripping them rebased the AAC
// encoder's PTS to 0 with no primer samples and produced empty
// (199-byte) first audio segments after every seek, which DASH players
// hang on indefinitely.
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
	if !containsString(out.Changes, "skip_to_segment:passthrough=522") {
		t.Fatalf("missing skip_to_segment:passthrough=522 tag: %v", out.Changes)
	}
	stsIdx := indexOfArg(out.Args, "-skip_to_segment", 0)
	if stsIdx < 0 || out.Args[stsIdx+1] != "522" {
		t.Fatal("-skip_to_segment must pass through to ffmpeg (value=522)")
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

// Initial-play session (`-skip_to_segment 1`, no `-ss`) — flag passes
// through untouched. Diagnostic tag emitted. No -ss → no
// -output_ts_offset injection.
func TestRewriter_SkipToSegment_InitialPlayCaptured(t *testing.T) {
	out := Rewrite(swArgsAV1H264, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "skip_to_segment:passthrough=1") {
		t.Fatalf("missing skip_to_segment:passthrough=1 tag: %v", out.Changes)
	}
	stsIdx := indexOfArg(out.Args, "-skip_to_segment", 0)
	if stsIdx < 0 || out.Args[stsIdx+1] != "1" {
		t.Fatal("-skip_to_segment must pass through to ffmpeg")
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
// B1 retired 2026-05-13: jellyfin-ffmpeg7 hevc_vaapi handles the IDR
// storm at -copyts + seek without our offset-by-seek rewrite (validated
// on Plex Web DASH HW transcode + seek to 4794s). The rewriter no
// longer modifies the expr; -force_key_frames:0 passes through as-is.
func TestRewriter_ForceKeyFrames_PassthroughOnSeek(t *testing.T) {
	args := append([]string{"-ss", "2344"}, swArgsAV1H264...)
	out := Rewrite(args, map[string]string{}, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Changes, "force_key_frames:offset-by-seek") {
		t.Fatalf("offset-by-seek rewrite should be retired: %v", out.Changes)
	}
	idx := indexOfArg(out.Args, "-force_key_frames:0", 0)
	if idx < 0 || idx+1 >= len(out.Args) {
		t.Fatal("missing -force_key_frames:0")
	}
	// PMS-emitted expr stays intact; encoder handles the seek-time t.
	want := "expr:gte(t,n_forced*3)"
	if out.Args[idx+1] != want {
		t.Errorf("force_key_frames expr=%q want %q (passthrough)", out.Args[idx+1], want)
	}
}

// HLS-ssegment seek: scaleplex-ffmpeg7's segment muxer (shared with
// ssegment) no longer rebases end_pts by reference_stream_first_pts
// (patch 0103), so split cadence works with -copyts intact. The
// rewriter passes -copyts through unchanged.
func TestRewriter_HLS_CopytsKept(t *testing.T) {
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
	if !containsString(out.Args, "-copyts") {
		t.Errorf("HLS argv must KEEP -copyts post-patch-0103 (jellyfin first-pts adjust removed)")
	}
	if containsString(out.Changes, "hls:drop:-copyts(seek)") {
		t.Errorf("hls:drop:-copyts(seek) tag must NOT appear post-patch-0103: %v", out.Changes)
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

// Plain `<base>.<lang>.srt` (the historical case) must still work.

// Bitmap embedded burn-in (PGS in a Blu-ray remux). Bitmap now burns through
// the SAME unified inlineass path as text (composeBurn) — no overlay_vaapi, no
// full-frame sub2video. The fork's -map_inlineass binding routes the bitmap
// codec to replay_bitmap and renders at render_height (band). Filter graph:
//
//	[0:0]hwupload → scale_vaapi(nv12) → inlineass(render_height)
//
// And: NO extraction (the stream stays in -i 0), -map_inlineass KEPT.
func TestRewriter_BitmapInlineass_Embedded_PGS(t *testing.T) {
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
	if !containsString(out.Changes, "filter:bitmap-inlineass-vaapi") {
		t.Fatalf("expected unified bitmap-inlineass-vaapi mode: %v", out.Changes)
	}
	if containsString(out.Changes, "drop:-map_inlineass") {
		t.Errorf("bitmap now burns via inlineass — -map_inlineass must be KEPT: %v", out.Changes)
	}
	if i := indexOfArg(out.Args, "-map_inlineass", 0); i < 0 || out.Args[i+1] != "0:3" {
		t.Errorf("-map_inlineass 0:3 must remain for the replay_bitmap binding: %v", out.Args)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	for _, must := range []string{
		"[0:0]hwupload[0]",
		"[0]scale_vaapi=w=3840:h=2160:format=nv12[1]",
		"[1]inlineass=render_height=1080[2]",
	} {
		if !strings.Contains(f, must) {
			t.Errorf("filter missing %q\n%s", must, f)
		}
	}
	for _, banned := range []string{"overlay_vaapi", "format=bgra", "subtitles=filename="} {
		if strings.Contains(f, banned) {
			t.Errorf("unified bitmap burn must NOT use %q:\n%s", banned, f)
		}
	}
}

// Bitmap sidecar (.sup file as second -i). Rare but possible. The rewriter
// must KEEP the second -i — the fork's -map_inlineass binding reads the .sup
// stream from it (1:s:0) — and route through inlineass, not overlay_vaapi.
func TestRewriter_BitmapInlineass_Sidecar_KeepsInput(t *testing.T) {
	probe := func(source, spec string) string {
		return "hdmv_pgs_subtitle"
	}
	out := Rewrite(swArgsWithSubsRealSidecar, nil, &RewriteOpts{
		ProbeSubtitleCodec: probe,
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "filter:bitmap-inlineass-vaapi") {
		t.Fatalf("expected unified bitmap-inlineass-vaapi mode: %v", out.Changes)
	}
	if containsString(out.Changes, "drop:-i(sidecar-input)") {
		t.Fatal("bitmap-sidecar must NOT drop second -i (the binding reads the stream)")
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
	if i := indexOfArg(out.Args, "-map_inlineass", 0); i < 0 || out.Args[i+1] != "1:s:0" {
		t.Errorf("-map_inlineass 1:s:0 must be present for the replay_bitmap binding: %v", out.Args)
	}
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	if !strings.Contains(f, "inlineass=render_height=1080") {
		t.Errorf("filter must burn via inlineass(render_height):\n%s", f)
	}
	for _, banned := range []string{"overlay_vaapi", "format=bgra"} {
		if strings.Contains(f, banned) {
			t.Errorf("unified bitmap burn must NOT use %q:\n%s", banned, f)
		}
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

// subtitleIsAnimated routes animated ASS to the inlineass path and
// SRT / static ASS to the pre-render overlay path.
func TestSubtitleIsAnimated(t *testing.T) {
	read := func(content string) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return []byte(content), nil }
	}
	readErr := func(string) ([]byte, error) { return nil, fmt.Errorf("boom") }

	tests := []struct {
		name  string
		codec string
		path  string
		read  func(string) ([]byte, error)
		want  bool
	}{
		{"srt never animated", "subrip", "/x.srt", nil, false},
		{"srt alias", "srt", "/x.srt", nil, false},
		{"static ass dialogue", "ass", "/x.ass",
			read("Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,Hello world"), false},
		{"ass karaoke \\k", "ass", "/x.ass",
			read(`Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,{\k50}la{\k30}la`), true},
		{"ass transform \\t", "ass", "/x.ass",
			read(`Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,{\t(0,500,\fscx120)}grow`), true},
		{"ass movement \\move", "ass", "/x.ass",
			read(`Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,{\move(0,0,100,100)}slide`), true},
		{"ass fade \\fad", "ass", "/x.ass",
			read(`Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,{\fad(200,200)}fade`), true},
		{"embedded ass conservative", "ass", "", nil, true},
		{"ass read error conservative", "ass", "/x.ass", readErr, true},
		{"mov_text static", "mov_text", "", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := subtitleIsAnimated(tt.codec, tt.path, tt.read); got != tt.want {
				t.Errorf("subtitleIsAnimated(%q, %q) = %v want %v", tt.codec, tt.path, got, tt.want)
			}
		})
	}
}

// plexInlineassToForceStyle maps Plex's inlineass params onto a
// subtitles-filter force_style= value.
func TestPlexInlineassToForceStyle(t *testing.T) {
	params := "font_scale=1.000000:font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf:fontconfig_file=/usr/lib/plexmediaserver/Resources/fonts.conf:language=en:overrides=ScaledBorderAndShadow=yes,FontName=Noto Sans Medium,Bold=500,PrimaryColour=&H00FFFFFF,OutlineColour=&H00020713,BackColour=&HCC000000:outline=2.6:shadow=1.7:font_size=54"
	got := plexInlineassToForceStyle(params)
	for _, want := range []string{
		"FontName=Noto Sans Medium", "Bold=500",
		"PrimaryColour=&H00FFFFFF", "OutlineColour=&H00020713",
		"BackColour=&HCC000000", "Outline=2.6", "Shadow=1.7", "FontSize=54",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("force_style missing %q: %s", want, got)
		}
	}
	// Non-style keys and script-info fields must not leak through.
	for _, bad := range []string{
		"ScaledBorderAndShadow", "font_path", "fontconfig_file",
		"language", "font_scale", "NotoSans-Medium",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("force_style should not contain %q: %s", bad, got)
		}
	}
}

func TestPlexInlineassToForceStyle_Empty(t *testing.T) {
	if got := plexInlineassToForceStyle("font_scale=1.0:language=en"); got != "" {
		t.Errorf("expected empty force_style, got %q", got)
	}
}

// scrubPlexInlineassFilesystemPaths must drop font_path + fontconfig_file
// (PMS-install-only absolute paths the worker can't honor) and leave the
// rest of Plex's inlineass node intact. The exact argv shape is taken
// from a live force-burn session that hit the exit-145 fontconfig bug on
// prod 2026-05-30/31 (worker exits 145 with "Cannot load config file
// /usr/lib/plexmediaserver/Resources/fonts.conf"). Idempotent.
func TestScrubPlexInlineassFilesystemPaths(t *testing.T) {
	cases := []struct {
		name           string
		in             string
		wantChanged    bool
		wantContains   []string
		wantNotContain []string
	}{
		{
			name: "HW-reshape graph (live prod argv shape)",
			in: "[0:#0x01]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1];[1]hwdownload,format=nv12[2];" +
				"[2]inlineass=font_scale=1.000000:" +
				"font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf:" +
				"fontconfig_file=/usr/lib/plexmediaserver/Resources/fonts.conf:" +
				"language=en:overrides=ScaledBorderAndShadow=yes,FontName=Noto Sans Medium," +
				"Bold=500,PrimaryColour=&H00FFFFFF,OutlineColour=&H00020713,BackColour=&HCC000000:" +
				"outline=2.6:shadow=1.7:font_size=54[3];[3]hwupload[4]",
			wantChanged: true,
			wantContains: []string{
				"hwupload[0]", "scale_vaapi=w=1920:h=1080",
				"font_scale=1.000000", "language=en", "overrides=ScaledBorderAndShadow",
				"FontName=Noto Sans Medium", "outline=2.6", "shadow=1.7", "font_size=54",
			},
			wantNotContain: []string{
				"font_path=", "fontconfig_file=",
				"/usr/lib/plexmediaserver/",
			},
		},
		{
			name: "SW-passthrough graph (libx264 path)",
			in: "[0:#0x01]scale=w=1920:h=1080:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1];" +
				"[1]inlineass=font_scale=1.0:" +
				"font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf:" +
				"fontconfig_file=/usr/lib/plexmediaserver/Resources/fonts.conf:" +
				"language=en:outline=2.6:shadow=1.7:font_size=54[2]",
			wantChanged: true,
			wantContains: []string{
				"scale=w=1920", "font_scale=1.0", "language=en", "outline=2.6",
			},
			wantNotContain: []string{
				"font_path=", "fontconfig_file=", "/usr/lib/plexmediaserver/",
			},
		},
		{
			name:           "no inlineass node — passthrough",
			in:             "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1];[1]hwupload[2]",
			wantChanged:    false,
			wantContains:   []string{"scale_vaapi=w=1280:h=720"},
			wantNotContain: []string{"font_path=", "fontconfig_file="},
		},
		{
			name:           "inlineass without the scrubbed keys — passthrough",
			in:             "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1];[1]inlineass=font_size=54:outline=2:shadow=1[2];[2]hwupload[3]",
			wantChanged:    false,
			wantContains:   []string{"inlineass=font_size=54:outline=2:shadow=1"},
			wantNotContain: []string{"font_path=", "fontconfig_file="},
		},
		{
			name: "two inlineass nodes — both scrubbed",
			in: "[0]inlineass=font_path=/a:font_size=12[1];" +
				"[2]inlineass=fontconfig_file=/b:font_size=24[3]",
			wantChanged: true,
			wantContains: []string{
				"inlineass=font_size=12", "inlineass=font_size=24",
			},
			wantNotContain: []string{
				"font_path=", "fontconfig_file=", "/a", "/b",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := scrubPlexInlineassFilesystemPaths(tc.in)
			if changed != tc.wantChanged {
				t.Errorf("changed=%v, want %v\n  in:  %s\n  out: %s", changed, tc.wantChanged, tc.in, got)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\n  out: %s", want, got)
				}
			}
			for _, bad := range tc.wantNotContain {
				if strings.Contains(got, bad) {
					t.Errorf("output should not contain %q\n  out: %s", bad, got)
				}
			}
			// Idempotency.
			got2, changed2 := scrubPlexInlineassFilesystemPaths(got)
			if changed2 {
				t.Errorf("second pass reports change (not idempotent)\n  out: %s", got2)
			}
			if got2 != got {
				t.Errorf("second pass mutates output (not idempotent)\n  pass1: %s\n  pass2: %s", got, got2)
			}
		})
	}
}

// scrubPlexInlineassFilesystemPathsInArgs only rewrites -filter_complex
// values that need it; other args (including other -filter_complex
// values without inlineass) pass through.
func TestScrubPlexInlineassFilesystemPathsInArgs(t *testing.T) {
	args := []string{
		"-i", "/tmp/in.mp4",
		"-filter_complex", "[0:0]scale=w=1280:h=720[1]", // no inlineass, untouched
		"-filter_complex", "[0:0]inlineass=font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf:font_size=54[1]",
		"-f", "dash",
	}
	out, did := scrubPlexInlineassFilesystemPathsInArgs(args)
	if !did {
		t.Fatalf("expected scrub to fire")
	}
	if len(out) != len(args) || out[0] != "-i" || out[1] != "/tmp/in.mp4" ||
		out[2] != "-filter_complex" || out[4] != "-filter_complex" ||
		out[6] != "-f" || out[7] != "dash" {
		t.Fatalf("arg structure mutated: %v", out)
	}
	if out[3] != "[0:0]scale=w=1280:h=720[1]" {
		t.Errorf("first filter_complex (no inlineass) unexpectedly changed: %q", out[3])
	}
	if strings.Contains(out[5], "font_path=") || strings.Contains(out[5], "plexmediaserver") {
		t.Errorf("second filter_complex still carries PMS paths: %q", out[5])
	}
	if !strings.Contains(out[5], "font_size=54") {
		t.Errorf("second filter_complex lost legitimate font_size: %q", out[5])
	}
	// No-op pass returns the original slice (or equivalent).
	out2, did2 := scrubPlexInlineassFilesystemPathsInArgs(out)
	if did2 {
		t.Errorf("idempotent pass reports change")
	}
	if len(out2) != len(out) {
		t.Errorf("idempotent pass mutated arg count: %d -> %d", len(out), len(out2))
	}
}

// End-to-end regression: a live HW-reshape force-burn argv (the shape
// captured 2026-05-31 on prod that hit exit-145) must come out of
// Rewrite without the two PMS-install-only inlineass keys, and the
// scrub tag must land in res.Changes. Covers both SW-passthrough bail
// paths (when the rewriter skips reshape) and HW-reshape paths (when
// extractGraphFacts captures already-scrubbed params).
func TestRewriter_ScrubsPlexInlineassFilesystemPaths(t *testing.T) {
	// SW-passthrough shape: Plex Optimize MP4 (libx264) target with
	// inlineass burn — the worker bails on no-decoder for this shape
	// (Plex provides Optimize's bare codec hints, no -i decoder), so
	// without the top-level scrub the filter graph used to pass through
	// verbatim and ffmpeg would exit 145.
	args := []string{
		"-codec:#0x01", "hevc",
		"-codec:#0x02", "aac",
		"-i", "/media/in.mp4",
		"-i", "/transcode/temp-0.srt",
		"-start_at_zero", "-copyts",
		"-fps_mode", "cfr",
		"-y", "-nostats", "-loglevel", "quiet",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:#0x01]scale=w=1920:h=1080:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1];" +
			"[1]inlineass=font_scale=1.000000:" +
			"font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf:" +
			"fontconfig_file=/usr/lib/plexmediaserver/Resources/fonts.conf:" +
			"language=en:overrides=ScaledBorderAndShadow=yes,FontName=Noto Sans Medium," +
			"Bold=500,PrimaryColour=&H00FFFFFF,OutlineColour=&H00020713,BackColour=&HCC000000:" +
			"outline=2.6:shadow=1.7:font_size=54[2]",
		"-map", "[2]",
		"-codec:0", "libx264",
		"-crf:0", "16",
		"-f", "dash",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("rewriter did not apply: %v", out.Changes)
	}
	if !containsString(out.Changes, TagFilterInlineassScrubPlexFontPaths) {
		t.Errorf("expected change tag %q, got %v", TagFilterInlineassScrubPlexFontPaths, out.Changes)
	}
	fcIdx := -1
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-filter_complex" && strings.Contains(out.Args[i+1], "inlineass=") {
			fcIdx = i + 1
			break
		}
	}
	if fcIdx < 0 {
		t.Fatalf("no -filter_complex with inlineass in output args: %v", out.Args)
	}
	fc := out.Args[fcIdx]
	for _, bad := range []string{
		"font_path=", "fontconfig_file=", "/usr/lib/plexmediaserver/",
	} {
		if strings.Contains(fc, bad) {
			t.Errorf("rewritten filter_complex still contains %q\n  out: %s", bad, fc)
		}
	}
	for _, want := range []string{
		"inlineass=", "font_scale=1.000000", "language=en",
		"outline=2.6", "shadow=1.7", "font_size=54",
		"overrides=ScaledBorderAndShadow",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("rewritten filter_complex missing %q\n  out: %s", want, fc)
		}
	}
}

// CodeRabbit finding (PR #138): on a bail path where the ONLY mutation
// was the top-of-Rewrite inlineass scrub (no EAE swap, no framedrop BSF,
// no input-hint drops, no -progressurl/-segment_list scrub), the
// bail-local `applied` calc used to be false → caller would think no
// rewrite happened and execute the ORIGINAL unsanitized argv, defeating
// the scrub entirely. Regression test: minimal argv that bails
// no-decoder (no -codec:0 before -i) but carries the PMS font paths in
// -filter_complex must come back Applied=true AND with the scrub
// applied to args.
func TestRewriter_BailWithOnlyInlineassScrub_AppliedTrue(t *testing.T) {
	args := []string{
		"-i", "/tmp/in.mp4",
		"-filter_complex", "[0:0]inlineass=font_path=/usr/lib/plexmediaserver/Resources/Fonts/NotoSans-Medium.otf:" +
			"fontconfig_file=/usr/lib/plexmediaserver/Resources/fonts.conf:" +
			"font_size=54[1]",
		"-f", "mp4",
		"/tmp/out.mp4",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("Applied=false on scrub-only bail; caller would execute unsanitized argv. changes=%v args=%v", out.Changes, out.Args)
	}
	if !containsString(out.Changes, TagFilterInlineassScrubPlexFontPaths) {
		t.Errorf("expected change tag %q in %v", TagFilterInlineassScrubPlexFontPaths, out.Changes)
	}
	// Confirm we actually bailed (not took the main path).
	bailed := false
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "skip:") {
			bailed = true
			break
		}
	}
	if !bailed {
		t.Fatalf("expected a skip: bail tag in changes (this test repros the scrub-only-on-bail path); changes=%v", out.Changes)
	}
	// And the args returned to the caller no longer carry the PMS paths.
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] != "-filter_complex" {
			continue
		}
		if strings.Contains(out.Args[i+1], "font_path=") || strings.Contains(out.Args[i+1], "/usr/lib/plexmediaserver/") {
			t.Errorf("rewritten -filter_complex still carries PMS paths: %q", out.Args[i+1])
		}
	}
}

// PMS stream-id-by-id specifier syntax (`:#0xNN`, hex) gets normalized
// to ordinal form (`:0`, `:1`, ...) at top-of-Rewrite so downstream
// detector + reshape stages (all keyed on literal `-hwaccel:0` /
// `-codec:0`) actually engage instead of silently bailing
// `skip:no-decoder`. Repro: Ghosts S2E1 force-burn 2026-05-31 — Plex
// Versions / Optimized for TV argv carried `-codec:#0x01 hevc
// -hwaccel:#0x01 vaapi ... -filter_complex "[0:#0x01]..."`.
func TestNormalizePlexStreamSpecsToOrdinal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		in          []string
		wantOut     []string
		wantChanged bool
	}{
		{
			name: "HW-passthrough Ghosts shape — #0x01 video + #0x02 audio",
			in: []string{
				"-codec:#0x01", "hevc",
				"-hwaccel:#0x01", "vaapi",
				"-hwaccel_output_format:#0x01", "vaapi",
				"-hwaccel_device:#0x01", "vaapi",
				"-codec:#0x02", "aac",
				"-i", "src.mp4",
				"-filter_complex", "[0:#0x01]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1]",
				"-codec:0", "hevc_vaapi",
			},
			wantOut: []string{
				"-codec:0", "hevc",
				"-hwaccel:0", "vaapi",
				"-hwaccel_output_format:0", "vaapi",
				"-hwaccel_device:0", "vaapi",
				"-codec:1", "aac",
				"-i", "src.mp4",
				"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1]",
				"-codec:0", "hevc_vaapi",
			},
			wantChanged: true,
		},
		{
			name: "m2ts high-PID #0x1011 maps to ordinal 0",
			in: []string{
				"-codec:#0x1011", "hevc",
				"-hwaccel:#0x1011", "vaapi",
				"-i", "src.m2ts",
			},
			wantOut: []string{
				"-codec:0", "hevc",
				"-hwaccel:0", "vaapi",
				"-i", "src.m2ts",
			},
			wantChanged: true,
		},
		{
			name: "already-ordinal argv passes through unchanged",
			in: []string{
				"-codec:0", "hevc",
				"-hwaccel:0", "vaapi",
				"-i", "src.mp4",
				"-filter_complex", "[0:0]hwupload[0]",
				"-codec:0", "hevc_vaapi",
			},
			wantOut: []string{
				"-codec:0", "hevc",
				"-hwaccel:0", "vaapi",
				"-i", "src.mp4",
				"-filter_complex", "[0:0]hwupload[0]",
				"-codec:0", "hevc_vaapi",
			},
			wantChanged: false,
		},
		{
			name: "filter-complex only — no top-level flag carries #0xNN",
			in: []string{
				"-i", "src.mp4",
				"-filter_complex", "[0:#0x01]format=nv12[a];[a]inlineass[b]",
				"-map", "[b]",
				"-f", "mp4", "out.mp4",
			},
			wantOut: []string{
				"-i", "src.mp4",
				"-filter_complex", "[0:0]format=nv12[a];[a]inlineass[b]",
				"-map", "[b]",
				"-f", "mp4", "out.mp4",
			},
			wantChanged: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, did := normalizePlexStreamSpecsToOrdinal(tc.in)
			if did != tc.wantChanged {
				t.Fatalf("changed=%v want=%v", did, tc.wantChanged)
			}
			if !reflect.DeepEqual(out, tc.wantOut) {
				t.Errorf("output mismatch\n got: %v\nwant: %v", out, tc.wantOut)
			}
		})
	}
}

// End-to-end: a `#0xNN`-style argv that previously hit `skip:no-decoder`
// now engages the HW-passthrough reshape (no bail). Detector hits
// `-hwaccel:0` after normalization.
func TestRewriter_HWPassthrough_NormalizesStreamSpecsAndReshapes(t *testing.T) {
	args := []string{
		"-codec:#0x01", "hevc",
		"-hwaccel:#0x01", "vaapi",
		"-hwaccel_output_format:#0x01", "vaapi",
		"-hwaccel_device:#0x01", "vaapi",
		"-codec:#0x02", "aac",
		"-i", "/media/src.mp4",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:#0x01]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1]",
		"-map", "[1]",
		"-codec:0", "hevc_vaapi", "-qp:0", "15",
		"-f", "dash", "out.mpd",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("Applied=false; changes=%v", out.Changes)
	}
	if !containsString(out.Changes, TagNormalizeStreamSpecsToOrdinal) {
		t.Errorf("expected %q in changes; got %v", TagNormalizeStreamSpecsToOrdinal, out.Changes)
	}
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "skip:no-decoder") {
			t.Fatalf("rewriter still bailed no-decoder after normalization; changes=%v", out.Changes)
		}
	}
	// The reshaped argv must NOT carry any residual `:#0x` specifiers.
	for _, a := range out.Args {
		if strings.Contains(a, ":#0x") {
			t.Errorf("residual #0xNN specifier in output arg %q", a)
		}
	}
}

// Bail-mode safety net: a `#0xNN`-shape argv with `-f dash +
// -manifest_name http://127.0.0.1:32400/...` that still falls through
// to the bail (covers any unmodeled flag combo the main path can't
// reshape) must have its manifest_name URL rewritten to relay before
// the dash muxer tries to POST and ECONNREFUSEs → exit 145. Carries
// real `#0xNN` syntax so the normalize+bail interaction is exercised:
// the normalizer fires at top-of-Rewrite, then the argv still trips
// the no-input bail (no second `-i` source after `dropSidecarInput`
// is hypothetical here — we just omit `-codec:0` so the main path
// can't engage), and the bail's manifest_name rewrite runs on the
// already-normalized argv.
func TestRewriter_BailRewritesManifestNameToRelay(t *testing.T) {
	args := []string{
		// PMS `#0xNN` shape. After normalization the rewriter still
		// can't find a `-codec:0` peer for reshape (no encoder output
		// declared) and bails — but the bail must already see the
		// normalized argv so downstream tests would catch shape drift.
		"-i", "src.mp4",
		"-i", "temp-0.srt",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:#0x01]format=nv12[a];[a]inlineass=font_size=54[b]",
		"-map", "[b]",
		"-f", "dash",
		"-manifest_name", "http://127.0.0.1:32400/video/:/transcode/session/abc/uuid/manifest?X-Plex-Http-Pipeline=infinite",
		"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/abc/uuid/progress",
		"out.mpd",
	}
	env := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay.test:32499",
		"X_PLEX_TOKEN":           "tok-xyz",
	}
	out := Rewrite(args, env, nil)
	if !out.Applied {
		t.Fatalf("Applied=false on bail with mutations; caller would execute unsanitized argv. changes=%v", out.Changes)
	}
	if !containsString(out.Changes, TagNormalizeStreamSpecsToOrdinal) {
		t.Errorf("normalize tag missing — `#0x01` should have been folded to ordinal before bail; changes=%v", out.Changes)
	}
	if !containsString(out.Changes, TagBailManifestNameRewriteToRelay) {
		t.Fatalf("expected %q in changes; got %v", TagBailManifestNameRewriteToRelay, out.Changes)
	}
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] != "-manifest_name" {
			continue
		}
		if strings.HasPrefix(out.Args[i+1], "http://127.0.0.1:32400") {
			t.Errorf("manifest_name still loopback after bail: %q", out.Args[i+1])
		}
		if !strings.Contains(out.Args[i+1], "relay.test:32499") {
			t.Errorf("manifest_name not rewritten to SCALEPLEX_PMS_BASE_URL: %q", out.Args[i+1])
		}
		if !strings.Contains(out.Args[i+1], "X-Plex-Token=tok-xyz") {
			t.Errorf("manifest_name missing X-Plex-Token append: %q", out.Args[i+1])
		}
		// And the filter_complex must carry the normalized form too —
		// proves the bail returned the post-normalize argv, not the
		// raw input.
		for j := 0; j+1 < len(out.Args); j++ {
			if out.Args[j] == "-filter_complex" && strings.Contains(out.Args[j+1], ":#0x") {
				t.Errorf("residual `:#0x` in filter_complex after bail: %q", out.Args[j+1])
			}
		}
		return
	}
	t.Errorf("-manifest_name flag missing from bail output args: %v", out.Args)
}

// HDR source + a plain SDR-target argv: scaleplex does NOT inject a
// tonemap. A plain chain with no tonemap filter is exactly what Plex
// emits when HW tone mapping is off — Plex then does no tonemapping
// itself — so scaleplex matches that rather than second-guessing it.
// HDR is still detected (diagnostic change tag) but the graph stays
// plain.
func TestRewriter_HDRSource_PlainTarget_NoImplicitTonemap(t *testing.T) {
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
	idx := findFilterComplex(out.Args, "[0:0]")
	f := out.Args[idx]
	if strings.Contains(f, "tonemap_vaapi") || strings.Contains(f, "tonemap_opencl") {
		t.Errorf("no tonemap must be injected on a plain HDR→SDR chain:\n%s", f)
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
		// `-loglevel_plex` + `-f ssegment` pass through natively now
		// (scaleplex-ffmpeg7 patch 0098: option sink + segment-muxer
		// AVFMT_GLOBALHEADER); rewriter no longer touches them.
		// -copyts only stripped on seek sessions
		// (`hls:drop:-copyts(seek)`); kept on initial play so chunk PTS
		// rebases via segment.c's reference_stream_first_pts.
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

	// HLS: -f ssegment passes through (patch 0098 + ff_stream_segment_muxer
	// with AVFMT_GLOBALHEADER). Don't expect it to be rewritten.
	// -copyts must REMAIN on initial play (no -ss). Stripping caused a
	// 10s in-chunk PTS offset on PS4 BH6 (chunk-0-loop bug 2026-05-12).
	if indexOfArg(out.Args, "-copyts", 0) < 0 {
		t.Fatalf("-copyts must remain on initial-play HLS argv")
	}
	// -loglevel_plex passes through (patch 0098 stub).
	if indexOfArg(out.Args, "-loglevel_plex", 0) < 0 {
		t.Fatalf("-loglevel_plex must passthrough post-0098 stub; argv: %v", out.Args)
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

// PMS HW-decode + embedded PGS bitmap burn-in. PMS emits its own
// overlay_vaapi graph that sub2video-bridges the PGS stream and
// SW-upscales it inline (no -map_inlineass). Fed straight to
// overlay_vaapi, framesync drains the sparse stream at frame rate and
// the 4K upscale runs flat-out → ~2 cores, transcode collapses. The
// rewriter must reroute the bitmap through the sub pre-render: swap
// the SW-upscale branch for a read of the pre-render's CFR qtrle
// the merged inlineass filter via a -map_inlineass feed (self-decoded by the
// fork, patch 0120 — no dvdsub decode-sink output).
func TestRewriter_HWDecode_PGS_Inlineass(t *testing.T) {
	env := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay.svc:32499",
		"X_PLEX_TOKEN":           "tok123",
	}
	args := append([]string(nil), hwDecodeArgsAV1HEVC...)
	vfIdx := indexOfArg(args, "-filter_complex", 0)
	args[vfIdx+1] = "[0:5]scale=3840:2160,hwupload[0];" +
		"[0:0]hwupload[1];" +
		"[1]scale_vaapi=w=3840:h=2160:format=p010[2];" +
		"[2][0]overlay_vaapi,scale_vaapi=format=p010[3];" +
		"[3]hwupload[4]"
	for i := vfIdx + 1; i+1 < len(args); i++ {
		if args[i] == "-map" && args[i+1] == "[2]" {
			args[i+1] = "[4]"
			break
		}
	}

	out := Rewrite(args, env, nil)
	if !out.Applied {
		t.Fatalf("rewriter NOT applied; changes=%v", out.Changes)
	}
	// Merged inlineass HW branch (patch 0115): the PGS bitmap routes through
	// inlineass (replay_bitmap feed), NOT the FIFO pre-render.
	if !containsString(out.Changes, "hw-decode:filter:bitmap-inlineass-vaapi") {
		t.Fatalf("missing bitmap-inlineass-vaapi change; got %v", out.Changes)
	}
	gotVF := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	if strings.Contains(gotVF, "overlay_vaapi") || strings.Contains(gotVF, "scale=3840:2160,hwupload[0]") {
		t.Errorf("Plex overlay_vaapi sub2video graph must be replaced: %q", gotVF)
	}
	if !strings.Contains(gotVF, "inlineass=render_height=") {
		t.Errorf("inlineass bitmap composite missing: %q", gotVF)
	}
	// -map_inlineass added for the bitmap stream (Plex emits none for PGS).
	if mi := indexOfArg(out.Args, "-map_inlineass", 0); mi < 0 || out.Args[mi+1] != "0:5" {
		t.Errorf("-map_inlineass 0:5 must be added for the PGS feed; args=%v", out.Args)
	}
	// No decode sink appended: patch 0120 self-decodes the -map_inlineass
	// binding (paced), so the old `-map 0:5 -f null -codec dvdsub nullfile`
	// output is gone.
	if indexOfArg(out.Args, "nullfile", 0) >= 0 {
		t.Errorf("bitmap decode sink must NOT be appended (0120 self-decodes): %v", out.Args)
	}
}

// On a seek session the rewriter must shift the FIFO branch up by the
// seek offset before overlay_vaapi: `-start_at_zero` only zeroes the
// muxer output, the filter still sees the main video at PTS seekOff+,
// so a 0-based FIFO would drift cues forward by exactly seekOff.
func TestRewriter_HWDecode_PGS_Seek(t *testing.T) {
	env := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay.svc:32499",
		"X_PLEX_TOKEN":           "tok123",
	}
	args := append([]string(nil), hwDecodeArgsAV1HEVC...)
	// Inject -ss before -i so the rewriter captures a seek offset.
	inputIdx := indexOfArg(args, "-i", 0)
	args = append(args[:inputIdx], append([]string{"-ss", "540"}, args[inputIdx:]...)...)
	vfIdx := indexOfArg(args, "-filter_complex", 0)
	args[vfIdx+1] = "[0:5]scale=3840:2160,hwupload[0];" +
		"[0:0]hwupload[1];" +
		"[1]scale_vaapi=w=3840:h=2160:format=p010[2];" +
		"[2][0]overlay_vaapi,scale_vaapi=format=p010[3];" +
		"[3]hwupload[4]"
	for i := vfIdx + 1; i+1 < len(args); i++ {
		if args[i] == "-map" && args[i+1] == "[2]" {
			args[i+1] = "[4]"
			break
		}
	}

	out := Rewrite(args, env, nil)
	if !out.Applied {
		t.Fatalf("rewriter NOT applied; changes=%v", out.Changes)
	}
	// Seek is native in the merged filter (real PTS): no FIFO, no setpts
	// shift — native seek in the merged filter.
	gotVF := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	if strings.Contains(gotVF, "setpts=") {
		t.Errorf("merged HW branch needs no setpts seek dance: %q", gotVF)
	}
	if !strings.Contains(gotVF, "inlineass=render_height=") {
		t.Errorf("inlineass bitmap composite missing: %q", gotVF)
	}
	if !containsString(out.Changes, "hw-decode:filter:bitmap-inlineass-vaapi") {
		t.Errorf("missing bitmap-inlineass-vaapi change; got %v", out.Changes)
	}
}

// HDR variant of the PGS HW-decode burn — the shape that escaped every
// optimizer before 2026-05-26 and ran sub-realtime (0.37x → the LG buffer
// Frank hit 2026-05-25). Plex splices a tonemap_opencl between the scaled
// video and overlay_vaapi; detectBitmapOverlayBurn matches regardless and
// composeBurn recomposes VA-resident scale_vaapi(p010) → tonemap(honored algo)
// → inlineass(render_height) — no full-frame overlay, no decode→sysmem→
// re-upload round-trip. See project_scaleplex_perf_tuning.
func TestRewriter_HWDecode_PGS_HDR_Tonemap_Inlineass(t *testing.T) {
	env := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay.svc:32499",
		"X_PLEX_TOKEN":           "tok123",
	}
	args := append([]string(nil), hwDecodeArgsAV1HEVC...)
	vfIdx := indexOfArg(args, "-filter_complex", 0)
	// The captured 4K HDR PGS passthrough graph (post tonemap-normalize).
	args[vfIdx+1] = "[0:5]scale=3840:2160,hwupload[0];" +
		"[0:0]hwupload[1];" +
		"[1]scale_vaapi=w=3840:h=2160:format=p010[2];" +
		"[2]hwmap=derive_device=opencl,tonemap_opencl=tonemap=mobius:transfer=bt709:matrix=bt709:primaries=bt709:format=nv12,hwmap=derive_device=vaapi:reverse=1[5];" +
		"[5][0]overlay_vaapi,scale_vaapi=format=nv12[6];" +
		"[6]hwupload[7]"
	for i := vfIdx + 1; i+1 < len(args); i++ {
		if args[i] == "-map" && args[i+1] == "[2]" {
			args[i+1] = "[7]"
			break
		}
	}

	out := Rewrite(args, env, nil)
	if !out.Applied {
		t.Fatalf("rewriter NOT applied; changes=%v", out.Changes)
	}
	if !containsString(out.Changes, "hw-decode:filter:bitmap-inlineass-vaapi:hdr-tonemap(mobius)") {
		t.Fatalf("missing hdr-tonemap bitmap change; got %v", out.Changes)
	}
	gotVF := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	// The ~5x hog (full-frame overlay) + the sysmem round-trip are gone.
	for _, banned := range []string{"overlay_vaapi", "scale=3840:2160,hwupload[0]", "[0:0]hwupload"} {
		if strings.Contains(gotVF, banned) {
			t.Errorf("graph must not contain %q: %q", banned, gotVF)
		}
	}
	// VA-resident scale → p010 (HDR) → honored tonemap → inlineass(band).
	for _, must := range []string{
		"[0:0]scale_vaapi=w=3840:h=2160:format=p010",
		"tonemap_opencl=tonemap=mobius",
		"inlineass=render_height=1080",
	} {
		if !strings.Contains(gotVF, must) {
			t.Errorf("graph missing %q: %q", must, gotVF)
		}
	}
	if !containsString(out.Changes, "tonemap:ocl:inject-opencl-device") {
		t.Errorf("opencl device must be injected for the tonemap: %v", out.Changes)
	}
	if of := indexOfArg(out.Args, "-hwaccel_output_format:0", 0); of < 0 || of+1 >= len(out.Args) || out.Args[of+1] != "vaapi" {
		t.Errorf("expected -hwaccel_output_format:0 vaapi (VA-resident decode); args=%v", out.Args)
	}
	if mi := indexOfArg(out.Args, "-map_inlineass", 0); mi < 0 || out.Args[mi+1] != "0:5" {
		t.Errorf("-map_inlineass 0:5 must be added for the PGS feed: %v", out.Args)
	}
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-map" && out.Args[i+1] == "[7]" {
			t.Errorf("stale -map [7] not retargeted to the new label: %v", out.Args)
		}
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
	if !containsString(out.Changes, "filter:text-inlineass-vaapi") {
		t.Fatalf("expected filter:text-inlineass-vaapi; got %v", out.Changes)
	}
	if !containsString(out.Changes, "map-label-update") {
		t.Fatalf("MAP NOT UPDATED — this is the bug. changes=%v", out.Changes)
	}
	// composeBurn's text output is [2]; the input's trailing label is also [2],
	// so the retarget is a no-op and the video -map stays [2] — now pointing at
	// the inlineass output. Assert it resolves (no orphaned label).
	foundVideoMap := false
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-map" && out.Args[i+1] == "[2]" {
			foundVideoMap = true
			break
		}
	}
	if !foundVideoMap {
		t.Fatalf("video -map should resolve to composeBurn's [2] output: %v", out.Args)
	}
	// Unified text reshape keeps -map_inlineass + the sidecar -i. The fork
	// reads sub packets via that side-channel; we must NOT strip them.
	if indexOfArg(out.Args, "-map_inlineass", 0) < 0 {
		t.Errorf("-map_inlineass should be kept in pass-through mode")
	}
	if containsString(out.Changes, "drop:-i(sidecar-input)") {
		t.Errorf("sidecar -i must NOT be dropped in pass-through mode: %v", out.Changes)
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

// TestStripPlexInlineassFilterArgs removed in patch 0119: the strip helper
// is gone — the fork's vf_inlineass now parses Plex's overrides/outline/
// shadow/language keys directly, so the rewriter passes them through.

func TestRewriter_InlineassPassthrough_SW_KeepsSidecarAndStrip(t *testing.T) {
	// pass-through is hardcoded since B6 (PR #4); no env knob
	args := []string{
		"-loglevel", "quiet",
		"-codec:0", "libdav1d",
		"-i", "/media/x.mkv",
		"-ss", "0",
		"-codec:1", "subrip",
		"-i", "/transcode/Sub/temp-0.srt",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]scale=w=1280:h=720:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1];[1]inlineass=font_scale=1.0:font_path=/x:fontconfig_file=/y:language=en:overrides=foo:outline=2.6:shadow=1.7:font_size=54[2]",
		"-map", "[2]",
		"-codec:0", "libx264",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists: func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string {
			return "subrip"
		},
	})
	if !out.Applied {
		t.Fatalf("expected rewrite; changes=%v", out.Changes)
	}
	// 1. inlineass= filter retained, Plex-only keys stripped.
	vfIdx := -1
	for i, a := range out.Args {
		if a == "-filter_complex" {
			vfIdx = i + 1
			break
		}
	}
	if vfIdx < 0 {
		t.Fatal("missing -filter_complex in output")
	}
	if !strings.Contains(out.Args[vfIdx], "inlineass=") {
		t.Errorf("inlineass= dropped from filter: %s", out.Args[vfIdx])
	}
	// Plex's styling keys now pass through (fork 0119 parses them).
	for _, key := range []string{":language=", ":overrides=", ":outline=", ":shadow="} {
		if !strings.Contains(out.Args[vfIdx], key) {
			t.Errorf("Plex styling key %q must pass through: %s", key, out.Args[vfIdx])
		}
	}
	// 2. -map_inlineass kept.
	if indexOfArg(out.Args, "-map_inlineass", 0) < 0 {
		t.Errorf("-map_inlineass dropped (should pass through)")
	}
	// 3. Sidecar -i kept (count of -i flags).
	iCount := 0
	for _, a := range out.Args {
		if a == "-i" {
			iCount++
		}
	}
	if iCount != 2 {
		t.Errorf("expected 2 -i (sidecar kept), got %d", iCount)
	}
	// 4. Null-sub decode sink: KEPT for sidecar bindings (file_idx >= 1) —
	//    patch 0120's sink-less self-decode doesn't pump the sidecar
	//    demuxer (scheduler choke after first packet; live-validated
	//    2026-05-26). The decode sink is the only thing that pulls the
	//    SRT through. For embedded subs (file_idx == 0) the sink IS
	//    stripped because the main demuxer is already pumped by video.
	if indexOfArg(out.Args, "nullfile", 0) < 0 {
		t.Errorf("sidecar null-sub decode sink must be KEPT: %v", out.Args)
	}
	// 5. Mode tag — unified text sub-burn via composeBurn.
	if !containsString(out.Changes, "filter:text-inlineass-vaapi") {
		t.Errorf("missing text-inlineass-vaapi change tag: %v", out.Changes)
	}
}

// HW-decode + SRT sidecar burn-in routes to the GPU-overlay
// pre-render path: the inlineass bracket is replaced by an
// overlay_vaapi graph, an overlay FIFO is appended as a third input,
// the merged inlineass filter composites it on the VAAPI surface.
func TestRewriter_HWText_SRT_Sidecar(t *testing.T) {
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc",
		"-i", "/media/x.mkv",
		"-codec:1", "subrip",
		"-i", "/transcode/Sub/temp-0.srt",
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:language=en:overrides=foo:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied {
		t.Fatalf("expected rewrite; changes=%v", out.Changes)
	}
	vfIdx := findFilterComplex(out.Args, "[0:0]")
	if vfIdx < 0 {
		t.Fatal("missing -filter_complex")
	}
	graph := out.Args[vfIdx]
	if !strings.Contains(graph, "inlineass=") {
		t.Errorf("inlineass missing on merged HW branch: %s", graph)
	}
	if strings.Contains(graph, "overlay_vaapi") || strings.Contains(graph, "hwdownload") {
		t.Errorf("overlay_vaapi + hwdownload/hwupload bracket must be gone: %s", graph)
	}
	if !strings.Contains(graph, "language=") || !strings.Contains(graph, "overrides=") {
		t.Errorf("Plex styling keys must pass through (fork 0119 parses them): %s", graph)
	}
	if !strings.Contains(graph, ":render_height=") {
		t.Errorf("render_height option missing: %s", graph)
	}
	if mi := indexOfArg(out.Args, "-map_inlineass", 0); mi < 0 || out.Args[mi+1] != "1:s:0" {
		t.Error("-map_inlineass 1:s:0 must be kept")
	}
	iCount := 0
	for _, a := range out.Args {
		if a == "-i" {
			iCount++
		}
	}
	if iCount != 2 {
		t.Errorf("expected 2 -i (source + sidecar, no FIFO), got %d", iCount)
	}
	// Sidecar binding (1:s:0): decode sink MUST be kept. 0120's
	// sink-less self-decode doesn't pump the sidecar demuxer past
	// the first packet (live-validated 2026-05-26).
	if indexOfArg(out.Args, "nullfile", 0) < 0 {
		t.Errorf("sidecar decode sink (nullfile) must be KEPT: %v", out.Args)
	}
	if containsString(out.Changes, "drop:inlineass-decode-sink") {
		t.Errorf("sidecar must NOT trigger drop:inlineass-decode-sink: %v", out.Changes)
	}
	if !containsString(out.Changes, "hw-decode:filter:inlineass-vaapi") {
		t.Errorf("missing inlineass-vaapi tag: %v", out.Changes)
	}
}

// Embedded ASS can't be scanned for animation tags (no sidecar file
// on disk), so it conservatively keeps the per-frame inlineass path
// rendered per-frame by the merged inlineass filter (no pre-render).
// On a seek session the overlay graph rebases both branches to 0
// before overlay_vaapi and rebases the composite back to the seek
// offset, so framesync gets a 0-based pair and the main-video
// timeline reaching dashenc is unchanged.
func TestRewriter_HWText_Seek(t *testing.T) {
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc",
		"-ss", "1800",
		"-i", "/media/x.mkv",
		"-codec:1", "subrip",
		"-ss", "1800",
		"-i", "/transcode/Sub/temp-0.srt",
		"-start_at_zero", "-copyts",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied {
		t.Fatalf("expected rewrite; changes=%v", out.Changes)
	}
	vfIdx := findFilterComplex(out.Args, "[0:0]")
	if vfIdx < 0 {
		t.Fatal("missing -filter_complex")
	}
	graph := out.Args[vfIdx]
	// Native seek: the merged filter sees real PTS - no setpts rebase dance.
	if strings.Contains(graph, "setpts=") {
		t.Errorf("merged HW branch needs no setpts seek dance: %s", graph)
	}
	if !strings.Contains(graph, "inlineass=") {
		t.Errorf("inlineass missing: %s", graph)
	}
	if strings.Contains(graph, "overlay_vaapi") {
		t.Errorf("overlay_vaapi must be gone: %s", graph)
	}
}

func TestRewriter_HWText_OpenCLTonemap(t *testing.T) {
	// PMS emits this when HW tone mapping is ON: the video branch
	// scale_vaapi(p010) → hwmap(opencl) → tonemap_opencl(<algo>) →
	// hwdownload before inlineass. The sub pre-render rewrite must keep
	// the tone map — dropping it renders HDR washed.
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc",
		"-i", "/media/x.mkv",
		"-codec:1", "subrip",
		"-i", "/transcode/Sub/temp-0.srt",
		"-start_at_zero", "-copyts",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=p010[1];" +
			"[1]hwmap=derive_device=opencl[2];" +
			"[2]tonemap_opencl=tonemap=mobius:format=nv12:m=bt709:p=bt709:r=tv[3];" +
			"[3]hwdownload,format=nv12[4];" +
			"[4]inlineass=font_scale=1.0:font_path=/x:language=en:overrides=foo:outline=2:shadow=1:font_size=54[5];" +
			"[5]hwupload[6]",
		"-map", "[6]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied {
		t.Fatalf("expected rewrite; changes=%v", out.Changes)
	}
	vfIdx := findFilterComplex(out.Args, "[0:0]")
	if vfIdx < 0 {
		t.Fatal("missing -filter_complex")
	}
	graph := out.Args[vfIdx]
	if !strings.Contains(graph, "inlineass=") {
		t.Errorf("inlineass missing on merged HW branch: %s", graph)
	}
	if strings.Contains(graph, "overlay_vaapi") {
		t.Errorf("overlay_vaapi must be gone: %s", graph)
	}
	// The tone map must survive - default mode preserves Plex's algo.
	if !strings.Contains(graph, "tonemap_opencl=tonemap=mobius") {
		t.Errorf("tonemap dropped - HDR would render washed: %s", graph)
	}
	if !strings.Contains(graph, "scale_vaapi=w=1280:h=720:format=p010") {
		t.Errorf("scale must target p010 ahead of the tonemap: %s", graph)
	}
	// PMS map [6] must retarget to the merged graph output [4].
	if containsString(out.Args, "[6]") {
		t.Errorf("-map [6] not retargeted to [4]: %v", out.Args)
	}
	if !containsString(out.Changes, "hw-decode-sub:tonemap-preserved(mobius)") {
		t.Errorf("missing tonemap-preserved tag: %v", out.Changes)
	}
}

func TestRewriter_InlineassPassthrough_HW_EmbeddedASS(t *testing.T) {
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc",
		"-i", "/media/x.mkv",
		"-map_inlineass", "0:3",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1];[1]hwdownload,format=nv12[2];[2]inlineass=font_scale=1.0:font_path=/x:language=en:overrides=foo:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "0:3", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "ass" },
	})
	if !out.Applied {
		t.Fatalf("expected rewrite; changes=%v", out.Changes)
	}
	vfIdx := findFilterComplex(out.Args, "[0:0]")
	if vfIdx < 0 {
		t.Fatal("missing -filter_complex")
	}
	if !strings.Contains(out.Args[vfIdx], "inlineass=") {
		t.Errorf("inlineass should be kept for embedded ASS: %s", out.Args[vfIdx])
	}
	// Embedded ASS is conservatively treated as animated -> tier-down toggle.
	if !strings.Contains(out.Args[vfIdx], "animated_tier_down=1") {
		t.Errorf("embedded ASS should enable animated_tier_down: %s", out.Args[vfIdx])
	}
	if !containsString(out.Changes, "hw-decode:filter:inlineass-vaapi") {
		t.Errorf("missing inlineass-vaapi tag: %v", out.Changes)
	}
}

// SW-decode reshape + embedded ASS: composeBurn must emit animated_tier_down=1
// (embedded ASS has no readable file → subtitleIsAnimated conservatively true).
// Parity with the HW-decode-text path (TestRewriter_InlineassPassthrough_HW_EmbeddedASS).
func TestRewriter_SWReshape_EmbeddedASS_AnimatedTierDown(t *testing.T) {
	args := []string{
		"-codec:0", "libdav1d",
		"-i", "/media/anime.mkv",
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-map_inlineass", "0:3",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1];[1]inlineass=font_scale=1.0:font_size=54[2]",
		"-map", "[2]",
		"-codec:0", "libx264", "-crf:0", "21", "-preset:0", "veryfast",
		"-segment_format", "mpegts", "-f", "ssegment", "-segment_time", "1",
		"-segment_start_number", "0", "media-%05d.ts",
		"-map", "0:3", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, map[string]string{}, &RewriteOpts{
		ProbeSubtitleCodec: func(string, string) string { return "ass" },
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	vfIdx := findFilterComplex(out.Args, "[0:0]")
	if vfIdx < 0 {
		t.Fatal("missing -filter_complex")
	}
	graph := out.Args[vfIdx]
	if !strings.Contains(graph, "animated_tier_down=1") {
		t.Errorf("embedded ASS on SW reshape must enable animated_tier_down: %s", graph)
	}
	if !containsString(out.Changes, "filter:text-inlineass-vaapi") {
		t.Errorf("missing filter:text-inlineass-vaapi tag: %v", out.Changes)
	}
}

// SW-decode reshape + sidecar SRT: composeBurn must NOT emit animated_tier_down
// (SRT carries no override tags → subtitleIsAnimated returns false). Guards
// against a regression that would tier-down every text sub.
func TestRewriter_SWReshape_SidecarSRT_NoTierDown(t *testing.T) {
	dir := t.TempDir()
	srt := filepath.Join(dir, "temp-0.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:04,000\nHello\n\n"), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	args := []string{
		"-codec:0", "hevc",
		"-i", "/media/m.mkv",
		"-i", srt,
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1];[1]inlineass=font_size=54[2]",
		"-map", "[2]",
		"-codec:0", "libx264", "-crf:0", "21", "-preset:0", "veryfast",
		"-segment_format", "mpegts", "-f", "ssegment", "-segment_time", "1",
		"-segment_start_number", "0", "media-%05d.ts",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, map[string]string{}, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	vfIdx := findFilterComplex(out.Args, "[0:0]")
	if vfIdx < 0 {
		t.Fatal("missing -filter_complex")
	}
	if strings.Contains(out.Args[vfIdx], "animated_tier_down") {
		t.Errorf("sidecar SRT must NOT enable animated_tier_down: %s", out.Args[vfIdx])
	}
}

// HDR-source change-tag must surface exactly ONCE per session, even
// on the path that historically hit two emit sites: HW-decode +
// text-sub-burn + HDR source. Pre-fix (sha-c53f4b4 / v1.7.0) the tag
// fired from both the HW-decode passthrough block AND the
// HW-decode-sub-burn text branch, so a single session emitted
// `video:hdr-source(smpte2084),video:hdr-source(smpte2084),...`.
// Hoisted to a session-level probe in the post-v1.7.0 dedupe;
// this test pins the contract.
//
// `[KNOWN: DupHDRTag]` close — release-gate sweep 2026-05-26.
func TestRewriter_HDRSource_EmittedExactlyOnce_HWDecodeTextBurnHDR(t *testing.T) {
	probe := func(string) (transfer, primaries, space string) {
		return "smpte2084", "bt2020", "bt2020nc"
	}
	// Same shape as TestRewriter_HWText_OpenCLTonemap (HW decode +
	// embedded SRT burn + HDR), but with HDR probe wired so the tag
	// flows through the SW-decode probe + the HW-decode-passthrough
	// path + the HW-decode-text-sub-burn sub-branch.
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc",
		"-i", "/media/x.mkv",
		"-codec:1", "subrip",
		"-i", "/transcode/Sub/temp-0.srt",
		"-start_at_zero", "-copyts",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=p010[1];" +
			"[1]hwmap=derive_device=opencl[2];" +
			"[2]tonemap_opencl=tonemap=mobius:format=nv12:m=bt709:p=bt709:r=tv[3];" +
			"[3]hwdownload,format=nv12[4];" +
			"[4]inlineass=font_scale=1.0:font_path=/x:language=en:overrides=foo:outline=2:shadow=1:font_size=54[5];" +
			"[5]hwupload[6]",
		"-map", "[6]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeVideoColor:    probe,
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	count := 0
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "video:hdr-source(") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 video:hdr-source(...) tag, got %d: %v", count, out.Changes)
	}
	if !containsString(out.Changes, "video:hdr-source(smpte2084)") {
		t.Errorf("expected video:hdr-source(smpte2084): %v", out.Changes)
	}
}

// Same single-emit contract for SW-reshape + sub-burn + HDR (the
// "honor:plex-sw" + "force-hw:reshape-hybrid" + classic SW reshape
// paths all share one hoisted probe).
func TestRewriter_HDRSource_EmittedExactlyOnce_SWReshape(t *testing.T) {
	probe := func(string) (string, string, string) {
		return "smpte2084", "bt2020", "bt2020nc"
	}
	out := Rewrite(swArgsAV1H264, nil, &RewriteOpts{ProbeVideoColor: probe})
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	count := 0
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "video:hdr-source(") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 video:hdr-source(...) tag, got %d: %v", count, out.Changes)
	}
}
