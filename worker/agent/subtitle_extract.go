package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// probeVideoColor runs ffprobe synchronously to learn the source's
// video color metadata. The rewriter uses `transfer` to detect HDR
// sources and inject tonemap_vaapi when needed.
//
// Cheap (~30-100 ms — header read only). Returns empty strings on
// probe failure; the rewriter then assumes SDR (current behaviour
// pre-tonemap-injection, safe default).
func probeVideoColor(source string) (transfer, primaries, space string) {
	if source == "" {
		return "", "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=color_transfer,color_primaries,color_space",
		"-of", "default=nokey=0:noprint_wrappers=1",
		source,
	}
	out, err := exec.CommandContext(ctx, ffprobeBin, args...).Output()
	if err != nil {
		log.Printf("probeVideoColor: ffprobe %s: %v", source, err)
		return "", "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "color_transfer":
			transfer = v
		case "color_primaries":
			primaries = v
		case "color_space":
			space = v
		}
	}
	return
}

// probeSubtitleCodec runs ffprobe synchronously to learn the codec_name
// of the subtitle stream Plex's `-map_inlineass` references. The
// rewriter uses this to pick text vs bitmap burn-in chains.
//
// Cheap (~30-100 ms — ffprobe just reads container headers, no decode).
// Returns "" on probe failure; the rewriter treats unknown as text and
// the agent's extraction step will fail loud on bitmap inputs.
func probeSubtitleCodec(source, streamSpec string) string {
	if source == "" || streamSpec == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-select_streams", streamSpec,
		"-show_entries", "stream=codec_name",
		"-of", "default=nokey=1:noprint_wrappers=1",
		source,
	}
	out, err := exec.CommandContext(ctx, ffprobeBin, args...).Output()
	if err != nil {
		log.Printf("probeSubtitleCodec: ffprobe %s %s: %v", source, streamSpec, err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// extractSubtitleStream runs a side ffmpeg invocation to extract one
// subtitle stream from the source media into a standalone file the
// rewriter's `subtitles=filename=...` filter can consume.
//
// Plex emits `-map_inlineass <stream>` for sub burn-in: the filter that
// reads it is Plex-private. Stock ffmpeg's `subtitles=` only takes a
// filename, so we extract the embedded stream to disk first. PMS does
// this same dance for sidecar subs (it stages a `temp-N.srt` in the
// session dir before invoking ffmpeg). For embedded streams there's no
// such pre-staged file — we make one.
//
// Synchronous: blocks the /task handler until extraction completes.
// Latency on a typical 1-2 hour movie's SRT track: 100-300 ms (ffmpeg
// stream-copies a tiny stream and exits). Worth waiting for; running
// the main transcode against a half-written file is a non-starter.
func extractSubtitleStream(ctx context.Context, ex *SubtitleExtract) error {
	if ex == nil {
		return nil
	}
	if ex.SourceFile == "" || ex.StreamSpec == "" || ex.OutputFile == "" {
		return fmt.Errorf("incomplete extract spec: %+v", ex)
	}

	if err := os.MkdirAll(filepath.Dir(ex.OutputFile), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	codec := ex.Format
	if codec == "" {
		codec = "srt"
	}

	// `-c:s copy` would be faster but only works when source codec is
	// already the target container's accepted format. SRT/SubRip tracks
	// re-mux freely; ASS/SSA needs `-c:s ass`; mov_text needs
	// conversion. Start simple: convert everything to srt. This loses
	// ASS styling (bold, colour, position) but gains compatibility —
	// most Plex sub-burn requests are SRT anyway.
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", ex.SourceFile,
		"-map", ex.StreamSpec,
		"-c:s", codec,
		ex.OutputFile,
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, ffmpegBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Truncate ffmpeg's verbose output for the API error.
		tail := strings.TrimSpace(string(out))
		if len(tail) > 400 {
			tail = "..." + tail[len(tail)-400:]
		}
		return fmt.Errorf("ffmpeg extract: %w (stderr: %s)", err, tail)
	}

	st, err := os.Stat(ex.OutputFile)
	if err != nil {
		return fmt.Errorf("stat output: %w", err)
	}
	if st.Size() == 0 {
		return fmt.Errorf("ffmpeg produced empty file (stream %s in %s)", ex.StreamSpec, ex.SourceFile)
	}

	return nil
}
