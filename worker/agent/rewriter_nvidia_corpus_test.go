package main

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Corpus replay smoke test — feeds every .argv in test/corpus/nvenc/
// through Rewrite() under WORKER_BACKEND=nvidia (activeDialect =
// nvidiaDialect{}) and asserts the basic invariants that the migrated
// rewriter layers (PR #2 of the dialect rollout) guarantee today:
//
//  1. No panics. Rewrite() must terminate for every captured argv.
//  2. When Rewrite applies, no VAAPI-shape literals leak into the
//     output argv (`scale_vaapi`, `vaapi=vaapi:`, etc. would mean a
//     missed call-site migration).
//  3. When Rewrite applies and emits a new -init_hw_device, it must
//     emit the NVIDIA dialect's form (`cuda=cuda:N`), never the
//     VAAPI literal.
//
// The test is intentionally permissive about WHICH captures bail vs
// apply — Phase 1's call-site migration is incremental, so most
// captures may still bail. The point is to catch BACKWARDS regressions
// (a NVIDIA-mode rewrite leaking a VAAPI literal would be a bug).
//
// A summary line is logged at the end giving the apply / bail
// breakdown across the corpus — useful as a diagnostic for which
// branches still need work.
func TestRewriter_NVENCCorpus_NoVAAPILeakage(t *testing.T) {
	prev := activeDialect
	activeDialect = nvidiaDialect{}
	defer func() { activeDialect = prev }()

	corpus, err := findCorpusFiles("../../test/corpus/nvenc")
	if err != nil {
		t.Fatalf("locate corpus: %v", err)
	}
	if len(corpus) == 0 {
		t.Fatalf("corpus empty — expected files under test/corpus/nvenc/")
	}

	// Tokens that, if present in an applied rewrite's output, indicate
	// a missed call-site migration (VAAPI literal leaked under NVIDIA
	// dialect). Substring match across each output arg.
	bannedSubstrings := []string{
		"vaapi=vaapi:",
		"scale_vaapi=",
		"tonemap_vaapi",
		"overlay_vaapi",
		"h264_vaapi",
		"hevc_vaapi",
	}

	var applied, bailed int
	bannedHits := map[string][]string{} // file -> banned tokens found
	tagHist := map[string]int{}         // change-tag → count across all applies

	for _, f := range corpus {
		args, err := loadCorpusArgv(f)
		if err != nil {
			t.Errorf("load %s: %v", f, err)
			continue
		}
		out := Rewrite(args, nil, nil)
		if !out.Applied {
			bailed++
			continue
		}
		applied++
		for _, c := range out.Changes {
			// Normalize bail/decode tags to their prefix for histogram
			// sanity (e.g. "decode:libdav1d->av1" → "decode:libdav1d->av1"
			// stays distinct; bail tags already grouped).
			tagHist[c]++
		}

		joined := strings.Join(out.Args, " ")
		for _, ban := range bannedSubstrings {
			if strings.Contains(joined, ban) {
				bannedHits[f] = append(bannedHits[f], ban)
			}
		}

		// -init_hw_device, when present in the applied output, must
		// be the NVIDIA form. Plex's captured form (cuda=cuda:pci:...)
		// stays untouched (rewriter leaves Plex's -init_hw_device
		// alone); a freshly INJECTED one must be cuda=cuda:N.
		for i := 0; i+1 < len(out.Args); i++ {
			if out.Args[i] != "-init_hw_device" {
				continue
			}
			v := out.Args[i+1]
			switch {
			case strings.HasPrefix(v, "cuda=cuda:"):
				// OK — either Plex's pci form or worker's index form.
			case strings.HasPrefix(v, "opencl="):
				// gpuResidentOpenCLTonemap should not fire on NVIDIA
				// (no tonemap_opencl in the chain). Treat as leak.
				bannedHits[f] = append(bannedHits[f], "opencl=...")
			default:
				bannedHits[f] = append(bannedHits[f], "-init_hw_device "+v)
			}
		}
	}

	t.Logf("NVENC corpus replay: %d/%d applied, %d bailed",
		applied, applied+bailed, bailed)

	// Histogram of change-tags emitted, sorted by frequency for triage.
	type tagCount struct {
		tag string
		n   int
	}
	hist := make([]tagCount, 0, len(tagHist))
	for k, v := range tagHist {
		hist = append(hist, tagCount{k, v})
	}
	sort.Slice(hist, func(i, j int) bool {
		if hist[i].n != hist[j].n {
			return hist[i].n > hist[j].n
		}
		return hist[i].tag < hist[j].tag
	})
	for _, tc := range hist {
		t.Logf("  %4d  %s", tc.n, tc.tag)
	}

	if len(bannedHits) > 0 {
		for f, hits := range bannedHits {
			t.Errorf("VAAPI literal leak in %s: %v", filepath.Base(f), hits)
		}
	}
}

// findCorpusFiles walks dir for *.argv files. Returns sorted paths.
func findCorpusFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".argv") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// loadCorpusArgv parses a captured PMS argv dump. Format:
//
//	ARGC: 91
//	PID: ...
//	TS_NS: ...
//	SID: ...
//	---
//	<argv[0]>
//	<argv[1]>
//	...
//
// Returns only the argv tokens (post-`---` section). Empty trailing
// lines are stripped.
func loadCorpusArgv(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var args []string
	inArgv := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // accommodate long inlineass params
	for sc.Scan() {
		line := sc.Text()
		if !inArgv {
			if strings.HasPrefix(line, "---") {
				inArgv = true
			}
			continue
		}
		args = append(args, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// Strip a trailing empty token if the file ended with a newline.
	for len(args) > 0 && args[len(args)-1] == "" {
		args = args[:len(args)-1]
	}
	return args, nil
}
