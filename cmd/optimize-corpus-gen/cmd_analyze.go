package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// runAnalyze clusters captured Optimize argvs by shape fingerprint
// and reports the distribution. The output answers the core question
// the corpus exists to answer: how many DISTINCT argv shapes does
// PMS's Optimize produce across the matrix? — and therefore how many
// branches a fully-orthogonal Optimize rewriter would need to handle.
//
// Fingerprint axes (deliberately broad enough to cluster cosmetically
// different argvs that the rewriter would treat identically, but
// narrow enough to surface the branching points):
//
//   - decode_codec       (-codec:0 value)
//   - decode_hwaccel     (-hwaccel:0 present? value?)
//   - encode_codec       (-codec:0 post-`-i`)
//   - filter_present     (-filter_complex non-empty?)
//   - filter_shape       (canonicalized: scale dims masked, label ints normalized)
//   - sub_burn           (filter mentions inlineass / sub2video / overlay?)
//   - tonemap            (filter mentions tonemap?)
//   - audio_codec        (-codec:1 post-`-i`)
//   - num_outputs        (count of -map flags pointing at "[N]" labels)
//
// Captures whose fingerprints match are bucketed together; one
// representative cell per bucket is logged with its source/target/prefs.
func runAnalyze() {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	corpusDir := fs.String("corpus-dir", os.Getenv("HOME")+"/scaleplex-corpus/optimize-sweep", "Directory of captures + sidecars to analyze")
	verbose := fs.Bool("v", false, "List every cell per bucket (not just one representative)")
	fs.Parse(os.Args[1:])

	entries, err := os.ReadDir(*corpusDir)
	if err != nil {
		fatalf("read %s: %v", *corpusDir, err)
	}

	type capture struct {
		path string
		argv []string
		tag  cellTagPartial
	}
	var captures []capture
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if strings.HasSuffix(e.Name(), ".optimize-cell.json") || e.Name() == "manifest.json" {
			continue
		}
		p := filepath.Join(*corpusDir, e.Name())
		body, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c struct {
			Argv []string `json:"argv"`
		}
		if json.Unmarshal(body, &c) != nil || len(c.Argv) == 0 {
			continue
		}
		// Sidecar.
		sc := strings.TrimSuffix(p, ".json") + ".optimize-cell.json"
		var tag cellTagPartial
		if scBody, err := os.ReadFile(sc); err == nil {
			_ = json.Unmarshal(scBody, &tag)
		}
		captures = append(captures, capture{path: p, argv: c.Argv, tag: tag})
	}
	log.Printf("loaded %d captures from %s", len(captures), *corpusDir)
	if len(captures) == 0 {
		fatalf("no captures found")
	}

	type bucket struct {
		fp      fingerprint
		cells   []capture
	}
	buckets := map[string]*bucket{}
	for _, c := range captures {
		fp := fingerprintArgv(c.argv)
		key := fp.String()
		b, ok := buckets[key]
		if !ok {
			b = &bucket{fp: fp}
			buckets[key] = b
		}
		b.cells = append(b.cells, c)
	}

	// Sort buckets by member count desc.
	var keys []string
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(buckets[keys[i]].cells) > len(buckets[keys[j]].cells) })

	fmt.Printf("\n=== %d distinct argv shapes from %d captures ===\n\n", len(buckets), len(captures))
	for i, k := range keys {
		b := buckets[k]
		pct := float64(len(b.cells)) * 100 / float64(len(captures))
		fmt.Printf("[%d] %5d cells (%5.1f%%) — %s\n", i+1, len(b.cells), pct, b.fp)
		rep := b.cells[0]
		fmt.Printf("    rep source: %s\n", rep.tag.Source.Title)
		fmt.Printf("    rep target: %s\n", rep.tag.Target.Title)
		fmt.Printf("    rep prefs:  HWcodec=%s HWenc=%s HEVCmode=%s HEVCopt=%s HDRtm=%s\n",
			rep.tag.Prefs["HardwareAcceleratedCodecs"],
			rep.tag.Prefs["HardwareAcceleratedEncoders"],
			rep.tag.Prefs["TranscoderHEVCEncodingMode"],
			rep.tag.Prefs["TranscoderHEVCOptimize"],
			rep.tag.Prefs["TranscoderToneMapping"])
		if *verbose {
			for _, c := range b.cells {
				fmt.Printf("      · %s → %s | %v\n", c.tag.Source.Title, c.tag.Target.Title, c.tag.Prefs)
			}
		}
		fmt.Println()
	}

	// Quick implication summary for the rewriter-refactor question.
	fmt.Println("Implication for the tryOptimizeRemux fold question:")
	fmt.Printf("  - distinct shapes: %d\n", len(buckets))
	if len(buckets) <= 3 {
		fmt.Println("  - LOW variety → the existing fast-path is doing little real branching; folding into the main pipeline is straightforward.")
	} else if len(buckets) <= 8 {
		fmt.Println("  - MEDIUM variety → fast-path captures more than one shape; refactor needs per-shape handling but core is small.")
	} else {
		fmt.Println("  - HIGH variety → the fast-path masks real shape diversity; the orthogonal core would need to subsume this branching too, OR the fast-path stays separate (correct architectural call).")
	}
}

