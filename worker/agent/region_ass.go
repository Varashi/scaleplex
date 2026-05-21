package main

// ASS multi-region support. Mirrors region.go's SRT side but walks ASS
// `[Events] Dialogue` rows instead of SRT cue blocks. Style-table-based
// default anchor lookup is intentionally not implemented: most homelab
// ASS sidecars use the bundled Default style (Alignment=2 = bottom),
// and the per-event `{\anN}` override is what flips region. Falling
// through to regionBottom on style-only anchoring is the same answer
// for >99 % of real-world content.

import (
	"bufio"
	"os"
	"strings"
)

// countASSRegions walks an ASS file and bins each Dialogue event into
// {Bottom, Middle, Top} using the same rule classifyCueAnchor uses for
// SRT (default bottom unless an `\anN` override is present). Events
// with `\pos` / `\move` / `\org` overrides increment Positional and
// force the caller to bail to single-region.
func countASSRegions(path string) (SRTRegionCounts, error) {
	var rc SRTRegionCounts
	f, err := os.Open(path)
	if err != nil {
		return rc, err
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
		if section != "[events]" {
			continue
		}
		if strings.HasPrefix(line, "Format:") {
			eventsFormat = splitASSCSV(strings.TrimSpace(line[len("Format:"):]))
			continue
		}
		if !strings.HasPrefix(line, "Dialogue:") {
			continue
		}
		body := strings.TrimSpace(line[len("Dialogue:"):])
		text := extractASSDialogueText(body, eventsFormat)
		if text == "" {
			continue
		}
		if srtAbsoluteOverridePattern.MatchString(text) {
			rc.Positional++
			continue
		}
		lines := assEventLineCount(text)
		anchor := regionBottom
		if m := srtAnchorAnPattern.FindStringSubmatch(text); m != nil {
			anchor = anchorFromTag(m[1])
		}
		switch anchor {
		case regionBottom:
			rc.Bottom++
			if lines > rc.MaxLinesBottom {
				rc.MaxLinesBottom = lines
			}
		case regionMiddle:
			rc.Middle++
			if lines > rc.MaxLinesMiddle {
				rc.MaxLinesMiddle = lines
			}
		case regionTop:
			rc.Top++
			if lines > rc.MaxLinesTop {
				rc.MaxLinesTop = lines
			}
		}
	}
	return rc, sc.Err()
}

// filterASSByAnchor writes a subset ASS file containing every header
// section ([Script Info], [V4+ Styles], [Aegisub Project Garbage],
// [Fonts], [Graphics], etc.) verbatim plus the [Events] Format row
// followed by only the Dialogue rows whose anchor matches. Non-Dialogue
// lines in [Events] (Comment:, Picture:, ...) are dropped to keep the
// filtered file compact.
func filterASSByAnchor(srcPath, dstPath string, anchor int) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	defer w.Flush()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	section := ""
	var eventsFormat []string
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		// Section headers are written verbatim so libass parses them.
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(line)
			if _, err := w.WriteString(raw + "\n"); err != nil {
				return err
			}
			continue
		}
		// Outside [Events] every line passes through unchanged so the
		// styles / fonts / project info needed by libass survive.
		if section != "[events]" {
			if _, err := w.WriteString(raw + "\n"); err != nil {
				return err
			}
			continue
		}
		// Inside [Events]:
		//  - Format row: copy + remember columns
		//  - Dialogue: keep only matching anchor
		//  - everything else (Comment:, blank): drop (we already
		//    skipped blanks via TrimSpace = ""; explicit non-Dialogue
		//    rows would leak otherwise, drop to keep the file clean).
		if strings.HasPrefix(line, "Format:") {
			eventsFormat = splitASSCSV(strings.TrimSpace(line[len("Format:"):]))
			if _, err := w.WriteString(raw + "\n"); err != nil {
				return err
			}
			continue
		}
		if !strings.HasPrefix(line, "Dialogue:") {
			continue
		}
		body := strings.TrimSpace(line[len("Dialogue:"):])
		text := extractASSDialogueText(body, eventsFormat)
		if text == "" {
			continue
		}
		if srtAbsoluteOverridePattern.MatchString(text) {
			// Should not reach here — countASSRegions bails to single-
			// region the moment Positional > 0. Drop defensively.
			continue
		}
		evAnchor := regionBottom
		if m := srtAnchorAnPattern.FindStringSubmatch(text); m != nil {
			evAnchor = anchorFromTag(m[1])
		}
		if evAnchor != anchor {
			continue
		}
		if _, err := w.WriteString(raw + "\n"); err != nil {
			return err
		}
	}
	return sc.Err()
}
