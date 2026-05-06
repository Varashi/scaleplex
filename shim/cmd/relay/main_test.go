package main

import (
	"strings"
	"testing"
)

// PMS reads -segment_list CSV rows to know each chunk's playlist window
// and serves a 0-byte body when start_time mismatches N*segDur. Stock
// ffmpeg writes 0-based timestamps when -copyts is stripped (which it
// must be — copyts blocks splits on seek). Relay rewrites every
// `media-NNNNN.ts,start,end` row to `media-NNNNN.ts,N*segDur,(N+1)*segDur`
// so the global-timeline contract holds.
func TestRewriteSegmentListCSV_SeekChunks(t *testing.T) {
	body := "media-00111.ts,0.041667,13.041667\nmedia-00112.ts,13.041667,21.041667\n"
	got := string(rewriteSegmentListCSV([]byte(body), 8.0))
	want := "media-00111.ts,888.000000,896.000000\nmedia-00112.ts,896.000000,904.000000\n"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

func TestRewriteSegmentListCSV_InitialPlay(t *testing.T) {
	body := "media-00000.ts,0.000000,8.000000\nmedia-00001.ts,8.000000,16.000000\n"
	got := string(rewriteSegmentListCSV([]byte(body), 8.0))
	want := "media-00000.ts,0.000000,8.000000\nmedia-00001.ts,8.000000,16.000000\n"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

// Non-CSV rows (blank lines, headers the muxer might emit) must pass
// through untouched.
func TestRewriteSegmentListCSV_PassThrough(t *testing.T) {
	body := "#EXTM3U\n\nmedia-00005.ts,0.0,8.0\n"
	got := string(rewriteSegmentListCSV([]byte(body), 8.0))
	if !strings.Contains(got, "#EXTM3U\n") {
		t.Errorf("non-CSV row stripped: %q", got)
	}
	if !strings.Contains(got, "media-00005.ts,40.000000,48.000000") {
		t.Errorf("CSV row not rewritten: %q", got)
	}
}

// Different segment times scale linearly.
func TestRewriteSegmentListCSV_SegTime3(t *testing.T) {
	body := "media-00010.ts,0.0,3.0\n"
	got := string(rewriteSegmentListCSV([]byte(body), 3.0))
	want := "media-00010.ts,30.000000,33.000000\n"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}
