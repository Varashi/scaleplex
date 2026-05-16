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
	if !strings.HasSuffix(vf, ",mpdecimate") {
		t.Errorf("mpdecimate missing: %q", vf)
	}
	if strings.Contains(vf, "setpts=") {
		t.Errorf("no seek → no setpts expected: %q", vf)
	}
	if !argvHasPair(args, "-c:v", "ffv1") {
		t.Error("ffv1 encoder missing")
	}
	if !argvHasPair(args, "-fps_mode", "vfr") {
		t.Error("-fps_mode vfr missing")
	}
	if !argvHasPair(args, "-f", "matroska") {
		t.Error("matroska muxer missing")
	}
	if args[len(args)-1] != spec.FIFOPath {
		t.Errorf("output not the FIFO: %q", args[len(args)-1])
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
