package main

// First-segment-ready push (LATENCY.md lever 7) + per-stream sequence
// tracking for checkpoint/recovery.
//
// watchFirstSegment: fires once when the first segment file appears,
// emits `[scaleplex] segment-ready: <name>` into the response stream so
// orchestrator/shim can release PMS without polling.
//
// watchChunkSequence: observes chunk-stream<S>-NNNNN.m4s creates and
// bumps task.lastSeq so /task/<id>/checkpoint can hand a recovering
// worker the right `-skip_to_segment <N>` value. Chunk renumbering /
// tfdt+sidx patching used to live here but are no longer needed —
// scaleplex-ffmpeg7's dashenc backports `-skip_to_segment N` natively
// (libavformat/dashenc.c patch 0095) so ffmpeg emits chunks at the
// right index AND with `+frag_discont` movflag set, which makes tfdt
// baseMediaDecodeTime land on the global timeline automatically.

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// watchChunkSequence updates lastSeq each time ffmpeg creates a new
// chunk-stream<S>-NNNNN.m4s, so the orchestrator's checkpoint cache
// can resume a mid-stream worker recovery at the right segment index.
// Tracks the highest seq observed across all streams.
func watchChunkSequence(ctx context.Context, dir, sessionID string, lastSeq *atomic.Int64) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("session %s: chunk-watch mkdir %s: %v", sessionID, dir, err)
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("session %s: chunk-watch init: %v", sessionID, err)
		return
	}
	defer watcher.Close()
	if err := watcher.Add(dir); err != nil {
		log.Printf("session %s: chunk-watch add %s: %v", sessionID, dir, err)
		return
	}
	log.Printf("session %s: chunk-watch tracking %s", sessionID, dir)
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
			m := chunkRE.FindStringSubmatch(filepath.Base(ev.Name))
			if m == nil {
				continue
			}
			seq, _ := strconv.Atoi(m[2])
			if seq <= 0 || lastSeq == nil {
				continue
			}
			for {
				prev := lastSeq.Load()
				if int64(seq) <= prev || lastSeq.CompareAndSwap(prev, int64(seq)) {
					break
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("session %s: chunk-watch err: %v", sessionID, err)
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
