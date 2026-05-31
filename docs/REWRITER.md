# Argv rewriter

`worker/agent/rewriter.go` translates a Plex SW transcode invocation into
a stock-ffmpeg VAAPI invocation that produces the same output bytes
under the same names in the same directory.

> **Orthogonal detector — all paths.** Every reshape now runs through one
> fact-extractor + one composer:
> `extractGraphFacts(graph) → {w/h, hdr+algo, subKind/params/spec}` → `composeBurn` emits the canonical
> `[0:0] → [hwupload]? → scale_vaapi(p010|nv12) → [tonemap]? → [inlineass]?`
> graph. The per-shape `reFilterAss / reFilterPlain / reFilterHDR / reFilterHDRAss`
> SW branches collapsed in v1.6.1 (#37/#39); the HW-decode-text branch
> (`reFilterHWAss` / `reFilterHWOpenCLAss`) collapsed next — **6 of 6 reFilter\*
> regexes removed**. A node-allow-list guard bails on any unmodeled node
> (`crop`, `eq`, `fps`, …) — preserving the strict reFilter\* fall-through.
> Bitmap sub-burn (PGS / DVD / DVB) also routes through the unified
> `inlineass` filter via `detectBitmapOverlayBurn` — no `overlay_vaapi`
> anywhere. `animated_tier_down` is a composeBurn axis now, so the
> SW-reshape path picks up the animated-cue tier-down knob the HW-decode-text
> path always had. See CHANGELOG v1.6.1 / #37 / #39 / #40.

The translator is **conservative** — it bails (returns `applied=false`,
caller spawns ffmpeg with the original argv unchanged) on anything it
doesn't recognise. This means scaleplex degrades to running Plex's own
transcoder argv on stock ffmpeg, which mostly fails — but fails
visibly, with ffmpeg's own error messages, instead of silently
producing wrong output.

The change list returned in `RewriteResult.Changes` is the primary
diagnostic surface. Every transformation appends a label there, and the
agent logs the comma-joined labels at task start:

```
session Balls_Up...c11a7e73: rewriter applied:
  decode:libdav1d->av1, inject:init_hw_device+filter_hw_device,
  filter:plain, map-label-update, encode:libx264->h264_vaapi,
  inject:sei+a53_cc, audio:eac3_eae->eac3, drop:-eae_prefix:1,
  hls:segment_list:rewrite-to-relay, seek-offset:captured=888.000s,
  progress:append-X-Plex-Token,
  progressurl:captured-for-reporter,
  inject:-canthrottleurl(scaleplex-ffmpeg7-canThrottle),
  loglevel:->info, drop:-nostats,
  env:strip:EAE_ROOT, env:strip:FFMPEG_EXTERNAL_LIBS, env:LIBVA
```

Each label below maps to one of the transformations documented here.

scaleplex-ffmpeg7 patches `0094`–`0107` absorb the rewrites that
earlier versions of the worker did at argv-time. As Plex-fork
behaviour moved into the fork, the matching rewriter "bandaids" were
retired — by v1.0 the rewriter only strips Plex-private flags the
fork still can't parse, does intentional integration (relay routing,
auth tokens), or swaps SW→HW codecs. The rewriter no longer emits
these tags (ffmpeg accepts the originals natively):

- `drop:-loglevel_plex` — patch 0098 registers the option as an
  OPT_TYPE_STRING sink.
- `drop:-strict_ts` — patch 0107 registers `-strict_ts` as an
  OPT_TYPE_STRING sink.
- `hls:f=ssegment->segment` — patch 0098 adds `AVFMT_GLOBALHEADER`
  to `ff_stream_segment_muxer`; Plex's `-f ssegment` shape works
  without translation.
- `hls:segment_list_size:5->99999` — patch 0106 force-buffers the
  full chunk history on URL-handler segment lists; `list_size` is
  inert there.
- `hls:drop:-segment_list_unfinished` / `-segment_list_separate_stream_times`
  — patch 0096 registers both as `AVOptions` (functional
  separate-stream-times CSV emit landed in the 0096 v2 rev).
- the CQP→VBR rate-control rewrite — patch 0105: `vaapi_encode`
  auto-selects QVBR when `-qp` + `-maxrate` are both set.
- `force_key_frames:offset-by-seek` — dead-code retired; jellyfin
  7.1's `hevc_vaapi` handles the IDR expression natively.
- `inject:-canthrottleurl` is NEW (from patch 0097) — rewriter
  injects a relay-URL for ffmpeg's in-binary canThrottle handler.

`inject:sei+a53_cc` is kept deliberately — not a bandaid; see the
encoder section below.

## Codec swaps

### `libdav1d` → `av1` (decoder)

Plex selects `libdav1d` as the AV1 software decoder. We swap to ffmpeg's
default AV1 decoder name and add VAAPI hwaccel below it. **Label:**
`decode:libdav1d->av1`.

### Bare short codec name without `-hwaccel:0` (decoder upgrade)

