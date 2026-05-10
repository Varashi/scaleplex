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
	"encoding/binary"
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

// watchAndRenumberChunks runs alongside watchFirstSegment. PMS expects:
//
//   Initial play (Plex argv: `-skip_to_segment 1`, no `-ss`): chunks
//   numbered 1, 2, 3, ... — startSeq=1, seekOffsetSeconds=0, no patch.
//
//   Seek (Plex argv: `-ss <off> -skip_to_segment N`): PMS asks
//   `.../0/(N-1).m4s` — file chunk-stream<S>-<N>.m4s on disk.
//   startSeq=N. Each ffmpeg-emitted chunk gets RENAMED to its
//   N-aligned name AND patched: tfdt baseMediaDecodeTime and
//   sidx earliest_presentation_time both get `<off> * timescale`
//   added so MSE places the chunks at the correct global-timeline
//   position. Without the patch, every seek chunk has tfdt=0 (stock
//   dashenc resets it per-segment regardless of -ss/-copyts/+cmaf)
//   and Plex Web's MSE buffers them at timeline 0, leaving the
//   player's currentTime=<off> with no playable data.
//
// Renaming (vs hardlinking) avoids leaving the original 1-indexed
// filename around — that prevented dashenc's window-size rotation
// from cleaning anything up and produced thousands of stale chunks.
// dashenc only ever sees the new file appear (rename is atomic) and
// proceeds.
func watchAndRenumberChunks(ctx context.Context, dir, sessionID string, startSeq int, seekOffsetSeconds float64, lastSeq *atomic.Int64) {
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
			parsedNum, _ := strconv.Atoi(m[2])
			// Skip our own renames: a fresh ffmpeg chunk lands at a low
			// 1-indexed number; any chunk whose number is >= startSeq
			// is a rename target we just produced (the rename triggers
			// a Create event on the target name). Without this check
			// the watcher renames its own output and loops forever
			// (00201 → 00202 → 00203 ...).
			if parsedNum >= startSeq {
				continue
			}
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
			// Patch tfdt + sidx.ept on seek sessions so chunks land
			// on the global timeline.
			if seekOffsetSeconds > 0 {
				if err := patchSeekChunkTimestamps(target, seekOffsetSeconds); err != nil {
					log.Printf("session %s: chunk-renumber tfdt-patch %s: %v", sessionID, filepath.Base(target), err)
				}
			}
			if streamCount[streamID] <= 3 {
				log.Printf("session %s: chunk-renumber stream%s: %s → %s", sessionID, streamID, name, filepath.Base(target))
			}
			// Bump checkpoint counter to highest emitted seq across
			// streams so /task/<id>/checkpoint can hand a recovering
			// worker the right -segment_start_number.
			if lastSeq != nil {
				for {
					prev := lastSeq.Load()
					if int64(seq) <= prev || lastSeq.CompareAndSwap(prev, int64(seq)) {
						break
					}
				}
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

// patchSeekChunkTimestamps rewrites tfdt baseMediaDecodeTime and
// sidx earliest_presentation_time in a single-fragment CMAF chunk by
// adding `seekOffsetSeconds * timescale` (timescale read from sidx).
//
// File layout (post-CMAF fix): styp + sidx + moof + mdat. We read the
// whole file into memory (chunks are 100 KB - 8 MB; cheap), find the
// boxes by scanning, rewrite the relevant fields in place, write back.
//
// All multi-byte ints are big-endian per ISO BMFF.
func patchSeekChunkTimestamps(path string, seekOffsetSeconds float64) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	timescale, sidxStart, sidxEPTOffset, sidxEPTSize, err := findSidxEPT(data)
	if err != nil {
		return fmt.Errorf("sidx: %w", err)
	}
	tfdtStart, tfdtBMDTOffset, tfdtBMDTSize, err := findTfdtBMDT(data)
	if err != nil {
		return fmt.Errorf("tfdt: %w", err)
	}
	delta := u64(uint64(seekOffsetSeconds * float64(timescale)))
	if delta == 0 {
		return nil
	}
	// Read current values
	currEPT := readUintBE(data[sidxStart+sidxEPTOffset:], sidxEPTSize)
	currBMDT := readUintBE(data[tfdtStart+tfdtBMDTOffset:], tfdtBMDTSize)
	newEPT := currEPT + delta
	newBMDT := currBMDT + delta
	writeUintBE(data[sidxStart+sidxEPTOffset:], sidxEPTSize, newEPT)
	writeUintBE(data[tfdtStart+tfdtBMDTOffset:], tfdtBMDTSize, newBMDT)
	return os.WriteFile(path, data, 0o644)
}

// findSidxEPT returns (timescale, sidx_box_start, ept_offset_within_box,
// ept_size_bytes). v0 = 4-byte ept, v1 = 8-byte.
func findSidxEPT(data []byte) (timescale uint32, sidxStart, eptOff int, eptSize int, err error) {
	off, size, err := findBoxAt(data, "sidx", 0)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	body := data[off+8 : off+size]
	if len(body) < 12 {
		return 0, 0, 0, 0, fmt.Errorf("sidx body too short")
	}
	version := body[0]
	timescale = readUintBE(body[8:], 4).low32()
	if version == 0 {
		eptSize = 4
	} else {
		eptSize = 8
	}
	// ept starts at offset 12 in body (after vf=4, refid=4, timescale=4)
	eptOff = 8 + 12
	sidxStart = off
	return
}

func findTfdtBMDT(data []byte) (tfdtStart, bmdtOff, bmdtSize int, err error) {
	moofOff, moofSize, err := findBoxAt(data, "moof", 0)
	if err != nil {
		return 0, 0, 0, err
	}
	trafOff, trafSize, err := findBoxAt(data[moofOff+8:moofOff+moofSize], "traf", 0)
	if err != nil {
		return 0, 0, 0, err
	}
	trafAbs := moofOff + 8 + trafOff
	tfdtRel, _, err := findBoxAt(data[trafAbs+8:trafAbs+8+trafSize-8], "tfdt", 0)
	if err != nil {
		return 0, 0, 0, err
	}
	tfdtStart = trafAbs + 8 + tfdtRel
	body := data[tfdtStart+8:]
	if len(body) < 8 {
		return 0, 0, 0, fmt.Errorf("tfdt body too short")
	}
	version := body[0]
	if version == 0 {
		bmdtSize = 4
	} else {
		bmdtSize = 8
	}
	bmdtOff = 8 + 4 // size=4 + type=4 + version+flags=4
	return
}

func findBoxAt(buf []byte, want string, start int) (int, int, error) {
	i := start
	for i+8 <= len(buf) {
		size := int(binary.BigEndian.Uint32(buf[i : i+4]))
		typ := string(buf[i+4 : i+8])
		if size == 0 {
			size = len(buf) - i
		} else if size == 1 {
			if i+16 > len(buf) {
				return 0, 0, fmt.Errorf("trunc %s", typ)
			}
			size = int(binary.BigEndian.Uint64(buf[i+8 : i+16]))
		}
		if typ == want {
			return i, size, nil
		}
		if size <= 0 {
			return 0, 0, fmt.Errorf("non-positive size at offset %d", i)
		}
		i += size
	}
	return 0, 0, fmt.Errorf("box %q not found", want)
}

type u64 uint64

func (u u64) low32() uint32 { return uint32(u & 0xffffffff) }

func readUintBE(b []byte, n int) u64 {
	switch n {
	case 4:
		return u64(binary.BigEndian.Uint32(b))
	case 8:
		return u64(binary.BigEndian.Uint64(b))
	}
	return 0
}

func writeUintBE(b []byte, n int, v u64) {
	switch n {
	case 4:
		binary.BigEndian.PutUint32(b, uint32(v))
	case 8:
		binary.BigEndian.PutUint64(b, uint64(v))
	}
}
