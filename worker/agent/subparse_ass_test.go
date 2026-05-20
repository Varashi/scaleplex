package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeASS(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

const assHeader = `[Script Info]
ScriptType: v4.00+
PlayResX: 1920
PlayResY: 1080
WrapStyle: 0

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,48,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,1,2,10,10,30,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`

func TestParseASS_SingleLineBottomCues(t *testing.T) {
	body := assHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Plain single line\n" +
		"Dialogue: 0,0:00:05.00,0:00:08.00,Default,,0,0,0,,Another single line\n"
	r, err := parseASS(writeASS(t, "a.ass", body))
	if err != nil {
		t.Fatalf("parseASS: %v", err)
	}
	if r.PositionedCue {
		t.Errorf("PositionedCue=true, want false")
	}
	if r.Cues != 2 || r.MaxLines != 1 {
		t.Errorf("Cues=%d MaxLines=%d, want 2/1", r.Cues, r.MaxLines)
	}
}

func TestParseASS_MultiLineBackslashN(t *testing.T) {
	body := assHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Line one\\NLine two\n" +
		"Dialogue: 0,0:00:05.00,0:00:08.00,Default,,0,0,0,,Line A\\NLine B\\NLine C\n"
	r, err := parseASS(writeASS(t, "ml.ass", body))
	if err != nil {
		t.Fatalf("parseASS: %v", err)
	}
	if r.MaxLines != 3 {
		t.Errorf("MaxLines=%d, want 3", r.MaxLines)
	}
	if r.PositionedCue {
		t.Errorf("PositionedCue=true on plain multi-line")
	}
}

func TestParseASS_LowercaseBackslashNAlsoCounts(t *testing.T) {
	// libass treats `\n` as a soft break (sometimes hard). We count it
	// conservatively.
	body := assHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Line\\nbreak\n"
	r, err := parseASS(writeASS(t, "ln.ass", body))
	if err != nil {
		t.Fatalf("parseASS: %v", err)
	}
	if r.MaxLines != 2 {
		t.Errorf("MaxLines=%d, want 2 (\\n counted)", r.MaxLines)
	}
}

func TestParseASS_PositionalAn8Bails(t *testing.T) {
	body := assHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,{\\an8}Top sign\n"
	r, err := parseASS(writeASS(t, "an8.ass", body))
	if err != nil {
		t.Fatalf("parseASS: %v", err)
	}
	if !r.PositionedCue {
		t.Errorf("PositionedCue=false, want true (\\an8)")
	}
}

func TestParseASS_PositionalPosBails(t *testing.T) {
	body := assHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,{\\pos(640,360)}Pinned\n"
	r, err := parseASS(writeASS(t, "pos.ass", body))
	if err != nil {
		t.Fatalf("parseASS: %v", err)
	}
	if !r.PositionedCue {
		t.Errorf("PositionedCue=false, want true (\\pos)")
	}
}

func TestParseASS_PositionalAn1To3_NoBail(t *testing.T) {
	body := assHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,{\\an1}Bottom-left\n" +
		"Dialogue: 0,0:00:05.00,0:00:08.00,Default,,0,0,0,,{\\an2}Bottom-center\n" +
		"Dialogue: 0,0:00:09.00,0:00:11.00,Default,,0,0,0,,{\\an3}Bottom-right\n"
	r, err := parseASS(writeASS(t, "bot.ass", body))
	if err != nil {
		t.Fatalf("parseASS: %v", err)
	}
	if r.PositionedCue {
		t.Errorf("PositionedCue=true, want false (an1/2/3 stay on bottom row)")
	}
	if r.MaxLines != 1 {
		t.Errorf("MaxLines=%d, want 1", r.MaxLines)
	}
}

// Override runs are stripped from line-count math; `{\b1}` inside a cue
// must not be counted.
func TestParseASS_OverridesStrippedFromCount(t *testing.T) {
	body := assHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,{\\b1}Bold{\\b0} line\\NSecond\n"
	r, err := parseASS(writeASS(t, "ovr.ass", body))
	if err != nil {
		t.Fatalf("parseASS: %v", err)
	}
	if r.MaxLines != 2 {
		t.Errorf("MaxLines=%d, want 2 (override runs not counted)", r.MaxLines)
	}
}

// Comment: rows must not contribute to the cue count.
func TestParseASS_CommentRowsSkipped(t *testing.T) {
	body := assHeader +
		"Comment: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,this is meta\n" +
		"Dialogue: 0,0:00:03.00,0:00:06.00,Default,,0,0,0,,Real cue\n"
	r, err := parseASS(writeASS(t, "cmt.ass", body))
	if err != nil {
		t.Fatalf("parseASS: %v", err)
	}
	if r.Cues != 1 {
		t.Errorf("Cues=%d, want 1 (comment skipped)", r.Cues)
	}
}

func TestParseASS_MissingFile(t *testing.T) {
	if _, err := parseASS("/nope/none.ass"); err == nil {
		t.Errorf("expected error for missing file")
	}
}

// Agent-side dispatch: resolveSubBand picks parseASS for an .ass file
// and computes a tight band on a plain ASS sidecar at 4K.
func TestResolveSubBand_ASS_Tight(t *testing.T) {
	body := assHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Hello world\n"
	got, ok := resolveSubBand(writeASS(t, "ok.ass", body), 2160, 864)
	if !ok {
		t.Fatalf("resolveSubBand: ok=false, want true for plain ASS 1-line")
	}
	if got >= 864 {
		t.Errorf("resolveSubBand: got %d, want < 864", got)
	}
}

func TestResolveSubBand_ASS_PositionedFallback(t *testing.T) {
	body := assHeader +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,{\\an8}Top\n"
	got, ok := resolveSubBand(writeASS(t, "an8.ass", body), 2160, 864)
	if ok || got != 864 {
		t.Errorf("ASS positional: got=%d ok=%v, want 864/false", got, ok)
	}
}

func TestResolveSubBand_UnknownExtFallback(t *testing.T) {
	p := writeASS(t, "thing.bogus", "irrelevant")
	got, ok := resolveSubBand(p, 2160, 864)
	if ok || got != 864 {
		t.Errorf("unknown ext: got=%d ok=%v, want 864/false", got, ok)
	}
}

func TestLowerExt(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/x/y.SRT", ".srt"},
		{"/x/y.ass", ".ass"},
		{"/x/y.SSA", ".ssa"},
		{"/x/y", ""},
		{"/x/y.", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := lowerExt(c.in); got != c.want {
			t.Errorf("lowerExt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Sanity: stripping override braces never produces visible {} when the
// braces were balanced.
func TestStripASSOverrides_Balanced(t *testing.T) {
	got := stripASSOverrides("Plain {\\b1}bold{\\b0} done")
	if strings.Contains(got, "{") || strings.Contains(got, "}") {
		t.Errorf("braces leaked: %q", got)
	}
}
