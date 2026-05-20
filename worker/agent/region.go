package main

// Multi-region pre-render planning (v1.2.2 Phase 3).
//
// Single-region tight-band (Phase 1+2) bails to the static 40 % fallback
// the moment any cue carries a non-bottom anchor (`\an7-9`, `\pos`). HI
// (hearing-impaired) SRTs commonly mix default-bottom dialogue with the
// occasional `{\an8}` sign translation or environmental cue at the top,
// which forced the entire session onto the wide band.
//
// Phase 3 buckets the cues by anchor row and, when more than one row is
// populated, emits one pre-render per row at a tight band sized to that
// row's own max-line count. The main filter graph chains overlay_vaapi
// instances per region. The agent owns this decision because the cue
// data only becomes available post-extract (embedded SRT) or post-parse
// (sidecar) — same reason Phase 1 moved the resolve to the agent.
//
// Scope in v1.2.2:
//   - SRT only (sidecar + extracted-from-embedded). ASS multi-region is
//     a follow-up: the parser already classifies positional cues, but
//     the chained-overlay filter-graph mutation is currently SRT-shape-
//     specific.
//   - Bottom (\an1-3 / default) + Top (\an7-9). Middle (\an4-6) bails
//     to single-region fallback.
//   - `\pos`, `\move`, `\org` overrides bail to single-region fallback
//     (no anchor row to assign).

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// regionBottom — default + `\an1`/`\an2`/`\an3`.
	regionBottom = 2
	// regionTop — `\an7`/`\an8`/`\an9`.
	regionTop = 8
)

// srtAnchorAnPattern catches `{\anN}` at the head of a cue text line.
// We classify by N; non-bottom-non-top values (4,5,6) trigger bail.
var srtAnchorAnPattern = regexp.MustCompile(`\{\\an([1-9])\}`)

// srtAbsoluteOverridePattern catches absolute-position tags. Presence
// of any forces bail (no row assignment makes sense).
var srtAbsoluteOverridePattern = regexp.MustCompile(`\\(?:pos\s*\(|move\s*\(|org\s*\()`)

// SRTRegionCounts is the cue distribution across anchor rows. Cues with
// override-bail tags increment Positional. The agent uses this to
// decide between single-region and multi-region pre-render.
type SRTRegionCounts struct {
	Bottom     int
	Top        int
	Middle     int
	Positional int
	// MaxLinesBottom / MaxLinesTop — largest line count per region,
	// used to size each region's band.
	MaxLinesBottom int
	MaxLinesTop    int
}