PMS sometimes stages a SW-shaped argv (libx264/libx265 encoder, software
`scale` filter) but emits the canonical short codec name (`hevc`, `h264`,
`av1`, `vp9`) in `-codec:0` instead of a `libfoo` SW decoder. Without
`-hwaccel:0`, ffmpeg decodes in software and the SW encoder runs — no
hardware acceleration. The rewriter detects this shape and injects the
hwaccel flags so the rest of the SW-upgrade tail runs (encoder swap,
filter chain rewrite). Only fires when the post-`-i` encoder is a known
SW encoder (`libx264`/`libx265`); a HW encoder there means the argv is
malformed in a way we can't safely reshape, and we bail with
`skip:unknown-decoder:<codec>`. **Label:** `decode:bare-hw-upgrade:hevc`
(or `h264` / `av1` / `vp9`).

### `libx264` → `h264_vaapi` (encoder)

The whole point. Plex passes its libx264 invocation; we replace with
the VAAPI HW encoder.

Side effects:
- `-crf:0 N` is left untouched. The fork's VAAPI encoder accepts
  libx264-style `-crf` and maps it to `QP = crf + crf_qp_offset`
  (default 6) before rate-control selection (patch `0117`); patch `0105`
  then routes the QP + `-maxrate` to QVBR. No rewriter label.
