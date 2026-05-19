package main

import (
	"strings"
	"testing"
)

func argvHasPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

func argvVal(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func TestBuildSubPrerenderArgs_Basic(t *testing.T) {
	spec := &SubPrerenderSpec{
		FIFOPath: "/transcode/s/scaleplex-sub-overlay.fifo",
		Width:    1920,
		Height:   1080,
	}
	args := buildSubPrerenderArgs(spec, "/transcode/s/temp-0.srt")

	if !strings.Contains(argvVal(args, "-i"), "color=c=black@0.0:s=1920x1080:r=5") {
		t.Errorf("canvas wrong: %q", argvVal(args, "-i"))
	}
	vf := argvVal(args, "-vf")
	if !strings.Contains(vf, "subtitles=/transcode/s/temp-0.srt") {
		t.Errorf("subtitles filter missing/wrong: %q", vf)
	}
	// alpha=1 is mandatory — without it the text renders with alpha 0
	// on the transparent canvas and the overlay is invisible.
	if !strings.Contains(vf, ":alpha=1") {
		t.Errorf("subtitles filter missing alpha=1: %q", vf)
	}
	// mpdecimate must NOT be present in any form — even bounded
	// (max=N) it leaves overlay gaps that overrun the main decoder's
	// VAAPI surface pool. See the subPrerenderFPS comment.
	if strings.Contains(vf, "mpdecimate") {
		t.Errorf("mpdecimate must not be applied (overruns surface pool): %q", vf)
	}
	if strings.Contains(vf, "setpts=") {
		t.Errorf("no seek → no setpts expected: %q", vf)
	}
	// qtrle (inter-frame, lossless, alpha) into fragmented MOV — see
	// buildSubPrerenderArgs. argb is qtrle's pixel format.
	if !argvHasPair(args, "-c:v", "qtrle") {
		t.Error("qtrle encoder missing")
	}
	if !strings.HasSuffix(vf, ",format=argb") {
		t.Errorf("vf must end with format=argb for qtrle: %q", vf)
	}
	if !argvHasPair(args, "-fps_mode", "vfr") {
		t.Error("-fps_mode vfr missing")
	}
	if !argvHasPair(args, "-f", "mov") {
		t.Error("mov muxer missing")
	}
	if !argvHasPair(args, "-movflags", "frag_keyframe+empty_moov+default_base_moof") {
		t.Error("streamable MOV -movflags missing")
	}
	if args[len(args)-1] != spec.FIFOPath {
		t.Errorf("output not the FIFO: %q", args[len(args)-1])
	}
	// BandHeight unset (0) → no crop (full frame).
	if strings.Contains(vf, "crop=") {
		t.Errorf("no crop expected when BandHeight unset: %q", vf)
	}
}

func TestSubPrerenderBandHeight(t *testing.T) {
	// bottom 2/5, rounded even.
	cases := []struct{ h, want int }{
		{1600, 640}, {1080, 432}, {536, 214}, {720, 288}, {2160, 864},
	}
	for _, c := range cases {
		if got := subPrerenderBandHeight(c.h); got != c.want {
			t.Errorf("subPrerenderBandHeight(%d) = %d, want %d", c.h, got, c.want)
		}
		if subPrerenderBandHeight(c.h)%2 != 0 {
			t.Errorf("band height for %d is odd", c.h)
		}
	}
}

func TestBuildSubPrerenderArgs_Band(t *testing.T) {
	// SRT → BandHeight < Height → render full frame, crop the bottom band.
	spec := &SubPrerenderSpec{
		FIFOPath:   "/t/f.fifo",
		Width:      3840,
		Height:     1600,
		BandHeight: 640,
	}
	vf := argvVal(buildSubPrerenderArgs(spec, "/t/s.srt"), "-vf")
	// Canvas is still the FULL frame — libass needs it for positioning.
	if !strings.Contains(argvVal(buildSubPrerenderArgs(spec, "/t/s.srt"), "-i"), "s=3840x1600") {
		t.Error("canvas must stay full-frame for correct libass positioning")
	}
	// Crop the bottom 640 band (y = 1600-640).
	if !strings.Contains(vf, "crop=3840:640:0:960") {
		t.Errorf("expected bottom-band crop: %q", vf)
	}
	// crop sits after subtitles, before format=argb.
	if strings.Index(vf, "subtitles=") > strings.Index(vf, "crop=") ||
		strings.Index(vf, "crop=") > strings.Index(vf, "format=argb") {
		t.Errorf("filter order wrong (want subtitles,crop,format=argb): %q", vf)
	}
}

func TestBuildSubPrerenderArgs_FullFrameWhenBandEqualsHeight(t *testing.T) {
	// ASS → BandHeight == Height → no crop, full frame emitted.
	spec := &SubPrerenderSpec{
		FIFOPath:   "/t/f.fifo",
		Width:      1920,
		Height:     1080,
		BandHeight: 1080,
	}
	vf := argvVal(buildSubPrerenderArgs(spec, "/t/s.ass"), "-vf")
	if strings.Contains(vf, "crop=") {
		t.Errorf("BandHeight==Height must not crop: %q", vf)
	}
}

func TestBuildSubPrerenderArgs_SeekOffset(t *testing.T) {
	spec := &SubPrerenderSpec{
		FIFOPath:          "/t/f.fifo",
		Width:             1280,
		Height:            720,
		SeekOffsetSeconds: 83.5,
	}
	vf := argvVal(buildSubPrerenderArgs(spec, "/t/s.srt"), "-vf")
	// A single up-shift to the seek offset: the subtitles filter picks
	// the cue at that point and the overlay output PTS matches the
	// seeked main video (which keeps real timestamps via -copyts). No
	// down-shift — the overlay must stay at the seek offset.
	if !strings.HasPrefix(vf, "setpts=PTS+83.500/TB,subtitles=") {
		t.Errorf("missing up-shift before subtitles: %q", vf)
	}
	if strings.Contains(vf, "setpts=PTS-") {
		t.Errorf("overlay must stay at the seek offset, no down-shift: %q", vf)
	}
}

func TestBuildSubPrerenderArgs_EscapesPath(t *testing.T) {
	spec := &SubPrerenderSpec{FIFOPath: "/t/f.fifo", Width: 1920, Height: 1080}
	vf := argvVal(buildSubPrerenderArgs(spec, "/media/Movie: The Thing/sub.srt"), "-vf")
	if !strings.Contains(vf, `Movie\: The Thing`) {
		t.Errorf("filter-path colon not escaped: %q", vf)
	}
}

func TestBuildSubPrerenderArgs_NoForceStyle(t *testing.T) {
	// force_style must NOT be applied: Plex's font sizes assume a
	// render-height PlayRes, but the stock subtitles filter renders a
	// raw SRT at libass's 384x288 default, so applying them oversizes
	// the text ~3-4x. libass's default SRT style is correctly sized.
	spec := &SubPrerenderSpec{
		FIFOPath:   "/t/f.fifo",
		Width:      1920,
		Height:     1080,
		ForceStyle: "FontSize=54,Outline=2.6",
	}
	vf := argvVal(buildSubPrerenderArgs(spec, "/t/s.srt"), "-vf")
	if strings.Contains(vf, "force_style") {
		t.Errorf("force_style must not be applied (oversizes text): %q", vf)
	}
}

func TestSubPrerenderEnv_HomeOverride(t *testing.T) {
	env := subPrerenderEnv()
	homeCount := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			homeCount++
			if kv != "HOME=/home/ubuntu" {
				t.Errorf("HOME = %q, want /home/ubuntu", kv)
			}
		}
	}
	if homeCount != 1 {
		t.Errorf("expected exactly one HOME entry, got %d", homeCount)
	}
}

func argvHasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func TestBuildBitmapPrerenderArgs(t *testing.T) {
	spec := &SubPrerenderSpec{
		FIFOPath:   "/transcode/s/scaleplex-sub-overlay.fifo",
		SourcePath: "/media/Movies/Avatar.mkv",
		StreamSpec: "0:5",
		Embedded:   true,
		Bitmap:     true,
		Width:      3840,
		Height:     2160,
	}
	args := buildSubPrerenderArgs(spec, spec.SourcePath)

	// reads the source media directly; video + audio streams skipped
	if !argvHasPair(args, "-i", "/media/Movies/Avatar.mkv") {
		t.Errorf("source -i missing: %v", args)
	}
	if !argvHasFlag(args, "-vn") || !argvHasFlag(args, "-an") {
		t.Errorf("-vn/-an (skip video+audio decode) missing: %v", args)
	}
	// -copyts is mandatory: without it ffmpeg rebases the first PGS cue
	// (rarely at 0) to PTS 0, shifting the whole overlay timeline early.
	if !argvHasFlag(args, "-copyts") {
		t.Errorf("-copyts missing — overlay timeline would rebase: %v", args)
	}
	fc := argvVal(args, "-filter_complex")
	// bitmap path: no libass subtitles=
	if strings.Contains(fc, "subtitles=") {
		t.Errorf("bitmap path must not use libass subtitles=: %q", fc)
	}
	// CFR `color` canvas drives the output timeline (input 0); `fps`
	// after the scale would rebase the sub2video to PTS 0 regardless of
	// -copyts (bug repro 2026-05-19), so the canvas+overlay form is the
	// correct shape.
	if !argvHasPair(args, "-f", "lavfi") {
		t.Error("lavfi canvas input missing")
	}
	canvasIn := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" && strings.Contains(args[i+1], "color=c=black@0.0:s=3840x2160:r=5") {
			canvasIn = true
			break
		}
	}
	if !canvasIn {
		t.Errorf("canvas -i (color@0.0:s=3840x2160:r=5) missing: %v", args)
	}
	// Source becomes input 1 in the pre-render, so the StreamSpec
	// remaps "0:5" → "[1:5]" in the filter.
	if !strings.Contains(fc, "[1:5]scale=3840:2160[sub]") {
		t.Errorf("source-input streamspec not remapped to [1:5]: %q", fc)
	}
	// Overlay onto the canvas with repeatlast/eof_action=pass.
	if !strings.Contains(fc, "[0:v][sub]overlay=eof_action=pass:repeatlast=1") {
		t.Errorf("overlay onto canvas missing: %q", fc)
	}
	if !strings.HasSuffix(fc, ",format=argb[o]") {
		t.Errorf("filter must end format=argb[o] for qtrle: %q", fc)
	}
	// fps= must NOT appear — the canvas is the rate driver.
	if strings.Contains(fc, "fps=") {
		t.Errorf("fps filter must not appear (canvas drives the rate): %q", fc)
	}
	// Multi-thread the filter graph: scale + canvas branches can run
	// in parallel before merging at overlay.
	if !argvHasPair(args, "-filter_complex_threads", "4") {
		t.Error("-filter_complex_threads 4 missing")
	}
	if !argvHasPair(args, "-c:v", "qtrle") {
		t.Error("qtrle encoder missing")
	}
	if !argvHasPair(args, "-f", "mov") {
		t.Error("mov muxer missing")
	}
	if args[len(args)-1] != spec.FIFOPath {
		t.Errorf("output not the FIFO: %q", args[len(args)-1])
	}
	if strings.Contains(fc, "setpts=") {
		t.Errorf("no seek → no setpts expected: %q", fc)
	}
}