// countSRTRegions walks an SRT file and classifies each cue's text
// lines by the leading anchor tag (or default bottom). Mirrors parseSRT
// in iteration shape but doesn't bail on the first positional cue —
// it records the distribution so the agent can pick the right plan.
func countSRTRegions(srtPath string) (SRTRegionCounts, error) {
	var rc SRTRegionCounts
	f, err := os.Open(srtPath)
	if err != nil {
		return rc, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	inCue := false
	cueLines := 0
	cueAnchor := regionBottom // default
	cueBail := false
	flushCue := func() {
		if cueBail {
			rc.Positional++
			return
		}
		switch cueAnchor {
		case regionBottom:
			rc.Bottom++
			if cueLines > rc.MaxLinesBottom {
				rc.MaxLinesBottom = cueLines
			}
		case regionTop:
			rc.Top++
			if cueLines > rc.MaxLinesTop {
				rc.MaxLinesTop = cueLines
			}
		default:
			rc.Middle++
		}
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if inCue {
			if line == "" {
				flushCue()
				inCue = false
				cueLines = 0
				cueAnchor = regionBottom
				cueBail = false
				continue
			}
			if srtAbsoluteOverridePattern.MatchString(line) {
				cueBail = true
				cueLines++
				continue
			}
			if m := srtAnchorAnPattern.FindStringSubmatch(line); m != nil {
				switch m[1] {
				case "1", "2", "3":
					cueAnchor = regionBottom
				case "7", "8", "9":
					cueAnchor = regionTop
				default:
					cueAnchor = 5 // middle marker — not in v1.2.2 plan
				}
			}
			cueLines++
			continue
		}
		if srtTimingPattern.MatchString(line) {
			inCue = true
			cueLines = 0
			cueAnchor = regionBottom
			cueBail = false
		}
	}
	if inCue {
		flushCue()
	}
	return rc, sc.Err()
}

// planMultiRegion decides whether a multi-region plan is worth running
// for the given SRT + canvas height. Returns the regions to render or
// an empty slice when the single-region path should be used (which the
// caller then handles via ResolveAgentBand as in Phase 1+2).
//
// Multi-region triggers only when:
//   - bottom + top rows both have cues
//   - no positional / middle cues (those bail to single-region fallback)
//   - both regions' tight bands beat the static fallback meaningfully
func planMultiRegion(srtPath string, frameH, fallbackBandH int) []RegionPrerenderSpec {
	if srtPath == "" || frameH <= 0 || fallbackBandH <= 0 {
		return nil
	}
	rc, err := countSRTRegions(srtPath)
	if err != nil {
		return nil
	}
	if rc.Bottom == 0 || rc.Top == 0 {
		return nil // single-region; let Phase 1+2 handle it
	}
	if rc.Middle > 0 || rc.Positional > 0 {
		return nil // bail to single-region fallback (handled upstream)
	}

	bottomBand := srtTightBandHeight(frameH, rc.MaxLinesBottom)
	topBand := srtTightBandHeight(frameH, rc.MaxLinesTop)
	if bottomBand <= 0 || topBand <= 0 {
		return nil
	}
	if bottomBand >= fallbackBandH || topBand >= fallbackBandH {
		return nil // no win
	}

	sessionDir := filepath.Dir(srtPath)
	bottomFile := filepath.Join(sessionDir, "scaleplex-sub-region-bottom.srt")
	topFile := filepath.Join(sessionDir, "scaleplex-sub-region-top.srt")
	if err := filterSRTByAnchor(srtPath, bottomFile, regionBottom); err != nil {
		return nil
	}
	if err := filterSRTByAnchor(srtPath, topFile, regionTop); err != nil {
		return nil
	}

	return []RegionPrerenderSpec{
		{
			Anchor:       regionBottom,
			FIFOPath:     filepath.Join(sessionDir, "scaleplex-sub-overlay-bottom.fifo"),
			FilteredFile: bottomFile,
			BandHeight:   bottomBand,
			BandY:        frameH - bottomBand,
			MaxLines:     rc.MaxLinesBottom,
		},
		{
			Anchor:       regionTop,
			FIFOPath:     filepath.Join(sessionDir, "scaleplex-sub-overlay-top.fifo"),
			FilteredFile: topFile,
			BandHeight:   topBand,
			BandY:        0,
			MaxLines:     rc.MaxLinesTop,
		},
	}
}

// filterSRTByAnchor copies cues belonging to the requested anchor row
// from srcPath into dstPath. Default-anchored cues are emitted with no
// override (bottom row) regardless of the requested target; only when
// the requested target is regionBottom do they actually get written. A
// `{\anN}` at the head of a cue text line determines its row.
func filterSRTByAnchor(srcPath, dstPath string, anchor int) error {
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

	type cue struct {
		header []string
		body   []string
	}
	emit := func(c *cue) error {
		for _, ln := range c.header {
			if _, err := w.WriteString(ln + "\n"); err != nil {
				return err
			}
		}
		for _, ln := range c.body {
			if _, err := w.WriteString(ln + "\n"); err != nil {
				return err
			}
		}
		if _, err := w.WriteString("\n"); err != nil {
			return err
		}
		return nil
	}

	cur := &cue{}
	cueIdx := 0
	state := 0 // 0=between, 1=after-id, 2=in-body
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		switch state {
		case 0:
			if line == "" {
				continue
			}
			cur = &cue{header: []string{line}}
			state = 1
		case 1:
			cur.header = append(cur.header, line)
			if srtTimingPattern.MatchString(line) {
				state = 2
			}
		case 2:
			if line == "" {
				if classifyCueAnchor(cur.body) == anchor {
					cueIdx++
					cur.header[0] = fmt.Sprintf("%d", cueIdx)
					if err := emit(cur); err != nil {
						return err
					}
				}
				cur = &cue{}
				state = 0
				continue
			}
			cur.body = append(cur.body, line)
		}
	}
	if state == 2 && classifyCueAnchor(cur.body) == anchor {
		cueIdx++
		cur.header[0] = fmt.Sprintf("%d", cueIdx)
		if err := emit(cur); err != nil {
			return err
		}
	}
	return sc.Err()
}

// classifyCueAnchor inspects a cue's text lines and returns the anchor
// row marker (regionBottom or regionTop). Default = regionBottom.
func classifyCueAnchor(body []string) int {
	for _, ln := range body {
		if m := srtAnchorAnPattern.FindStringSubmatch(ln); m != nil {
			switch m[1] {
			case "7", "8", "9":
				return regionTop
			case "1", "2", "3":
				return regionBottom
			}
		}
	}
	return regionBottom
}

// regionSentinel returns the per-region BandY placeholder the agent
// patches once the actual y-offset is known. Region 0 (bottom) reuses
// the legacy BandYSentinel so the single-region patcher remains valid
// for that slot. Additional regions get `__SP_BANDY{anchor}__`.
func regionSentinel(idx, anchor int) string {
	if idx == 0 {
		return BandYSentinel
	}
	return fmt.Sprintf("__SP_BANDY%d__", anchor)
}

