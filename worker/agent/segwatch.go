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
	"strings"
	"sync"

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

// watchFirstSegment fires once when the first segment file appears in
// `dir`, then returns. Best-effort — any error logs and exits without
// emitting. The caller should run this in its own goroutine.
//
// `dir` is created if missing so we don't race ffmpeg's mkdir.
func watchFirstSegment(ctx context.Context, dir, sessionID string, w *lockedWriter) {
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