- `-preset:0 <name>` is left untouched. The fork's VAAPI encoder accepts
  libx264 preset names and maps them to `compression_level` (iHD
  TargetUsage 1=quality..7=fastest) at init when `compression_level`
  wasn't set directly (patch `0118`). The baked map (`ultrafast/superfast/
  veryfast → 7/6/6`, `faster/fast → 5/4`, `medium/slow → 4/3`,
  `slower/veryslow → 2/1`) comes from on-cluster bench (3× Arc A310,
  2026-05-05). The `SCALEPLEX_PRESET_MAP` env knob is retired; override by
  passing `-compression_level` directly. No rewriter label.
- `-x264opts:0` / `-x265-params:0` are left untouched — the fork's VAAPI
  encoder accepts and ignores them (patch `0118`), so Plex's libx264/x265
  argv reaches the encoder without error. No rewriter label.
- `-sei:0 -a53_cc` is injected to match Plex prod's convention. PMS
  emits `-sei:0 -a53_cc` on HEVC/libx265 sessions but omits it on
  libx264 ones; in `AV_OPT_TYPE_FLAGS` syntax `-a53_cc` *removes* the
  a53_cc flag, and jellyfin-ffmpeg's `vaapi_encode` defaults it ON.
  Injecting it on the libx264-originated sessions keeps every
  HW-encode session consistent with Plex's behaviour (no a53_cc SEI;
  Plex drives captions through its own pipeline). Kept deliberately —
  an intentional integration, not a bandaid. **Label:** `inject:sei+a53_cc`.

### `*_eae` → stock encoder (audio encoder)

Plex's `*_eae` codec names invoke the EasyAudioEncoder sidecar (a
separate process). EAE is licensed and its temp dir is tied to PMS's
per-restart UUID — see
[LESSONS-FROM-CLUSTERPLEX.md#5](LESSONS-FROM-CLUSTERPLEX.md). The
rewriter strips the `_eae` suffix and substitutes the stock libavcodec
equivalent:

| Plex codec    | Replacement | Notes                                                               |
| ------------- | ----------- | ------------------------------------------------------------------- |
| `eac3_eae`    | `eac3`      | Stock encoder is clean.                                             |
| `ac3_eae`     | `ac3`       | Stock encoder is clean.                                             |
| `truehd_eae`  | `eac3`      | Stock `truehd` encoder is flagged experimental and runs sub-realtime; client loses bitstream passthrough but keeps audio. |
| `<other>_eae` | `eac3`      | Forward-compatible default for codecs the family table doesn't list yet. |

The matcher accepts any `-codec:N` or `-c:a:N` flag for any
non-negative N — the audio track index reflects which audio stream the
client picked, not a fixed slot. Hardcoding `:0`/`:1` only crashes the
moment a multi-audio source is touched (validated 2026-05-10:
`-codec:2 eac3_eae` from Plex Android audio-track switch on a multi-
language anime episode). The drop-suffix logic also strips
`-eae_prefix:N` for the same N.

**Labels:** `audio:eac3_eae->eac3`, `audio:truehd_eae->eac3`,
`drop:-eae_prefix:1` (etc.).

## Filter chain rewrites

### Plain SW filter → VAAPI filter

Plex's typical filter shape:

```
[0:0]scale=w=1022:h=426:force_divisible_by=4[0];
[0]format=pix_fmts=yuv420p|nv12[1]
```

becomes (rewriter mode `plain`):

```
[0:0]scale=w=1022:h=426:force_divisible_by=4[0];
[0]format=pix_fmts=yuv420p|nv12,hwupload[1]
```

Plus, when Plex emitted no `-init_hw_device`, we inject a bare
`-init_hw_device "vaapi=vaapi:"` and `-filter_hw_device vaapi` ahead of the
input. We no longer bake a device path here: the scaleplex-ffmpeg fork
retargets the VAAPI device from `SCALEPLEX_RENDER_DEVICE` at device-open
(patch `0116-vaapi-device-env-retarget`), and the driver comes from
`LIBVA_DRIVER_NAME` (injected into the subprocess env). When Plex *did*
emit `-init_hw_device`, we leave it untouched — the fork overrides the
path regardless. **Labels:** `filter:plain`,
`inject:init_hw_device+filter_hw_device`, `map-label-update`.

The map-label update is needed because we add `hwupload` inside the
existing chain, which can shift `[N]` labels — the rewriter walks the
graph, increments labels mentioned later in the argv (`-map "[1]"` etc.).

### Sub burn-in (merged `inlineass` filter, v1.3.0)

All subtitle burn-in runs through the fork's `inlineass` filter, which has
two format-adaptive branches sharing one libass track (fed via the
`-map_inlineass` side-channel, patch 0100). The branch is chosen from the
**negotiated input frame format**, not from the argv:

- **VAAPI surface in → HW branch** (single-input VAAPI VPP): libass renders
  each cue once on-change to a premultiplied BGRA buffer, uploads it to a
  cached filter-owned VAAPI surface, then VPP-blends it onto the video. One
  input → no framesync → no AV1 decoder surface-pool overrun. Text, PGS/DVD
  bitmap, animated ASS, seek and the `render_height` raster cap are all
  handled in-filter (patch 0115).
- **CPU frame in → SW branch**: the original per-frame libass + FFDraw blend.
  The non-GPU / CPU-fallback path.

> **History.** Through v1.2.x, SRT/static-ASS/PGS burn ran a *second* ffmpeg
> (the GPU-overlay pre-render) writing a qtrle FIFO that the main graph
> composited with `overlay_vaapi`, and only animated ASS used `inlineass`.
> v1.3.0 collapsed all of that into the merged filter — `SubPrerenderSpec`,
> the FIFO splice, the `__SP_BAND*` sentinels and the setpts-seek dance are
> gone. See `CHANGELOG.md` (v1.3.0) and `docs/UNIFIED_SUB_FILTER.md`.

#### HW-decode text (SRT / ASS)

Plex sends `[0:0]hwupload[0];[0]scale_vaapi=W:H[1];[1]hwdownload,format=nv12[2];[2]inlineass=…[3];[3]hwupload[4]`
(SDR; the HDR variant splices a `hwmap=opencl → tonemap_opencl → hwdownload`
chain in place of `scale_vaapi`'s straight `format=nv12`). Both shapes go
through `extractGraphFacts + composeBurn(burnSpec{vaResident:true, …})` —
the same orthogonal core the SW-reshape and HW-decode-bitmap branches use.
Plex's redundant leading `[0:0]hwupload[0]` is dropped (the source is already
a VA surface), the `hwdownload → inlineass → hwupload` bracket is absent by
construction, and the OpenCL detour collapses into the canonical
`scale_vaapi(p010) → tonemap_stage → inlineass` shape:

```
[0:0]scale_vaapi=w=W:h=H:format=nv12[0];   # (+ tonemap stage if Plex sent one — p010 then)
[0]inlineass=<plex-params>:render_height=N[:animated_tier_down=1][1]
```

- `render_height=N` is `SCALEPLEX_SUB_RENDER_HEIGHT` (default 1080) as a filter
  option — libass rasterises at the cap and the VPP blend upscales.
- `animated_tier_down=1` is added when `subtitleIsAnimated()` is true (ASS with
  `\move`/`\t`/`\k`/`\fad`); the filter then renders animated cues one
  resolution tier below `render_height`. Static cues are unaffected. Same knob
  applies on the SW-reshape path (`composeBurn`'s `animatedTierDown` axis).
- Plex's `-map_inlineass <spec>` is **kept** — it drives the libass feed.
- Plex's `-map <spec> -f null -codec ass` decode-sink is **conditionally
  stripped** by `stripInlineassDecodeSink`, gated on (a) `-map_inlineass`
  still being present AND (b) the binding's `file_idx == 0` (embedded sub
  stream — shares the main demuxer with video). As of fork patch `0120`
  (v1.5.0) the `-map_inlineass` binding self-decodes via a sink-less
  decoder paced by the demux; for embedded subs the shared demuxer is
  pumped by main video, so the null-mux is redundant and competes with the
  NFS read during the pre-throttle buffer fill (the original reason to
  strip it). For **sidecar bindings** (`file_idx >= 1`, e.g.
  `-map_inlineass 1:s:0` pointing at a PMS-staged `temp-0.srt`) the
  sidecar has its own demuxer thread with no other downstream consumer; the
  scheduler chokes after the first packet and the binding sees zero cues.
  There the decode-sink is **kept** so the sidecar demuxer has a real
  consumer (patch `0122` makes the dual-consumer state safe for
  `dst_finished` allocation). See `docs/PACED_SELF_DECODE.md`.
- Plex's full `inlineass` node is passed through **verbatim** — including
  the styling keys `language`/`overrides`/`outline`/`shadow`. As of patch
  `0119` the fork's `inlineass` parses them (`overrides` →
  `ass_set_style_overrides`, libass parses the ASS colours/bools), so the
  user's subtitle appearance (font/colour/border/shadow) is preserved.
  Earlier builds stripped them (`stripPlexInlineassFilterArgs`, removed in
  0119), which lost the styling and crashed any verbatim path.
- OpenCL-tonemap variant: the tonemap algorithm is preserved (via
  `tm.stage(facts.algo)`) and PMS's `-map [6]` is retargeted via
  `retargetMapLabel` (composeBurn's output label is `[1]` for HDR text
  here — `[0]` is the scale stage).
- `-hwaccel_output_format:0` is forced to `vaapi` defensively (parity with
  the HW-decode-bitmap branch) so the composer's no-leading-hwupload
  assumption holds even on argv shapes that omitted it.

**Labels:** `hw-decode:filter:inlineass-vaapi` (SDR),
`hw-decode:filter:opencl-tonemap->vaapi:inlineass-vaapi` +
`hw-decode-sub:tonemap-preserved(<algo>)` (HDR/OpenCL).

#### HW-decode bitmap (PGS / DVD / DVB)

Plex burns bitmap subs with a `sub2video` bridge + `overlay_vaapi` and emits
**no** `-map_inlineass` (`reFilterHWBitmapOverlay`). The rewriter replaces
that graph with the merged filter and feeds the bitmap through the same
side-channel:

```
[0:0]hwupload[0];
[0]scale_vaapi=w=W:h=H:format=nv12[1];
[1]inlineass=render_height=N[4]
```

- **Adds `-map_inlineass <spec>`** (before `-filter_complex`) so the fftools
  binding routes the decoded bitmap presentation to the filter's
  `replay_bitmap` (it blits palettised rects to a cached VAAPI surface).
- **No decode-sink.** Adds `-map_inlineass <spec>` (Plex emits none for PGS);
  fork patch `0120` (v1.5.0) self-decodes that binding, so the rewriter no
  longer appends a `-map <spec> -f null -codec dvdsub nullfile` sink to trigger
  the bitmap decode.
- Drops Plex's `overlay_vaapi` sub2video graph. Seek is native (real PTS).

**Label:** `hw-decode:filter:bitmap-inlineass-vaapi`.

#### SW-decode (force-HW reshape)

When Plex sends a software-decode shape (HW acceleration disabled, force-HW
on the worker), `rewriteVideoFilter` runs `extractGraphFacts(graph, subSrc)`
to lift the orthogonal facts and `composeBurn(burnSpec{vaResident:false, …})`
emits the canonical shape — `inlineass` on the VA surface itself (the same
VAAPI branch the HW-decode-text path has exercised in prod since v1.3.0), no
`hwdownload`/`hwupload` bracket, with the fork's `render_height` band:

```
[0:0]hwupload[0];[0]scale_vaapi=W:H:format=nv12[1];[1]inlineass=<params>:render_height=N[2]
```

For an HDR source, scale_vaapi emits p010 and the tonemap stage sits between
scale and inlineass — Plex's honored algo extracted from the input's
`tonemap_opencl=tonemap=X` or bare `tonemap=X`. **Labels:**
`filter:text-inlineass-vaapi` (text), `filter:bitmap-inlineass-vaapi(:hdr-tonemap(<algo>))`
(bitmap), `filter:hdr-tonemap-vaapi` (no-sub HDR), `filter:plain` (no-sub SDR).

### HDR tonemap (HDR source → SDR output)

**scaleplex never decides whether to tone-map — Plex does.** Plex's
tonemap policy depends on **both** the "Use hardware-accelerated tone
mapping" pref (`TranscoderToneMapping`) AND whether HW codecs are
available for the session:

- **HW tonemap on + HW codecs on** → argv carries a `tonemap_opencl`
  chain (`hwmap=opencl → tonemap_opencl=tonemap=<algo> → hwmap=vaapi:reverse=1`).
- **HW tonemap on + HW codecs off** (SW fallback) → argv carries a
  **SW tonemap** node as part of the SW pipeline (`[0]format=p010,
  tonemap=<algo>[N];[N]format=pix_fmts=yuv420p|nv12[N]`). Algo tracks
  `TranscoderTonemapAlgorithm` (defaults to `mobius`). Confirmed by
  the 2026-05-26 Optimize-corpus sweep (~3% of cells).
- **HW tonemap off** (any HW state) → plain SDR-target chain, no
  tonemap node. Output is washed/dim — Plex's own behavior. scaleplex
  matches it and injects nothing.

So the argv is authoritative: scaleplex honors a tonemap filter when
Plex sent one, and injects nothing when Plex didn't.

When Plex's argv carries the OpenCL chain, `substituteOpenCLTonemap`
re-emits it in canonical comma form, keeping Plex's chosen algorithm:

```
[X]hwmap=derive_device=opencl,
   tonemap_opencl=tonemap=<algo>:transfer=bt709:matrix=bt709:primaries=bt709:format=nv12,
   hwmap=derive_device=vaapi:reverse=1[C]
