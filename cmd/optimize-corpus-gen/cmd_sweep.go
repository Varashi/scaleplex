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
	"github.com/Varashi/scaleplex/cmd/optimize-corpus-gen/matrix"
	"github.com/Varashi/scaleplex/cmd/optimize-corpus-gen/plex"
	"github.com/Varashi/scaleplex/cmd/optimize-corpus-gen/source"
)

// runSweep is the main Phase-2 matrix loop. Reads the synth library
// section, expands sources × targets × prefs into a Cell list,
// triggers one Optimize per cell (paced by Plex's
// OptimizerTranscodeCountLimit=1), captures the argv, cancels +
// cleans up, persists progress to manifest.json. Resumable.
func runSweep() {
	fs := flag.NewFlagSet("sweep", flag.ExitOnError)
	cf := addCommonFlags(fs)
	sectionTitle := fs.String("source-section", "scaleplex-test-corpus", "Library section holding the synth clips")
	corpusDir := fs.String("corpus-dir", os.Getenv("HOME")+"/scaleplex-corpus/optimize-sweep", "Local dir for captures + sidecars + manifest")
	corpusExec := fs.String("corpus-exec", "", "Wrapper command to access remote corpus (e.g. 'kubectl exec -n plex-test POD --')")
	corpusRemoteDir := fs.String("corpus-remote-dir", "/transcode/_argv-corpus", "Remote corpus dir inside the pod")
	ffprobeExec := fs.String("ffprobe-exec", "", "Wrapper command for ffprobe")
	var pathTranslate pathTranslateFlag
	fs.Var(&pathTranslate, "path-translate", "Path rewrite rule FROM=TO for ffprobe")
	captureTimeout := fs.Duration("capture-timeout", 45*time.Second, "Per-cell capture timeout")
	intercell := fs.Duration("intercell-delay", 1*time.Second, "Delay between cells (lets PMS settle)")
	maxCells := fs.Int("max-cells", 0, "Run at most N cells (0 = all). Useful for short test runs.")
	onlyTargetTagIDs := fs.String("only-target-tags", "", "Comma-separated targetTagIDs to include (empty = all). E.g. '1,2'.")
	resume := fs.Bool("resume", true, "Skip cells already recorded as captured/skipped in manifest.json")
	fs.Parse(os.Args[1:])

	cf.resolve()
	if *corpusExec == "" {
		log.Fatal("--corpus-exec is required for the homelab (NFS isn't local). E.g. 'kubectl exec -n plex-test plex-test-worker-XXX --'")
	}

	if err := os.MkdirAll(*corpusDir, 0o755); err != nil {
		fatalf("mkdir corpus-dir: %v", err)
	}

	c := plex.New(cf.plexURL, cf.token)

	id, err := c.Identity()
	if err != nil {
		fatalf("PMS ping: %v", err)
	}
	log.Printf("PMS %s (v%s) reachable", id.MachineIdentifier, id.Version)

	// 1. Resolve section + items.
	sec, err := c.FindSectionByTitle(*sectionTitle)
	if err != nil {
		fatalf("section %q: %v (did you run `library` subcommand first?)", *sectionTitle, err)
	}
	log.Printf("section: key=%s uuid=%s items expected", sec.Key, sec.UUID)
	items, err := c.SectionItems(sec.Key)
	if err != nil {
		fatalf("list items: %v", err)
	}
	if len(items) == 0 {
		fatalf("section %q is empty — did you run `synth` and `library` first?", sec.Title)
	}
	log.Printf("section holds %d items", len(items))

	// 2. Probe each item (need source file path).
	prober, err := source.NewProber(*ffprobeExec, pathTranslate)
	if err != nil {
		fatalf("prober: %v", err)
	}
	probes := map[string]*source.Profile{} // ratingKey → probe
	var sources []matrix.SourceRef
	for _, it := range items {
		if len(it.Media) == 0 || len(it.Media[0].Part) == 0 {
			full, err := c.Metadata(it.RatingKey)
			if err != nil {
				log.Printf("WARN: metadata %s: %v — skipping", it.RatingKey, err)
				continue
			}
			it = *full
		}
		if len(it.Media) == 0 || len(it.Media[0].Part) == 0 {
			log.Printf("WARN: %q has no Media/Part — skipping", it.Title)
			continue
		}
		srcPath := it.Media[0].Part[0].File
		probe, err := prober.Probe(srcPath)
		if err != nil {
			log.Printf("WARN: probe %s: %v — skipping", srcPath, err)
			continue
		}
		probes[it.RatingKey] = probe
		sources = append(sources, matrix.SourceRef{
			RatingKey: it.RatingKey,
			Title:     it.Title,
			Path:      srcPath,
		})
	}
	log.Printf("probed %d sources", len(sources))

	// 3. Resolve targets.
	targets, err := c.OptimizeTargets()
	if err != nil {
		fatalf("targets: %v", err)
	}
	allowedTags := parseTagList(*onlyTargetTagIDs)
	var targetRefs []matrix.TargetRef
	for _, t := range targets {
		if len(allowedTags) > 0 && !allowedTags[t.TagID] {
			continue
		}
		targetRefs = append(targetRefs, matrix.TargetRef{TagID: t.TagID, Title: t.Title})
	}
	if len(targetRefs) == 0 {
		fatalf("no Optimize targets after filter (have: %v, allowed: %v)", targets, allowedTags)
	}
	log.Printf("targets: %d", len(targetRefs))

	// 4. Pref matrix.
	prefs := matrix.DefaultPrefMatrix()
	log.Printf("pref combinations: %d", len(prefs))

	// 5. Expand cells.
	cells := matrix.Expand(sources, targetRefs, prefs)
	log.Printf("total cells: %d (sources=%d × targets=%d × prefs=%d)",
		len(cells), len(sources), len(targetRefs), len(prefs))

	// 6. Manifest (resume).
	mf, err := matrix.LoadOrInit(*corpusDir)
	if err != nil {
		fatalf("manifest: %v", err)
	}
	if *resume {
		s := mf.Summary(len(cells))
		log.Printf("manifest: captured=%d timeout=%d error=%d skipped=%d remaining=%d",
			s.Captured, s.Timeout, s.Error, s.Skipped, s.Remaining)
	}

	// 7. Pref snapshot for restore-on-exit.
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
		log.Printf("signal — restoring prefs and exiting")
		_ = c.Restore(snap)
		os.Exit(130)
	}()

	// 8. Background-processing key (for trigger + cancel).
	bgKey, err := c.BackgroundProcessingKey()
	if err != nil {
		fatalf("bg key: %v", err)
	}

	// 9. Watcher.
	w, err := capture.NewRemoteWatcher(*corpusExec, *corpusRemoteDir, *corpusDir)
	if err != nil {
		fatalf("watcher: %v", err)
	}

	// 10. Cell loop.
	executed := 0
	for i, cell := range cells {
		if *maxCells > 0 && executed >= *maxCells {
			log.Printf("hit --max-cells %d, stopping", *maxCells)
			break
		}
		if *resume && mf.Done(cell.ID) {
			continue
		}
		log.Printf("[%d/%d] cell %s: %s → %s (HW=%s HEVCopt=%s)",
			i+1, len(cells), cell.ID, cell.SourceTitle, cell.TargetTitle,
			cell.Prefs["HardwareAcceleratedCodecs"], cell.Prefs["TranscoderHEVCOptimize"])

		started := time.Now().UTC()
		res := runCell(c, prober, w, bgKey, sec.UUID, cell, probes[cell.SourceRatingKey], *captureTimeout)
		res.StartedAt = started
		res.CompletedAt = time.Now().UTC()
		if err := mf.Record(cell.ID, res); err != nil {
			log.Printf("WARN: manifest record: %v", err)
		}
		log.Printf("  → %s (%d captures, dur=%s)", res.Status, len(res.Captures), res.CompletedAt.Sub(started).Truncate(time.Millisecond))
		executed++

		// Brief inter-cell pacing — lets PMS finish dispatching/clearing
		// the prior cell's state before we re-trigger. Without this PMS
		// occasionally rejects with "media version already exists" even
		// after our cancel sweep.
		if *intercell > 0 {
			time.Sleep(*intercell)
		}
	}

	s := mf.Summary(len(cells))
	log.Printf("sweep done: captured=%d timeout=%d error=%d skipped=%d (out of %d total)",
		s.Captured, s.Timeout, s.Error, s.Skipped, s.Total)
}

