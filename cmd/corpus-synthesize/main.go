// corpus-synthesize emits synthetic replay-corpus cells for argv-shape
// combinations the organic capture corpus is missing — specifically the
// `hw-subburn-transcode` class (inlineass= sub-burn + *_vaapi encoder)
// that PR #144 had to fix three latent bugs to support, yet had zero
// corpus coverage because Frank's household rarely force-burns (#150).
//
// Output is replayCapture-shaped JSON the build-tagged replay harness
// (worker/agent/replay_test.go, +tags=replay) consumes. Drop the cells
// into worker/agent/testdata/replay-corpus/ and PR CI's TestReplayCorpus
// runs them through Rewrite() — a regression that makes the rewriter
// bail on this class (the #144 class) flips the cell PASS -> FAIL.
//
// Unlike optimize-corpus-gen (which drives a live PMS to *capture* real
// argv), this generator *transforms* a checked-in sanitized capture —
// no cluster, no Plex, deterministic output. See README.md.
//
// Usage:
//
//	corpus-synthesize -out-dir worker/agent/testdata/replay-corpus
//	corpus-synthesize -list                # print the matrix, write nothing
package main

import (
	"encoding/json"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

//go:embed templates/base__av1__hevc_vaapi__ordinal__dash-extsrt.json
var baseTemplateJSON []byte

func main() {
	outDir := flag.String("out-dir", "", "directory to write synthesized-*.json cells into (required unless -list)")
	list := flag.Bool("list", false, "print the axis matrix and exit without writing")
	flag.Parse()

	var base Cell
	if err := json.Unmarshal(baseTemplateJSON, &base); err != nil {
		log.Fatalf("decode embedded base template: %v", err)
	}

	shapes := DefaultMatrix()

	if *list {
		fmt.Printf("%d synthetic cells:\n", len(shapes))
		for _, s := range shapes {
			fmt.Printf("  synth__%s.json\n", s.sig())
		}
		return
	}
	if *outDir == "" {
		log.Fatal("-out-dir is required (or pass -list)")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *outDir, err)
	}

	for _, s := range shapes {
		cell, err := build(base, s)
		if err != nil {
			log.Fatalf("build %s: %v", s.sig(), err)
		}
		body, err := json.MarshalIndent(cell, "", "  ")
		if err != nil {
			log.Fatalf("marshal %s: %v", cell.SessionID, err)
		}
		path := filepath.Join(*outDir, cell.SessionID+".json")
		if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
			log.Fatalf("write %s: %v", path, err)
		}
		log.Printf("wrote %s (argv %d tokens)", filepath.Base(path), len(cell.Argv))
	}
	log.Printf("done; %d cells in %s", len(shapes), *outDir)
}
