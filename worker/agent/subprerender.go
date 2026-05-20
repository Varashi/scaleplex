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

// PrerenderResult bundles the per-region pre-render processes + FIFOs
// that the caller is responsible for tearing down. In the single-region
// (Phase 1+2) path Cmds and FIFOs each hold one entry; in the multi-
// region (Phase 3) path they hold one entry per anchor region.
type PrerenderResult struct {
	Cmds  []*exec.Cmd
	FIFOs []string
}

// spawnSubPrerender resolves the subtitle file, decides between single-
// region and multi-region pre-render, mutates mainArgs as needed (filter
// graph + extra -i inputs), and starts the pre-render process(es). The
// returned mainArgs may be a longer slice than the input when the
// multi-region path appended extra FIFO inputs.
//
// Failure at any step removes any FIFOs already created and returns an
// error; the caller must then NOT spawn the main ffmpeg (its `-i fifo`
// would block forever on an empty FIFO).
func spawnSubPrerender(ctx context.Context, spec *SubPrerenderSpec, mainArgs []string) (*PrerenderResult, []string, error) {
	// Original single-region FIFO — always created. Phase 3 multi-region
	// reuses it as the first (bottom) region's FIFO, so the rewriter's
	// existing `-i <FIFOPath>` and `[N:v]` references stay valid.
	_ = os.Remove(spec.FIFOPath)
	if err := syscall.Mkfifo(spec.FIFOPath, 0o600); err != nil {
		return nil, mainArgs, fmt.Errorf("mkfifo %s: %w", spec.FIFOPath, err)
	}
	res := &PrerenderResult{FIFOs: []string{spec.FIFOPath}}
	subFile, err := resolveSubFile(ctx, spec)
	if err != nil {
		res.cleanup()
		return nil, mainArgs, err
	}

	// Try the multi-region plan first — only fires for SRT text specs
	// with cues in more than one anchor row (Phase 3). Falls through
	// to the single-region path otherwise. Bitmap path stays out (its
	// own pre-render shape lives in buildBitmapPrerenderArgs).
	if spec.ResolveBandPostExtract && !spec.Bitmap {
		fallback := subPrerenderBandHeight(spec.Height)
		regions := planMultiRegion(subFile, spec.Height, fallback)
		if len(regions) >= 2 {
			out, newArgs, err := spawnMultiRegion(ctx, spec, mainArgs, regions, res)
			if err != nil {
				res.cleanup()
				return nil, mainArgs, err
			}
			return out, newArgs, nil
		}
	}

	// Single-region path (Phase 1+2). Data-driven band: with the SRT
	// now on disk, pick the tight band and overwrite BandHeight in
	// place. Caller patches the main argv's BandYSentinel afterwards.
	ResolveAgentBand(spec, subFile)
	cmd := exec.CommandContext(ctx, ffmpegBin, buildSubPrerenderArgs(spec, subFile)...)
	cmd.Env = subPrerenderEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM, Setpgid: true}
	// Start() returns immediately; the process then blocks in-kernel
	// opening the FIFO for write until the main transcode opens the
	// read end. No deadlock — the caller spawns the main ffmpeg next.
	if err := cmd.Start(); err != nil {
		res.cleanup()
		return nil, mainArgs, fmt.Errorf("spawn subtitle pre-render: %w", err)
	}
	res.Cmds = append(res.Cmds, cmd)
	return res, mainArgs, nil
}

// cleanup removes any FIFOs the result tracks. Safe to call multiple
// times; missing files are ignored.
func (r *PrerenderResult) cleanup() {
	for _, f := range r.FIFOs {
		_ = os.Remove(f)
	}
}