// runCell triggers one Optimize, waits for capture, tags + cancels,
// returns CellResult. The cleanup (cancel queue jobs + delete optimize
// child Media) is `defer`ed so EVERY exit path — captured, skipped,
// timeout, error — clears the (source, target) version-existence state
// before the next cell's trigger. PMS keys "already exists" (code 1006)
// on the queue entry AND the on-disk child, so both must be swept.
func runCell(c *plex.Client, prober *source.Prober, w *capture.Watcher,
	bgKey, sectionUUID string, cell matrix.Cell, probe *source.Profile, timeout time.Duration) matrix.CellResult {

	jobTitle := fmt.Sprintf("scaleplex-corpus-gen-%s", cell.ID)

	// Defer the full cleanup chain. Runs even on skipped/error paths
	// so a flaky 1006 doesn't leave the queue tainted for the next cell.
	defer func() {
		cancelCorpusJobByTitle(c, bgKey, jobTitle, 3) // retry while PMS catches up
		cancelAllCorpusJobs(c, bgKey)
		if n, err := c.DeleteOptimizeChildren(cell.SourceRatingKey); err != nil {
			log.Printf("  · WARN: post-cleanup delete on %s: %v", cell.SourceRatingKey, err)
		} else if n > 0 {
			log.Printf("  · post-cleanup deleted %d optimize child(ren) on source %s", n, cell.SourceRatingKey)
		}
	}()

	if err := c.SetPrefs(cell.Prefs); err != nil {
		return matrix.CellResult{Status: "error", Error: fmt.Sprintf("set prefs: %v", err)}
	}
	if err := w.Snapshot(); err != nil {
		return matrix.CellResult{Status: "error", Error: fmt.Sprintf("snapshot: %v", err)}
	}
	if err := c.TriggerOptimize(bgKey, cell.SourceRatingKey, sectionUUID, jobTitle, cell.TargetTagID); err != nil {
		// "media version already exists" → mark skipped. Defer'd
		// cleanup will still run so the NEXT cell can re-trigger.
		if strings.Contains(err.Error(), "code\":1006") || strings.Contains(err.Error(), "already exists") {
			return matrix.CellResult{Status: "skipped", Error: err.Error()}
		}
		return matrix.CellResult{Status: "error", Error: fmt.Sprintf("trigger: %v", err)}
	}
	fresh, err := w.WaitForNew(timeout)
	if err != nil {
		return matrix.CellResult{Status: "error", Error: fmt.Sprintf("wait: %v", err)}
	}
	if len(fresh) == 0 {
		return matrix.CellResult{Status: "timeout"}
	}
	for _, f := range fresh {
		tag := &capture.CellTag{
			CellID: cell.ID,
			Source: capture.SourceRef{RatingKey: cell.SourceRatingKey, Title: cell.SourceTitle, Probe: probe},
			Target: capture.TargetRef{TagID: cell.TargetTagID, Title: cell.TargetTitle},
			Prefs:  cell.Prefs,
		}
		if err := tag.Write(f); err != nil {
			log.Printf("WARN: tag %s: %v", f, err)
		}
	}
	return matrix.CellResult{Status: "captured", Captures: fresh}
}

// cancelCorpusJobByTitle polls the optimize queue up to `tries` times
// (500 ms apart) looking for a job by exact title and cancels it.
// Bridges the timing race where a just-triggered job hasn't appeared
// in OptimizedItems yet when our cleanup runs.
func cancelCorpusJobByTitle(c *plex.Client, bgKey, title string, tries int) {
	for i := 0; i < tries; i++ {
		jobs, err := c.OptimizedItems(bgKey)
		if err != nil {
			return
		}
		for _, j := range jobs {
			if j.Title != title {
				continue
			}
			if err := c.CancelOptimize(bgKey, j.ID); err != nil {
				log.Printf("  · WARN: cancel %d %q: %v", j.ID, title, err)
				return
			}
			log.Printf("  · cancelled optimize id=%d %q", j.ID, title)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func parseTagList(csv string) map[int]bool {
	out := map[int]bool{}
	if csv == "" {
		return out
	}
	for _, tok := range strings.Split(csv, ",") {
		t := strings.TrimSpace(tok)
		if t == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			out[n] = true
		}
	}
	return out
}
