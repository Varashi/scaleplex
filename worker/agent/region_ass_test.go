package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SRT 3-region — bottom + middle + top all populated.
func TestPlanMultiRegion_ThreeRegions(t *testing.T) {
	body := `1
00:00:01,000 --> 00:00:04,000
Bottom one

2
00:00:05,000 --> 00:00:08,000
{\an5}Middle one

3
00:00:09,000 --> 00:00:12,000
{\an8}Top one

4
00:00:13,000 --> 00:00:16,000
Bottom two
`
	dir := t.TempDir()
	p := filepath.Join(dir, "mixed3.srt")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := planMultiRegion(p, 2160, subPrerenderBandHeight(2160))
	if len(plan) != 3 {
		t.Fatalf("planMultiRegion: %d regions, want 3", len(plan))
	}
	if plan[0].Anchor != regionBottom || plan[1].Anchor != regionMiddle || plan[2].Anchor != regionTop {
		t.Errorf("region order = [%d, %d, %d], want bottom/middle/top",
			plan[0].Anchor, plan[1].Anchor, plan[2].Anchor)
	}
	// Middle BandY centered.
	wantMidY := (2160 - plan[1].BandHeight) / 2
	if wantMidY%2 == 1 {
		wantMidY--
	}
	if plan[1].BandY != wantMidY {
		t.Errorf("middle BandY = %d, want %d (centered)", plan[1].BandY, wantMidY)
	}
}

func TestFilterSRTByAnchor_Middle(t *testing.T) {
	body := `1
00:00:01,000 --> 00:00:04,000
Bottom dialogue

2
00:00:05,000 --> 00:00:08,000
{\an5}Middle label

3
00:00:09,000 --> 00:00:12,000
{\an8}Top cue
`
	dir := t.TempDir()
	src := filepath.Join(dir, "mid.srt")
	dst := filepath.Join(dir, "out-mid.srt")
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := filterSRTByAnchor(src, dst, regionMiddle); err != nil {
		t.Fatalf("filterSRTByAnchor middle: %v", err)
	}
	out, _ := os.ReadFile(dst)
	s := string(out)
	if !strings.Contains(s, "Middle label") {
		t.Errorf("missing middle cue: %s", s)
	}
	if strings.Contains(s, "Bottom dialogue") || strings.Contains(s, "Top cue") {
		t.Errorf("leaked non-middle cues: %s", s)
	}
}