```

On the jellyfin-ffmpeg 7.x base `hwmap` no longer self-derives the OpenCL
device inside a `-filter_complex` (it did on ffmpeg-6), so
`gpuResidentOpenCLTonemap` post-fixes the emitted graph: injects
`-init_hw_device opencl=ocl@<vaapi-device>`, forces VA-resident decode, drops
a leading `[0:0]hwupload`, and collapses any reverse-map→download round-trip.
`SCALEPLEX_TONEMAP=vaapi` instead collapses the chain to iHD's fixed-curve
`tonemap_vaapi` (BT.2390 EETF, no per-algorithm tuning) — an OpenCL-trouble
fallback. SW-shaped HDR tonemap chains (Plex's bare `tonemap=X` or
`zscale…tonemap`) are reshaped the same way: `extractGraphFacts` captures the
algo from whichever tonemap node Plex emitted, and `composeBurn` re-emits
through `tm.stage(algo)`.

> The worker **must** strip `OCL_ICD_VENDORS` from the spawn env (it
> does — see `stripEAEEnvVars`). PMS sets `OCL_ICD_VENDORS=0` to disable
> OpenCL ICD discovery in its own ffmpeg; inherited by a worker ffmpeg
> it makes the OpenCL loader find zero platforms (`clGetPlatformIDs` →
> `-1001`) and the whole tonemap_opencl transcode fails.

> The VAAPI↔Vulkan `libplacebo` route (richer tunables) is **not** used:
> Intel's ANV Vulkan driver reports `sync import caps: 0x0`, so the
> zero-copy VAAPI→Vulkan interop can't synchronize.

### Subtitle bail conditions

The translator returns `applied=false` and the agent runs original argv
when:

- Filter graph contains a `subtitles=...` already (Plex's own SW path)
- Decoder is unrecognised
- Filter graph carries an unmodeled node — `extractGraphFacts`'s allow-list
  guard bails on anything outside `scale/scale_vaapi/hwupload/hwdownload/hwmap/
  format/setparams/tonemap/tonemap_opencl/tonemap_vaapi/zscale/inlineass/
  overlay_vaapi`. The composed modes (the `filter:<mode>` change tag):
  - `plain` — straight transcode, no subs, no tonemap
  - `hdr-tonemap-vaapi` — no-sub HDR
  - `text-inlineass-vaapi` — text sub burn-in (SDR or HDR), inlineass on the VA surface
  - `bitmap-inlineass-vaapi` (`:hdr-tonemap(<algo>)` suffix when HDR) — bitmap sub burn-in via the fork's `replay_bitmap` binding
  - HW-decode-text uses the `hw-decode:filter:inlineass-vaapi` tag
    (`opencl-tonemap->vaapi:` prefix on the HDR/OpenCL variant) — same
    composeBurn(vaResident=true) shape as the SW path, just labelled with
    the `hw-decode:` prefix so log readers can tell from one grep whether
    Plex's argv arrived HW-shaped or SW-shaped.

These are diagnostic dead-ends rather than failures: stock ffmpeg with
the original Plex argv will fail, but the failure surface is ffmpeg's
own error rather than scaleplex producing bad output.

## URL rewrites

ffmpeg has three flags that POST/PUT to a callback URL during the run.
Plex hardcodes them at `http://127.0.0.1:32400/...` (PMS's loopback,
unreachable from worker pods). The rewriter rewrites these to the
relay's URL on the PMS pod:

