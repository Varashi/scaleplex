package main

import (
	"strings"
	"testing"
)

// Plex for Windows desktop emits a different output shape than DASH-mp4
// (Plex Web) and HLS-mpegts (Plex mobile): segmented matroska via stock
// `-f segment` muxer with `-segment_format matroska -segment_format_options
// live=1`. The chunk-list semantics match HLS — `-segment_list` points at
// PMS's loopback, so the worker must rewrite that URL to the relay. Live
// captured 2026-05-11 from Big Hero 6 at 8 Mbps 1080p, session crashed
// with "Connection refused" before this branch existed.
func TestRewriter_PlexWindows_SegmentMkv_RewritesURL(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-ss", "13",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/media/Movies/BigHero6.mkv",
		"-start_at_zero", "-copyts",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-y", "-nostats", "-loglevel", "quiet", "-loglevel_plex", "error",
		"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/abc/def/progress",
		"-filter_complex", "[0:0]scale=w=1920:h=1080:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264",
		"-crf:0", "23",
		"-maxrate:0", "7246k",
		"-bufsize:0", "14492k",
		"-r:0", "23.976",
		"-preset:0", "veryfast",
		"-f", "segment",
		"-segment_format", "matroska",
		"-segment_format_options", "live=1",
		"-segment_time", "1",
		"-segment_header_filename", "header",
		"-segment_start_number", "0",
		"-segment_list", "http://127.0.0.1:32400/video/:/transcode/session/abc/def/manifest?X-Plex-Http-Pipeline=infinite",
		"-segment_list_type", "csv",
		"-segment_list_unfinished", "1",
		"-segment_list_size", "5",
		"-segment_list_separate_stream_times", "1",
		"-avoid_negative_ts", "disabled",
		"chunk-%05d",
	}
	env := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://clusterplex-pms.clusterplex.svc:32499",
		"X_PLEX_TOKEN":           "tok123",
	}
	out := Rewrite(args, env, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	// Segment list URL must be rewritten to relay address.
	segListIdx := indexOfArg(out.Args, "-segment_list", 0)
	if segListIdx < 0 {
		t.Fatalf("-segment_list missing from output")
	}
	got := out.Args[segListIdx+1]
	if strings.Contains(got, "127.0.0.1") {
		t.Errorf("segment_list URL still points at loopback: %s", got)
	}
	if !strings.Contains(got, "clusterplex-pms.clusterplex.svc:32499") {
		t.Errorf("segment_list URL not rewritten to relay base: %s", got)
	}
	if !strings.Contains(got, "X-Plex-Token=tok123") {
		t.Errorf("segment_list URL missing X-Plex-Token: %s", got)
	}
	if !strings.Contains(got, "scaleplex_seg_time=1") {
		t.Errorf("segment_list URL missing scaleplex_seg_time hint: %s", got)
	}
	if !containsString(out.Changes, "hls:segment_list:rewrite-to-relay") {
		t.Errorf("expected hls:segment_list:rewrite-to-relay tag: %v", out.Changes)
	}
	// -copyts dropped (stock segment muxer with -copyts + -ss won't split).
	if containsString(out.Args, "-copyts") {
		t.Errorf("-copyts must be stripped: %v", out.Args)
	}
	if !containsString(out.Changes, "hls:drop:-copyts") {
		t.Errorf("expected hls:drop:-copyts tag: %v", out.Changes)
	}
	// DASH-specific flags must NOT be injected on this shape.
	if containsString(out.Args, "-extra_window_size") {
		t.Errorf("-extra_window_size must not be injected for -f segment: %v", out.Args)
	}
	// segment_format_options: live=1 must be rewritten to live=0 so
	// the stock matroska muxer writes Duration from -metadata into the
	// header. Plex's fork patches matroskaenc.c to always write
	// Duration regardless of live mode; stock honours is_live and
	// skips. Without the rewrite, Plex Windows client sees an
	// unknown-duration matroska stream and shows a growing slider.
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-segment_format_options" && out.Args[i+1] == "live=1" {
			t.Errorf("live=1 must be rewritten to live=0: %v", out.Args)
		}
	}
	if !containsString(out.Changes, "hls:segment_format_options:live=1->live=0") {
		t.Errorf("expected live=1->live=0 tag: %v", out.Changes)
	}
	// -output_ts_offset 13 must be injected: chunks need global-timeline
	// PTS so the matroska Cluster Timecode lands at 13s+ and Plex Windows
	// shows the correct playhead position after a seek.
	ofIdx := indexOfArg(out.Args, "-output_ts_offset", 0)
	if ofIdx < 0 {
		t.Errorf("expected -output_ts_offset to be injected for matroska seek session: %v", out.Args)
	} else if out.Args[ofIdx+1] != "13" {
		t.Errorf("output_ts_offset value: got %q want %q", out.Args[ofIdx+1], "13")
	}
	// Must land BEFORE -f segment (output scope).
	if fIdx := indexOfArg(out.Args, "-f", 0); ofIdx >= 0 && fIdx >= 0 && ofIdx > fIdx {
		t.Errorf("-output_ts_offset must be before -f segment: ofIdx=%d fIdx=%d", ofIdx, fIdx)
	}
}

// Initial-play (no -ss): -output_ts_offset must NOT be injected — clusters
// start at 0 naturally and PTS=0 matches Cluster Timecode=0.
func TestRewriter_PlexWindows_SegmentMkv_NoSeek_NoOutputTsOffset(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/media/Movies/BigHero6.mkv",
		"-start_at_zero",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_hw_device", "vaapi",
		"-y", "-nostats", "-loglevel", "quiet", "-loglevel_plex", "error",
		"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/abc/def/progress",
		"-filter_complex", "[0:0]scale=w=1280:h=720[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264",
		"-crf:0", "23",
		"-preset:0", "veryfast",
		"-f", "segment",
		"-segment_format", "matroska",
		"-segment_format_options", "live=1",
		"-segment_time", "1",
		"-segment_list", "http://127.0.0.1:32400/video/:/transcode/session/abc/def/manifest",
		"-segment_list_type", "csv",
		"chunk-%05d",
	}
	env := map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://relay:32499",
		"X_PLEX_TOKEN":           "tok",
	}
	out := Rewrite(args, env, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if indexOfArg(out.Args, "-output_ts_offset", 0) >= 0 {
		t.Errorf("must NOT inject -output_ts_offset for non-seek session: %v", out.Args)
	}
}
