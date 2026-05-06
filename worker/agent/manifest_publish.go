package main

// manifest_publish — POST the DASH manifest body to PMS the way Plex's
// ffmpeg fork does (driven by `-manifest_name <http://...>`).
//
// Why: PMS gates `/video/:/transcode/universal/session/<sid>/<id>/header`
// on a "first manifest received" signal. PT.real fires
// `POST .../manifest?X-Plex-Http-Pipeline=infinite` ~3-4s after spawn and
// PMS unblocks /header within ~30ms. Without that POST, PMS waits the
// full SegmentedTranscoderTimeout (~125s) before falling back to a disk
// probe of init-stream0.m4s. Stock ffmpeg's dashenc treats
// `-manifest_name` as a filename, not a URL, so we have to do the POST
// from the worker.
//
// Wiring: rewriter.go captures the rewritten manifest URL on
// RewriteResult.ManifestURL; main.go starts runManifestPublisher in a
// goroutine. The publisher watches the session cwd via fsnotify and
// POSTs the manifest body whenever ffmpeg's output `dash` file is
// written. Each POST is debounced (200ms) so atomic rewrites don't
// trigger duplicate sends.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var metricManifestPOST = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "scaleplex_worker_manifest_post_total",
	Help: "POSTs of the DASH manifest body to PMS, by HTTP class.",
}, []string{"result"}) // 2xx|3xx|4xx|5xx|err|empty

// manifestFilename is the basename ffmpeg writes the .mpd to. PMS argv
// uses `dash` as the muxer output URL (last positional), so ffmpeg writes
// the manifest to <cwd>/dash. If a future PMS build switches the output
// URL, this needs to follow.
const manifestFilename = "dash"

// runManifestPublisher watches `dir` for writes to `dir/dash` and POSTs
// the file's contents to `manifestURL`. Returns when ctx is cancelled or
// fsnotify dies. Best-effort: any single POST failure is logged but the
// publisher keeps running.
//
// startSeq mirrors the renumber watcher: on a seek session ffmpeg is
// writing chunks named 1, 2, 3, ... from the start of THIS session, but
// the renumber rename targets are <startSeq>, <startSeq>+1, ... PMS
// reads the manifest body to decide whether the transcoder has reached
// the requested chunk yet (gates the segment GET response on it). If we
// POST ffmpeg's raw manifest with `startNumber="1"`, PMS sees "only
// segment 1 is produced" and waits SegmentedTranscoderTimeout (~120s)
// before falling back to a disk probe. We rewrite startNumber to match
// the renumber's startSeq so PMS sees the right window.
func runManifestPublisher(ctx context.Context, dir, manifestURL, sessionID string, startSeq int) {
	if dir == "" || manifestURL == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("session %s: manifest-publisher mkdir %s: %v", sessionID, dir, err)
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("session %s: manifest-publisher init: %v", sessionID, err)
		return
	}
	defer watcher.Close()
	if err := watcher.Add(dir); err != nil {
		log.Printf("session %s: manifest-publisher add %s: %v", sessionID, dir, err)
		return
	}
	log.Printf("session %s: manifest-publisher watching %s/%s → %s", sessionID, dir, manifestFilename, manifestURL)

	manifestPath := filepath.Join(dir, manifestFilename)
	client := &http.Client{Timeout: 4 * time.Second}
	if startSeq < 1 {
		startSeq = 1
	}

	// Debounce POSTs: ffmpeg may rewrite the file atomically (write to
	// .tmp + rename) or update it twice in quick succession when
	// flushing init+chunk together. Collapse bursts into one POST per
	// 200ms window.
	var (
		mu        sync.Mutex
		pending   bool
		debounceD = 200 * time.Millisecond
	)
	schedule := func() {
		mu.Lock()
		if pending {
			mu.Unlock()
			return
		}
		pending = true
		mu.Unlock()
		go func() {
			time.Sleep(debounceD)
			mu.Lock()
			pending = false
			mu.Unlock()
			postManifest(ctx, client, manifestURL, manifestPath, sessionID, startSeq)
		}()
	}

	// If the file already exists when we start (race between ffmpeg
	// startup and our watcher arm), POST once immediately.
	if _, err := os.Stat(manifestPath); err == nil {
		schedule()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != manifestFilename {
				continue
			}
			// React to anything that changes the file — Create (atomic
			// rename), Write (append), Rename (target side).
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
				continue
			}
			schedule()
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("session %s: manifest-publisher err: %v", sessionID, err)
			return
		}
	}
}

// startNumberRE matches each `startNumber="N"` attribute in a DASH MPD.
// dashenc emits one per Representation/SegmentTemplate.
var startNumberRE = regexp.MustCompile(`startNumber="\d+"`)

// postManifest reads the manifest file and POSTs it. Empty or missing
// file → skipped (we'll catch the next event).
func postManifest(ctx context.Context, client *http.Client, manifestURL, path, sessionID string, startSeq int) {
	body, err := os.ReadFile(path)
	if err != nil {
		// Likely a transient atomic-rename race. Don't log.
		metricManifestPOST.WithLabelValues("err").Inc()
		return
	}
	if len(body) == 0 {
		metricManifestPOST.WithLabelValues("empty").Inc()
		return
	}
	if startSeq > 1 {
		// Renumber watcher renames ffmpeg's 1-indexed chunks to
		// <startSeq>+k, but ffmpeg's manifest still says startNumber=1.
		// PMS gates the chunk GET response on segment_index parsed from
		// this body, so rewrite the attribute. Skipping the timeline
		// `<S t=... d=...>` entries is fine — PMS only logs
		// "Transcoder segment range: 0 - N" once it sees the count
		// advance, which it does fine off startNumber alone.
		repl := []byte(fmt.Sprintf(`startNumber="%d"`, startSeq))
		body = startNumberRE.ReplaceAll(body, repl)
	}
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodPost, manifestURL, bytes.NewReader(body))
	if err != nil {
		metricManifestPOST.WithLabelValues("err").Inc()
		return
	}
	// Match Plex Transcoder.real's request shape so PMS's pipeline parser
	// doesn't reject on a mismatched header. PT.real captured 2026-05-06:
	//   Transfer-Encoding: chunked
	//   User-Agent: Lavf/60.16.100
	//   Accept: */*
	//   Connection: close
	req.Header.Set("Content-Type", "application/dash+xml")
	req.Header.Set("User-Agent", "Lavf/60.16.100")
	req.Header.Set("Accept", "*/*")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("session %s: manifest POST: %v", sessionID, err)
		}
		metricManifestPOST.WithLabelValues("err").Inc()
		return
	}
	resp.Body.Close()
	metricManifestPOST.WithLabelValues(httpClass(resp.StatusCode)).Inc()
	if resp.StatusCode >= 400 {
		log.Printf("session %s: manifest POST status=%d", sessionID, resp.StatusCode)
	}
}