// MutateGraphForMultiRegion rewrites the filter_complex string the
// rewriter emitted (single-region with `y=__SP_BANDY__`) into a multi-
// region chained-overlay form. Returns the new graph; the caller is
// responsible for appending the extra `-i <fifo>` inputs in the same
// order as regions[1:].
//
// Rewriter shapes recognised (the only two it emits for this path):
//
//	initial play:
//	  ...[N:v]format=bgra,hwupload[12];[11][12]overlay_vaapi=x=0:y=__SP_BANDY__:...[4]
//
//	seek:
//	  ...[N:v]setpts=PTS-STARTPTS,format=bgra,hwupload[12];[11][12]overlay_vaapi=x=0:y=__SP_BANDY__:...[13];[13]setpts=PTS+...[4]
//
// firstFIFOInput is the input index that the rewriter assigned to the
// single FIFO (the bottom region in the new plan); region 1, 2, ...
// get firstFIFOInput+1, +2, ... as their input indices.
func MutateGraphForMultiRegion(graph string, regions []RegionPrerenderSpec, firstFIFOInput int, seek bool) (string, error) {
	if len(regions) < 2 {
		return graph, fmt.Errorf("multi-region rewrite called with %d regions", len(regions))
	}
	const bandyToken = "x=0:y=" + BandYSentinel + ":eof_action=pass:repeatlast=1"
	idx := strings.Index(graph, bandyToken)
	if idx < 0 {
		return graph, fmt.Errorf("multi-region: BandYSentinel not found in graph")
	}
	headLabel := "[4]"
	if seek {
		headLabel = "[13]"
	}
	tail := graph[idx+len(bandyToken):]
	if !strings.HasPrefix(tail, headLabel) {
		return graph, fmt.Errorf("multi-region: expected %s after overlay_vaapi, got %q",
			headLabel, tail[:min(len(tail), 12)])
	}
	fifoInputToken := fmt.Sprintf("[%d:v]", firstFIFOInput)
	segStart := strings.Index(graph, fifoInputToken)
	if segStart < 0 {
		return graph, fmt.Errorf("multi-region: input token %s not found", fifoInputToken)
	}
	segEnd := idx + len(bandyToken) + len(headLabel)

	// Intermediate labels live in the 20+ range to avoid collision with
	// the rewriter's [4]/[10]/[11]/[12]/[13]. For region i (i>0):
	//   FIFO format label = [20+i*2]   (e.g. region 1 -> [22])
	//   chain output label = [21+i*2]  (e.g. region 1 -> [23])
	// First region keeps [12] for its fifo-format slot to match the
	// rewriter's downstream chain expectations.
	setptsPrefix := ""
	if seek {
		setptsPrefix = "setpts=PTS-STARTPTS,"
	}

	var b strings.Builder
	for i, r := range regions {
		fmtLabel := "[12]"
		if i > 0 {
			fmtLabel = fmt.Sprintf("[%d]", 20+i*2)
		}
		prevMainLabel := "[11]"
		if i > 0 {
			prevMainLabel = fmt.Sprintf("[%d]", 21+(i-1)*2)
		}
		ovOut := headLabel
		if i < len(regions)-1 {
			ovOut = fmt.Sprintf("[%d]", 21+i*2)
		}
		bandySlot := regionSentinel(i, r.Anchor)
		if i > 0 {
			b.WriteString(";")
		}
		fmt.Fprintf(&b, "[%d:v]%sformat=bgra,hwupload%s",
			firstFIFOInput+i, setptsPrefix, fmtLabel)
		fmt.Fprintf(&b, ";%s%soverlay_vaapi=x=0:y=%s:eof_action=pass:repeatlast=1%s",
			prevMainLabel, fmtLabel, bandySlot, ovOut)
	}
	return graph[:segStart] + b.String() + graph[segEnd:], nil
}

// PatchMainArgsBandYMulti substitutes each region's BandY placeholder
// in args with the resolved integer y-offset. Region 0 patches the
// legacy BandYSentinel (so single-region paths can still reuse this
// helper); additional regions patch their per-anchor sentinel.
// Returns the count of substitutions for diagnostic logging.
func PatchMainArgsBandYMulti(args []string, regions []RegionPrerenderSpec) int {
	if len(args) == 0 || len(regions) == 0 {
		return 0
	}
	n := 0
	for i, r := range regions {
		token := regionSentinel(i, r.Anchor)
		repl := fmt.Sprintf("%d", r.BandY)
		for j, a := range args {
			if !strings.Contains(a, token) {
				continue
			}
			c := strings.Count(a, token)
			args[j] = strings.ReplaceAll(a, token, repl)
			n += c
		}
	}
	return n
}
