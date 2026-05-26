// optimize-corpus-gen drives a Plex Media Server through Optimize jobs
// across a matrix of {source × target × prefs}, capturing the ffmpeg
// argv each cell produces via the worker's existing WORKER_DUMP_ARGV
// instrumentation. Output is a representative corpus of Plex Optimize
// argvs that the rewriter's parity harness can consume.
//
// Phase 1 (this file): one-cell smoke test. Loads PMS creds from
// ~/.config/plex.env, pings the server, ffprobes one source,
// enumerates Optimize targets, snapshots prefs, sets one pref combo,
// triggers one Optimize, watches for the capture, cancels the job,
// tags the capture, restores prefs. Validates the end-to-end loop
// before Phase 2 generalizes it to a matrix.
//
// Usage (Phase 1):
//
//	source ~/.config/plex.env
//	go run ./cmd/optimize-corpus-gen \
//	    -plex $PLEX_TEST_URL \
//	    -token $PLEX_TOKEN \
//	    -source-section "Movies Kids NL" \
//	    -corpus-dir ~/scaleplex-corpus \
//	    -dry-run
//
// -dry-run skips actually triggering Optimize — useful for validating
// API connectivity, pref snapshot, and source enumeration without
// disturbing PMS state.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Varashi/scaleplex/cmd/optimize-corpus-gen/capture"
	"github.com/Varashi/scaleplex/cmd/optimize-corpus-gen/plex"
	"github.com/Varashi/scaleplex/cmd/optimize-corpus-gen/source"
)

