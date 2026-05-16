# Argv rewriter

`worker/agent/rewriter.go` translates a Plex SW transcode invocation into
a stock-ffmpeg VAAPI invocation that produces the same output bytes
under the same names in the same directory.

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
  crf->qp, preset:veryfast->compression_level:6, drop:-x264opts:0,
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
- `-crf:0 N` → `-qp:0 N`. **Label:** `crf->qp`.
- `-preset:0 <name>` → `-compression_level:0 N`. iHD's TargetUsage scale
  has 7 levels (1=quality, 7=fastest); x264 has 9 named presets. The
  bucketing maps `ultrafast/superfast/veryfast → 7/6/6`, `faster/fast →
  5/4`, `medium/slow → 3/2`, `slower/veryslow → 1/1`. Picked from
  on-cluster benchmark (3× Arc A310, 2026-05-05): cl=7 yielded
  +30-70% throughput over cl=2 on no-sub workloads with no visible
  quality difference at QP=22. **Label:** `preset:veryfast->compression_level:6`.
- `-x264opts:0 <stuff>` is dropped — those options are libx264-specific,
  not portable to VAAPI. **Label:** `drop:-x264opts:0`.
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

Plus we inject `-init_hw_device "vaapi=va:/dev/dri/renderD128,kernel_driver=i915,driver=iHD"`
and `-filter_hw_device va` ahead of the input. **Labels:** `filter:plain`,
`inject:init_hw_device+filter_hw_device`, `map-label-update`.

The map-label update is needed because we add `hwupload` inside the
existing chain, which can shift `[N]` labels — the rewriter walks the
graph, increments labels mentioned later in the argv (`-map "[1]"` etc.).

### Text sub burn-in

A text-sub burn-in session takes one of two routes, picked per session
by `subtitleIsAnimated()`:

- **SRT / static ASS → GPU overlay pre-render** — the default for
  effectively all text subs (new in v1.1).
- **Animated ASS → `inlineass` pass-through** — ASS carrying karaoke /
  `\t` / `\move` / `\fad` tags needs per-frame rendering and can't be
  pre-rendered, so it keeps the per-frame `inlineass` filter.

`subtitleIsAnimated()`: SRT carries no override tags so it is never
animated; ASS is scanned for animation tags, and an unreadable file is
treated as animated (conservative — falls back to the safe `inlineass`
path).

#### GPU overlay pre-render (SRT / static ASS)

The rewriter replaces Plex's per-frame CPU `inlineass` bracket
(`hwdownload` → libass → `hwupload`) with a GPU `overlay_vaapi`
composite. The agent spawns a second ffmpeg — the *pre-render* — from
the `SubPrerenderSpec` the rewriter returns: it rasterises the subtitle
to a sparse transparent video (`subtitles` → `mpdecimate` → `ffv1` /
Matroska) and streams it through a FIFO. The main transcode reads that
FIFO as a second video input and composites it on the GPU. All filters
are stock scaleplex-ffmpeg7 — **no fork patch involved**.

Graph (initial play):

```
[0:0]hwupload[10];
[10]scale_vaapi=...[11];               # +tonemap_vaapi for HDR
[N:v]format=bgra,hwupload[12];         # N = the FIFO input index
[11][12]overlay_vaapi=eof_action=pass:repeatlast=1[4]
```

The rewriter also:

- **Drops `-map_inlineass <spec>`** — no `inlineass` filter consumes it
  on this path; the pre-render reads the subtitle itself.
- **Appends the FIFO `-i`** immediately after the last real input's
  path (never just before `-filter_complex` — Plex parks output-side
  options like `-start_at_zero` / `-copyts` / `-fps_mode` there, and a
  new `-i` would mis-parse them as input options). FIFO input flags:
  `-copyts -probesize 32 -analyzeduration 0` — `-copyts` keeps the
  overlay timestamps; the minimal probe stops `find_stream_info` from
  reading megabytes of the sparse FIFO at startup (~5 s grind → ~0.8 s).

