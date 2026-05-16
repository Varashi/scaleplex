package main

// Subtitle pre-render — the agent half of the SRT/static-ASS GPU
// overlay burn-in path. When the rewriter returns a SubPrerenderSpec,
// the agent creates the overlay FIFO and spawns a CPU ffmpeg that
// rasterizes the subtitle into a sparse transparent video streamed
// into that FIFO. The main transcode reads the FIFO as a second video
// input and composites it with overlay_vaapi.
// See project_scaleplex_srt_to_pgs_gpu.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// subPrerenderFPS — the transparent subtitle canvas is rendered at this
// frame rate, then mpdecimate collapses runs of identical frames to one
// per cue transition. 5 fps gives ±200ms cue timing and keeps the
// pre-render comfortably ahead of the encoder; mpdecimate makes the
// emitted frame count independent of this value.
const subPrerenderFPS = 5

// subExtractTimeout bounds the embedded-subtitle extraction pre-step.
const subExtractTimeout = 60 * time.Second

// buildSubPrerenderArgs builds the ffmpeg argv for the subtitle
// pre-render: a transparent canvas at the target resolution with the
// subtitle burned in by libass, decimated to cue-transition frames,
// encoded lossless (ffv1) into a streaming Matroska written to the
// FIFO the main transcode consumes.
func buildSubPrerenderArgs(spec *SubPrerenderSpec, subFile string) []string {
	canvas := fmt.Sprintf("color=c=black@0.0:s=%dx%d:r=%d,format=rgba",
		spec.Width, spec.Height, subPrerenderFPS)

	vf := ""
	if spec.SeekOffsetSeconds > 0 {
		// Shift the synthetic timeline up to the seek offset. This does
		// double duty: the subtitles filter picks cues by frame PTS so
		// it renders the cues active at the seek point, AND the overlay
		// output PTS then matches the seeked main video, which keeps
		// its real (non-zero) timestamps via `-ss N ... -copyts`. The
		// agent also passes `-copyts` on the overlay `-i` so ffmpeg
		// does not rebase the overlay input back to zero — without both
		// halves overlay_vaapi framesync never aligns and the transcode
		// grinds frame-by-frame from 0 up to the seek point.
		vf += "setpts=PTS+" +
			strconv.FormatFloat(spec.SeekOffsetSeconds, 'f', 3, 64) + "/TB,"
	}
	// alpha=1 is required: the subtitles filter leaves the alpha
	// channel untouched by default, so rendering onto the transparent
	// canvas yields text with the correct RGB but alpha 0 — an
	// invisible overlay. alpha=1 makes it composite the alpha too.
	vf += "subtitles=" + escapeFilterPath(subFile) + ":alpha=1"
	// force_style (spec.ForceStyle) is deliberately NOT applied. Plex's
	// font_size / outline / shadow are sized for a PlayRes matching the
	// render height, but the stock subtitles filter renders a raw SRT
	// against libass's default 384x288 script resolution, so those
	// values scale up ~3-4x (oversized text). libass's default SRT
	// style renders correctly sized for the canvas; honoring Plex's
	// exact sizes needs the SRT converted to ASS with PlayRes = canvas
	// first — deferred.
	vf += ",mpdecimate"

	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", canvas,
		"-vf", vf,
		"-fps_mode", "vfr",
		"-c:v", "ffv1",
		"-f", "matroska", spec.FIFOPath,
	}
}

// subPrerenderEnv returns the environment for the pre-render ffmpeg:
// the agent's own env with HOME forced to /home/ubuntu so libass finds
// the per-user fontconfig cache (matching the main transcode env).
func subPrerenderEnv() []string {
	out := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if len(kv) >= 5 && kv[:5] == "HOME=" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME=/home/ubuntu")
}

// resolveSubFile returns a path the `subtitles` filter can read. A
// sidecar spec is used as-is; an embedded subtitle is extracted to a
// temp .srt next to the FIFO so the filter has a deterministic
// single-stream input.
func resolveSubFile(ctx context.Context, spec *SubPrerenderSpec) (string, error) {
	if !spec.Embedded {
		return spec.SourcePath, nil
	}
	extracted := filepath.Join(filepath.Dir(spec.FIFOPath), "scaleplex-sub-extracted.srt")
	ectx, cancel := context.WithTimeout(ctx, subExtractTimeout)
	defer cancel()
	cmd := exec.CommandContext(ectx, ffmpegBin,
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", spec.SourcePath,
		"-map", spec.StreamSpec,
		"-c:s", "srt",
		extracted,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("extract embedded subtitle %s: %w: %s",
			spec.StreamSpec, err, out)
	}
	return extracted, nil
}

// spawnSubPrerender creates the overlay FIFO and starts the subtitle
// pre-render ffmpeg writing to it. The returned *exec.Cmd is tracked
// by the caller for teardown; ctx cancellation also kills it. On any
// failure the FIFO is removed and an error returned — the caller must
// then NOT spawn the main transcode, whose `-i <fifo>` would otherwise
// block forever waiting for a writer.
func spawnSubPrerender(ctx context.Context, spec *SubPrerenderSpec) (*exec.Cmd, error) {
	_ = os.Remove(spec.FIFOPath)
	if err := syscall.Mkfifo(spec.FIFOPath, 0o600); err != nil {
		return nil, fmt.Errorf("mkfifo %s: %w", spec.FIFOPath, err)
	}
	subFile, err := resolveSubFile(ctx, spec)
	if err != nil {
		os.Remove(spec.FIFOPath)
		return nil, err
	}
	cmd := exec.CommandContext(ctx, ffmpegBin, buildSubPrerenderArgs(spec, subFile)...)
	cmd.Env = subPrerenderEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM, Setpgid: true}
	// Start() returns immediately; the process then blocks in-kernel
	// opening the FIFO for write until the main transcode opens the
	// read end. No deadlock — the caller spawns the main ffmpeg next.
	if err := cmd.Start(); err != nil {
		os.Remove(spec.FIFOPath)
		return nil, fmt.Errorf("spawn subtitle pre-render: %w", err)
	}
	return cmd, nil
}
