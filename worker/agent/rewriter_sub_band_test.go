package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sidecar SRT burn-in: the rewriter emits the static-fallback band as
// the placeholder + a sentinel y= in overlay_vaapi, plus the
// `sub-prerender:band:agent-resolve` tag. The actual tight-band
// decision (parsing the SRT) happens in the agent — see
// TestAgentBandResolve_SidecarTight.
func TestRewriter_SubPrerender_SRT_Sidecar_EmitsSentinel(t *testing.T) {
	dir := t.TempDir()
	srt := filepath.Join(dir, "temp-0.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:04,000\nHello world\n\n"), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc",
		"-i", "/media/x.mkv",
		"-codec:1", "subrip",
		"-i", srt,
		"-start_at_zero", "-copyts", "-fps_mode", "cfr",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied || out.SubPrerender == nil {
		t.Fatalf("expected rewrite + SubPrerender; changes=%v", out.Changes)
	}
	sp := out.SubPrerender
	if !sp.ResolveBandPostExtract {
		t.Errorf("ResolveBandPostExtract = false, want true for SRT path")
	}
	if got, want := sp.BandHeight, subPrerenderBandHeight(2160); got != want {
		t.Errorf("BandHeight = %d, want %d (static fallback at rewrite time)", got, want)
	}
	graph := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	if !strings.Contains(graph, "overlay_vaapi=x=0:y="+BandYSentinel+":") {
		t.Errorf("filter graph missing sentinel y= placeholder: %s", graph)
	}
	hasAgentTag, hasTightTag := false, false
	for _, c := range out.Changes {
		switch c {
		case "sub-prerender:band:agent-resolve":
			hasAgentTag = true
		case "sub-prerender:band:tight":
			hasTightTag = true
		}
	}
	if !hasAgentTag {
		t.Errorf("missing sub-prerender:band:agent-resolve tag: %v", out.Changes)
	}
	if hasTightTag {
		t.Errorf("rewriter shouldn't emit :tight (it's now resolved agent-side): %v", out.Changes)
	}
}

// Embedded SRT path also emits the sentinel + agent-resolve flag — same
// code path as sidecar in v1.2.2 (was static-band-only in v1.2.1).
func TestRewriter_SubPrerender_SRT_Embedded_EmitsSentinel(t *testing.T) {
	args := []string{
		"-loglevel", "quiet",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-codec:0", "hevc",
		"-i", "/media/x.mkv",
		"-map_inlineass", "0:3",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
	}
	out := Rewrite(args, nil, &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	})
	if !out.Applied || out.SubPrerender == nil {
		t.Fatalf("expected rewrite + SubPrerender; changes=%v", out.Changes)
	}
	sp := out.SubPrerender
	if !sp.ResolveBandPostExtract {
		t.Errorf("ResolveBandPostExtract = false, want true for embedded SRT")
	}
	if !sp.Embedded {
		t.Errorf("Embedded = false, want true (no sidecar file path)")
	}
	if got, want := sp.BandHeight, subPrerenderBandHeight(2160); got != want {
		t.Errorf("BandHeight = %d, want %d (static fallback at rewrite time)", got, want)
	}
	graph := out.Args[indexOfArg(out.Args, "-filter_complex", 0)+1]
	if !strings.Contains(graph, "overlay_vaapi=x=0:y="+BandYSentinel+":") {
		t.Errorf("filter graph missing sentinel y= placeholder: %s", graph)
	}
}

