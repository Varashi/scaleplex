package main

import (
	"context"
	"log"
	"os/exec"
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
// rewriter uses this to pick text (inlineass pass-through) vs bitmap
// (overlay_vaapi) burn-in chains.
//
// Cheap (~30-100 ms — ffprobe just reads container headers, no decode).
// Returns "" on probe failure; the rewriter treats unknown as text
// (the common case — bitmap streams are rarer).
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