func main() {
	var (
		plexURL        = flag.String("plex", "", "PMS base URL (e.g. http://172.16.4.106:32400)")
		token          = flag.String("token", "", "X-Plex-Token; default $PLEX_TOKEN env")
		sectionTitle   = flag.String("source-section", "", "Library section to draw sources from (exact title match)")
		sourceRating   = flag.String("source-rating-key", "", "Specific item ratingKey to use (overrides 'pick first'); useful for retry after an Optimize-already-exists collision")
		corpusDir      = flag.String("corpus-dir", os.Getenv("HOME")+"/scaleplex-corpus", "Local corpus dir to watch (mode=local) OR sink dir captures get copied to (mode=remote)")
		corpusExec     = flag.String("corpus-exec", "", "Wrapper command to access a remote corpus (e.g. 'kubectl exec -n plex-test POD --'); empty = read corpus-dir locally")
		corpusRemoteDir = flag.String("corpus-remote-dir", "/transcode/_argv-corpus", "Remote corpus dir inside the pod (used only with -corpus-exec)")
		targetTagFlag  = flag.Int("target-tag", 0, "Optimize target tagID (0 = pick first listed)")
		dryRun         = flag.Bool("dry-run", false, "Skip Optimize trigger; just probe + list + snapshot")
		captureTimeout = flag.Duration("capture-timeout", 60*time.Second, "How long to wait for worker capture after trigger")
		ffprobeExec    = flag.String("ffprobe-exec", "", "Wrapper command for ffprobe (e.g. 'kubectl exec -n media-toolkit POD -- ffprobe'); empty = local ffprobe")
		pathTranslate  pathTranslateFlag
	)
	flag.Var(&pathTranslate, "path-translate", "Path rewrite rule FROM=TO applied before ffprobe (repeat for multiple). E.g. /media=/mnt/media/media when PMS sees /media but ffprobe sees /mnt/media/media.")
	flag.Parse()

	if *plexURL == "" {
		log.Fatal("--plex is required (e.g. $PLEX_TEST_URL from ~/.config/plex.env)")
	}
	if *token == "" {
		*token = os.Getenv("PLEX_TOKEN")
	}
	if *token == "" {
		log.Fatal("--token or $PLEX_TOKEN is required")
	}
	if *sectionTitle == "" {
		log.Fatal("--source-section is required (e.g. \"Movies Kids NL\")")
	}

	c := plex.New(*plexURL, *token)

	// 1. Ping — fail fast on bad token / unreachable PMS.
	id, err := c.Identity()
	if err != nil {
		log.Fatalf("PMS ping failed: %v", err)
	}
	log.Printf("PMS %s (v%s, API %s) reachable", id.MachineIdentifier, id.Version, id.APIVersion)

	// 2. Source section + first item.
	sec, err := c.FindSectionByTitle(*sectionTitle)
	if err != nil {
		log.Fatalf("section lookup: %v", err)
	}
	log.Printf("section: %s (key=%s, type=%s)", sec.Title, sec.Key, sec.Type)

	items, err := c.SectionItems(sec.Key)
	if err != nil {
		log.Fatalf("list items: %v", err)
	}
	if len(items) == 0 {
		log.Fatalf("section %s is empty — point at one with content", sec.Title)
	}
	var srcItem plex.Item
	if *sourceRating != "" {
		found := false
		for _, it := range items {
			if it.RatingKey == *sourceRating {
				srcItem = it
				found = true
				break
			}
		}
		if !found {
			log.Fatalf("--source-rating-key %s not in section %s", *sourceRating, sec.Title)
		}
		log.Printf("section has %d items; using ratingKey=%s: %q", len(items), srcItem.RatingKey, srcItem.Title)
	} else {
		srcItem = items[0]
		log.Printf("section has %d items; using first: %q (ratingKey=%s)", len(items), srcItem.Title, srcItem.RatingKey)
	}

	// For a show section, descend to episodes (Plex Optimize on a series
	// ratingKey would queue the whole series — not what we want).
	if sec.Type == "show" {
		eps, err := c.SeriesEpisodes(srcItem.RatingKey)
		if err != nil {
			log.Fatalf("list episodes: %v", err)
		}
		if len(eps) == 0 {
			log.Fatalf("series %s has no episodes", srcItem.Title)
		}
		srcItem = eps[0]
		log.Printf("descended to first episode: %q (ratingKey=%s)", srcItem.Title, srcItem.RatingKey)
	}

	// 3. ffprobe the actual file (NOT Plex's library metadata — Tdarr stales).
	if len(srcItem.Media) == 0 || len(srcItem.Media[0].Part) == 0 {
		// Need to fetch full metadata when SectionItems didn't include Media.
		full, err := c.Metadata(srcItem.RatingKey)
		if err != nil {
			log.Fatalf("metadata %s: %v", srcItem.RatingKey, err)
		}
		srcItem = *full
	}
	if len(srcItem.Media) == 0 || len(srcItem.Media[0].Part) == 0 {
		log.Fatalf("source %q has no Media/Part — can't ffprobe", srcItem.Title)
	}
	srcPath := srcItem.Media[0].Part[0].File
	log.Printf("source file: %s", srcPath)
	prober, err := source.NewProber(*ffprobeExec, pathTranslate)
	if err != nil {
		log.Fatalf("prober: %v", err)
	}
	probe, err := prober.Probe(srcPath)
	if err != nil {
		log.Fatalf("ffprobe: %v", err)
	}
	log.Printf("probe: codec=%s profile=%s %dx%d %s pixfmt=%s HDR=%s audio=%d subs=%d",
		probe.VideoCodec, probe.VideoProfile, probe.Width, probe.Height,
		probe.ColorTransfer, probe.PixFmt, probe.HDRFormat, len(probe.Audio), len(probe.Subs))

	// 4. Optimize targets.
	targets, err := c.OptimizeTargets()
	if err != nil {
		log.Fatalf("list optimize targets: %v", err)
	}
	if len(targets) == 0 {
		log.Fatalf("no Optimize targets on server (Settings → Library → Optimize)")
	}
	log.Printf("Optimize targets:")
	for _, t := range targets {
		log.Printf("  - tagID=%d  %s", t.TagID, t.Title)
	}
	target := targets[0]
	if *targetTagFlag != 0 {
		found := false
		for _, t := range targets {
			if t.TagID == *targetTagFlag {
				target = t
				found = true
				break
			}
		}
		if !found {
			log.Fatalf("--target-tag %d not in target list", *targetTagFlag)
		}
	}
	log.Printf("using target: tagID=%d  %s", target.TagID, target.Title)

	// 5. Snapshot prefs so we can restore on exit (incl. SIGINT/SIGTERM).
	snap, err := c.SaveSnapshot()
	if err != nil {
		log.Fatalf("snapshot prefs: %v", err)
	}
	log.Printf("pref snapshot:")
	for k, v := range snap.Values {
		log.Printf("  - %s = %s", k, v)
	}
	defer func() {
		log.Printf("restoring %d prefs", len(snap.Values))
		if err := c.Restore(snap); err != nil {
			log.Printf("WARN: pref restore failed: %v", err)
		}
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("signal received — restoring prefs and exiting")
		_ = c.Restore(snap)
		os.Exit(130)
	}()

	if *dryRun {
		log.Printf("--dry-run: skipping trigger. End-to-end plumbing OK.")
		return
	}

	// 6. Phase 1: pick one specific pref combo for the smoke cell.
	// HW everything ON, HW tonemap ON, HEVC encoding "always", HEVC optimize ON
	// — the most common production prefset. Phase 2 expands to the matrix.
	cellPrefs := map[string]string{
		"HardwareAcceleratedCodecs":   "true",
		"HardwareAcceleratedEncoders": "true",
		"TranscoderToneMapping":       "true",
		"TranscoderHEVCEncodingMode":  "always",
		"TranscoderHEVCOptimize":      "true",
	}
	log.Printf("setting cell prefs: %v", cellPrefs)
	if err := c.SetPrefs(cellPrefs); err != nil {
		log.Fatalf("set prefs: %v", err)
	}

	// 7. Watch corpus dir + trigger Optimize.
	var w *capture.Watcher
	if *corpusExec != "" {
		w, err = capture.NewRemoteWatcher(*corpusExec, *corpusRemoteDir, *corpusDir)
	} else {
		w, err = capture.NewLocalWatcher(*corpusDir)
	}
	if err != nil {
		log.Fatalf("watcher: %v", err)
	}
	if err := w.Snapshot(); err != nil {
		log.Fatalf("snapshot: %v", err)
	}
	bgKey, err := c.BackgroundProcessingKey()
	if err != nil {
		log.Fatalf("background-processing key: %v", err)
	}
	log.Printf("background-processing playlist: %s", bgKey)
	if sec.UUID == "" {
		log.Fatalf("section %q has no UUID — re-list with fresh client", sec.Title)
	}
	jobTitle := fmt.Sprintf("scaleplex-corpus-gen-%d", time.Now().UnixNano())
	log.Printf("triggering Optimize: %s → target=%d (%s)", srcItem.Title, target.TagID, target.Title)
	if err := c.TriggerOptimize(bgKey, srcItem.RatingKey, sec.UUID, jobTitle, target.TagID); err != nil {
		log.Fatalf("trigger: %v", err)
	}

	// 8. Wait for capture.
	log.Printf("waiting up to %s for worker capture", *captureTimeout)
	fresh, err := w.WaitForNew(*captureTimeout)
	if err != nil {
		log.Fatalf("wait: %v", err)
	}
	if len(fresh) == 0 {
		log.Printf("TIMEOUT — no capture appeared. Possible: worker WORKER_DUMP_ARGV not set, PMS hasn't dispatched yet, or corpus dir mismatch.")
	} else {
		log.Printf("captured %d new files:", len(fresh))
		for _, f := range fresh {
			log.Printf("  - %s", f)
		}
	}

	// 9. Cancel the Optimize job. Sweep all jobs whose title carries the
	// generator's "scaleplex-corpus-gen-" prefix — covers both this
	// cell's job AND any leftovers from prior runs (interrupted SIGINT,
	// PMS lag dropping our exact-title lookup, etc.) so the queue stays
	// clean between runs. Cancel does NOT abort the transcode session
	// directly; sweep StopTranscodeSession on the matching static
	// session too (best-effort — session may already be torn down).
	jobs, err := c.OptimizedItems(bgKey)
	if err != nil {
		log.Printf("WARN: list optimize jobs failed: %v", err)
	}
	cancelled := 0
	for _, j := range jobs {
		if !strings.HasPrefix(j.Title, "scaleplex-corpus-gen-") {
			continue
		}
		if err := c.CancelOptimize(bgKey, j.ID); err != nil {
			log.Printf("WARN: cancel %d %q failed: %v", j.ID, j.Title, err)
			continue
		}
		log.Printf("cancelled optimize id=%d %q", j.ID, j.Title)
		cancelled++
	}
	if cancelled == 0 {
		log.Printf("note: no optimize jobs to cancel (cell finished before listing? PMS lag?)")
	}
	sessions, err := c.TranscodeSessions()
	if err != nil {
		log.Printf("WARN: list sessions: %v", err)
	}
	for _, s := range sessions {
		if s.Context != "static" {
			continue
		}
		if err := c.StopTranscodeSession(s.Key); err != nil {
			log.Printf("WARN: stop session %s: %v", s.Key, err)
			continue
		}
		log.Printf("stopped transcode session %s", s.Key)
	}

	// 10. Tag captures with cell metadata.
	for _, f := range fresh {
		tag := &capture.CellTag{
			CellID: fmt.Sprintf("%s-%s-%d", time.Now().UTC().Format(time.RFC3339Nano), srcItem.RatingKey, target.TagID),
			Source: capture.SourceRef{
				RatingKey: srcItem.RatingKey,
				Title:     srcItem.Title,
				Probe:     probe,
			},
			Target: capture.TargetRef{TagID: target.TagID, Title: target.Title},
			Prefs:  cellPrefs,
		}
		if err := tag.Write(f); err != nil {
			log.Printf("WARN: tag write %s: %v", f, err)
		} else {
			log.Printf("tagged %s", f)
		}
	}

	log.Printf("phase-1 smoke cell complete")
}

// pathTranslateFlag is a repeatable -path-translate flag (flag.Var
// pattern). Each invocation appends one FROM=TO rule to the slice;
// ffprobe applies them first-match-wins.
type pathTranslateFlag []string

func (p *pathTranslateFlag) String() string {
	return fmt.Sprintf("%v", []string(*p))
}

func (p *pathTranslateFlag) Set(v string) error {
	*p = append(*p, v)
	return nil
}
