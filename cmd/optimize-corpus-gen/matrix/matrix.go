// Package matrix expands {sources × targets × prefs} into the list of
// Cells the corpus run executes. Each Cell carries everything the
// runner needs to trigger one Optimize, tag the resulting capture, and
// resume from a partial run.
package matrix

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strings"
)

// PrefSet is one row of the pref-axis: the values to push via
// /:/prefs before triggering. Keyed by Plex pref ID.
type PrefSet map[string]string

// Cell is one corpus cell — one Optimize trigger's worth of context.
type Cell struct {
	SourceRatingKey string
	SourceTitle     string
	SourcePath      string // PMS-view path; the generator translates for ffprobe
	TargetTagID     int
	TargetTitle     string
	Prefs           PrefSet

	// ID is a stable fingerprint of the cell — sha1(source.title|target|prefs).
	// Used to identify the cell across runs (resume-from-manifest) and to
	// detect duplicates (same fingerprint = same Plex Optimize argv shape).
	ID string
}

// DefaultPrefMatrix returns the 32-cell Cartesian product of the
// pref-axis the generator sweeps. Each row toggles one of:
//   - HardwareAcceleratedCodecs       {true,false}    HW decode umbrella
//   - HardwareAcceleratedEncoders     {true,false}    HW encode
//   - TranscoderToneMapping           {true,false}    HW HDR tonemap
//   - TranscoderHEVCEncodingMode      {always,never}  HEVC for standard transcodes
//   - TranscoderHEVCOptimize          {true,false}    HEVC for Optimize jobs (per-axis!)
//
// 2^5 = 32 rows. Some are degenerate (HW encode on while HW codecs off
// → PMS falls back to SW); we generate all rows and let the captured
// argv reveal which combinations collapse to identical shapes.
func DefaultPrefMatrix() []PrefSet {
	bools := []string{"true", "false"}
	hevcModes := []string{"always", "never"}
	var out []PrefSet
	for _, hwc := range bools {
		for _, hwe := range bools {
			for _, tm := range bools {
				for _, hm := range hevcModes {
					for _, ho := range bools {
						out = append(out, PrefSet{
							"HardwareAcceleratedCodecs":   hwc,
							"HardwareAcceleratedEncoders": hwe,
							"TranscoderToneMapping":       tm,
							"TranscoderHEVCEncodingMode":  hm,
							"TranscoderHEVCOptimize":      ho,
						})
					}
				}
			}
		}
	}
	return out
}

// SourceRef is a minimal pointer to a library item.
type SourceRef struct {
	RatingKey string
	Title     string
	Path      string // PMS-view file path
}

// TargetRef is a minimal pointer to an Optimize target.
type TargetRef struct {
	TagID int
	Title string
}

// Expand produces Cells = sources × targets × prefs. Order: source
// outer (groups by source for cleaner resume), target middle, pref
// inner — matches how a human would batch-trigger via the UI.
func Expand(sources []SourceRef, targets []TargetRef, prefs []PrefSet) []Cell {
	var out []Cell
	for _, s := range sources {
		for _, t := range targets {
			for _, p := range prefs {
				c := Cell{
					SourceRatingKey: s.RatingKey,
					SourceTitle:     s.Title,
					SourcePath:      s.Path,
					TargetTagID:     t.TagID,
					TargetTitle:     t.Title,
					Prefs:           p,
				}
				c.ID = fingerprint(c)
				out = append(out, c)
			}
		}
	}
	return out
}

// fingerprint = sha1 of "<title>|<tagID>|<prefs sorted>". The sha1 is
// truncated to 12 hex chars — plenty of entropy at <10k cells.
func fingerprint(c Cell) string {
	keys := make([]string, 0, len(c.Prefs))
	for k := range c.Prefs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(c.SourceTitle)
	b.WriteByte('|')
	b.WriteString(itoa(c.TargetTagID))
	b.WriteByte('|')
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(c.Prefs[k])
		b.WriteByte(';')
	}
	sum := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:6])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
