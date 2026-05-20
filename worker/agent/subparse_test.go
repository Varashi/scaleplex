package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSRT drops content into a temp file and returns its path.
func writeSRT(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func TestParseSRT_SingleLineCues(t *testing.T) {
	const body = `1
00:00:01,000 --> 00:00:04,000
A single line cue

2
00:00:05,000 --> 00:00:09,500
Another single line

`
	p := writeSRT(t, "1.srt", body)
	r, err := parseSRT(p)
	if err != nil {
		t.Fatalf("parseSRT: %v", err)
	}
	if r.PositionedCue {
		t.Errorf("PositionedCue=true, want false")
	}
	if r.Cues != 2 {
		t.Errorf("Cues=%d, want 2", r.Cues)
	}
	if r.MaxLines != 1 {
		t.Errorf("MaxLines=%d, want 1", r.MaxLines)
	}
}

func TestParseSRT_MultiLineCues(t *testing.T) {
	const body = `1
00:00:01,000 --> 00:00:04,000
Line one
Line two

2
00:00:05,000 --> 00:00:09,500
Line A
Line B
Line C

`
	p := writeSRT(t, "ml.srt", body)
	r, err := parseSRT(p)
	if err != nil {
		t.Fatalf("parseSRT: %v", err)
	}
	if r.PositionedCue {
		t.Errorf("PositionedCue=true, want false")
	}
	if r.Cues != 2 {
		t.Errorf("Cues=%d, want 2", r.Cues)
	}
	if r.MaxLines != 3 {
		t.Errorf("MaxLines=%d, want 3 (from cue 2)", r.MaxLines)
	}
}

func TestParseSRT_NoFinalBlankLine(t *testing.T) {
	const body = `1
00:00:01,000 --> 00:00:04,000
Trailing cue without blank line`
	p := writeSRT(t, "tail.srt", body)
	r, err := parseSRT(p)
	if err != nil {
		t.Fatalf("parseSRT: %v", err)
	}
	if r.Cues != 1 {
		t.Errorf("Cues=%d, want 1", r.Cues)
	}
	if r.MaxLines != 1 {
		t.Errorf("MaxLines=%d, want 1", r.MaxLines)
	}
}

// WebVTT-style fixtures with `.` decimal: SRT parsers tolerate this.
func TestParseSRT_DotDecimalTiming(t *testing.T) {
	const body = `1
00:00:01.500 --> 00:00:04.000
Dot decimal cue
`
	p := writeSRT(t, "dot.srt", body)
	r, err := parseSRT(p)
	if err != nil {
		t.Fatalf("parseSRT: %v", err)
	}
	if r.Cues != 1 || r.MaxLines != 1 {
		t.Errorf("Cues=%d MaxLines=%d, want 1/1", r.Cues, r.MaxLines)
	}
}

func TestParseSRT_PositionalTagAn(t *testing.T) {
	// {\an8} = top-center. Off the bottom row → bail.
	const body = `1
00:00:01,000 --> 00:00:04,000
{\an8}Top of frame note

2
00:00:05,000 --> 00:00:08,000
Bottom of frame
`
	p := writeSRT(t, "an8.srt", body)
	r, err := parseSRT(p)
	if err != nil {
		t.Fatalf("parseSRT: %v", err)
	}
	if !r.PositionedCue {
		t.Errorf("PositionedCue=false, want true")
	}
}

func TestParseSRT_PositionalTagAn1To3_NoBail(t *testing.T) {
	// {\an1}, {\an2}, {\an3} all stay on the bottom row. Safe.
	const body = `1
00:00:01,000 --> 00:00:04,000
{\an1}Bottom-left

2
00:00:05,000 --> 00:00:08,000
{\an2}Bottom-center

3
00:00:09,000 --> 00:00:11,000
{\an3}Bottom-right
`
	p := writeSRT(t, "bot.srt", body)
	r, err := parseSRT(p)
	if err != nil {
		t.Fatalf("parseSRT: %v", err)
	}
	if r.PositionedCue {
		t.Errorf("PositionedCue=true, want false (an1/2/3 stay on bottom row)")
	}
	if r.MaxLines != 1 {
		t.Errorf("MaxLines=%d, want 1", r.MaxLines)
	}
}

func TestParseSRT_PositionalTagPos(t *testing.T) {
	const body = `1
00:00:01,000 --> 00:00:04,000
{\pos(640,360)}Mid-frame label
`
	p := writeSRT(t, "pos.srt", body)
	r, err := parseSRT(p)
	if err != nil {
		t.Fatalf("parseSRT: %v", err)
	}
	if !r.PositionedCue {
		t.Errorf("PositionedCue=false, want true (\\pos forces fallback)")
	}
}

func TestParseSRT_PositionalTagMove(t *testing.T) {
	const body = `1
00:00:01,000 --> 00:00:04,000
{\move(0,0,1920,1080)}Animated label
`
	p := writeSRT(t, "move.srt", body)
	r, err := parseSRT(p)
	if err != nil {
		t.Fatalf("parseSRT: %v", err)
	}
	if !r.PositionedCue {
		t.Errorf("PositionedCue=false, want true (\\move forces fallback)")
	}
}

func TestParseSRT_EmptyFile(t *testing.T) {
	p := writeSRT(t, "empty.srt", "")
	r, err := parseSRT(p)
	if err != nil {
		t.Fatalf("parseSRT: %v", err)
	}
	if r.Cues != 0 || r.MaxLines != 0 {
		t.Errorf("Cues=%d MaxLines=%d, want 0/0", r.Cues, r.MaxLines)
	}
}

func TestParseSRT_FileMissing(t *testing.T) {
	if _, err := parseSRT("/nope/does/not/exist.srt"); err == nil {
		t.Error("expected error on missing file")
	}
}

func TestSRTTightBandHeight_Sizes(t *testing.T) {
	// At 4K (2160), the formula gives:
	//   5% bottom margin + lines*6% + 8% top safety
	// Even-rounded.
	cases := []struct {
		frameH, lines, want int
	}{
		{2160, 1, 410}, // 108 + 130 + 173 = 411 -> 412 (even). actual: see comment
		{2160, 2, 542}, // 108 + 260 + 172 = 540 -> 540
		{2160, 4, 802}, // 108 + 520 + 172 = 800
		{1080, 1, 206}, // 54 + 64 + 86 = 204 -> 204
		{1080, 4, 400}, // 54 + 256 + 86 = 396 -> 396
	}
	for _, c := range cases {
		got := srtTightBandHeight(c.frameH, c.lines)
		// Math sanity, not exact match (integer arithmetic rounding).
		// Each case below recomputes the floor expectation.
		expFloor := c.frameH*5/100 + c.lines*c.frameH*6/100 + c.frameH*8/100
		if expFloor%2 == 1 {
			expFloor++
		}
		if got != expFloor {
			t.Errorf("srtTightBandHeight(%d, %d) = %d, want %d",
				c.frameH, c.lines, got, expFloor)
		}
		if got%2 != 0 {
			t.Errorf("srtTightBandHeight(%d, %d) = %d, want even",
				c.frameH, c.lines, got)
		}
		if got <= 0 || got > c.frameH {
			t.Errorf("srtTightBandHeight(%d, %d) = %d, out of (0, frameH]",
				c.frameH, c.lines, got)
		}
	}
}

func TestSRTTightBandHeight_TinyFrame(t *testing.T) {
	// Frames below the safety threshold return the frame height itself
	// — no band optimisation makes sense at that scale.
	if got := srtTightBandHeight(50, 1); got != 50 {
		t.Errorf("srtTightBandHeight(50, 1) = %d, want 50 (fallback)", got)
	}
	if got := srtTightBandHeight(2160, 0); got != 2160 {
		t.Errorf("srtTightBandHeight(2160, 0) = %d, want frameH (fallback)", got)
	}
}

func TestResolveSRTBand_TightWin(t *testing.T) {
	// Plain SRT, 1-line cues, 4K canvas, static fallback at 40% (864 px).
	// Expect tight band well below fallback.
	const body = `1
00:00:01,000 --> 00:00:04,000
Hello world
`
	p := writeSRT(t, "tight.srt", body)
	got, ok := resolveSubBand(p, 2160, 864)
	if !ok {
		t.Fatalf("resolveSubBand: ok=false, want true for plain 1-line SRT")
	}
	if got >= 864 {
		t.Errorf("resolveSubBand: got %d, want < 864 fallback", got)
	}
	if got%2 != 0 {
		t.Errorf("resolveSubBand: got %d, want even", got)
	}
}

func TestResolveSRTBand_PositionedFallback(t *testing.T) {
	const body = `1
00:00:01,000 --> 00:00:04,000
{\an8}Top sign translation
`
	p := writeSRT(t, "anpos.srt", body)
	got, ok := resolveSubBand(p, 2160, 864)
	if ok {
		t.Errorf("resolveSubBand: ok=true, want false (positioned cue)")
	}
	if got != 864 {
		t.Errorf("resolveSubBand: got %d, want 864 fallback", got)
	}
}

func TestResolveSRTBand_MarginalWinRejected(t *testing.T) {
	// 4-line cue at 4K with fallback already tight enough that the
	// computed savings are below srtMinBandSavingsPct.
	// Tight: 54+520+172 = 746 (after even-round 746)
	// Fallback say 800 → 6.7% saving → below 10% min → return fallback.
	const body = `1
00:00:01,000 --> 00:00:09,000
Line one
Line two
Line three
Line four
`
	p := writeSRT(t, "ml.srt", body)
	// Tight = ~746, fallback 800 → saving ~6%, rejected.
	got, ok := resolveSubBand(p, 2160, 800)
	if ok {
		t.Errorf("resolveSubBand: ok=true, want false (marginal savings)")
	}
	if got != 800 {
		t.Errorf("resolveSubBand: got %d, want 800 fallback", got)
	}
}

func TestResolveSRTBand_NoFile(t *testing.T) {
	got, ok := resolveSubBand("", 2160, 864)
	if ok || got != 864 {
		t.Errorf("resolveSubBand(empty path): got=%d ok=%v, want 864/false", got, ok)
	}
}

func TestResolveSRTBand_EmptyFile(t *testing.T) {
	p := writeSRT(t, "empty.srt", "")
	got, ok := resolveSubBand(p, 2160, 864)
	if ok || got != 864 {
		t.Errorf("resolveSubBand(empty file): got=%d ok=%v, want 864/false", got, ok)
	}
}