// spawnMultiRegion sets up the per-region FIFOs, mutates mainArgs's
// filter graph to chain overlay_vaapi instances + appends extra `-i`
// inputs, then spawns one pre-render per region (each reading its
// region-filtered subtitle file). On success spec.MultiRegion is
// populated for later patching by PatchMainArgsBandYMulti.
func spawnMultiRegion(ctx context.Context, spec *SubPrerenderSpec, mainArgs []string, regions []RegionPrerenderSpec, res *PrerenderResult) (*PrerenderResult, []string, error) {
	// Region 0 reuses spec.FIFOPath (already mkfifo'd) as the bottom
	// region's FIFO — keeps the rewriter's `-i <FIFOPath>` valid + the
	// `[N:v]` reference at firstFIFOInput unchanged. Sub file for
	// region 0 is the agent-filtered bottom subset.
	regions[0].FIFOPath = spec.FIFOPath

	// Locate the rewriter's `-i <spec.FIFOPath>` so we know:
	//  - firstFIFOInput  = its position in input-count terms
	//  - the splice index where additional -i entries land (right after it)
	firstFIFOInput := -1
	inputCount := 0
	fifoArgIdx := -1
	for i, a := range mainArgs {
		if a == "-i" && i+1 < len(mainArgs) {
			if mainArgs[i+1] == spec.FIFOPath {
				firstFIFOInput = inputCount
				fifoArgIdx = i
			}
			inputCount++
		}
	}
	if firstFIFOInput < 0 || fifoArgIdx < 0 {
		return nil, mainArgs, fmt.Errorf("multi-region: original FIFO -i not found in args")
	}

	// Detect seek mode by inspecting the filter graph for the
	// `setpts=PTS-STARTPTS` prefix the rewriter inserts on -ss > 0.
	vfIdx := indexOfArg(mainArgs, "-filter_complex", 0)
	if vfIdx < 0 {
		return nil, mainArgs, fmt.Errorf("multi-region: -filter_complex missing")
	}
	seek := strings.Contains(mainArgs[vfIdx+1], "setpts=PTS-STARTPTS,format=bgra")

	// Mutate the filter_complex string in place.
	newGraph, err := MutateGraphForMultiRegion(mainArgs[vfIdx+1], regions, firstFIFOInput, seek)
	if err != nil {
		return nil, mainArgs, err
	}
	mainArgs[vfIdx+1] = newGraph

	// Splice extra `-i <fifo>` entries immediately after the original
	// FIFO -i. -copyts + minimal probe match the original entry's flags.
	insertAt := fifoArgIdx + 2 // past `-i <path>`
	for i := 1; i < len(regions); i++ {
		entry := []string{
			"-copyts", "-probesize", "32", "-analyzeduration", "0",
			"-i", regions[i].FIFOPath,
		}
		mainArgs = append(mainArgs[:insertAt],
			append(append([]string(nil), entry...), mainArgs[insertAt:]...)...)
		insertAt += len(entry)
	}

	// mkfifo + spawn for each additional region (region 0 already has
	// its FIFO; spawn its pre-render below). On any failure unwind.
	for i := 1; i < len(regions); i++ {
		_ = os.Remove(regions[i].FIFOPath)
		if err := syscall.Mkfifo(regions[i].FIFOPath, 0o600); err != nil {
			return nil, mainArgs, fmt.Errorf("mkfifo %s: %w", regions[i].FIFOPath, err)
		}
		res.FIFOs = append(res.FIFOs, regions[i].FIFOPath)
	}

	for i, r := range regions {
		subSpec := *spec // shallow copy
		subSpec.FIFOPath = r.FIFOPath
		subSpec.BandHeight = r.BandHeight
		regionSubFile := r.FilteredFile
		cmd := exec.CommandContext(ctx, ffmpegBin, buildSubPrerenderArgs(&subSpec, regionSubFile)...)
		cmd.Env = subPrerenderEnv()
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM, Setpgid: true}
		if err := cmd.Start(); err != nil {
			return nil, mainArgs, fmt.Errorf("spawn region %d pre-render: %w", i, err)
		}
		res.Cmds = append(res.Cmds, cmd)
	}

	spec.MultiRegion = regions
	return res, mainArgs, nil
}