| Flag | Plex sets | Rewritten to |
|---|---|---|
| `-progressurl` | `http://127.0.0.1:32400/.../progress` | `${SCALEPLEX_PMS_BASE_URL}/.../progress?X-Plex-Token=${X_PLEX_TOKEN}` |
| `-manifest_name` | `http://127.0.0.1:32400/.../manifest?...` | `${SCALEPLEX_PMS_BASE_URL}/.../manifest?...&X-Plex-Token=...` — rewritten in-place; ffmpeg's dashenc PUTs the `.mpd` body itself (patch 0095 backports Plex's `-manifest_name` extension) |
| `-segment_list` | `http://127.0.0.1:32400/.../manifest?...` | `${SCALEPLEX_PMS_BASE_URL}/.../manifest?...&X-Plex-Token=...&scaleplex_seg_time=<N>` |

`SCALEPLEX_PMS_BASE_URL` is set in the worker DaemonSet env, e.g.
`http://<pms-service>.<namespace>.svc:32499`. The relay sidecar
listens on 32499 and forwards to 32400.

**Labels:** `progress:append-X-Plex-Token`, `progressurl:captured-for-reporter`,
`manifest_name:rewrite-to-relay`, `hls:segment_list:rewrite-to-relay`.

ffmpeg's `-progress` doesn't attach Authorization headers, so the token
goes in the query string — the relay just forwards it through.

## DASH-specific rewrites

The rewriter detects DASH via `outputFormat == "dash"`.

Most of the DASH handling moved into scaleplex-ffmpeg7 — the worker
no longer post-processes chunks or publishes the manifest itself:

- `-delete_removed false` passes through. Patch 0095 backports Plex's
  dashenc `-delete_removed` extension; chunks survive past the rolling
  window so PMS's async NFS-readdir serve can't race a cleanup. (This
  retired the old `-extra_window_size 999999` injection.)
- CMAF-strict `movflags` are applied by dashenc itself — patch 0104
  defaults `+empty_moov+default_base_moof+separate_moof+cmaf` on
  URL-handler output, so each chunk is `styp+sidx+moof+mdat` (MSE
  rejects the self-contained-mp4 default as duplicate-init). The
  rewriter no longer injects `-format_options`.
- `-skip_to_segment N` passes through. Patch 0095 starts dashenc's
  segment_index at N natively, so `chunk-stream0-NNNNN.m4s` aligns
  with PMS's `.mpd` `startNumber=`. Diagnostic tag only
  (`skip_to_segment:passthrough=N`).
- `-manifest_name <url>` is rewritten in-place to the relay URL;
  dashenc PUTs the `.mpd` body itself (patch 0095). The old
  worker-side `manifest_publish.go` and the `patchSeekChunkTimestamps`
  tfdt/sidx post-processor were removed once the fork handled seek
  timestamps natively (patch 0103 dropped jellyfin's
  `reference_stream_first_pts` adjust). See [SEEK.md](SEEK.md) for the
  history.
