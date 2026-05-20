package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mixedSRT = `1
00:00:01,000 --> 00:00:04,000
Bottom dialogue

2
00:00:05,000 --> 00:00:08,000
Another bottom line

3
00:00:09,000 --> 00:00:12,000
{\an8}Top sign translation

4
00:00:13,000 --> 00:00:16,000
Back to bottom

5
00:00:17,000 --> 00:00:19,500
{\an8}Another top cue
`

func writeMixedSRT(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "temp-0.srt")
	if err := os.WriteFile(p, []byte(mixedSRT), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func TestCountSRTRegions_BottomPlusTop(t *testing.T) {
	rc, err := countSRTRegions(writeMixedSRT(t))
	if err != nil {
		t.Fatalf("countSRTRegions: %v", err)
	}
	if rc.Bottom != 3 || rc.Top != 2 {
		t.Errorf("Bottom=%d Top=%d, want 3/2", rc.Bottom, rc.Top)
	}
	if rc.Middle != 0 || rc.Positional != 0 {
		t.Errorf("Middle=%d Positional=%d, want 0/0", rc.Middle, rc.Positional)
	}
	if rc.MaxLinesBottom != 1 || rc.MaxLinesTop != 1 {
		t.Errorf("MaxLinesBottom=%d MaxLinesTop=%d, want 1/1",
			rc.MaxLinesBottom, rc.MaxLinesTop)
	}
}

func TestPlanMultiRegion_BottomPlusTop(t *testing.T) {
	plan := planMultiRegion(writeMixedSRT(t), 2160, subPrerenderBandHeight(2160))
	if len(plan) != 2 {
		t.Fatalf("planMultiRegion: %d regions, want 2", len(plan))
	}
	// Region 0 = bottom (anchor 2), Region 1 = top (anchor 8).
	if plan[0].Anchor != regionBottom || plan[1].Anchor != regionTop {
		t.Errorf("region order = [%d, %d], want [%d, %d]",
			plan[0].Anchor, plan[1].Anchor, regionBottom, regionTop)
	}
	if plan[0].BandY != 2160-plan[0].BandHeight {
		t.Errorf("bottom BandY = %d, want %d", plan[0].BandY, 2160-plan[0].BandHeight)
	}
	if plan[1].BandY != 0 {
		t.Errorf("top BandY = %d, want 0", plan[1].BandY)
	}
	// Filtered files exist and are non-empty.
	for i, r := range plan {
		st, err := os.Stat(r.FilteredFile)
		if err != nil {
			t.Fatalf("region %d filtered file: %v", i, err)
		}
		if st.Size() == 0 {
			t.Errorf("region %d filtered file is empty", i)
		}
	}
}

func TestPlanMultiRegion_BottomOnlyReturnsNil(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "all-bottom.srt")
	body := "1\n00:00:01,000 --> 00:00:04,000\nLine\n\n2\n00:00:05,000 --> 00:00:08,000\nAnother\n\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if plan := planMultiRegion(p, 2160, 864); plan != nil {
		t.Errorf("expected nil plan for bottom-only SRT, got %d regions", len(plan))
	}
}

func TestPlanMultiRegion_PositionalBails(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pos.srt")
	body := "1\n00:00:01,000 --> 00:00:04,000\nBottom\n\n2\n00:00:05,000 --> 00:00:08,000\n{\\pos(640,360)}Pinned\n\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if plan := planMultiRegion(p, 2160, 864); plan != nil {
		t.Errorf("expected nil plan when \\pos is present, got %d regions", len(plan))
	}
}

