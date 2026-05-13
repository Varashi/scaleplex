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
	// -copyts must be KEPT on matroska segment seek. scaleplex-ffmpeg7
	// patch 0103 (2026-05-13) drops the jellyfin
	// `reference_stream_first_pts` end_pts adjustment that broke split
	// cadence on `-ss + -copyts`, restoring Plex-fork behaviour. With
	// -copyts kept, Cluster.Timecode lands at absolute source PTS
	// natively — relay has nothing to patch.
	if !containsString(out.Args, "-copyts") {
		t.Errorf("-copyts must be kept for matroska segment shape (patch 0103): %v", out.Args)
	}
	if containsString(out.Changes, "hls:drop:-copyts(seek)") {
		t.Errorf("hls:drop:-copyts(seek) tag must NOT appear post-patch-0103: %v", out.Changes)
	}
	// scaleplex_mkv_offset_ms retired with patch 0103 — relay has
	// nothing to patch when -copyts is kept.
	if strings.Contains(got, "scaleplex_mkv_offset_ms") {
		t.Errorf("scaleplex_mkv_offset_ms must NOT be emitted (retired with patch 0103): %s", got)
	}
	// Output filename pattern stays `chunk-%05d` (no .tmp suffix).
	// Stage-rename was tried + reverted 2026-05-11 — ffmpeg's 0-byte
	// chunks broke the assumption that .tmp files always have content.
	for _, a := range out.Args {
		if a == "chunk-%05d.tmp" {
			t.Errorf("output filename must NOT be chunk-%%05d.tmp: %v", out.Args)
		}
	}
	// DASH-specific flags must NOT be injected on this shape.
	if containsString(out.Args, "-extra_window_size") {
		t.Errorf("-extra_window_size must not be injected for -f segment: %v", out.Args)
	}
	// segment_format_options live=1 now passes through unchanged.
	// scaleplex-ffmpeg7 patch 0094 makes matroskaenc.c always write
	// Duration from -metadata regardless of is_live; jellyfin's
	// `IS_SEEKABLE = pb seekable && !is_live` falls to the
	// cluster-defaults else-branch at live=1 → 1000 ms / 32 KB ≈
	// per-frame clusters that Plex Windows needs. Both behaviours the
	// previous rewrite forced now come from the fork + stock defaults.
	foundLive1 := false
	for i := 0; i+1 < len(out.Args); i++ {
		if out.Args[i] == "-segment_format_options" {
			if out.Args[i+1] != "live=1" {
				t.Errorf("segment_format_options: got %q, want live=1 (unchanged)", out.Args[i+1])
			} else {
				foundLive1 = true
			}
			break
		}
	}
	if !foundLive1 {
		t.Errorf("-segment_format_options live=1 missing from output: %v", out.Args)
	}
	if containsString(out.Changes, "hls:segment_format_options:live=1->live=0+per-frame-clusters") {
		t.Errorf("live=1 rewrite should be gone post-audit: %v", out.Changes)
	}
}
