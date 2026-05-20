package main

import (
	"testing"
)

// The codec probe is called against the file referenced by each `-i`
// input. For sidecar inputs (the second `-i ...` carries the SRT/ASS),
// ffprobe sees the sidecar as input 0, so the stream-spec MUST be
// re-anchored from PMS-argv form `N:s:0` to ffprobe form `0:s:0`.
// Otherwise the probe selects a non-existent input, returns empty, and
// the rewriter loses Codec → loses the SRT band optimisation downstream.
func TestRewriter_ProbeSpec_Sidecar_ReanchoredToInput0(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-i", "/media/x.mkv",
		"-i", "/transcode/Sub/temp-0.srt",
		"-start_at_zero", "-copyts",
		"-map_inlineass", "1:s:0",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	}
	var gotSrc, gotSpec string
	Rewrite(args, nil, &RewriteOpts{
		FSExists: func(string) bool { return true },
		ProbeSubtitleCodec: func(src, spec string) string {
			gotSrc, gotSpec = src, spec
			return "subrip"
		},
	})
	if gotSrc != "/transcode/Sub/temp-0.srt" {
		t.Errorf("probe source = %q, want sidecar path", gotSrc)
	}
	if gotSpec != "0:s:0" {
		t.Errorf("probe spec = %q, want 0:s:0 (re-anchored to ffprobe input 0)", gotSpec)
	}
}

// Embedded subtitles probe against input 0 (the source mkv) — no
// re-anchoring needed, the spec stays as PMS emitted it.
func TestRewriter_ProbeSpec_Embedded_Preserved(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-i", "/media/x.mkv",
		"-start_at_zero", "-copyts",
		"-map_inlineass", "0:3",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]",
		"-map", "[4]",
		"-codec:0", "hevc_vaapi",
		"-f", "matroska", "/transcode/out.mkv",
	}
	var gotSpec string
	Rewrite(args, nil, &RewriteOpts{
		FSExists: func(string) bool { return true },
		ProbeSubtitleCodec: func(src, spec string) string {
			gotSpec = spec
			return "subrip"
		},
	})
	if gotSpec != "0:3" {
		t.Errorf("probe spec = %q, want 0:3 (embedded, no re-anchor)", gotSpec)
	}
}
