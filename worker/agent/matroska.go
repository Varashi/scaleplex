package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

// Matroska chunk filename pattern emitted by Plex Windows (segment muxer
// + matroska container): "chunk-NNNNN" with no extension.
var matroskaChunkRE = regexp.MustCompile(`^chunk-0*(\d+)$`)

// watchAndPatchMatroskaChunks runs alongside the segment muxer when
// outputFormat=segment + segment_format=matroska (Plex Windows shape).
// It watches for new chunk-NNNNN files and patches the Cluster Timecode
// in each to add `seekOffsetSeconds`. Without this, the first packet of
// every seek-session chunk has Cluster Timecode=0 (we strip -copyts so
// stock segment muxer can split, and stock segment muxer doesn't carry
// non-zero start PTS through Plex-fork's end_pts offset trick), which
// makes Plex Windows show playback position 0 after a seek and—worse—
// causes audio-track-swap to re-spawn the transcode from offset 0
// because the client passes its (wrong) current position to PMS.
//
// Initial-play sessions (seekOffsetSeconds == 0) skip the patch; the
// fsnotify Watcher is still installed to keep `lastSeq` updated for
// /task/<id>/checkpoint reporting.
func watchAndPatchMatroskaChunks(ctx context.Context, dir, sessionID string, seekOffsetSeconds float64, lastSeq *atomic.Int64) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("session %s: mkv-watch mkdir %s: %v", sessionID, dir, err)
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("session %s: mkv-watch init: %v", sessionID, err)
		return
	}
	defer watcher.Close()
	if err := watcher.Add(dir); err != nil {
		log.Printf("session %s: mkv-watch add %s: %v", sessionID, dir, err)
		return
	}
	log.Printf("session %s: mkv-watch watching %s seekOffset=%.3fs", sessionID, dir, seekOffsetSeconds)

	offsetMs := uint64(seekOffsetSeconds * 1000)
	patched := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Patch on WRITE-close (fsnotify CLOSE_WRITE doesn't exist on
			// all backends; use the last-WRITE-then-CREATE heuristic).
			// Simpler: patch on every WRITE for files that haven't been
			// patched yet, but the chunk may still be growing. Use the
			// segment muxer's behaviour: it writes one chunk, then opens
			// the next. The "current" chunk's file size keeps growing
			// until the NEXT chunk's CREATE event fires; at that point
			// the previous chunk is closed and safe to patch.
			name := filepath.Base(ev.Name)
			m := matroskaChunkRE.FindStringSubmatch(name)
			if m == nil {
				continue
			}
			seq, _ := strconv.Atoi(m[1])
			if lastSeq != nil {
				for {
					prev := lastSeq.Load()
					if int64(seq) <= prev || lastSeq.CompareAndSwap(prev, int64(seq)) {
						break
					}
				}
			}
			if offsetMs == 0 {
				continue
			}
			// On a CREATE of chunk-NNNNN, the previous chunk-(NNNNN-1)
			// is now closed and safe to patch.
			if ev.Op&fsnotify.Create != 0 && seq > 0 {
				prevPath := filepath.Join(dir, fmt.Sprintf("chunk-%05d", seq-1))
				if patched[prevPath] {
					continue
				}
				if err := patchMatroskaClusterTimecode(prevPath, offsetMs); err != nil {
					log.Printf("session %s: mkv-patch %s: %v", sessionID, filepath.Base(prevPath), err)
				} else {
					patched[prevPath] = true
					if len(patched) <= 3 {
						log.Printf("session %s: mkv-patch %s +%dms", sessionID, filepath.Base(prevPath), offsetMs)
					}
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("session %s: mkv-watch err: %v", sessionID, err)
		}
	}
}