**Seek:** the main video reaches the filtergraph at the seek offset
(PTS N) but the overlay starts at ~0, so `overlay_vaapi` framesync would
drain the overlay 0→N hunting for a pair. The rewriter rebases both
branches to zero with `setpts=PTS-STARTPTS` around `overlay_vaapi`, then
rebases the composite back with `setpts=PTS+offset` so dashenc and the
seek-chunk/tfdt machinery see the unchanged source timeline (client
playhead unaffected). Initial play (offset 0) keeps the plain graph.

**Label:** `hw-decode:filter:sub-prerender-overlay`. See
[`SEEK.md`](SEEK.md) and `project_scaleplex_srt_to_pgs_gpu`.

#### inlineass pass-through (animated ASS)

Plex's `-map_inlineass 0:N` + `inlineass=...` filter is **Plex-private**
in stock ffmpeg, but scaleplex-ffmpeg7 ports it directly
(patches 0099-0101: `libavfilter/vf_inlineass.c`, fftools wiring, and
the pre-graph sub-chunk buffer). The fork's `scaleplex_inlineass`
binding consumes the sub stream via `-map_inlineass` side-channel and
renders glyphs onto CPU NV12 frames via libass under a process-wide
mutex.

The rewriter keeps Plex's argv shape intact and only:

1. **Rewrites the filter chain** to insert the hwdownload→inlineass→
   hwupload sandwich at the right place. For SW-decode (PMS sent SW
   shape) the chain becomes:

   ```
   [0:0]hwupload[10];
   [10]scale_vaapi=w=W:h=H:format=nv12[11];
   [11]hwdownload[12];
   [12]format=pix_fmts=nv12[13];
   [13]inlineass=<stripped-params>[14];
   [14]hwupload[15]
   ```

   For HW-decode (PMS already hwaccel'd) the chain is similar but
   shorter — the source is already on the GPU.

2. **Strips four Plex-only AVOption keys** from the `inlineass=`
   filter args: `language`, `overrides`, `outline`, `shadow`. These
   aren't AVOptions on `vf_inlineass` in the fork; PMS emits them but
   the filter rejects them at init. Keeps `font_scale`, `font_path`,
   `fontconfig_file`, `font_size`.

3. **Keeps everything else** — sidecar `-i`, `-map_inlineass`,
   trailing `-f null -codec ass` null-sub output. The fork's binding
   owns those.

**Labels:** `filter:passthrough-inlineass` (SDR) /
`filter:passthrough-inlineass-hdr` (HDR) /
`hw-decode:filter:inlineass-passthrough` (HW-decode path),
`map-label-update`.

### Bitmap sub burn-in (overlay_vaapi)

PGS / VobSub / DVDSub streams are bitmap images; libass can't render
them. The rewriter routes them through `overlay_vaapi` instead:

```
[0:0]hwupload[10];
[10]scale_vaapi=w=W:h=H:format=nv12[11];      # or +tonemap_vaapi for HDR
[streamSpec]format=bgra[12];
[12]hwupload[13];
[11][13]overlay_vaapi=eof_action=pass:repeatlast=1[15]
```

Strips `-map_inlineass` (the fork's text-sub binding doesn't apply
here; the bitmap stream is referenced directly via its stream spec in
the filter graph).

**Labels:** `filter:overlay-vaapi-bitmap`, `subtitle:bitmap:<spec>(<codec>)`.

### HDR tonemap (HDR source → SDR output)

`tonemap_vaapi` is **not in Plex's ffmpeg build** (they keep
`tonemap_cuda` and `tonemap_opencl` only). Workers run scaleplex-ffmpeg7
(jellyfin-ffmpeg + a small Plex-backport patch layer — see
[`scaleplex-ffmpeg/`](../scaleplex-ffmpeg/)) which has `tonemap_vaapi`.

When the rewriter sees an HDR source (color_transfer=smpte2084 etc. via
ffprobe) targeting SDR output, it injects:

```
scale_vaapi=w=W:h=H:format=p010,
tonemap_vaapi=transfer=bt709:format=nv12
```

into the VAAPI filter chain.

### Subtitle bail conditions

The translator returns `applied=false` and the agent runs original argv
when:

- Filter graph contains a `subtitles=...` already (Plex's own SW path)
- Decoder is unrecognised
- Filter shape doesn't match any known mode (plain / sub-prerender-overlay /
  passthrough-inlineass / overlay-vaapi-bitmap / hdr-tonemap-vaapi)

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
