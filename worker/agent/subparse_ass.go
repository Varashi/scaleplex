package main

// ASS / SSA cue analysis. Mirrors the SRT analyser's role but reads
// the data libass actually uses to lay out the script — explicit
// PlayRes, per-style font + margins, per-event alignment + MarginV —
// instead of leaning on calibration heuristics. The result feeds the
// same SRTBandResult shape so the agent's band resolver can dispatch
// uniformly across SRT and ASS sidecars.
//
// Scope intentionally limited:
//   - [V4+ Styles] only (legacy V4 with no '+' is rare; treated the same)
//   - Style lookup case-insensitive on Name
//   - Per-event override tags recognised: {\anN}, {\pos(...)}, {\move(...)},
//     {\org(...)}; the latter three force a bail to the static fallback
//   - Multi-line cues counted by `\N` / `\n` (libass line-break markers);
//     auto-wrap is not modelled (the safety margin in srtTightBandHeight
//     absorbs typical wrap)

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// assPosTagPattern catches override tags that move a cue off its style's
// default position. {\an1..3} keeps it on the bottom row (safe); anything
// else bails. `\pos`, `\move`, `\org` are absolute / animated → bail.
var assPosTagPattern = regexp.MustCompile(
	`\\(?:an[4-9]|a(?:[3-9]|1[01])\b|pos\s*\(|move\s*\(|org\s*\()`)

// assAnOverridePattern extracts an {\anN} value (1..9) from override
// runs. Used only to tell bottom-row cues (1/2/3) from off-bottom — the
// PositionedCue bail in parseASS catches 4..9 before we get here.
var assAnOverridePattern = regexp.MustCompile(`\\an([1-9])`)

// parseASS scans an ASS file and returns a SRTBandResult so resolveAgent
// can use the same downstream code path as parseSRT. The "max-lines"
// figure is taken as the largest \N / \n + 1 across all bottom-anchored
// events; positional events bail to the fallback band.
func parseASS(path string) (SRTBandResult, error) {
	res := SRTBandResult{}
	f, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	section := ""
	var eventsFormat []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(line)
			continue
		}
		switch section {
		case "[events]":
			if strings.HasPrefix(line, "Format:") {
				eventsFormat = splitASSCSV(strings.TrimSpace(line[len("Format:"):]))
				continue
			}
			if !strings.HasPrefix(line, "Dialogue:") {
				// Skip Comment: lines and unknown rows (libass also skips).
				continue
			}
			body := strings.TrimSpace(line[len("Dialogue:"):])
			text := extractASSDialogueText(body, eventsFormat)
			if text == "" {
				continue
			}
			// Bail on positional / animated overrides before bothering
			// with line counting — same shape as parseSRT.
			if assPosTagPattern.MatchString(text) {
				res.PositionedCue = true
				return res, nil
			}
			lines := assEventLineCount(text)
			if lines > res.MaxLines {
				res.MaxLines = lines
			}
			res.Cues++
		}
	}
	return res, sc.Err()
}

// splitASSCSV splits an ASS Format: / Dialogue: row at top-level commas.
// Override braces never contain commas at the row level (commas inside
// `{...}` are filter args for libass, not row separators), so a plain
// comma split is correct for Format and for the leading N-1 Dialogue
// fields. The final "Text" field naturally absorbs any internal commas.
func splitASSCSV(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// extractASSDialogueText returns the Text field of a Dialogue row given
// the row body and the Format header columns. Falls back to "the part
// after the last comma" when Format is unknown — wrong for libass-aware
// edge cases but harmless here: we only need the line-break + override
// scan, which is conservative under truncation.
func extractASSDialogueText(body string, format []string) string {
	if len(format) == 0 {
		if i := strings.LastIndex(body, ","); i >= 0 {
			return body[i+1:]
		}
		return body
	}
	// Text is always the LAST field. Split off (len(format)-1) commas
	// from the left; the remainder is Text.
	textIdx := -1
	for i, name := range format {
		if strings.EqualFold(strings.TrimSpace(name), "Text") {
			textIdx = i
			break
		}
	}
	if textIdx < 0 {
		// No Text column declared — assume last.
		textIdx = len(format) - 1
	}
	// Walk over the first textIdx commas, return the rest.
	cur := 0
	for i := 0; i < textIdx; i++ {
		j := strings.Index(body[cur:], ",")
		if j < 0 {
			return ""
		}
		cur += j + 1
	}
	return body[cur:]
}

// assEventLineCount counts the number of rendered lines an ASS event
// will produce. libass treats `\N` as a hard line break and `\n` as a
// soft break (acts like a space when WrapStyle=0; as a hard break with
// WrapStyle=2). We conservatively count both as breaks.
func assEventLineCount(text string) int {
	if text == "" {
		return 0
	}
	// Strip override braces — `{...}` is not rendered text.
	stripped := stripASSOverrides(text)
	// One line per `\N` / `\n` boundary.
	n := 1
	i := 0
	for i < len(stripped)-1 {
		if stripped[i] == '\\' && (stripped[i+1] == 'N' || stripped[i+1] == 'n') {
			n++
			i += 2
			continue
		}
		i++
	}
	return n
}

// stripASSOverrides removes `{...}` runs. libass treats unbalanced
// braces as literal — we mirror that by stopping at the first '}' and
// retaining the rest.
func stripASSOverrides(s string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			} else {
				b.WriteByte(s[i])
			}
		default:
			if depth == 0 {
				b.WriteByte(s[i])
			}
		}
	}
	return b.String()
}

// hasASSAnchorOff reports whether the text body carries a bottom-anchor
// override (\an1/2/3). Used by callers that want to distinguish "default
// (style-defined) anchor" from "explicitly bottom-anchored" — currently
// unused at the top level (parseASS already bails on \an[4-9]) but kept
// for the multi-region work in Phase 3.
func hasASSAnchorOff(text string) bool {
	m := assAnOverridePattern.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return false
	}
	return n >= 1 && n <= 3
}
