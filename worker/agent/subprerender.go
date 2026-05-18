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
// steady frame rate. 5 fps gives ±200ms cue timing.
//
// The overlay stream must NOT be decimated — not even bounded. To emit
// a main-video frame at PTS T, overlay_vaapi framesync needs an overlay
// frame past T; until it has one it holds the decoded main frames, each
// pinning a VAAPI surface from the main AV1 decoder's small fixed pool.
// An overlay gap of G seconds makes framesync hold ~G*24 surfaces — and
// the pool overruns well before 1s. Plain `mpdecimate` (unbounded gap)
// killed playback ~4min in; `mpdecimate=max=10` (2s gap) killed it
// ~12s in — both surfaced as the AV1 HW decoder failing with "Failed
// to upload decode parameters: 18". A steady 5fps holds the gap at
// 0.2s (~5 surfaces) — safe. Do not re-add mpdecimate; keep the stream
// dense. The encode codec (qtrle, inter-frame) is what keeps a dense
// stream cheap — see buildSubPrerenderArgs.
const subPrerenderFPS = 5

// subExtractTimeout bounds the embedded-subtitle extraction pre-step.
const subExtractTimeout = 60 * time.Second

// subPrerenderBandFracNum / subPrerenderBandFracDen — the band the
// pre-render emits is this fraction of the frame height (bottom 2/5).
// SRT subtitles sit in the bottom ~25%; 40% leaves comfortable headroom
// for tall multi-line cues while still cutting the canvas-proportional
// pre-render + overlay-upload CPU ~2.5x.
const (
	subPrerenderBandFracNum = 2
	subPrerenderBandFracDen = 5
)

// subPrerenderBandHeight returns the bottom-band height (even, encoders
// require even dimensions) for an output frame of height h. The rewriter
// uses it for SRT sources; ASS keeps the full frame.
func subPrerenderBandHeight(h int) int {
	if h < 10 {
		return h
	}
	b := h * subPrerenderBandFracNum / subPrerenderBandFracDen
	if b%2 == 1 {
		b++
	}
	if b >= h {
		return h
	}
	return b
}

// buildSubPrerenderArgs builds the ffmpeg argv for the subtitle
// pre-render: a steady-fps transparent canvas with the subtitle burned
// in by libass, encoded lossless into a streaming container on the FIFO
// the main transcode consumes.
//
// Codec is qtrle (QuickTime Animation): lossless, carries an alpha
// channel (argb), and inter-frame — an unchanged frame encodes as a
// near-empty delta. The stream stays dense at subPrerenderFPS (framesync
// needs that — see the subPrerenderFPS comment), but the long runs of
// identical transparent frames between subtitle cues cost almost
// nothing to encode. Measured: ~9x less encode CPU than the intra-only
// ffv1 it replaced (which re-encoded every identical 4K frame in full).
//
// Container is fragmented MOV. qtrle in Matroska loses its pixel-format
// metadata (decoder then fails "Unsupported colorspace: 0 bits/sample");
// NUT and AVI also mis-handle it. `frag_keyframe+empty_moov+
// default_base_moof` makes MOV streamable through the FIFO with the
// moov header up front so the main transcode's tiny `-probesize 32`
// probe still resolves the codec.
func buildSubPrerenderArgs(spec *SubPrerenderSpec, subFile string) []string {
	if spec.Bitmap {
		return buildBitmapPrerenderArgs(spec)
	}
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
	// Crop to the bottom band: libass rendered against the full frame
	// (so positioning is correct), but only the bottom band carries SRT
	// text — emitting just that band shrinks the qtrle encode, the
	// format convert, and the main transcode's overlay hwupload. The
	// rewriter places the band at y=Height-BandHeight via overlay_vaapi.
	// BandHeight == Height (ASS, can be positioned anywhere) → no crop.
	if spec.BandHeight > 0 && spec.BandHeight < spec.Height {
		vf += fmt.Sprintf(",crop=%d:%d:0:%d",
			spec.Width, spec.BandHeight, spec.Height-spec.BandHeight)
	}
	// qtrle encodes argb; the subtitles filter leaves the canvas rgba.
	vf += ",format=argb"

	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", canvas,
		"-vf", vf,
		"-fps_mode", "vfr",
		"-c:v", "qtrle",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mov", spec.FIFOPath,
	}
}

// buildBitmapPrerenderArgs builds the pre-render argv for a bitmap
// subtitle (PGS / VobSub / DVDSub). Unlike the text path there is no
// libass `subtitles=` filter and no `color` canvas: the bitmap
// subtitle stream is sub2video-bridged straight from the source,
// `scale` upscales that frame to the canvas size, and `fps` makes it
// the steady CFR stream overlay_vaapi's framesync needs.
//
// Why a separate process: feeding the sparse sub2video stream straight
// into the main transcode's overlay_vaapi lets framesync drain it at
// frame rate, re-running the (expensive) 4K upscale per pulled frame —
// CPU climbs to ~2 cores and the transcode collapses. Doing the
// upscale here, rate-bounded by `fps` at subPrerenderFPS in an
// isolated process, and handing the main transcode a clean CFR qtrle
// stream, removes the upscale from the framesync hot path.
// See project_scaleplex_pgs_prerender.
func buildBitmapPrerenderArgs(spec *SubPrerenderSpec) []string {
	sel := spec.StreamSpec
	if sel == "" {
		sel = "0:s:0"
	}
	vf := fmt.Sprintf("[%s]scale=%d:%d,fps=%d",
		sel, spec.Width, spec.Height, subPrerenderFPS)
	if spec.SeekOffsetSeconds > 0 {
		// -ss seeks the source 0-based; shift the output PTS back up to
		// the seek offset so it aligns with the main video's -copyts
		// stream (same role as the text path's setpts shift).
		vf += ",setpts=PTS+" +
			strconv.FormatFloat(spec.SeekOffsetSeconds, 'f', 3, 64) + "/TB"
	}
	// qtrle carries alpha as argb; the sub2video frame is rgba.
	vf += ",format=argb[o]"

	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-vn", "-an"}
	if spec.SeekOffsetSeconds > 0 {
		args = append(args, "-ss",
			strconv.FormatFloat(spec.SeekOffsetSeconds, 'f', 3, 64))
	}
	return append(args,
		"-i", spec.SourcePath,
		"-filter_complex", vf,
		"-map", "[o]",
		"-fps_mode", "vfr",
		"-c:v", "qtrle",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mov", spec.FIFOPath,
	)
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
	// Bitmap subs are read straight from the source by buildBitmapPrerenderArgs
	// (sub2video bridge) — no .srt extraction step.
	if spec.Bitmap {
		return spec.SourcePath, nil
	}
	if !spec.Embedded {
		return spec.SourcePath, nil
	}
	extracted := filepath.Join(filepath.Dir(spec.FIFOPath), "scaleplex-sub-extracted.srt")
	ectx, cancel := context.WithTimeout(ctx, subExtractTimeout)
	defer cancel()
	// `-vn -an` as INPUT options: tell the demuxer to skip the video
	// and audio streams entirely. Without them ffmpeg reads (and sets
	// up decoders for) the whole multi-GB 4K source just to reach the
	// interleaved subtitle blocks — ~15s on a 4.8 GB file vs ~1s with
	// the streams skipped (measured).
	cmd := exec.CommandContext(ectx, ffmpegBin,
		"-hide_banner", "-loglevel", "error", "-y",
		"-vn", "-an",
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
