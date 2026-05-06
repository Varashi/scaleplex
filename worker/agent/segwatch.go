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

// watchAndRenumberChunks runs alongside watchFirstSegment. The
// rewriter forces stock dashenc to count segments 1, 2, 3, ... from
// the start of THIS session (via `-output_ts_offset 0` and stripping
// `-copyts/-start_at_zero`). PMS expects:
//
//   Initial play (Plex argv: `-skip_to_segment 1`, no `-ss`): chunks
//   numbered 1, 2, 3, ... — startSeq=1, no rename happens.
//
//   Seek (Plex argv: `-ss <off> -skip_to_segment N`): PMS asks
//   `.../0/(N-1).m4s` — file chunk-stream<S>-<N>.m4s on disk.
//   startSeq=N. Each ffmpeg-emitted chunk gets RENAMED to its
//   N-aligned name.
//
// Renaming (vs hardlinking) avoids leaving the original 1-indexed
// filename around — that prevented dashenc's window-size rotation
// from cleaning anything up and produced thousands of stale chunks.
// dashenc only ever sees the new file appear (rename is atomic) and
// proceeds.
func watchAndRenumberChunks(ctx context.Context, dir, sessionID string, startSeq int) {
	if dir == "" {
		return
	}
	if startSeq < 1 {
		startSeq = 1
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
	log.Printf("session %s: chunk-renumber watching %s startSeq=%d", sessionID, dir, startSeq)
	streamCount := map[string]int{} // streamID → emitted-chunk count
	debugCount := 0
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if debugCount < 25 {
				log.Printf("session %s: chunk-renumber raw event op=%v name=%s", sessionID, ev.Op, filepath.Base(ev.Name))
				debugCount++
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
			streamCount[streamID]++
			seq := startSeq + streamCount[streamID] - 1
			target := filepath.Join(dir, fmt.Sprintf("chunk-stream%s-%05d.m4s", streamID, seq))
			if name == filepath.Base(target) {
				// ffmpeg already wrote at the right number — nothing to do.
				continue
			}
			// Atomic rename. Removes the source side so dashenc's
			// window-rotation can keep up; the alternative (hardlink)
			// left thousands of stale 1-indexed files behind that
			// dashenc never cleaned up.
			if err := os.Rename(ev.Name, target); err != nil {
				log.Printf("session %s: chunk-renumber rename %s→%s: %v", sessionID, name, filepath.Base(target), err)
				continue
			}
			if streamCount[streamID] <= 3 {
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