func TestAnchorFromTag(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"1", regionBottom}, {"2", regionBottom}, {"3", regionBottom},
		{"4", regionMiddle}, {"5", regionMiddle}, {"6", regionMiddle},
		{"7", regionTop}, {"8", regionTop}, {"9", regionTop},
		{"x", regionBottom},
	}
	for _, c := range cases {
		if got := anchorFromTag(c.in); got != c.want {
			t.Errorf("anchorFromTag(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// --- ASS multi-region -------------------------------------------------

const assMixedHeader = `[Script Info]
ScriptType: v4.00+
PlayResX: 1920
PlayResY: 1080

[V4+ Styles]
Format: Name, Fontname, Fontsize, Alignment, MarginV
Style: Default,Arial,48,2,30

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`

func TestCountASSRegions_BottomPlusTop(t *testing.T) {
	body := assMixedHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Bottom line\n" +
		"Dialogue: 0,0:00:05.00,0:00:08.00,Default,,0,0,0,,Another bottom\n" +
		"Dialogue: 0,0:00:09.00,0:00:12.00,Default,,0,0,0,,{\\an8}Top sign\n"
	p := writeASS(t, "mixed.ass", body)
	rc, err := countASSRegions(p)
	if err != nil {
		t.Fatalf("countASSRegions: %v", err)
	}
	if rc.Bottom != 2 || rc.Top != 1 || rc.Middle != 0 || rc.Positional != 0 {
		t.Errorf("counts: Bottom=%d Middle=%d Top=%d Positional=%d, want 2/0/1/0",
			rc.Bottom, rc.Middle, rc.Top, rc.Positional)
	}
}

func TestCountASSRegions_PositionalBails(t *testing.T) {
	body := assMixedHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,{\\pos(640,360)}Pinned\n"
	p := writeASS(t, "pos.ass", body)
	rc, err := countASSRegions(p)
	if err != nil {
		t.Fatalf("countASSRegions: %v", err)
	}
	if rc.Positional == 0 {
		t.Errorf("expected Positional>0 for \\pos override")
	}
}

func TestPlanMultiRegion_ASS_BottomPlusTop(t *testing.T) {
	body := assMixedHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Bottom line\n" +
		"Dialogue: 0,0:00:05.00,0:00:08.00,Default,,0,0,0,,{\\an8}Top sign\n"
	p := writeASS(t, "bt.ass", body)
	plan := planMultiRegion(p, 2160, subPrerenderBandHeight(2160))
	if len(plan) != 2 {
		t.Fatalf("planMultiRegion(ASS): %d regions, want 2", len(plan))
	}
	for _, r := range plan {
		if !strings.HasSuffix(r.FilteredFile, ".ass") {
			t.Errorf("filtered file has wrong extension: %s", r.FilteredFile)
		}
	}
}

func TestFilterASSByAnchor_PreservesHeaderAndFiltersDialogue(t *testing.T) {
	body := assMixedHeader +
		"Comment: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,meta\n" +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Bottom A\n" +
		"Dialogue: 0,0:00:05.00,0:00:08.00,Default,,0,0,0,,{\\an8}Top B\n" +
		"Dialogue: 0,0:00:09.00,0:00:11.00,Default,,0,0,0,,Bottom C\n"
	src := writeASS(t, "f.ass", body)
	dst := filepath.Join(filepath.Dir(src), "f-bot.ass")
	if err := filterASSByAnchor(src, dst, regionBottom); err != nil {
		t.Fatalf("filterASSByAnchor: %v", err)
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// Header sections must survive.
	for _, want := range []string{"[Script Info]", "PlayResX: 1920", "[V4+ Styles]", "Style: Default", "[Events]", "Format: Layer"} {
		if !strings.Contains(s, want) {
			t.Errorf("filtered file missing header line %q:\n%s", want, s)
		}
	}
	// Dialogue filtering.
	if !strings.Contains(s, "Bottom A") || !strings.Contains(s, "Bottom C") {
		t.Errorf("filtered file missing expected bottom dialogues:\n%s", s)
	}
	if strings.Contains(s, "Top B") {
		t.Errorf("filtered file leaked top dialogue:\n%s", s)
	}
	// Comment rows dropped from [Events] (cleaner libass input).
	if strings.Contains(s, "Comment:") {
		t.Errorf("filtered file leaked Comment row:\n%s", s)
	}
}

func TestFilterASSByAnchor_Top(t *testing.T) {
	body := assMixedHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Bottom\n" +
		"Dialogue: 0,0:00:05.00,0:00:08.00,Default,,0,0,0,,{\\an8}Top\n"
	src := writeASS(t, "f.ass", body)
	dst := filepath.Join(filepath.Dir(src), "f-top.ass")
	if err := filterASSByAnchor(src, dst, regionTop); err != nil {
		t.Fatalf("filterASSByAnchor top: %v", err)
	}
	out, _ := os.ReadFile(dst)
	s := string(out)
	if !strings.Contains(s, "{\\an8}Top") {
		t.Errorf("missing top: %s", s)
	}
	if strings.Contains(s, ",Bottom") {
		t.Errorf("leaked bottom: %s", s)
	}
}

func TestFilterSubByAnchor_DispatchesByExt(t *testing.T) {
	// SRT dispatch.
	dir := t.TempDir()
	srt := filepath.Join(dir, "x.srt")
	srtBody := "1\n00:00:01,000 --> 00:00:04,000\nHello\n\n2\n00:00:05,000 --> 00:00:08,000\n{\\an8}Top\n\n"
	if err := os.WriteFile(srt, []byte(srtBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := filterSubByAnchor(srt, filepath.Join(dir, "x-bot.srt"), regionBottom); err != nil {
		t.Fatalf("filterSubByAnchor SRT: %v", err)
	}
	// ASS dispatch.
	ass := writeASS(t, "y.ass", assMixedHeader+
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Hello\n")
	if err := filterSubByAnchor(ass, filepath.Join(filepath.Dir(ass), "y-bot.ass"), regionBottom); err != nil {
		t.Fatalf("filterSubByAnchor ASS: %v", err)
	}
	// Unknown extension.
	if err := filterSubByAnchor("/x/y.foo", "/x/out.foo", regionBottom); err == nil {
		t.Errorf("expected error for unsupported extension")
	}
}
