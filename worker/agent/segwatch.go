package main

// First-segment-ready push (LATENCY.md lever 7).
//
// Watches the ffmpeg output dir via inotify and emits one
// `[scaleplex] segment-ready: <name>` line into the streaming response
// the moment ffmpeg writes the first DASH/HLS/MP4 segment. The
// orchestrator/shim sees this signal and can return to PMS immediately
// instead of polling ffmpeg's progress URL — cuts ~100-500ms off the
// first-frame critical path on warm sessions.

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// lockedWriter serializes writes to an http.ResponseWriter from multiple
// goroutines (stdout pipe, stderr pipe, segwatch). net/http's
// ResponseWriter is not safe for concurrent Write() calls.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
	f  interface{ Flush() }
}

func newLockedWriter(w io.Writer) *lockedWriter {
	lw := &lockedWriter{w: w}
	if f, ok := w.(interface{ Flush() }); ok {
		lw.f = f
	}
	return lw
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	n, err := lw.w.Write(p)
	if lw.f != nil {
		lw.f.Flush()
	}
	return n, err
}

// writePrefixed writes prefix + p in a single locked operation so the
// prefix never gets interleaved between two streams' bytes.
func (lw *lockedWriter) writePrefixed(prefix string, p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if prefix != "" {
		if _, err := io.WriteString(lw.w, prefix); err != nil {
			return 0, err
		}
	}
	n, err := lw.w.Write(p)
	if lw.f != nil {
		lw.f.Flush()
	}
	return n, err
}

// Segment extensions we care about. Anything else under the watched dir
// (e.g. .tmp suffix files ffmpeg uses during atomic rename, or the dash
// manifest) is ignored.
var segmentExts = map[string]struct{}{
	".m4s": {},
	".ts":  {},
	".mp4": {},
}

// chunkRE captures stream id and sequence number from ffmpeg's DASH
// muxer output, e.g. "chunk-stream0-00130.m4s" → ("0", "00130").
var chunkRE = regexp.MustCompile(`^chunk-stream(\d+)-0*(\d+)\.m4s$`)

// watchAndRenumberChunks runs alongside watchFirstSegment. ffmpeg's
// stock DASH muxer numbers chunks based on accumulated PTS / seg
// duration, which our scaleplex argv (no Plex `-skip_to_segment 1`
// extension) emits as chunk-stream<N>-00130.m4s and similar — far past
// the startNumber=1 that PMS's manifest hardcodes. PMS waits ~124s
// for chunk-stream0-00001.m4s before falling back to a disk-probe of
// init-stream0.m4s, blocking /header for two minutes.
//
// Workaround: hardlink each new chunk to chunk-stream<N>-<seq>.m4s
// where seq is a per-stream counter that starts at 1 and increments
// in the order ffmpeg emits the file. PMS opens the file by the
// sequential name and reads the original chunk's bytes.
//
// Hardlinks survive ffmpeg's window_size cleanup (we strip
// -delete_removed so the source filename also stays around). Any
// failure here is best-effort: log and keep going.
func watchAndRenumberChunks(ctx context.Context, dir, sessionID string) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("session %s: chunk-renumber mkdir %s: %v", sessionID, dir, err)
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("session %s: chunk-renumber init: %v", sessionID, err)
		return
	}
	defer watcher.Close()
	if err := watcher.Add(dir); err != nil {
		log.Printf("session %s: chunk-renumber add %s: %v", sessionID, dir, err)
		return
	}
	log.Printf("session %s: chunk-renumber watching %s", sessionID, dir)
	streamSeq := map[string]int{} // streamID → next sequence to assign
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if ev.Op&fsnotify.Create == 0 {
				continue
			}
			name := filepath.Base(ev.Name)
			m := chunkRE.FindStringSubmatch(name)
			if m == nil {
				continue
			}
			streamID := m[1]
			streamSeq[streamID]++
			seq := streamSeq[streamID]
			target := filepath.Join(dir, fmt.Sprintf("chunk-stream%s-%05d.m4s", streamID, seq))
			if name == filepath.Base(target) {
				// ffmpeg already wrote at the right number — nothing to do.
				continue
			}
			// Best-effort hardlink. If we collide (target exists from a
			// prior session crash / restart), drop the existing one
			// first so PMS sees the freshest content.
			_ = os.Remove(target)
			if err := os.Link(ev.Name, target); err != nil {
				log.Printf("session %s: chunk-renumber link %s→%s: %v", sessionID, name, filepath.Base(target), err)
				continue
			}
			if seq <= 3 {
				log.Printf("session %s: chunk-renumber stream%s: %s → %s", sessionID, streamID, name, filepath.Base(target))
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("session %s: chunk-renumber err: %v", sessionID, err)
			return
		}
	}
}

// watchFirstSegment fires once when the first segment file appears in
// `dir`, then returns. Best-effort — any error logs and exits without
// emitting. The caller should run this in its own goroutine.
//
// `dir` is created if missing so we don't race ffmpeg's mkdir.
// `spawnedAt` is used to observe the spawn-to-first-segment latency
// histogram for status.boeye.net.
func watchFirstSegment(ctx context.Context, dir, sessionID string, w *lockedWriter, spawnedAt time.Time) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("session %s: segwatch mkdir %s: %v", sessionID, dir, err)
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("session %s: segwatch init: %v", sessionID, err)
		return
	}
	defer watcher.Close()
	if err := watcher.Add(dir); err != nil {
		log.Printf("session %s: segwatch add %q: %v", sessionID, dir, err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if ev.Op&fsnotify.Create == 0 {
				continue
			}
			ext := strings.ToLower(filepath.Ext(ev.Name))
			if _, want := segmentExts[ext]; !want {
				continue
			}
			name := filepath.Base(ev.Name)
			line := fmt.Sprintf("[scaleplex] segment-ready: %s\n", name)
			_, _ = w.Write([]byte(line))
			if !spawnedAt.IsZero() {
				metricFirstSegmentSeconds.Observe(time.Since(spawnedAt).Seconds())
			}
			log.Printf("session %s: first segment ready: %s", sessionID, name)
			return
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("session %s: segwatch err: %v", sessionID, err)
			return
		}
	}
}
