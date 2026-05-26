# Changelog

## v1.6.1 — 2026-05-26

Bitmap sub-burn unification + PGS cue-clear regression fix + orthogonal SW-reshape detector.

> **The 4K HDR + burned PGS/bitmap sub + tonemap shape ran sub-realtime** (~0.37×
> realtime → buffering) in the full-HW passthrough path because scaleplex
> trusted Plex's `sub2video → scale-to-4K → overlay_vaapi` graph (+ a
> decode→sysmem→re-upload round-trip from a leading `[0:0]hwupload`). The
> existing reroute only matched the SDR shape; the HDR variant (tonemap_opencl
> spliced before overlay) escaped every optimizer. **Separately**, the burned
> PGS cue stayed on screen until the next cue (clear not sticky in the apply
> path). Both fixed.

- **feat(worker): unify bitmap sub-burn onto inlineass** (#37). All sub-burn now
  routes through `inlineass` — zero `overlay_vaapi` emitters left. Introduces
  an orthogonal-stage composer `composeBurn`:
  `[0:0] → [hwupload]? → scale_vaapi(p010|nv12) → [tonemap]? → [inlineass]?`,
  each stage independent. `detectBitmapOverlayBurn` extracts facts (stream
  spec, W/H, optional tonemap algo) regardless of an intervening tonemap, so
  the HDR variant no longer escapes. Live-validated on plex-test: 4K HDR PGS
  burn **0.37× → 4.6× realtime**, deployed agent emits VA-resident
  `scale_vaapi(p010)` → honored opencl tonemap → `inlineass(render_height)`
  band — no full-frame overlay, no decode→sysmem→re-upload round-trip. Drops
  the dead `reFilterHWBitmapOverlay` regex + unused `filterRewrite.Sidecar`
  field. No fork change.
- **fix(ffmpeg): PGS cue-clear sticky** (#38, patch 0121). `refresh_bitmap()`
  recomputed `have_bmp` every frame from the last bitmap's window. PGS cues
  carry `end_display_time=0` → `bmp_end_ms=-1`, so the empty-PCS clear (which
  drops `have_bmp` and returns) was undone the next frame: the per-frame
  recompute resurrected the cue. Fix: on clear, set `bmp_end_ms = time_ms` to
  close the window. Cue clears at its end instead of hanging until the next
  cue. (Regression lived in patch 0115 since v1.3.0 — affects all burned
  PGS/VobSub/DVDSub.) Also makes `build-worker.yaml`'s deb-fetch use the
  branch's own ffmpeg build artifact (`github.ref_name`, env-var + quoted) so
  fork patches build per-branch.
- **refactor(rewriter): orthogonal SW-reshape detector** (#39).
  `rewriteVideoFilter` is now a thin adapter over `extractGraphFacts →
  composeBurn`. The per-shape `reFilterAss/Plain/HDR/HDRAss` branches collapse
  — **4 of 6 regexes removed**; `reFilterHWAss/HWOpenCLAss` stay for the
  not-yet-swapped HW-decode-text path. Node-allow-list safety guard bails on
  unmodeled nodes (`crop`, `eq`, `fps`, …); SW-decode-only prefix guard keeps
  HW-shaped graphs falling through. Emit improvements: trailing redundant
  hwupload dropped (scale/tonemap output is already a VA surface for the
  encoder), SW-text moves from the `hwdownload/inlineass(SW)/hwupload` bracket
  to inlineass-on-VA + render_height (mirrors the HW-decode-text path), mode
  tags consolidated. Emit-parity harness vs the corpus: **considered=1369
  parity=1369 other-diff=0** — lossless. Corpus replay: no new bails.
  Live-validated SW-text on plex-test.

## v1.6.0 — 2026-05-25

GPU-resident OpenCL HDR tonemap fix (regression) + FORCE_HW=0 readiness.

> **Fixes algo-honoring HDR→SDR tonemap on the jellyfin-ffmpeg 7.x base.** On
> ffmpeg-7, inside a `-filter_complex` the `hwmap=derive_device=opencl` va→opencl
> derive fails ENOSYS ("hardware pixel format 'opencl' is not supported by the
> device type 'VAAPI'", exit 218) unless the OpenCL device is created explicitly,
> the input is a real VA surface (no leading `hwupload`), and there's no
> reverse-map→download round-trip — all of which ffmpeg-6 handled implicitly.
> This silently broke HDR-with-HW-tonemap (latent: only full-HW HDR→SDR sessions
> hit it; Plex masks it by retrying in software). NOT compiled-out and NOT the
> driver — a graph/device-wiring regression.

- **fix(worker): GPU-resident OpenCL tonemap** (#36). `gpuResidentOpenCLTonemap`
  post-pass on any emitted `tonemap_opencl` graph (VA-resident decode): inject
  `-init_hw_device opencl=ocl@vaapi` (after the vaapi device), force
  `-hwaccel_output_format:0 vaapi`, drop a leading `[0:0]hwupload`, collapse the
  `hwmap=vaapi:reverse=1→hwdownload→hwupload` round-trip into a direct
  opencl→sysmem `hwdownload`. Fully GPU-resident — no sysmem bounce, algo
  preserved. Live-validated across AV1/HEVC/H.264 × HDR/SDR × text/PGS/no-sub
  burn × seek × TrueHD/EAC3, on Plex-Web + Android.
- **feat(worker): FORCE_HW=0 readiness** (#35). EAE-swap on *every* bail (bailed
  sessions degrade to "plays" instead of exit-8 on `*_eae` audio);
  `force-hw:would-honor-{sw,hwdec-swenc}` counterfactual logging to quantify SW
  exposure before flipping `FORCE_HW` off; FORCE_HW=1 reshapes a HW-decode +
  SW-encode hybrid to full HW instead of bailing.

## v1.5.0 — 2026-05-25

Paced self-decode for `-map_inlineass`. The subtitle stream that feeds
`vf_inlineass` now decodes via a **sink-less decoder** created from the binding
— no output stream, no encoder, no muxer — paced by the single demux thread's
video-read backpressure. The Go rewriter drops Plex's
`-map <spec> -f null -codec ass|dvdsub nullfile` decode-sink (gated on
`-map_inlineass` still being present).

> **Fixes the embedded-sub startup-skip burst.** The old null-mux was an
> unthrottled extra reader/encoder/muxer (canThrottle sleeps only the video
> encoder) that pulled the demuxer through the file during the pre-throttle
> buffer fill, competing with the video read → brief playback skips on long
> embedded-sub 4K titles. Sidecar-sub sessions never burst; now neither do
> embedded ones.

- Fork patch `0120-inlineass-paced-self-decode`: `SchDec.sink_less` +
  `sch_dec_set_sinkless()`; `start_prepare` tolerates the unconnected output;
  `sch_dec_send` discards when `nb_dst==0`; `unchoke_downstream` guards the
  NULL dst; `ist_inlineass_add()` + `scaleplex_inlineass_setup_decoders()`
  (runs before `sch_start()`).
- Rewriter: `stripInlineassDecodeSink()`; bitmap path no longer appends a
  dvdsub sink.
- Live-validated on plex-test (4K HDR, play + seek): no startup skips, subs
  render + synced, no crash, single paced ffmpeg. See
  [`docs/PACED_SELF_DECODE.md`](docs/PACED_SELF_DECODE.md).

## v1.4.0 — 2026-05-24

Rewriter→fork migration + honor-Plex-HW/SW. Per-session argv-reshaping that
the Go rewriter did on every spawn moves into the scaleplex-ffmpeg fork, so
Plex's argv reaches the binary largely verbatim — shrinking the rewriter and
moving toward a drop-in transcoder. The rewriter also stops force-accelerating:
it now honors Plex's HW/SW decision by default, with a per-node override.

> **Behavior change.** Honor-Plex-HW/SW is now the **default**. Set
> `SCALEPLEX_FORCE_HW=1` (per-node worker env) to keep the v1.3.0
> always-re-accelerate behavior — the all-GPU homelab fleet sets this, so its
> behavior is unchanged (aside from the subtitle-styling fidelity below).
>
> **Env:** new `SCALEPLEX_RENDER_DEVICE`, `SCALEPLEX_FORCE_HW`; retired
> `HW_QP_CRF_OFFSET` and `SCALEPLEX_PRESET_MAP` (now encoder argv options
> `-crf_qp_offset` / `-compression_level`).

### Moved into the fork (scaleplex-ffmpeg patches 0116–0119)

- **0116 — device retarget.** `vaapi_device_create` reads
  `SCALEPLEX_RENDER_DEVICE` and binds that node regardless of the DRM path the
  caller baked into `-init_hw_device` (which is PMS-host-local and meaningless
  on a distributed worker). Empty path + no env → stock auto-detect.
- **0117 — `crf`.** The VAAPI encoder accepts libx264-style `-crf` and maps it
  to `QP = clamp(crf + crf_qp_offset, 0, 51)` (default offset 6) before
  rate-control selection; patch 0105 then routes QP + `-maxrate` to QVBR.
- **0118 — `preset` + opt stubs.** The VAAPI encoder accepts libx264 `-preset`
  names → `compression_level` (iHD TargetUsage; baked Arc bench map) and
  accepts-and-ignores `-x264opts` / `-x265-params`.
- **0119 — `inlineass` styling keys.** `vf_inlineass` implements Plex's
  `overrides` (→ `ass_set_style_overrides`, libass parses the ASS
  colours/bools), `outline`, `shadow`, and `language`. Plex's subtitle
  appearance (font/colour/border/shadow) is now honored instead of dropped.

### Rewriter

- No longer rewrites the device path, `crf→qp`, or `preset→compression_level`,
  and no longer strips the 4 Plex-only `inlineass` keys
  (`stripPlexInlineassFilterArgs` removed) — the fork handles all of these, so
  Plex's argv passes through verbatim on every path. Net deletion of the
  device-overwrite, blocks 6/7, the opt-blob drop, and the preset map/env
  helpers.
- **honor-Plex-HW/SW (per-axis).** Honors Plex's decode and encode choices
  independently unless `SCALEPLEX_FORCE_HW=1`:
  - `honor:plex-sw` — no `-hwaccel` + SW encoder → runs fully on CPU (no VAAPI
    device, no reshape). The CPU / non-GPU fallback.
  - `honor:plex-hwdec-swenc` — `-hwaccel` (HW decode) + SW encoder → keeps HW
    decode + device, SW-encodes on CPU. The CPU-offload mode: the heavy decode
    stays on the GPU (e.g. 4K AV1 HW-decode → 720p libx264 ≈ 7× realtime,
    vs ~0.5× for full SW). Previously this hybrid bailed.
  - Backend targeting (which GPU) is always node-local (0116); these flags are
    only the HW-vs-SW axis.

### Subtitle styling fidelity

Burn-in now reflects the user's Plex appearance settings (font/colour/border/
shadow) on **all** paths, including the HW path — a visible change even under
`FORCE_HW=1`. Previously the rewriter stripped those keys, so burn-in fell back
to a hardcoded white/DejaVu default (and a per-cue Arial→DejaVu fontselect).

### Known issues

- **HW-decode/SW-encode hybrid — 4K HDR corruption (under investigation).**
  `honor:plex-hwdec-swenc` runs Plex's verbatim SW filter chain
  (`tonemap=mobius` + libass) on HW-decoded frames; on 4K HDR AV1 this shows
  graphical corruption (and buffering on heavy output res). Only reachable
  with `FORCE_HW=0`. **Ship with `FORCE_HW=1`** (honor paths inactive); don't
  rely on the hybrid until this is root-caused.

Validated on plex-test (`FORCE_HW=0`): exit-8 on Plex sub-burn fixed, honor-SW
runs on CPU, the hybrid reaches realtime at sane output res, fork accepts
Plex's styling keys (in-pod rc=0). Device/crf/preset confirmed in-pod (incl.
override beating a bogus argv device path). The HW prod path is unchanged
(intermittent skips on a marginal 4K-HDR session did not reproduce on retest).

## v1.3.0 — 2026-05-24

Subtitle burn-in unification. The separate GPU-overlay pre-render path (a
second ffmpeg process writing a qtrle FIFO that the main graph composited with
`overlay_vaapi`) is gone. All HW subtitle burn-in now runs inside one
fork-native filter — `inlineass`, extended with a single-input VAAPI VPP
branch (merged from the `overlay_sub_vaapi` prototype). One Plex-native
`inlineass` node serves both the GPU fast path and the CPU fallback, chosen
from the negotiated input frame format.

### Merged `inlineass` filter (scaleplex-ffmpeg patch 0115)

- `vf_inlineass` gains a HW (VAAPI) branch: libass renders each cue once
  on-change to a premultiplied BGRA buffer (no vsfilter colour mangle),
  uploads it to a cached filter-owned VAAPI surface, then VPP-blends it onto
  the video. Single input → no framesync → no AV1 decoder surface-pool
  overrun (the "error-18" lineage that forced the 2-process split).
- Text (SRT/ASS), PGS/DVD bitmap (via `replay_bitmap`), animated ASS
  (`animated_tier_down` toggle, one resolution tier below `render_height`),
  native seek (real PTS) and the `render_height` raster cap (folds in
  `SCALEPLEX_SUB_RENDER_HEIGHT`) are all handled in-filter.
- The SW branch is the original FFDraw per-frame blend, unchanged — the
  non-GPU / CPU-fallback path. The filter picks HW vs SW from the negotiated
  input frame format.
- Prototype patches 0112/0113 (standalone `overlay_sub_vaapi`) dropped; the
  fftools `-map_inlineass` binding gains a bitmap route (no `is_osv` dispatch).

### Rewriter

- HW-decode sub burn now emits `inlineass=…:render_height=N[:animated_tier_down=1]`
  straight on the VAAPI surface — the `hwdownload → inlineass(SW) → hwupload`
  bracket is stripped. No more `SubPrerenderSpec` / FIFO splice / `__SP_BAND*`
  sentinels / setpts-seek dance. PGS: the rewriter now adds `-map_inlineass`
  + a `dvdsub` decode-sink and drops Plex's `overlay_vaapi` sub2video graph.
- SW-decode path unchanged (CPU `inlineass` fallback).
- Change tags: `hw-decode:filter:inlineass-vaapi` (text),
  `hw-decode:filter:bitmap-inlineass-vaapi` (PGS). Net −427 lines.

### Fixes

- `probeSubtitleCodec` passed the `-map_inlineass` `input:stream` value (e.g.
  `0:3`) to ffprobe `-select_streams`, which wants the in-file specifier (`3`)
  → the probe always failed (`Invalid stream specifier`) → every sub defaulted
  to text, so bitmap detection and `animated_tier_down` never fired. Strip the
  leading input index.

Validated on plex-test at 4K: HW SRT, HW PGS, HW animated ASS, SW fallback —
all render correctly with seek, no crashes/restarts. ~0.13 core (4K SRT) /
~0.44 core (4K PGS) per session.

## v1.2.2 — 2026-05-22

Two sub-burn efficiency features. Measured impact (flat-out, 4K HEVC HDR, one
worker = 4 vCPU + Arc A310): sub-burn capacity ~5 concurrent streams, within
1 of the no-sub ceiling of 6 — the bottleneck is the GPU video (codec) engine
(HEVC decode+encode), not the subtitle pipeline. SRT (sidecar+embedded) and
PGS all reach 5; static ASS still renders full-frame (tracked in KNOWN_ISSUES).

### Agent-side SRT band resolve — embedded SRT now gets the tight band

v1.2.1 parsed sidecar SRT cues at rewrite time to size a tight pre-render
band, but embedded SRT had no file on disk yet (the agent extracts it
after rewrite) so it fell back to the static 40% band. This unifies both
paths by **deferring band resolution to the agent**, post-extraction.

- The rewriter seeds the static-fallback band, flags
  `ResolveBandPostExtract`, and leaves sentinels in the main argv where
  the band-dependent values belong: `overlay_vaapi y=__SP_BANDY__` and
  (under a render-res cap) `scale_vaapi h=__SP_BANDH__`.
- The agent runs `ResolveAgentBand` once the SRT is on disk (sidecar path
  or extracted `.srt`), overwrites the band, builds the pre-render from
  the final band, then `PatchMainArgsBand` substitutes both sentinels.
- Rewriter change tag `sub-prerender:band:tight` → `sub-prerender:band:agent-resolve`
  (the resolved band height is logged by the agent at resolve time).
- Embedded SRT now gets the tight-band CPU savings; sidecar behaviour is
  unchanged (same parse, same band, one extra hop). ASS / bitmap untouched.
- New `worker/agent/band.go`; composes with the render-res knob (the
  `h=` upscale target is patched alongside the `y=` offset). Salvaged from
  the abandoned v1.2.2 stack (was PR #28), rebuilt on current main.

### Sub pre-render render-resolution knob (`SCALEPLEX_SUB_RENDER_HEIGHT`)

The text sub-burn pre-render previously rasterised libass at the full output
resolution (the band crop only shrank the downstream qtrle encode + main
hwupload, not libass itself). A controlled breakdown (media-toolkit, libass
0.17.3, 707-cue Dutch SRT) showed libass ≈ 52 % and format+qtrle ≈ 48 % of
the pre-render, **both scaling ~linearly with rendered pixel area** — the 4K
full-frame libass render is the dominant remaining sub-burn cost.

New worker env knob `SCALEPLEX_SUB_RENDER_HEIGHT` caps the libass render
height. The pre-render rasterises the band at `renderW × renderH` and the
main graph HW-upscales it (`scale_vaapi`) back to the output band before
`overlay_vaapi`. Default `1080`: at 4K it renders 1920×1080 and upscales 2×,
cutting pre-render CPU ~4.25× (measured 4K vs 1080 canvas A/B) plus a smaller
FIFO frame ⇒ less main-side hwupload. Outputs ≤ the cap are untouched.

- Tiers `720`/`1080`/`1440`; `0` opts out to a native full-res render.
- Validated live on plex-test (From Dusk Till Dawn, 4K HEVC HDR + Dutch SRT):
  pre-render renders 1920×1080, main graph `scale_vaapi=w=3840:h=540`, no
  stall; A/B vs native render shows only marginal glyph softening at 1080.
- `scale_vaapi` on a BGRA overlay surface verified on Arc/iHD (the earlier
  PGS no-op was specific to sub2video bitmap surfaces).
- Emits a `sub-prerender:render=WxH` rewriter change tag when active.

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
