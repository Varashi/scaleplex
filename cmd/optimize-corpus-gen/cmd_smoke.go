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

// runSmoke is the Phase-1 one-cell end-to-end loop. Validates Plex
// REST, ffprobe, pref snapshot+restore, worker capture watcher,
// capture tagging, sweep-cancel — all the moving parts the bigger
// `sweep` subcommand composes.
func runSmoke() {
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	cf := addCommonFlags(fs)
	sectionTitle := fs.String("source-section", "", "Library section to draw sources from (exact title match)")
	sourceRating := fs.String("source-rating-key", "", "Specific item ratingKey (overrides 'first item')")
	corpusDir := fs.String("corpus-dir", os.Getenv("HOME")+"/scaleplex-corpus", "Local corpus dir (mode=local) OR sink dir (mode=remote)")
	corpusExec := fs.String("corpus-exec", "", "Wrapper command to access remote corpus (e.g. 'kubectl exec -n plex-test POD --')")
	corpusRemoteDir := fs.String("corpus-remote-dir", "/transcode/_argv-corpus", "Remote corpus dir inside the pod")
	targetTagFlag := fs.Int("target-tag", 0, "Optimize target tagID (0 = first listed)")
	dryRun := fs.Bool("dry-run", false, "Skip Optimize trigger; just probe + list + snapshot")
	captureTimeout := fs.Duration("capture-timeout", 60*time.Second, "How long to wait for worker capture after trigger")
	ffprobeExec := fs.String("ffprobe-exec", "", "Wrapper command for ffprobe (e.g. 'kubectl exec -n media-toolkit POD -- ffprobe')")
	var pathTranslate pathTranslateFlag
	fs.Var(&pathTranslate, "path-translate", "Path rewrite rule FROM=TO applied before ffprobe (repeat for multiple)")
	fs.Parse(os.Args[1:])

	cf.resolve()
	if *sectionTitle == "" {
		log.Fatal("--source-section is required")
	}
	c := plex.New(cf.plexURL, cf.token)

	id, err := c.Identity()
	if err != nil {
		fatalf("PMS ping: %v", err)
	}
	log.Printf("PMS %s (v%s) reachable", id.MachineIdentifier, id.Version)

	sec, err := c.FindSectionByTitle(*sectionTitle)
	if err != nil {
		fatalf("section: %v", err)
	}
	log.Printf("section: %s (key=%s, type=%s)", sec.Title, sec.Key, sec.Type)

	items, err := c.SectionItems(sec.Key)
	if err != nil {
		fatalf("list items: %v", err)
	}
	if len(items) == 0 {
		fatalf("section %s is empty", sec.Title)
	}
	var srcItem plex.Item
	if *sourceRating != "" {
		for _, it := range items {
			if it.RatingKey == *sourceRating {
				srcItem = it
				break
			}
		}
		if srcItem.RatingKey == "" {
			fatalf("--source-rating-key %s not in %s", *sourceRating, sec.Title)
		}
	} else {
		srcItem = items[0]
	}
	if sec.Type == "show" {
		eps, err := c.SeriesEpisodes(srcItem.RatingKey)
		if err != nil {
			fatalf("episodes: %v", err)
		}
		if len(eps) == 0 {
			fatalf("series %s has no episodes", srcItem.Title)
		}
		srcItem = eps[0]
	}
	log.Printf("using: %q (ratingKey=%s)", srcItem.Title, srcItem.RatingKey)

	if len(srcItem.Media) == 0 || len(srcItem.Media[0].Part) == 0 {
		full, err := c.Metadata(srcItem.RatingKey)
		if err != nil {
			fatalf("metadata: %v", err)
		}
		srcItem = *full
	}
	if len(srcItem.Media) == 0 || len(srcItem.Media[0].Part) == 0 {
		fatalf("source %q has no Media/Part", srcItem.Title)
	}
	srcPath := srcItem.Media[0].Part[0].File
	log.Printf("source file: %s", srcPath)

	prober, err := source.NewProber(*ffprobeExec, pathTranslate)
	if err != nil {
		fatalf("prober: %v", err)
	}
	probe, err := prober.Probe(srcPath)
	if err != nil {
		fatalf("ffprobe: %v", err)
	}
	log.Printf("probe: codec=%s profile=%s %dx%d xfer=%s pixfmt=%s HDR=%s audio=%d subs=%d",
		probe.VideoCodec, probe.VideoProfile, probe.Width, probe.Height,
		probe.ColorTransfer, probe.PixFmt, probe.HDRFormat, len(probe.Audio), len(probe.Subs))

	targets, err := c.OptimizeTargets()
	if err != nil {
		fatalf("targets: %v", err)
	}
	if len(targets) == 0 {
		fatalf("no Optimize targets on server")
	}
	target := targets[0]
	if *targetTagFlag != 0 {
		found := false
		for _, t := range targets {
			if t.TagID == *targetTagFlag {
				target = t
				found = true
			}
		}
		if !found {
			fatalf("--target-tag %d not in target list", *targetTagFlag)
		}
	}
	log.Printf("target: tagID=%d %s", target.TagID, target.Title)

	snap, err := c.SaveSnapshot()
	if err != nil {
		fatalf("snapshot: %v", err)
	}
	defer func() {
		log.Printf("restoring %d prefs", len(snap.Values))
		_ = c.Restore(snap)
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
		log.Printf("--dry-run: plumbing OK.")
		return
	}

	cellPrefs := map[string]string{
		"HardwareAcceleratedCodecs":   "true",
		"HardwareAcceleratedEncoders": "true",
		"TranscoderToneMapping":       "true",
		"TranscoderHEVCEncodingMode":  "always",
		"TranscoderHEVCOptimize":      "true",
	}
	if err := c.SetPrefs(cellPrefs); err != nil {
		fatalf("set prefs: %v", err)
	}

	var w *capture.Watcher
	if *corpusExec != "" {
		w, err = capture.NewRemoteWatcher(*corpusExec, *corpusRemoteDir, *corpusDir)
	} else {
		w, err = capture.NewLocalWatcher(*corpusDir)
	}
	if err != nil {
		fatalf("watcher: %v", err)
	}
	if err := w.Snapshot(); err != nil {
		fatalf("snapshot: %v", err)
	}

	bgKey, err := c.BackgroundProcessingKey()
	if err != nil {
		fatalf("bg key: %v", err)
	}
	if sec.UUID == "" {
		fatalf("section %q has no UUID", sec.Title)
	}
	jobTitle := fmt.Sprintf("scaleplex-corpus-gen-%d", time.Now().UnixNano())
	log.Printf("triggering Optimize: %s → %s", srcItem.Title, target.Title)
	if err := c.TriggerOptimize(bgKey, srcItem.RatingKey, sec.UUID, jobTitle, target.TagID); err != nil {
		fatalf("trigger: %v", err)
	}

	log.Printf("waiting up to %s for capture", *captureTimeout)
	fresh, err := w.WaitForNew(*captureTimeout)
	if err != nil {
		fatalf("wait: %v", err)
	}
	if len(fresh) == 0 {
		log.Printf("TIMEOUT — no capture appeared")
	} else {
		log.Printf("captured %d new files:", len(fresh))
		for _, f := range fresh {
			log.Printf("  - %s", f)
		}
	}

	cancelAllCorpusJobs(c, bgKey)

	for _, f := range fresh {
		tag := &capture.CellTag{
			CellID: fmt.Sprintf("%s-%s-%d", time.Now().UTC().Format(time.RFC3339Nano), srcItem.RatingKey, target.TagID),
			Source: capture.SourceRef{RatingKey: srcItem.RatingKey, Title: srcItem.Title, Probe: probe},
			Target: capture.TargetRef{TagID: target.TagID, Title: target.Title},
			Prefs:  cellPrefs,
		}
		if err := tag.Write(f); err != nil {
			log.Printf("WARN: tag %s: %v", f, err)
		}
	}
	log.Printf("smoke complete")
}

// cancelAllCorpusJobs sweeps all Optimize jobs whose title carries the
// scaleplex-corpus-gen prefix + stops any stuck static transcode
// sessions. Shared between smoke and sweep subcommands.
func cancelAllCorpusJobs(c *plex.Client, bgKey string) {
	jobs, err := c.OptimizedItems(bgKey)
	if err != nil {
		log.Printf("WARN: list optimize jobs: %v", err)
	}
	for _, j := range jobs {
		if !strings.HasPrefix(j.Title, "scaleplex-corpus-gen-") {
			continue
		}
		if err := c.CancelOptimize(bgKey, j.ID); err != nil {
			log.Printf("WARN: cancel %d %q: %v", j.ID, j.Title, err)
			continue
		}
		log.Printf("cancelled optimize id=%d %q", j.ID, j.Title)
	}
	sessions, err := c.TranscodeSessions()
	if err != nil {
		log.Printf("WARN: sessions: %v", err)
		return
	}
	for _, s := range sessions {
		if s.Context != "static" {
			continue
		}
		if err := c.StopTranscodeSession(s.Key); err != nil {
			log.Printf("WARN: stop session %s: %v", s.Key, err)
			continue
		}
		log.Printf("stopped static session %s", s.Key)
	}
}