// Agent-side resolver: a plain bottom-aligned SRT picks the tight band
// and the patch helper rewrites the main argv's sentinel y= to the
// resolved offset.
func TestAgentBandResolve_SidecarTight(t *testing.T) {
	dir := t.TempDir()
	srt := filepath.Join(dir, "temp-0.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:04,000\nHello world\n\n"), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	spec := &SubPrerenderSpec{
		FIFOPath:               "/tmp/x.fifo",
		SourcePath:             srt,
		Width:                  3840,
		Height:                 2160,
		BandHeight:             subPrerenderBandHeight(2160), // rewriter's static fallback
		ResolveBandPostExtract: true,
	}
	mainArgs := []string{
		"-filter_complex",
		"[0:0]hwupload[10];[10]scale_vaapi=w=3840:h=2160:format=nv12[11];" +
			"[2:v]format=bgra,hwupload[12];" +
			"[11][12]overlay_vaapi=x=0:y=" + BandYSentinel + ":eof_action=pass:repeatlast=1[4]",
	}
	bandY := ResolveAgentBand(spec, srt)
	if spec.BandHeight >= subPrerenderBandHeight(2160) {
		t.Errorf("agent resolve: BandHeight = %d, want < fallback (tight)", spec.BandHeight)
	}
	wantBandY := 2160 - spec.BandHeight
	if bandY != wantBandY {
		t.Errorf("agent resolve: bandY = %d, want %d", bandY, wantBandY)
	}
	n := PatchMainArgsBandY(mainArgs, bandY)
	if n != 1 {
		t.Errorf("PatchMainArgsBandY: patched %d, want 1", n)
	}
	if strings.Contains(mainArgs[1], BandYSentinel) {
		t.Errorf("sentinel still present after patch: %s", mainArgs[1])
	}
	if !strings.Contains(mainArgs[1], "overlay_vaapi=x=0:y=") {
		t.Errorf("filter graph corrupted: %s", mainArgs[1])
	}
}

// Agent-side resolver: positional cue forces fallback band, sentinel
// resolves to the static-fallback y offset.
func TestAgentBandResolve_PositionedFallback(t *testing.T) {
	dir := t.TempDir()
	srt := filepath.Join(dir, "temp-0.srt")
	if err := os.WriteFile(srt, []byte(
		"1\n00:00:01,000 --> 00:00:04,000\n{\\an8}Top sign\n\n"+
			"2\n00:00:05,000 --> 00:00:09,000\nBottom\n\n"), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	fallback := subPrerenderBandHeight(2160)
	spec := &SubPrerenderSpec{
		Width:                  3840,
		Height:                 2160,
		BandHeight:             fallback,
		ResolveBandPostExtract: true,
	}
	bandY := ResolveAgentBand(spec, srt)
	if spec.BandHeight != fallback {
		t.Errorf("BandHeight = %d, want %d (positional cue forces fallback)",
			spec.BandHeight, fallback)
	}
	if bandY != 2160-fallback {
		t.Errorf("bandY = %d, want %d", bandY, 2160-fallback)
	}
}

// Agent-side resolver: spec without ResolveBandPostExtract is untouched.
func TestAgentBandResolve_NoResolveFlag(t *testing.T) {
	spec := &SubPrerenderSpec{
		Width:      3840,
		Height:     2160,
		BandHeight: 2160, // ASS / full-canvas fallback shape
	}
	bandY := ResolveAgentBand(spec, "/nope.srt")
	if spec.BandHeight != 2160 {
		t.Errorf("BandHeight mutated without resolve flag: %d", spec.BandHeight)
	}
	if bandY != 0 {
		t.Errorf("bandY = %d, want 0 (Height - BandHeight)", bandY)
	}
}

// Agent-side resolver: bitmap path is a no-op even when the flag is set
// (the bitmap pre-render owns its own band logic in the rewriter).
func TestAgentBandResolve_BitmapNoOp(t *testing.T) {
	spec := &SubPrerenderSpec{
		Width:                  3840,
		Height:                 2160,
		BandHeight:             864,
		Bitmap:                 true,
		ResolveBandPostExtract: true, // shouldn't matter
	}
	_ = ResolveAgentBand(spec, "/whatever.sup")
	if spec.BandHeight != 864 {
		t.Errorf("BandHeight mutated on bitmap path: %d", spec.BandHeight)
	}
}

// Patch helper handles "no sentinel found" gracefully (already-patched
// argv, ASS path, etc.).
func TestPatchMainArgsBandY_NoSentinel(t *testing.T) {
	args := []string{"-filter_complex", "[0:0]hwupload[10];[10]scale_vaapi=...,y=1296:..."}
	if n := PatchMainArgsBandY(args, 1620); n != 0 {
		t.Errorf("PatchMainArgsBandY without sentinel: %d, want 0", n)
	}
}