// patchMatroskaClusterTimecode opens a matroska chunk file (Cluster
// sequence with no EBML header), finds each Cluster's Timecode element
// (EBML ID 0xE7), and adds `offsetMs` to its value. Strips optional
// CRC32 (0xBF) sibling — modifying Timecode invalidates the CRC, and
// recomputing is expensive; matroska parsers tolerate Clusters without
// CRC32.
//
// Timecode is always re-encoded as a fixed 8-byte uint so we never need
// to grow the size field's width. Cluster size grows by net (new
// timecode width - old timecode width - optional CRC32 size); we
// re-encode Cluster size at the same byte width as the original since
// the growth is small (~4 bytes) and the original width has headroom.
func patchMatroskaClusterTimecode(path string, offsetMs uint64) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	out := make([]byte, 0, len(data)+64)
	pos := 0
	clusterIDBytes := []byte{0x1F, 0x43, 0xB6, 0x75}
	for pos < len(data) {
		rel := bytes.Index(data[pos:], clusterIDBytes)
		if rel < 0 {
			out = append(out, data[pos:]...)
			break
		}
		// Copy any bytes between last cluster and this one (shouldn't
		// happen in well-formed chunks, but defensive).
		out = append(out, data[pos:pos+rel]...)
		clusterAbs := pos + rel
		// Cluster ID
		out = append(out, clusterIDBytes...)
		// Cluster size (EBML variable-length)
		szBytes, szValue, ok := readEBMLSize(data[clusterAbs+4:])
		if !ok || szBytes == 0 {
			// Malformed — bail by copying the rest verbatim.
			out = append(out, data[clusterAbs+4:]...)
			break
		}
		bodyStart := clusterAbs + 4 + szBytes
		bodyEnd := bodyStart + int(szValue)
		if bodyEnd > len(data) {
			out = append(out, data[clusterAbs+4:]...)
			break
		}
		newBody, patched := patchClusterBody(data[bodyStart:bodyEnd], offsetMs)
		if !patched {
			// No Timecode element found — preserve cluster as-is.
			out = append(out, data[clusterAbs+4:bodyEnd]...)
			pos = bodyEnd
			continue
		}
		// Re-encode Cluster size at the same byte width as the original
		// (newBody is at most a few bytes larger; original width has
		// headroom).
		newSize := uint64(len(newBody))
		out = append(out, encodeEBMLSizeFixedWidth(newSize, szBytes)...)
		out = append(out, newBody...)
		pos = bodyEnd
	}
	return os.WriteFile(path, out, 0o644)
}

// patchClusterBody walks a Cluster's body, strips optional CRC32, shifts
// the Timecode element by offsetMs, and returns the rewritten body plus
// a flag indicating whether a Timecode was actually patched.
func patchClusterBody(body []byte, offsetMs uint64) ([]byte, bool) {
	out := make([]byte, 0, len(body)+16)
	pos := 0
	// Strip optional CRC32 (EBML ID 0xBF) at start of body.
	if pos < len(body) && body[pos] == 0xBF {
		szBytes, szValue, ok := readEBMLSize(body[pos+1:])
		if ok && szBytes > 0 {
			pos = pos + 1 + szBytes + int(szValue)
		}
	}
	// Expect Timecode (EBML ID 0xE7) next.
	if pos >= len(body) || body[pos] != 0xE7 {
		return body, false
	}
	szBytes, szValue, ok := readEBMLSize(body[pos+1:])
	if !ok || szBytes == 0 {
		return body, false
	}
	valStart := pos + 1 + szBytes
	valEnd := valStart + int(szValue)
	if valEnd > len(body) {
		return body, false
	}
	oldVal := readUintBEBytes(body[valStart:valEnd])
	newVal := oldVal + offsetMs
	// Re-encode Timecode at fixed 8-byte width: E7 88 <8 bytes>.
	out = append(out, 0xE7, 0x88)
	for i := 7; i >= 0; i-- {
		out = append(out, byte(newVal>>(i*8)))
	}
	// Append rest of body verbatim.
	out = append(out, body[valEnd:]...)
	return out, true
}

// readEBMLSize parses a variable-length EBML size at the start of b.
// Returns (bytes_read, value, ok).
func readEBMLSize(b []byte) (int, uint64, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	first := b[0]
	if first == 0 {
		return 0, 0, false
	}
	// Count leading zeros to find marker bit position.
	var width int
	for i := 0; i < 8; i++ {
		if first&(0x80>>i) != 0 {
			width = i + 1
			break
		}
	}
	if width == 0 || len(b) < width {
		return 0, 0, false
	}
	mask := byte((1 << (8 - width)) - 1)
	value := uint64(first & mask)
	for i := 1; i < width; i++ {
		value = (value << 8) | uint64(b[i])
	}
	return width, value, true
}

// encodeEBMLSizeFixedWidth encodes a size value with exactly `width`
// bytes. Width is the marker-bit position (1..8). Used to keep the
// Cluster size field at the same byte width as the original.
func encodeEBMLSizeFixedWidth(value uint64, width int) []byte {
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(value & 0xFF)
		value >>= 8
	}
	out[0] |= byte(0x80 >> (width - 1))
	return out
}

// readUintBEBytes interprets b as a big-endian unsigned integer.
func readUintBEBytes(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = (v << 8) | uint64(x)
	}
	return v
}