- `-ss <off>` is still captured into `RewriteResult.SeekOffsetSeconds`
  for the orchestrator checkpoint/recovery path.

PTS handling on DASH **stays as Plex sends it** (`-copyts -start_at_zero
-avoid_negative_ts disabled`). Removing `-start_at_zero` blanked the AAC
encoder's primer samples and produced 199-byte empty audio segments
after every seek; the bug only became visible when DASH players hung
on initial-audio-buffer-fill while video chunks decoded fine.

## HLS-specific rewrites

Detected via `outputFormat == "ssegment"`. Plex's argv:

```
-f ssegment -segment_format matroska -individual_header_trailer 0
-segment_header_filename header -segment_time 8 -segment_start_number N
-segment_time_delta 0.0625 -segment_list <URL> -segment_list_type csv
-segment_list_size 5 -segment_list_separate_stream_times 1
-segment_list_unfinished 1 -segment_format_options output_ts_offset=10
-max_delay 5000000 -avoid_negative_ts disabled "media-%05d.ts"
```

Rewriter changes:

- `-f ssegment` passes through (patch 0098 adds `AVFMT_GLOBALHEADER`
  to `ff_stream_segment_muxer`; behaviour identical to `-f segment`
  for Plex's `-flags +global_header` / `-segment_header_filename`
  combination).
- `-segment_format matroska` and `-segment_header_filename header` are
  **kept**. Plex uses mkv-in-.ts when the codec/audio combo can't fit
  mpegts (4K HDR + 5.1 EAC3, Atmos passthrough, etc.). Stock segment
  muxer supports this — verified locally.
- `-segment_list_separate_stream_times` / `-segment_list_unfinished`
  pass through (patch 0096 no-op stubs).
- `-segment_list_size` passes through. scaleplex-ffmpeg patch 0106
  detects URL-handler outputs (`seg->list` contains `://`) and force-
  buffers the full chunk history regardless of `list_size`. Plex's
  bake-in of `5` becomes inert for URL outputs. Retired rewriter bump
  2026-05-14.
- `-copyts` is **kept** (matroska + ssegment, seek and initial-play
  alike). Stock jellyfin 7.x's segment muxer used to produce zero
  chunks on `-ss + -copyts` because jellyfin added a
  `reference_stream_first_pts` adjustment that shifted the split
  boundary past the live encode window. Patch 0103 drops that adjust,
  restoring Plex-fork split semantics — so the rewriter no longer
  strips `-copyts` at all. (The old env-gated legacy strip and the
  `hls:drop:-copyts(seek)` tag were removed in the v1.0 cleanup.)
- Rewrite `-segment_list <PMS-loopback>` to `<relay>?...&scaleplex_seg_time=<N>`.
  **Label:** `hls:segment_list:rewrite-to-relay`.

## `-force_key_frames` rewrite (seek path) — RETIRED 2026-05-14

Historical workaround for an IDR-storm on pre-fork ffmpeg 3.4. Tested
2026-05-13: jellyfin-ffmpeg 7.1's `hevc_vaapi` handles
`expr:gte(t,n_forced*N)` cleanly even with `-copyts -ss <large>`. The
rewriter retains `seekOffsetSeconds` capture for the orchestrator
checkpoint but no longer rewrites the expression. **Label retired:**
`force_key_frames:offset-by-seek`.

## Other tweaks

- `-loglevel quiet` / `-loglevel panic|fatal` → value of env
  `SCALEPLEX_FFMPEG_LOGLEVEL` (default `info`). We need ffmpeg's
  stream-mapping lines for the agent's progress reporter; bump to
  `debug` on the worker DaemonSet env to expose per-cycle
  `scaleplex/ct: PUT/avio_read/body` diagnostics from patch 0097
  without rebuilding ffmpeg. **Label:** `loglevel:->info` (or the
  configured level).
- `-loglevel_plex` passes through. scaleplex-ffmpeg7 patch 0098
  registers it as an OPT_TYPE_STRING sink so stock ffmpeg accepts
  + discards.
- `-nostats` dropped — PMS doesn't actually need it, and stripping it
  gives us periodic stderr-progress lines for the reporter.
  **Label:** `drop:-nostats`.

## Environment scrubbing

Plex passes some env vars that don't apply to a stock-ffmpeg run:

- `EAE_ROOT` (EAE state dir) — stripped. **Label:** `env:strip:EAE_ROOT`.
- `FFMPEG_EXTERNAL_LIBS` (Plex's codec sidecar dir) — stripped.
  **Label:** `env:strip:FFMPEG_EXTERNAL_LIBS`.
- `LIBVA_DRIVER_NAME` injected as `iHD` (overridable via worker env).
  **Label:** `env:LIBVA`.

`X_PLEX_TOKEN` is read out of env into the rewritten URLs and *not*
passed through to ffmpeg's environment (no need; the URL carries it).

## Optimize-remux fast-path

When PMS issues a Plex Optimize job whose target preset matches the
source resolution / bitrate, it emits a different argv shape than a
real transcode:

- bare `-codec:0 {h264|hevc|av1|vp9}` for input (no `-hwaccel:0`)
- `-codec:0 copy` on the first video output (video pass-through)
- `-codec:N <eae>` audio decoder hints + `-eae_prefix:N <token>`
- output written to `<library>/Plex Versions/Optimized for TV/.inProgress/<name>.mp4`

The main rewriter pipeline can't reason about this shape (its decoder
phase requires either an `-hwaccel:0` paired with a known short codec
name, or a known SW decoder like `libdav1d`). It used to bail with
`skip:unknown-decoder:h264` and ffmpeg failed downstream on the EAE
audio decoder.

