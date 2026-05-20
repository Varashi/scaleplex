# Changelog

## v1.2.1 — 2026-05-20

### SRT sub-burn pre-render — tight bottom band for plain sidecar SRT

For sidecar SRT inputs the rewriter now parses the cues at rewrite time and
sizes the pre-render's bottom band to the actual maximum-lines-per-cue plus
a safety margin, instead of the static 40% fallback shipped in v1.2.0.

Constants calibrated against the worker image with default `subtitles=`
filter + DejaVu Sans + libass default style (measured 2026-05-20 via
`cropdetect` on 1/2/4-line cues at 1080p + 4K):
`bandH = 5% + lines * 6% + 8% safety` of frame height, even-rounded.

Live readings on plex-test 4K HEVC HDR + sidecar Dutch SRT (707 cues, max
2 lines): pre-render `crop=3840:540:0:1620` (vs prior 864 px band, ~37 %
smaller). Pre-render CPU 47 % → 28 %, total session 1.69 → 1.31 cores
(~22 % saved). Emits a new rewriter change tag `sub-prerender:band:tight`.

Bails to the static fallback band when:
- the parser can't reach the file (embedded SRT extraction happens
  post-rewrite; embedded path stays on the static band for now);
- any cue carries a positional override (`\anN` with N>3, `\pos(...)`,
  `\move(...)`, `\org(...)`) that moves it off the default bottom row;
- the computed savings would be < 10 % of the fallback band (avoids
  churn for marginal wins).

The next-tier optimisation — multi-region pre-render so positional cues
in otherwise-bottom-aligned SRTs also benefit — is tracked for v1.2.2.

### Sidecar codec-probe stream-spec fix

Pre-existing bug surfaced during the tight-band rollout. The codec probe
for a sidecar subtitle input passed PMS-argv stream-spec `1:s:0`
verbatim to ffprobe against a single-stream sidecar file. ffprobe
rejects a file-index prefix when only one input is given ("Invalid
stream specifier"), the codec lookup returned empty, and any rewriter
code path that gated on `subSrc.Codec == "subrip"` silently degraded.

Worker now drops the leading `N:` when probing a sidecar input
(`1:s:0` → `s:0`). Embedded paths (input 0) keep their PMS-argv form
against the source file (e.g. `0:3`).

## v1.2.0 — 2026-05-20

### Build base — jellyfin-ffmpeg v7.1.3-1 → v7.1.3-6

The `scaleplex-ffmpeg` fork rebased onto jellyfin-ffmpeg v7.1.3-6. All
14 fork patches (0094-0107) apply cleanly against the new base. No
file overlap with jellyfin's debian patch series; no fork-patch
rebasing required.

Upstream changes that land on scaleplex's hot paths:

- **vaapi backports from upstream** (jellyfin patch 0017, new). Touches
  scaleplex's overlay_vaapi (sub burn), scale_vaapi (HDR tonemap),
  hevc_vaapi / h264_vaapi encode.
- **qsv backports from upstream** (jellyfin patch 0018, new). For
  the h264_qsv branches Plex's argv occasionally takes.
- **svt-av1 v4 unbreak** (jellyfin patch 0094, new). For Plex AV1
  output via libsvtav1.
- **scale_opencl SAR fix** (jellyfin patch 0005). Aspect-ratio data
  flow on the OpenCL scaler — relevant for the OpenCL tonemap path.
- **opencl tonemap EETF refactor** (jellyfin patch 0006). Same path.
- **ffprobe first-vframe runaway / safe bail** (jellyfin patch 0079).
  Marginal for scaleplex (worker doesn't ffprobe; orchestrator/PMS
  does).

Validated on `plex-test` worker `sha-187cda0` against the full v1.2
feature set (PGS HW-decode pre-render + seek, SRT GPU-overlay burn,
HDR PQ source passthrough, AV1 → HEVC + sub burn, 4K → 4K, 4K → 1024).
Single-stream throughput burst realtime × 9.5–14, throttled steady
realtime × 2; no VAAPI surface-pool errors; no rewriter regressions.

### PGS / bitmap sub burn-in — HW-decode pre-render path + seek alignment

Bitmap (PGS / VobSub / DVDSub) burn-in on HW-decode sessions used to
drive `overlay_vaapi`'s framesync to drain the sparse `sub2video`
stream and re-run the SW upscale flat-out, collapsing the transcode
to ~0.25× realtime within 30–60 s. The fix routes the bitmap through
a separate sub pre-render (canvas + sub2video composited with SW
`overlay`, encoded as qtrle into a fragmented MOV streamed via FIFO),
so the main graph gets a clean CFR FIFO at `subPrerenderFPS` instead
of a sparse stream. The rewriter detects PMS's HW-decode bitmap-overlay
graph (regex `reFilterHWBitmapOverlay`), splices the FIFO `-i`, and
rewrites `overlay_vaapi` to position the band at `y=H-BandH` with
`eof_action=pass:repeatlast=1`. Live readings: **~0.3–0.5 core total**,
steady, no climb. (See `docs/REWRITER.md` "Bitmap sub burn-in
(HW-decode pre-render path)".)

Bottom-band crop (2/5 of frame height) added to halve canvas-size-
proportional CPU, matching the SRT path. Trade-off: clips top-positioned
signs / forced narrative. PGS is overwhelmingly bottom SDH/dialogue;
accepted as default. A `subPrerenderBandHeight(H)` knob exists for
future override.

- **Seek-alignment fix.** Plex's HW-decode bitmap argv carries
  `-start_at_zero -copyts`; that flag zeroes only the muxer-side output
  PTS, **not** the filter input (verified offline against Avatar Fire
  and Ash AV1 with `-ss 540`: the source's first frame arrives in
  `-filter_complex` at `pts_time:540.003`).
  The pre-render emits a 0-based FIFO (canvas-driven, sub branch
  rebased by `setpts=PTS-N/TB`), so `overlay_vaapi` paired FIFO PTS T
  with main PTS T — cues drifted forward by exactly `seekOff` seconds
  ("[Kiri sobs]" at seekbar 9:00 was the PGS cue from movie 18:00).
  The rewriter now splices `setpts=PTS+<seekOff>/TB` onto the FIFO
  branch ahead of `format=bgra,hwupload[0]`, putting both branches on
  the absolute-PTS timeline the filter already uses for the main video.
  An earlier attempt (`sha-a451466`) mirrored the SRT pre-render's
  seek handling by adding `setpts=PTS-STARTPTS` on both the main video
  branch AND the FIFO branch. The FIFO half was a no-op; the
  main-branch setpts caused constant forward-skipping in real playback.
  Reverted in `sha-3d3efb5`. The right place is `setpts=PTS+seekOff/TB`
  on the FIFO branch alone.

- **Validation.** Avatar Fire and Ash 4K HDR AV1 + PGS on LG webOS
  (initial play), Avatar PGS on Plex Android (initial + seek/resume),
  Sing 2 4K HDR AV1 + EAC3 on Plex Android (initial + seek + audio
  swap), and From Dusk Till Dawn HEVC 4K HDR + external SRT-burn on
  Plex Android (initial + seek + audio swap; SRT direct without burn
  also validated). All on the `plex-test` bench.

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
