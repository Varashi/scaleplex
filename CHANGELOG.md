# Changelog

## Unreleased

### Subtitle burn-in (GPU-overlay pre-render) — correctness + performance

Post-v1.1.1 work on the SRT / static-ASS `overlay_vaapi` pre-render
path. Validated on the `plex-test` bench.

- **Fixed AV1 HW-decode corruption on subtitle-burn sessions.** The
  pre-render's `mpdecimate` left multi-second gaps in the overlay
  stream; `overlay_vaapi` framesync then held the main video's decoded
  frames waiting for an overlay frame, overran the AV1 decoder's VAAPI
  surface pool, and the decoder failed mid-stream (`Failed to upload
  decode parameters: 18` → motion corruption). The overlay is now a
  steady, undecimated 5 fps stream — `mpdecimate` removed, do not
  re-add it (even bounded `max=N` reintroduces the gap).
- **Preserved Plex's HDR tonemap on the sub-burn path.** The HW-decode
  sub rewrite hardcoded the scale step to `scale_vaapi(nv12)`, dropping
  the `tonemap_opencl` chain Plex's argv carried — HDR rendered
  washed/dim. The rewrite now keeps the tonemap (`scale_vaapi(p010)` +
  the resolved tonemap stage).
- **Pre-render codec `ffv1` → `qtrle`** (fragmented MOV). qtrle is
  lossless, carries alpha, and is inter-frame, so the long runs of
  identical transparent frames between cues encode as near-empty
  deltas — ~9× less encode CPU than the intra-only ffv1. (ffv1 stays
  intra; qtrle in Matroska/NUT/AVI mis-decodes — fragmented MOV with
  `empty_moov` is the working streaming container.)
- **Bottom-band pre-render (SRT).** SRT is always bottom-positioned, so
  the pre-render renders the full frame (libass needs it for correct
  positioning) then crops to the bottom 40% band before the encode;
  the main graph composites it with `overlay_vaapi=y=Height-BandHeight`.
  ~2.5× less canvas-size-proportional CPU. Sidecar ASS can be
  positioned anywhere, so it keeps the full frame.

Net effect: a 4K HDR + SRT-burn worker sustains roughly **8 concurrent
realtime streams**, up from ~4–5; single-stream transcode CPU roughly
halved.

## v1.1.1 — 2026-05-16

Documentation-only patch release. The v1.1.0 tag landed before the prose
docs were refreshed for the new feature; v1.1.1 carries the matching
docs.

- `README.md` — version references bumped to v1.1; subtitle burn-in
  description now covers both text-sub routes (SRT / static ASS via the
  `overlay_vaapi` pre-render, animated ASS via `inlineass`).
- `docs/REWRITER.md` — "Text sub burn-in" section rewritten for the
  GPU-overlay pre-render path: the `subtitleIsAnimated()` routing, the
  overlay graph, seek rebasing, FIFO input flags, and the
  `hw-decode:filter:sub-prerender-overlay` change tag.

## v1.1.0 — 2026-05-16

### GPU-overlay subtitle burn-in

New burn-in path for SRT and static ASS subtitles. The worker rewriter
replaces Plex's per-frame CPU `inlineass` filter (a `hwdownload` /
libass / `hwupload` bracket) with a GPU `overlay_vaapi` composite. A
second ffmpeg — the *pre-render* — rasterises the subtitle to a sparse
transparent video (`subtitles` → `mpdecimate` → `ffv1`/Matroska) and
streams it through a FIFO; the main transcode reads that FIFO as a
second input and composites on the GPU. All stock `scaleplex-ffmpeg7`
filters — **no new fork patch**.

- Animated ASS (karaoke, `\t`, `\move`, `\fad`) stays on `inlineass`;
  per-frame motion can't be pre-rendered.
- Sidecar subtitles are read directly; embedded subtitles are extracted
  to a temp file first (video/audio streams skipped for a fast extract).
- Seek rebases the overlay graph to zero around `overlay_vaapi` and back,
  so framesync aligns without grinding from 0 to the seek point; the
  main-video timeline is untouched (client playhead unaffected).
- Measured 4K HDR + SRT burn at ~4.7× realtime, vs ~2.2× on `inlineass`
  (~1.3× under contention). Validated on the `plex-test` bench: cold
  start, seek, sidecar and embedded SRT, 4K→4K and 4K→1080p.

### Tooling

- GPLv3 `LICENSE`.
- Renovate config (`renovate.json`) — tracks dependency and base-image
  updates, including the jellyfin-ffmpeg base.

## v1.0.0 — 2026-05-15

First tagged release. scaleplex replaces the `Plex Transcoder` binary on
distributed-transcode workers with stock ffmpeg (`scaleplex-ffmpeg7` —
jellyfin-ffmpeg + a small Plex-backport patch layer), a thin Go
orchestrator, and a PMS-side shim. See [`README.md`](README.md) for the
why and the architecture.

### Validated

End-to-end on the scaleplex PMS deployment: Plex Web DASH (Chrome /
Firefox), Plex Android HLS (mpegts + matroska), Plex Windows desktop
(segmented matroska), LG webOS HLS, and Plex Optimize jobs — initial
play, seek, quality change, and subtitle burn-in as applicable. Source
matrix covers AV1 / HEVC / H264, SDR / HDR10, embedded and sidecar
SRT / ASS text subs, and embedded PGS bitmap subs.

### scaleplex-ffmpeg7 patch series

14 patches (`0094`–`0107`) layered on jellyfin-ffmpeg `v7.1.3-1`,
continuing jellyfin's Debian patch numbering:

- **0094** matroskaenc — always write `Duration` (Plex Windows slider).
- **0095** dashenc — `-manifest_name` URL PUT, `-skip_to_segment`,
  `-break_non_keyframes`, `-delete_removed`.
- **0096** segment — functional `-segment_list_separate_stream_times`
  CSV emit.
- **0097** fftools — `-canthrottleurl` Plex canThrottle handler.
- **0098** fftools + segment — Plex CLI compat (`-loglevel_plex` sink,
  `ssegment` global-header flag).
- **0099–0101** libavfilter + fftools — `inlineass` filter binding and
  pre-graph subtitle buffering.
- **0102** segment — stage-rename chunks (NFS race), skips text-sub
  outputs.
- **0103** segment — drop jellyfin's `reference_stream_first_pts`
  adjust so `-ss + -copyts` splits correctly.
- **0104** dashenc — default CMAF-strict `movflags` on URL output.
- **0105** vaapi_encode — auto-select QVBR when `-qp` + `-maxrate` set.
- **0106** segment — URL-handler segment lists buffer full history.
- **0107** fftools — `-strict_ts` no-op stub.

### Resilience

PMS `canThrottle` pass-through, multi-engine GPU load reporting, and
transparent mid-stream worker recovery across DaemonSet rolls.

### Notes

- The worker rewriter carries no remaining "bandaid" translations — every
  Plex-private argv quirk is either handled by a fork patch or is an
  intentional integration (relay routing, auth token injection).
- Known limitations are tracked in [`docs/KNOWN_ISSUES.md`](docs/KNOWN_ISSUES.md);
  none block playback.
