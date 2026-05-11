package main

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// chunk-NNNNN filename in the matroska segment_list CSV. Plex Windows
// shape uses no extension; HLS-mpegts uses `media-NNNNN.ts` (handled by
// csvRowRE separately).
var mkvChunkFilenameRE = regexp.MustCompile(`^chunk-(\d+)$`)

// chunkFilenamesFromCSV scans the segment_list CSV body for chunk-NNNNN
// filenames. Used by the relay to determine which chunks to patch before
// forwarding the CSV to PMS.
func chunkFilenamesFromCSV(body []byte) []string {
	var out []string
	for _, line := range bytes.Split(body, []byte("\n")) {
		s := bytes.TrimSpace(line)
		if len(s) == 0 || s[0] == '#' {
			continue
		}
		comma := bytes.IndexByte(s, ',')
		if comma < 0 {
			continue
		}
		name := string(s[:comma])
		if mkvChunkFilenameRE.MatchString(name) {
			out = append(out, name)
		}
	}
	return out
}

// sessionDirFromManifestURL extracts the on-disk session dir from the
// segment_list URL's path. Plex's path is
// `/video/:/transcode/session/<sid>/<job>/manifest`, and the segment
// muxer writes chunks into
// `/transcode/Transcode/Sessions/plex-transcode-<sid>-<job>/`.
//
// The relay shares the NFS-mounted `/transcode` volume with PMS, so
// reading/writing chunks at this path works exactly like the worker
// does it.
var sessionPathRE = regexp.MustCompile(`^/video/:/transcode/session/([^/]+)/([^/]+)/manifest$`)

func sessionDirFromManifestURL(urlPath string) string {
	m := sessionPathRE.FindStringSubmatch(urlPath)
	if m == nil {
		return ""
	}
	return "/transcode/Transcode/Sessions/plex-transcode-" + m[1] + "-" + m[2]
}

// patchSessionMatroskaChunks reads each chunk file in `dir`, patches
// every Cluster's Timecode element by `offsetMs`, and writes the file
// back. Used by the relay when forwarding a segment_list CSV POST to
// PMS — patches must happen BEFORE the CSV announces chunks to PMS so
// mpv reads consistent timecodes from the first byte (mpv anchors the
// timeline to the first Cluster.Timecode it parses; if chunk-0 has
// Timecode=0 the entire stream timeline reads as starting at 0,
// breaking absolute position tracking and audio-track-swap).
func patchSessionMatroskaChunks(dir string, names []string, offsetMs uint64) (int, error) {
	if offsetMs == 0 || len(names) == 0 {
		return 0, nil
	}
	patched := 0
	for _, name := range names {
		// Defence in depth: don't allow path traversal.
		if strings.ContainsAny(name, "/\\") {
			continue
		}
		p := path.Join(dir, name)
		if err := patchMatroskaClusterTimecode(p, offsetMs); err != nil {
			// Log but continue; a single bad chunk shouldn't block
			// the rest of the CSV.
			return patched, fmt.Errorf("patch %s: %w", name, err)
		}
		patched++
	}
	return patched, nil
}

// patchMatroskaClusterTimecode shifts every Cluster's Timecode element
// in `path` by `offsetMs`. Duplicated from worker/agent/matroska.go to
// keep the relay binary self-contained.
func patchMatroskaClusterTimecode(filePath string, offsetMs uint64) error {
	data, err := os.ReadFile(filePath)
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
		out = append(out, data[pos:pos+rel]...)
		clusterAbs := pos + rel
		out = append(out, clusterIDBytes...)
		szBytes, szValue, ok := readEBMLSize(data[clusterAbs+4:])
		if !ok || szBytes == 0 {
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
			out = append(out, data[clusterAbs+4:bodyEnd]...)
			pos = bodyEnd
			continue
		}
		newSize := uint64(len(newBody))
		out = append(out, encodeEBMLSizeFixedWidth(newSize, szBytes)...)
		out = append(out, newBody...)
		pos = bodyEnd
	}
	return os.WriteFile(filePath, out, 0o644)
}

func patchClusterBody(body []byte, offsetMs uint64) ([]byte, bool) {
	out := make([]byte, 0, len(body)+16)
	pos := 0
	if pos < len(body) && body[pos] == 0xBF {
		szBytes, szValue, ok := readEBMLSize(body[pos+1:])
		if ok && szBytes > 0 {
			pos = pos + 1 + szBytes + int(szValue)
		}
	}
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
	out = append(out, 0xE7, 0x88)
	for i := 7; i >= 0; i-- {
		out = append(out, byte(newVal>>(i*8)))
	}
	out = append(out, body[valEnd:]...)
	return out, true
}

func readEBMLSize(b []byte) (int, uint64, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	first := b[0]
	if first == 0 {
		return 0, 0, false
	}
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

func encodeEBMLSizeFixedWidth(value uint64, width int) []byte {
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(value & 0xFF)
		value >>= 8
	}
	out[0] |= byte(0x80 >> (width - 1))
	return out
}

func readUintBEBytes(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = (v << 8) | uint64(x)
	}
	return v
}

// _ silences unused-import warnings when this file is compiled
// standalone for testing.
var _ = strconv.Atoi
