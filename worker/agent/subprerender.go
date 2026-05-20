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
	"strings"
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
	// CFR canvas (input 0) drives the output timeline at subPrerenderFPS.
	// When BandHeight < Height the canvas is band-sized — the sub is
	// scaled to full frame (so positioning is preserved), then cropped
	// to the bottom band, then composited onto this band canvas. Output
	// is band-sized → less qtrle encode, less main-side hwupload + less
	// overlay_vaapi blend area. The main graph places the band at
	// y = Height - BandHeight.
	canvasH := spec.Height
	if spec.BandHeight > 0 && spec.BandHeight < spec.Height {
		canvasH = spec.BandHeight
	}
	// Canvas stays 0-based (no setpts). The mov muxer rebases all
	// output timestamps to 0 anyway — any canvas setpts shift would be
	// silently flattened. Seek alignment is done by rebasing the SUB
	// branch to 0-based-from-seek (see below).
	canvas := fmt.Sprintf("color=c=black@0.0:s=%dx%d:r=%d,format=rgba",
		spec.Width, canvasH, subPrerenderFPS)

	// StreamSpec from the rewriter is in main-argv terms ("0:5" — input
	// 0 stream 5). In the pre-render the canvas (lavfi) is input 0 and
	// the source moves to input 1, so remap the leading "0:" → "1:".
	sel := spec.StreamSpec
	if sel == "" {
		sel = "1:s:0"
	} else if strings.HasPrefix(sel, "0:") {
		sel = "1:" + sel[2:]
	}

	// Sub branch: scale to FULL frame (PGS positioning is encoded as
	// pixel offsets in its full canvas, so the upscale must use full
	// dimensions to land at the right pixel coordinates), then optionally
	// crop the bottom band. The earlier lean form ([0:N]scale,fps=5,
	// format=argb) was simpler but `fps` rebased the output timeline to
	// PTS 0 regardless of -copyts; driving the timeline from a `color`
	// canvas keeps cue PTS aligned for initial play.
	//
	// On a seek session, rebase the sub timeline by the SEEK OFFSET
	// (`setpts=PTS-N/TB`), not by STARTPTS. With -copyts + -ss N the
	// sub frames are at absolute PTS; subtracting N puts them on the
	// 0-based-from-seek timeline matching the main video's
	// `-ss N -copyts -start_at_zero`. Using PTS-STARTPTS instead would
	// rebase from the FIRST SUB FRAME — which is the next cue AFTER
	// the seek (e.g. cue at 300.133 with seek 298) — landing that cue
	// at output PTS 0 instead of its correct PTS 2.133.
	//
	// Initial play (SeekOffsetSeconds=0) must NOT rebase — the first
	// cue's real absolute PTS (e.g. 38.956s) drives cue placement.
	subBranch := fmt.Sprintf("[%s]", sel)
	if spec.SeekOffsetSeconds > 0 {
		subBranch += "setpts=PTS-" +
			strconv.FormatFloat(spec.SeekOffsetSeconds, 'f', 3, 64) + "/TB,"
	}
	subBranch += fmt.Sprintf("scale=%d:%d", spec.Width, spec.Height)
	if spec.BandHeight > 0 && spec.BandHeight < spec.Height {
		subBranch += fmt.Sprintf(",crop=%d:%d:0:%d",
			spec.Width, spec.BandHeight, spec.Height-spec.BandHeight)
	}
	fc := subBranch +
		"[sub];[0:v][sub]overlay=eof_action=pass:repeatlast=1,format=argb[o]"

	// -copyts is mandatory. The pre-render reads the source's subtitle
	// stream with `-vn -an`; without -copyts ffmpeg rebases the first
	// PGS packet (rarely at 0) to PTS 0, and overlay then composites the
	// first cue against canvas-PTS 0 — same bug as before. With -copyts
	// the sub2video frames keep absolute PTS; overlay pairs each with
	// the canvas frame at the matching PTS.
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y", "-copyts",
		// Let the filter graph use multiple threads — the sub branch
		// (scale + optional crop) and the canvas branch can run in
		// parallel before merging at overlay.
		"-filter_complex_threads", "4",
		"-f", "lavfi", "-i", canvas,
		// -vn/-an are OUTPUT flags — the demuxer still reads video/audio
		// packets. For a 4K AV1 + audio MKV that demux alone is
		// expensive, and on a deep seek (Plex resume) it adds tens of
		// seconds to startup latency before the first subtitle packet
		// is reached. -discard:v/-discard:a drop those packets at the
		// demuxer boundary so only the subtitle stream is actually
		// parsed.
		"-vn", "-an",
		"-discard:v", "all", "-discard:a", "all",
	}
	if spec.SeekOffsetSeconds > 0 {
		args = append(args, "-ss",
			strconv.FormatFloat(spec.SeekOffsetSeconds, 'f', 3, 64))
	}
	return append(args,
		"-i", spec.SourcePath,
		"-filter_complex", fc,
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
	// Data-driven band: with the SRT now on disk (extracted or sidecar),
	// pick the tight band if the parser approves and overwrite the
	// rewriter's static-fallback BandHeight in place. Caller patches the
	// main argv's overlay_vaapi y= afterwards via PatchMainArgsBandY.
	ResolveAgentBand(spec, subFile)
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
