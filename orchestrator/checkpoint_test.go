package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPlexSessionFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			"valid lowercase",
			[]string{"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/0aaa95c3-1173-444e-b5f5-43c011520932/e7b2fcdb-aaed-4daa-b113-21c852858258/progress"},
			"0aaa95c3-1173-444e-b5f5-43c011520932",
		},
		{
			"uppercase normalised",
			[]string{"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/999845BE-AF1A-4B67-AEF1-68C09115F054/abcd/progress"},
			"999845be-af1a-4b67-aef1-68c09115f054",
		},
		{"absent", []string{"-i", "/media/x.mkv"}, ""},
		{"flag with no value", []string{"-progressurl"}, ""},
		{"malformed url", []string{"-progressurl", "not a url"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := plexSessionFromArgs(tc.args); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestInitialSeekFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want float64
	}{
		{"absent", []string{"-i", "/m.mkv"}, 0},
		{"input seek before -i", []string{"-ss", "30.5", "-i", "/m.mkv"}, 30.5},
		{"output seek ignored", []string{"-i", "/m.mkv", "-ss", "60"}, 0},
		{"both — input wins", []string{"-ss", "10", "-i", "/m.mkv", "-ss", "60"}, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := initialSeekFromArgs(tc.args); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestSegmentTimeFromArgs(t *testing.T) {
	if got := segmentTimeFromArgs([]string{"-segment_time", "2"}); got != 2.0 {
		t.Fatalf("explicit: got %v", got)
	}
	if got := segmentTimeFromArgs([]string{"-i", "x"}); got != 1.0 {
		t.Fatalf("default: got %v want 1.0", got)
	}
}

func TestInjectResumeFlags_AbsentInputSeek(t *testing.T) {
	args := []string{"-codec:0", "hevc", "-i", "/m.mkv", "-c:v", "h264_vaapi"}
	out := injectResumeFlags(args, 42.0, 43)

	// -ss + -copyts must appear before -i (in either order is fine).
	iIdx := -1
	ssIdx := -1
	copytsIdx := -1
	for i, a := range out {
		switch a {
		case "-i":
			if iIdx < 0 {
				iIdx = i
			}
		case "-ss":
			ssIdx = i
		case "-copyts":
			copytsIdx = i
		}
	}
	if iIdx < 0 || ssIdx < 0 || copytsIdx < 0 {
		t.Fatalf("missing flags: %v", out)
	}
	if ssIdx > iIdx || copytsIdx > iIdx {
		t.Fatalf("flags landed after -i: %v", out)
	}
	if out[ssIdx+1] != "42.000" {
		t.Fatalf("ss value=%q want 42.000", out[ssIdx+1])
	}
	// segment_start_number appended at end
	joined := strings.Join(out, " ")
	if !strings.HasSuffix(joined, "-segment_start_number 43") {
		t.Fatalf("missing segment_start_number suffix: %v", out)
	}
}

func TestInjectResumeFlags_ReplacesExisting(t *testing.T) {
	args := []string{
		"-ss", "0",
		"-copyts",
		"-i", "/m.mkv",
		"-c:v", "h264_vaapi",
		"-segment_start_number", "1",
	}
	out := injectResumeFlags(args, 12.5, 13)
	// existing -ss replaced
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "-ss" {
			if out[i+1] != "12.500" {
				t.Fatalf("-ss not replaced: %v", out)
			}
			break
		}
	}
	// existing -segment_start_number replaced
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "-segment_start_number" && out[i+1] != "13" {
			t.Fatalf("segment_start_number not replaced: %v", out)
		}
	}
	// must not duplicate -copyts
	count := 0
	for _, a := range out {
		if a == "-copyts" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("-copyts duplicated %d×: %v", count, out)
	}
}

func TestCheckpointCache_TTL(t *testing.T) {
	c := newCheckpointCache(50 * time.Millisecond)
	c.put("abc", &resumeHint{LastSeq: 5, UpdatedAt: time.Now()})
	if h := c.get("abc"); h == nil || h.LastSeq != 5 {
		t.Fatalf("fresh entry missing")
	}
	time.Sleep(80 * time.Millisecond)
	if h := c.get("abc"); h != nil {
		t.Fatalf("expired entry not pruned: %+v", h)
	}
}

func TestResumeIfApplicable_DropsOnReseek(t *testing.T) {
	cpCache = newCheckpointCache(time.Minute)
	cpCache.put("plexid", &resumeHint{
		OriginalSeek: 0,
		SegmentTime:  1,
		LastSeq:      10,
		UpdatedAt:    time.Now(),
	})
	args := []string{
		"-ss", "120", // user reseeked
		"-i", "/m.mkv",
		"-progressurl", "http://x/transcode/session/PLEXID-0000-0000-0000-000000000000/job/progress",
	}
	// Use a real-shape UUID matching the regex
	args[5] = "http://x/transcode/session/00000000-0000-0000-0000-000000000000/job/progress"
	cpCache = newCheckpointCache(time.Minute)
	cpCache.put("00000000-0000-0000-0000-000000000000", &resumeHint{
		OriginalSeek: 0, SegmentTime: 1, LastSeq: 10, UpdatedAt: time.Now(),
	})
	_, resumed := resumeIfApplicable(args)
	if resumed {
		t.Fatalf("resume should not fire when user re-seeks")
	}
	if cpCache.get("00000000-0000-0000-0000-000000000000") != nil {
		t.Fatalf("stale hint should be dropped on reseek")
	}
}

func TestResumeIfApplicable_HitInjects(t *testing.T) {
	cpCache = newCheckpointCache(time.Minute)
	cpCache.put("11111111-1111-1111-1111-111111111111", &resumeHint{
		OriginalSeek: 0,
		SegmentTime:  1,
		LastSeq:      7,
		UpdatedAt:    time.Now(),
	})
	args := []string{
		"-ss", "0",
		"-i", "/m.mkv",
		"-c:v", "h264_vaapi",
		"-progressurl", "http://x/transcode/session/11111111-1111-1111-1111-111111111111/job/progress",
	}
	out, resumed := resumeIfApplicable(args)
	if !resumed {
		t.Fatalf("expected resume")
	}
	joined := strings.Join(out, " ")
	if !strings.Contains(joined, "-ss 7.000") {
		t.Fatalf("expected -ss 7.000: %v", out)
	}
	if !strings.Contains(joined, "-segment_start_number 8") {
		t.Fatalf("expected -segment_start_number 8: %v", out)
	}
}

// fetchLastSeq parses the JSON shape worker emits.
func TestFetchLastSeq_ParsesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"last_segment_seq": 42, "session_id": "s"})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c := &http.Client{Timeout: time.Second}
	seq, ok := fetchLastSeq(ctx, c, srv.URL, "s")
	if !ok || seq != 42 {
		t.Fatalf("seq=%d ok=%v", seq, ok)
	}
}