`tryOptimizeRemux` runs at the top of `Rewrite()` before any of the
main pipeline. It detects the shape (bare decoder + no hwaccel + first
post-`-i` `-codec:0 copy`) and short-circuits: no `-init_hw_device`
inject, no encoder swap, no filter chain. Just the minimal fix-ups
that still apply, all sharing helpers with the main rewriter so a fix
in one path lands in both:

- Drop `-delete_removed`, `-xioerror` (`-loglevel_plex` and
  `-strict_ts:N` now pass through — fork patch 0098/0107 sinks)
- `swapEAEAudioDecoders` — `*_eae` → stock base codec (`eac3_eae`→`eac3`)
- `dropEAEPrefixFlags` — orphaned `-eae_prefix:N` pairs
- `capturePMSProgressURL` — strip `-progressurl`, surface the rewritten
  URL on `RewriteResult.ProgressURL` so the worker reporter can use it
- `upgradeLoglevelFromQuiet` — `quiet|panic|fatal` → `info`
- `dropNostatsFlag`, `stripEAEEnvVars`, `setWorkerHomeEnv`

Multi-input shapes (Optimize jobs reference up to 18 sub-sidecar
inputs via `-map 1:s:0`, `-map 2:s:0`, etc. for SRT-copy outputs) pass
through untouched.

**Labels:** `decode:remux:<codec>`, `encode:copy(passthrough)`, plus
the standard scrub labels.

**Tests:** `TestRewriter_OptimizeRemux_h264_EAE`,
`TestRewriter_OptimizeRemux_hevc_PreservesSidecars`. Live-validated
2026-05-10 against:

| Source | Codec / audio | Output | Note |
|---|---|---|---|
| Pat & Mat S01E04 | h264 SDTV + EAC3 | h264 + AAC 2ch (40 MB) | small SDTV control |
| All Creatures S04E04 | hevc 480p + EAC3 + 2 sub sidecars | hevc + AAC 6ch + 3 SRT sidecars (738 MB) | multi-input passthrough |

The four prior Optimize cases (Deadline / Bob's Burgers / Adventure
Time / Friends) hit the main rewriter's HW-decode-passthrough path
because their argv shape includes `-hwaccel:0 vaapi` (the 2026-05-08
commit `c93034d` accepts that shape directly).

## Stream-spec normalization (`:#0xNN` → `:N`)

PMS emits stream specifiers in **stream-id-by-id hex form** for some
file classes — `-codec:#0x01 hevc -hwaccel:#0x01 vaapi ... -filter_complex
"[0:#0x01]hwupload[0];..."` etc. Observed on:

- "Plex Versions / Optimized for TV" Optimize outputs (Plex re-numbers
  streams on Optimize encode).
- High-PID m2ts / M2TS containers (e.g. `#0x1011`).
- ~95 of 3629 captured argvs (~2.6 %) as of 2026-05-31.

Stock ffmpeg accepts `:#0xNN` syntax natively. The rewriter doesn't —
every detector site (`indexOfArg(args, "-hwaccel:0", 0)`,
`"-codec:0"`, `"-hwaccel_output_format:0"`, etc.) is keyed on the
literal `:N` ordinal form. Without normalization, `:#0xNN`-shape
argvs silently bail `skip:no-decoder`, fall through to the bail
path, and (until v1.11.1) the dash muxer POSTed `-manifest_name` to
the worker pod's loopback → ECONNREFUSED → exit-145. Live regression
hit prod 2026-05-31 on a Ghosts S2E1 Plex Web force-burn.

`normalizePlexStreamSpecsToOrdinal` runs at top-of-`Rewrite` (after
the inlineass scrub, before the bail closure). It collects each unique
`#0xNN` ID in first-seen order across the argv (flag suffixes + filter
graph refs) and maps them to ordinal `0, 1, 2, ...`. Single rewrite
pass: flag suffix `:#0xNN` → `:N`, filter graph `[INPUT:#0xNN]` →
`[INPUT:N]`. Idempotent: ordinal-form argvs short-circuit with no
allocation, no map entries, no changes.

Mapping assumption: PMS always emits stream-spec flags strictly in
pre-input position before any other `:N` ordinal usage, and in
container declaration order (video first, audio next). Holds across
every `:#0xNN` corpus entry as of 2026-05-31.

**Label:** `normalize:stream-specs:#0xNN->ordinal` on the changes list.

**Tests:** `TestNormalizePlexStreamSpecsToOrdinal` (4 cases — Ghosts
shape, m2ts high-PID, ordinal-form passthrough, filter-only),
`TestRewriter_HWPassthrough_NormalizesStreamSpecsAndReshapes` (end-to-
end: `#0xNN` no longer bails, reshape engages),
`TestRewriter_BailRewritesManifestNameToRelay` (normalize+bail
interaction).

