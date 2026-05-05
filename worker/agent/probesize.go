package main

// Adaptive probesize / analyzeduration via local ffprobe.
//
// Plex sets `-probesize 20MB / -analyzeduration 20MB` conservatively,
// which on a 4K AV1 NFS source is a 1-3 second NFS read for format
// detection (LATENCY.md lever 2). The worker has the source mounted
// locally, so we can ffprobe with a smaller probesize ourselves and
// substitute the smallest value that yields a complete stream listing.
//
// Best effort: any error → leave the original args unchanged. Cached by
// inode + mtime so a re-spawn for the same session is free.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// probesize candidates in bytes (1MB, 4MB, 20MB). 20MB matches Plex's
// upper bound and is the safe ceiling for trick-play AV1 files.
var probeSizeCandidates = []int{1 << 20, 4 << 20, 20 << 20}

const ffprobeBin = "/usr/bin/ffprobe"

type probeKey struct {
	inode uint64
	mtime int64
	dev   uint64
}

type probeCacheEntry struct {
	probesize int
	at        time.Time
}

var (
	probeCacheMu sync.RWMutex
	probeCache   = make(map[probeKey]probeCacheEntry)
)

func probeKeyFor(path string) (probeKey, error) {
	st, err := os.Stat(path)
	if err != nil {
		return probeKey{}, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return probeKey{}, fmt.Errorf("stat: not a syscall.Stat_t")
	}
	return probeKey{inode: sys.Ino, dev: sys.Dev, mtime: st.ModTime().UnixNano()}, nil
}

// adaptiveProbesize returns the smallest probesize (bytes) that ffprobe
// could parse a complete stream listing with, for the given media path.
// Returns 0 on failure (caller should keep PMS values).
func adaptiveProbesize(path string) int {
	key, err := probeKeyFor(path)
	if err != nil {
		return 0
	}
	probeCacheMu.RLock()
	if e, ok := probeCache[key]; ok {
		probeCacheMu.RUnlock()
		return e.probesize
	}
	probeCacheMu.RUnlock()

	for _, size := range probeSizeCandidates {
		if probeOK(path, size) {
			probeCacheMu.Lock()
			probeCache[key] = probeCacheEntry{probesize: size, at: time.Now()}
			probeCacheMu.Unlock()
			return size
		}
	}
	return 0
}

// probeOK runs ffprobe at the requested probesize/analyzeduration and
// returns true iff it exits 0 and emits at least one stream.
func probeOK(path string, size int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := strconv.Itoa(size)
	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "error",
		"-probesize", s,
		"-analyzeduration", s,
		"-print_format", "json",
		"-show_streams",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	var parsed struct {
		Streams []json.RawMessage `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return false
	}
	return len(parsed.Streams) > 0
}

// applyAdaptiveProbesize rewrites -probesize / -analyzeduration in args
// (in place semantics; returns the slice). No-op if either flag is
// missing, or if adaptive probing fails for the input path.
func applyAdaptiveProbesize(args []string) ([]string, []string) {
	changes := []string{}
	psIdx := indexOfArg(args, "-probesize", 0)
	adIdx := indexOfArg(args, "-analyzeduration", 0)
	if psIdx < 0 && adIdx < 0 {
		return args, changes
	}
	inputIdx := indexOfArg(args, "-i", 0)
	if inputIdx < 0 || inputIdx+1 >= len(args) {
		return args, changes
	}
	src := args[inputIdx+1]

	picked := adaptiveProbesize(src)
	if picked == 0 {
		return args, changes
	}
	s := strconv.Itoa(picked)
	if psIdx >= 0 && psIdx+1 < len(args) {
		args[psIdx+1] = s
		changes = append(changes, "probesize:"+s)
	}
	if adIdx >= 0 && adIdx+1 < len(args) {
		args[adIdx+1] = s
		changes = append(changes, "analyzeduration:"+s)
	}
	return args, changes
}