func TestFilterSRTByAnchor_BottomOnly(t *testing.T) {
	p := writeMixedSRT(t)
	dst := filepath.Join(filepath.Dir(p), "out.srt")
	if err := filterSRTByAnchor(p, dst, regionBottom); err != nil {
		t.Fatalf("filterSRTByAnchor: %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read filtered: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "Bottom dialogue") || !strings.Contains(s, "Another bottom line") || !strings.Contains(s, "Back to bottom") {
		t.Errorf("filtered file missing expected bottom cues:\n%s", s)
	}
	if strings.Contains(s, "Top sign translation") || strings.Contains(s, "Another top cue") {
		t.Errorf("filtered file leaked top cues:\n%s", s)
	}
}

func TestFilterSRTByAnchor_TopOnly(t *testing.T) {
	p := writeMixedSRT(t)
	dst := filepath.Join(filepath.Dir(p), "out.srt")
	if err := filterSRTByAnchor(p, dst, regionTop); err != nil {
		t.Fatalf("filterSRTByAnchor: %v", err)
	}
	body, _ := os.ReadFile(dst)
	s := string(body)
	if !strings.Contains(s, "Top sign translation") || !strings.Contains(s, "Another top cue") {
		t.Errorf("filtered file missing expected top cues:\n%s", s)
	}
	if strings.Contains(s, "Bottom dialogue") {
		t.Errorf("filtered file leaked bottom cues:\n%s", s)
	}
}

func TestMutateGraphForMultiRegion_InitialPlay(t *testing.T) {
	// Rewriter's single-region shape for initial play (no seek).
	graph := "[0:0]hwupload[10];[10]scale_vaapi=w=3840:h=2160:format=nv12[11];" +
		"[2:v]format=bgra,hwupload[12];" +
		"[11][12]overlay_vaapi=x=0:y=" + BandYSentinel + ":eof_action=pass:repeatlast=1[4]"
	regions := []RegionPrerenderSpec{
		{Anchor: regionBottom, BandY: 1748},
		{Anchor: regionTop, BandY: 0},
	}
	got, err := MutateGraphForMultiRegion(graph, regions, 2, false)
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	// Bottom region keeps the BandYSentinel.
	if !strings.Contains(got, "overlay_vaapi=x=0:y="+BandYSentinel+":") {
		t.Errorf("missing bottom sentinel: %s", got)
	}
	// Top region uses anchor-suffixed sentinel.
	topSent := regionSentinel(1, regionTop)
	if !strings.Contains(got, "overlay_vaapi=x=0:y="+topSent+":") {
		t.Errorf("missing top sentinel %s: %s", topSent, got)
	}
	// Top region's hwupload references [3:v] (input 3).
	if !strings.Contains(got, "[3:v]format=bgra,hwupload") {
		t.Errorf("expected [3:v] hwupload, got: %s", got)
	}
	// Final overlay output ends at [4].
	if !strings.HasSuffix(got, "[4]") {
		t.Errorf("expected graph to end at [4], got: ...%s", got[len(got)-20:])
	}
}

func TestMutateGraphForMultiRegion_Seek(t *testing.T) {
	// Rewriter's single-region shape on -ss > 0.
	graph := "[0:0]hwupload[10];[10]scale_vaapi=w=3840:h=2160:format=nv12,setpts=PTS-STARTPTS[11];" +
		"[2:v]setpts=PTS-STARTPTS,format=bgra,hwupload[12];" +
		"[11][12]overlay_vaapi=x=0:y=" + BandYSentinel + ":eof_action=pass:repeatlast=1[13];" +
		"[13]setpts=PTS+540.000/TB[4]"
	regions := []RegionPrerenderSpec{
		{Anchor: regionBottom, BandY: 1748},
		{Anchor: regionTop, BandY: 0},
	}
	got, err := MutateGraphForMultiRegion(graph, regions, 2, true)
	if err != nil {
		t.Fatalf("mutate seek: %v", err)
	}
	// Both regions should carry setpts=PTS-STARTPTS prefix on their FIFO formats.
	bottomFmt := "[2:v]setpts=PTS-STARTPTS,format=bgra,hwupload[12]"
	topFmt := "[3:v]setpts=PTS-STARTPTS,format=bgra,hwupload"
	if !strings.Contains(got, bottomFmt) {
		t.Errorf("seek mode missing bottom setpts: %s", got)
	}
	if !strings.Contains(got, topFmt) {
		t.Errorf("seek mode missing top setpts: %s", got)
	}
	// Final overlay output ends at [13] (the seek tail then takes it to [4]).
	if !strings.Contains(got, "overlay_vaapi=x=0:y="+regionSentinel(1, regionTop)+":eof_action=pass:repeatlast=1[13]") {
		t.Errorf("seek: final overlay should end at [13]: %s", got)
	}
}

func TestPatchMainArgsBandYMulti(t *testing.T) {
	args := []string{
		"-filter_complex",
		"[0:0]hwupload[10];[10]scale_vaapi=...[11];" +
			"[2:v]format=bgra,hwupload[12];" +
			"[11][12]overlay_vaapi=x=0:y=" + BandYSentinel + ":eof_action=pass:repeatlast=1[21];" +
			"[3:v]format=bgra,hwupload[22];" +
			"[21][22]overlay_vaapi=x=0:y=" + regionSentinel(1, regionTop) + ":eof_action=pass:repeatlast=1[4]",
	}
	regions := []RegionPrerenderSpec{
		{Anchor: regionBottom, BandY: 1748},
		{Anchor: regionTop, BandY: 0},
	}
	if n := PatchMainArgsBandYMulti(args, regions); n != 2 {
		t.Errorf("patched %d, want 2", n)
	}
	if strings.Contains(args[1], BandYSentinel) {
		t.Errorf("bottom sentinel not patched: %s", args[1])
	}
	if strings.Contains(args[1], regionSentinel(1, regionTop)) {
		t.Errorf("top sentinel not patched: %s", args[1])
	}
	if !strings.Contains(args[1], "y=1748:") || !strings.Contains(args[1], "y=0:") {
		t.Errorf("expected y=1748 and y=0 in patched graph: %s", args[1])
	}
}

func TestClassifyCueAnchor(t *testing.T) {
	cases := []struct {
		body []string
		want int
	}{
		{[]string{"Plain dialogue"}, regionBottom},
		{[]string{"{\\an2}Bottom-center"}, regionBottom},
		{[]string{"{\\an8}Top sign"}, regionTop},
		{[]string{"{\\an7}Top-left"}, regionTop},
	}
	for _, c := range cases {
		if got := classifyCueAnchor(c.body); got != c.want {
			t.Errorf("classifyCueAnchor(%v) = %d, want %d", c.body, got, c.want)
		}
	}
}