**Architectural followup:** GH #145 — lift this into a polymorphic
stream-spec matcher in the rewriter (so the detector accepts `#0xNN`
natively without upfront rewrite) or absorb into scaleplex-ffmpeg
fork alongside the rest of the rewriter→fork migration. The current
normalize is a workaround for the rewriter's narrow literal matching,
not a real argv-parse gap.

## Bail-path scrubs

When the main rewriter bails, the original argv is returned with one
focused fix applied: anything Plex-Transcoder-specific that stock
ffmpeg can't parse must come off, or the spawn fails on the first
unrecognised flag with empty stderr (loglevel quiet hides the error).

`scrubPlexFlagsOnBail` runs unconditionally on every bail. It:

- drops `-progressurl <url>` — its loopback URL is unreachable from a
  worker pod; ffmpeg would try to PUT and fail.
- rewrites a loopback `-segment_list` URL to the relay (same shape as
  the full rewriter's `rewriteSegmentList`) — observed on LG webOS
  sidecar-SRT side-channel argvs that hit the bail path.
- rewrites a loopback `-manifest_name` URL to the relay (DASH
  equivalent of the `-segment_list` block above; reuses the main-path
  `rewriteManifestName` helper). dashenc fork patch 0095 POSTs the
  `.mpd` body on each rewrite; without this the worker pod hits
  ECONNREFUSED → exit-145 on every dash-class bail. Added in v1.11.1
  after the live Ghosts force-burn regression (#144).

`-loglevel_plex` and `-strict_ts:N` are *not* scrubbed — fork patches
0098/0107 make stock ffmpeg accept them. `-xioerror` has never been
seen in the corpus. Bails with `Applied=true` when the scrub mutated
the argv, so the worker spawns the cleaned copy.

`dropInputAudioDecoderHints` runs on `bail("no-decoder")` specifically
— that bail reason fires when there's no `-codec:0` for video before
`-i`. PMS's audio-only jobs (Detection / intro / credits / voice
activity ML pre-pass) carry input-side audio decoder hints
(`-codec:1 aac`, `-codec:#0x02 aac` etc.) that PMS pipes through
expecting Plex's bundled EAE engine to bridge any source codec. Stock
ffmpeg honours the hint literally, fails on the bitstream parse when
the hint mismatches the actual codec, and exits 8.

`inputDecoderHintFlag` matches every ffmpeg stream-specifier shape per
`ffmpeg-all(1)`:

- `-codec:*`, `-c:*` — any specifier (digit, `#stream_id`, type+index,
  program, metadata pattern)
- `-c:v`, `-c:a`, `-c:s`, `-c:d`, `-c:t` — type-only shorthand

When it matches a flag *before* `-i`, the pair is dropped and
`-eae_prefix:N` companion pairs are also stripped. Stock ffmpeg auto-
detects each stream's decoder from `codec_id` when no hint is set,
which always picks correctly.

**Labels:** `drop:<flag>=<value>(bail)`, `drop:-eae_prefix(bail)`,
plus `skip:no-decoder` on the changes list.

**Tests:** `TestRewriter_Bail_StripsPlexPrivateFlags`,
`TestRewriter_Bail_NoDecoder_DropsAudioInputHints`.

## Test coverage

`worker/agent/rewriter_test.go` (~30 cases) covers every transformation
documented above and a few "must-NOT-touch" guards:

- Initial play with no `-ss` must not rewrite `force_key_frames`.
- `-copyts` is kept on both HLS and DASH (patch 0103 — no strip).
- VAAPI hwaccel injection must not duplicate when already present.
- Map-label updates must not corrupt graphs that already use shifted
  labels.
- Sidecar SRT discovery falls back to bail when no file is found
  (rather than producing a broken filter chain).
- `*_eae` audio swap accepts any `-codec:N` index (`rewriter_eae_test.go::TestRewriter_EAE_MultiStreamIndex`)
  + handles `truehd_eae` fallback to `eac3` (`TestRewriter_EAE_TrueHDFallback`).
- Text sub burn-in keeps Plex's `inlineass=` filter and strips only the
  Plex-private AVOption keys (`TestRewriter_InlineassPassthrough_*`).

## Replay regression set

The argv corpus on shared NFS at `/transcode/_argv-corpus` is the
canonical input for `worker/agent/replay_test.go` (build-tagged
`replay`). Each entry stores `{argv, env, client, outcome}` — the
client identification (Plex Product / Device / Platform / Version /
Username from `X_PLEX_*`) plus ffmpeg's exit status, duration,
segments-created, and stderr tail. Replay then re-runs every captured
argv through the current rewriter and compares outcomes — historical
exit-0 successes that now exit non-zero are flagged as regressions.
Captures come from two surfaces with identical outcome plumbing: the
scaleplex worker (Go agent — `persistArgvCapture` +
`persistArgvOutcome`) and, where a native Plex Transcoder is wrapped
for comparison, a bash tee-wrapper (`CLIENT:KEY=VALUE` headers +
`OUTCOME:exit_status=N ...` footer).

The bail path is exercised explicitly in tests
(`TestRewriter_Bail_*`) — a regression that quietly succeeded would be
worse than one that bailed.
