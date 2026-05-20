package main

// SRT cue analysis used to tighten the pre-render's bottom band.
// See project_scaleplex_perf_tuning / docs/KNOWN_ISSUES.md "SRT sub-burn
// pre-render renders full canvas".
//
// The pre-render emits a transparent canvas the main transcode composites
// via overlay_vaapi. CPU is proportional to canvas area (libass rasterise +
// qtrle encode + main-side hwupload). For SRT cues, only the bottom of
// the frame carries text — by parsing the file ahead of pre-render we
// derive a tighter band height than the static 40% fallback.
//
// Only safe when every cue is plain bottom-aligned. Any ASS override that
// moves a cue (`{\anN}` with N>3 = mid/top row, `{\pos(...)}`, `{\move}`)
// makes us fall back to the static band so positioned cues still render.

import (
	"bufio"
	"os"
	"regexp"
	"strings"
)

// SRT line-height heuristic constants. Measured 2026-05-20 on the worker
// image with stock `subtitles=` filter + DejaVu Sans + libass default
// style. At both 4K and 1080p:
//   - text bottom sits ~5% of H above canvas bottom
//   - each rendered line takes ~5-6% of H (font + spacing)
// We use 6% per line + 5% bottom margin + 8% top safety (~1.3 lines).
// These percentages stay valid at any frame height — libass scales the
// script-coordinate font (16 pt at PlayResY=288) to the canvas.
const (
	srtBottomMarginPct = 5
	srtLineHeightPct   = 6
	srtTopSafetyPct    = 8
)

// srtMinBandSavingsPct — only return a tighter band if it shaves at least
// this percent off the static fallback. Otherwise the encode + qtrle cost
// of the band-sized canvas is not meaningfully cheaper, and we keep the
// well-trodden fallback (avoids edge-case bbox bugs for tiny wins).
const srtMinBandSavingsPct = 10

// srtPosTagPattern catches ASS override tags that move a cue off the
// default bottom-anchored layout. libass honors these even inside SRT.
//   - `\an4..9` — alignment 4-9 is mid-row or top-row
//   - `\a1..11` — legacy alignment
//   - `\pos(...)` — absolute position
//   - `\move(...)` — animated position
//   - `\org(...)` — rotation origin (rare; conservatively bail)
// Cue-internal `{\an1}`, `{\an2}`, `{\an3}` keep the cue on the bottom
// row so they DON'T trigger fallback.
var srtPosTagPattern = regexp.MustCompile(
	`\\(?:an[4-9]|a(?:[3-9]|1[01])\b|pos\s*\(|move\s*\(|org\s*\()`)

// srtTimingPattern recognises an SRT timing line:
//   00:00:01,000 --> 00:00:04,500
// Coordinates after the timing (X1:Y1 X2:Y2) are tolerated and ignored.
var srtTimingPattern = regexp.MustCompile(
	`^\d{1,2}:\d{2}:\d{2}[,.]\d{3}\s*-->\s*\d{1,2}:\d{2}:\d{2}[,.]\d{3}`)

// SRTBandResult describes what the parser decided about an SRT file's
// pre-render band requirements.
type SRTBandResult struct {
	// MaxLines is the largest line count seen in any cue (after stripping
	// override tags). Zero if no cues were found (treat as bail).
	MaxLines int
	// PositionedCue is true if any cue carries a tag that moves it off
	// the bottom row. The caller MUST use the static fallback band.
	PositionedCue bool
	// Cues is the count of parsed cues. Zero → bail.
	Cues int
}

// parseSRT scans an SRT file and reports cue metrics + any positional
// override tags. Tolerates the WebVTT-style `.` decimal separator. Stops
// scanning the moment a positional tag is found.
func parseSRT(path string) (SRTBandResult, error) {
	res := SRTBandResult{}
	f, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// SRT cues can carry long lines (translated text, song lyrics) — bump
	// the default 64 KiB token limit. 1 MiB per line is far past any real
	// subtitle and well below the file's own size.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	// State machine: walk line by line. After a timing line, count
	// subsequent non-blank lines as one cue's text.
	inCue := false
	cueLines := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if inCue {
			if line == "" {
				if cueLines > res.MaxLines {
					res.MaxLines = cueLines
				}
				res.Cues++
				inCue = false
				cueLines = 0
				continue
			}
			if srtPosTagPattern.MatchString(line) {
				res.PositionedCue = true
				return res, nil
			}
			cueLines++
			continue
		}
		if srtTimingPattern.MatchString(line) {
			inCue = true
			cueLines = 0
		}
	}
	// Flush trailing cue (file without final blank line).
	if inCue {
		if cueLines > res.MaxLines {
			res.MaxLines = cueLines
		}
		res.Cues++
	}
	return res, sc.Err()
}

// srtTightBandHeight computes a bottom-band height tight enough to cover
// `lines` rendered libass lines at canvas height `frameH`, with safety
// margin for descenders and the occasional auto-wrap. Returns an even
// height (encoder requirement).
func srtTightBandHeight(frameH, lines int) int {
	if frameH < 100 || lines < 1 {
		return frameH
	}
	h := frameH*srtBottomMarginPct/100 +
		lines*frameH*srtLineHeightPct/100 +
		frameH*srtTopSafetyPct/100
	if h%2 == 1 {
		h++
	}
	if h >= frameH {
		return frameH
	}
	return h
}

// resolveSubBand picks the bottom-band height for a given subtitle file
// (SRT, ASS, or SSA — dispatched by extension via parseSubBand).
// Returns the (possibly tighter) band height and whether it differs from
// the static fallback. The static fallback is used when:
//   - the file can't be read (returns fallback band, ok=false)
//   - no cues were parsed (empty or unrecognized format)
//   - any cue carries a positional override tag
//   - the computed tight band saves less than srtMinBandSavingsPct of
//     the fallback — not worth diverging from the well-trodden path
func resolveSubBand(subPath string, frameH, fallbackBandH int) (int, bool) {
	if subPath == "" || frameH <= 0 {
		return fallbackBandH, false
	}
	res, err := parseSubBand(subPath)
	if err != nil || res.Cues == 0 || res.PositionedCue || res.MaxLines == 0 {
		return fallbackBandH, false
	}
	tight := srtTightBandHeight(frameH, res.MaxLines)
	if tight <= 0 || tight >= fallbackBandH {
		return fallbackBandH, false
	}
	// Require minimum savings to avoid edge-case churn for marginal wins.
	saved := (fallbackBandH - tight) * 100 / fallbackBandH
	if saved < srtMinBandSavingsPct {
		return fallbackBandH, false
	}
	return tight, true
}