func TestBuildBitmapPrerenderArgs_Seek(t *testing.T) {
	spec := &SubPrerenderSpec{
		FIFOPath:          "/transcode/s/scaleplex-sub-overlay.fifo",
		SourcePath:        "/media/x.mkv",
		StreamSpec:        "0:5",
		Bitmap:            true,
		Width:             3840,
		Height:            2160,
		SeekOffsetSeconds: 601.5,
	}
	args := buildSubPrerenderArgs(spec, spec.SourcePath)
	if !argvHasPair(args, "-ss", "601.500") {
		t.Errorf("-ss seek missing: %v", args)
	}
	if !argvHasFlag(args, "-copyts") {
		t.Errorf("-copyts missing on seek session: %v", args)
	}
	// On seek the canvas itself carries the setpts shift so the CFR
	// timeline starts at the seek offset (matching the main video's
	// -copyts stream). The filter chain stays setpts-free.
	canvasArg := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" && strings.Contains(args[i+1], "color=c=black") {
			canvasArg = args[i+1]
			break
		}
	}
	if !strings.Contains(canvasArg, "setpts=PTS+601.500/TB") {
		t.Errorf("seek canvas setpts shift missing: %q", canvasArg)
	}
	fc := argvVal(args, "-filter_complex")
	if strings.Contains(fc, "setpts=") {
		t.Errorf("filter chain must NOT have setpts (canvas carries it): %q", fc)
	}
}

// Band-crop reduces the qtrle/format/main-overlay work to the bottom
// slice of the frame. Sub is scaled full (positioning) then cropped.
func TestBuildBitmapPrerenderArgs_BandCrop(t *testing.T) {
	spec := &SubPrerenderSpec{
		FIFOPath:   "/transcode/s/scaleplex-sub-overlay.fifo",
		SourcePath: "/media/Movies/Avatar.mkv",
		StreamSpec: "0:5",
		Embedded:   true,
		Bitmap:     true,
		Width:      3840,
		Height:     2160,
		BandHeight: 864,
	}
	args := buildSubPrerenderArgs(spec, spec.SourcePath)

	// canvas is BAND-sized (band height, not full)
	canvasIn := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" && strings.Contains(args[i+1], "color=c=black") {
			canvasIn = args[i+1]
			break
		}
	}
	if !strings.Contains(canvasIn, "s=3840x864") {
		t.Errorf("canvas not band-sized: %q", canvasIn)
	}
	fc := argvVal(args, "-filter_complex")
	// sub scaled to FULL frame, then cropped to bottom band
	if !strings.Contains(fc, "[1:5]scale=3840:2160,crop=3840:864:0:1296[sub]") {
		t.Errorf("sub branch scale+crop wrong: %q", fc)
	}
}