// cellTagPartial is the subset of capture.CellTag the analyzer reads.
// Local copy to avoid importing the capture package's full struct +
// its source.Profile dependency just for the title strings.
type cellTagPartial struct {
	Source struct {
		Title string `json:"title"`
	} `json:"source"`
	Target struct {
		Title string `json:"title"`
	} `json:"target"`
	Prefs map[string]string `json:"prefs"`
}

// fingerprint is the canonical shape we cluster on.
type fingerprint struct {
	DecodeCodec   string
	DecodeHWAccel string
	EncodeCodec   string
	AudioCodec    string
	FilterShape   string
	SubBurn       string // "" | "inlineass" | "overlay" | "sub2video"
	Tonemap       string // "" | "vaapi" | "opencl" | "sw"
	NumMaps       int
}

func (f fingerprint) String() string {
	parts := []string{
		"decode=" + f.DecodeCodec,
		"hwaccel=" + f.DecodeHWAccel,
		"encode=" + f.EncodeCodec,
		"audio=" + f.AudioCodec,
		"filter=" + f.FilterShape,
	}
	if f.SubBurn != "" {
		parts = append(parts, "subburn="+f.SubBurn)
	}
	if f.Tonemap != "" {
		parts = append(parts, "tonemap="+f.Tonemap)
	}
	parts = append(parts, fmt.Sprintf("maps=%d", f.NumMaps))
	return strings.Join(parts, " ")
}

func fingerprintArgv(argv []string) fingerprint {
	fp := fingerprint{}
	// decode codec (-codec:0 before -i)
	iIdx := indexOf(argv, "-i")
	dIdx := indexOfBefore(argv, "-codec:0", iIdx)
	if dIdx >= 0 && dIdx+1 < len(argv) {
		fp.DecodeCodec = argv[dIdx+1]
	}
	if h := indexOfBefore(argv, "-hwaccel:0", iIdx); h >= 0 && h+1 < len(argv) {
		fp.DecodeHWAccel = argv[h+1]
	}
	// encode codec (-codec:0 after -i)
	eIdx := indexOfAfter(argv, "-codec:0", iIdx)
	if eIdx >= 0 && eIdx+1 < len(argv) {
		fp.EncodeCodec = argv[eIdx+1]
	}
	// audio codec (-codec:1 after -i)
	aIdx := indexOfAfter(argv, "-codec:1", iIdx)
	if aIdx >= 0 && aIdx+1 < len(argv) {
		fp.AudioCodec = argv[aIdx+1]
	}
	// filter
	if fcIdx := indexOf(argv, "-filter_complex"); fcIdx >= 0 && fcIdx+1 < len(argv) {
		fp.FilterShape = canonicalizeFilter(argv[fcIdx+1])
		fp.SubBurn = subBurnFromFilter(argv[fcIdx+1])
		fp.Tonemap = tonemapFromFilter(argv[fcIdx+1])
	} else {
		fp.FilterShape = "(none)"
	}
	// num -map flags
	for _, a := range argv {
		if a == "-map" {
			fp.NumMaps++
		}
	}
	return fp
}

// canonicalizeFilter strips per-cell variation (specific W/H, label
// indices) so two graphs with the same SHAPE cluster together. The
// rewriter cares about the structure not the dimensions.
func canonicalizeFilter(g string) string {
	// Replace label digits with N.
	r := strings.NewReplacer()
	out := g
	for i := 0; i < 30; i++ {
		out = strings.ReplaceAll(out, fmt.Sprintf("[%d]", i), "[N]")
	}
	// Replace specific W/H with W:H.
	for _, prefix := range []string{"scale=w=", "scale_vaapi=w="} {
		for {
			i := strings.Index(out, prefix)
			if i < 0 {
				break
			}
			j := i + len(prefix)
			// Find the end of the W=<digits>:h=<digits> block.
			k := j
			for k < len(out) && (out[k] >= '0' && out[k] <= '9') {
				k++
			}
			if k+3 < len(out) && out[k:k+3] == ":h=" {
				m := k + 3
				for m < len(out) && (out[m] >= '0' && out[m] <= '9') {
					m++
				}
				out = out[:j] + "W:h=H" + out[m:]
			} else {
				break
			}
		}
	}
	_ = r
	return out
}

func subBurnFromFilter(g string) string {
	switch {
	case strings.Contains(g, "inlineass"):
		return "inlineass"
	case strings.Contains(g, "overlay_vaapi"):
		return "overlay_vaapi"
	case strings.Contains(g, "subtitles="):
		return "subtitles="
	}
	return ""
}

func tonemapFromFilter(g string) string {
	switch {
	case strings.Contains(g, "tonemap_opencl"):
		return "opencl"
	case strings.Contains(g, "tonemap_vaapi"):
		return "vaapi"
	case strings.Contains(g, "tonemap="):
		return "sw"
	}
	return ""
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func indexOfBefore(s []string, v string, before int) int {
	if before < 0 {
		before = len(s)
	}
	for i := 0; i < before; i++ {
		if s[i] == v {
			return i
		}
	}
	return -1
}

func indexOfAfter(s []string, v string, after int) int {
	for i := after + 1; i < len(s); i++ {
		if s[i] == v {
			return i
		}
	}
	return -1
}
