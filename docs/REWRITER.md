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
  hls:drop:-copyts(seek),
  hls:segment_list:rewrite-to-relay, seek-offset:captured=888.000s,
  progress:append-X-Plex-Token,
  progressurl:captured-for-reporter,
  inject:-canthrottleurl(scaleplex-ffmpeg7-canThrottle),
  loglevel:->info, drop:-nostats,
  env:strip:EAE_ROOT, env:strip:FFMPEG_EXTERNAL_LIBS, env:LIBVA
```

Each label below maps to one of the transformations documented here.

scaleplex-ffmpeg7 patches 0094–0098 absorb several rewrites that
earlier versions of the worker did at argv-time. The rewriter no
longer emits these tags (and ffmpeg accepts the originals natively):

- `drop:-loglevel_plex` — patch 0098 registers the option as an
  OPT_TYPE_STRING sink.
- `hls:f=ssegment->segment` — patch 0098 adds `AVFMT_GLOBALHEADER`
  to `ff_stream_segment_muxer`; Plex's `-f ssegment` shape works
  without translation.
- `hls:drop:-segment_list_unfinished` / `-segment_list_separate_stream_times`
  — patch 0096 registers both as no-op `AVOptions` (Phase 2a stub;
  functional CSV-emit logic deferred to Phase 2b).
- `inject:-canthrottleurl` is NEW (from patch 0097) — rewriter
  injects a relay-URL for ffmpeg's in-binary canThrottle handler.

What WAS dropped from the audit but still has a rewrite path: the
`-segment_format_options live=1 → live=0+overrides` rewrite.
Production PMS argvs (Plex Android) carry `output_ts_offset=10`
and never `live=1`, so the rewrite is dead code in current
captures. But the synthetic Plex-Windows fixture exercises it,
and Plex Windows hardware has not been re-tested since patch 0094
landed; leaving the rewrite as a safety net.

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
- `-sei:0 a53_cc` is injected so VAAPI's encoder embeds Closed Caption
  608 data the same way Plex's libx264 build does. **Label:** `inject:sei+a53_cc`.

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

### Text sub burn-in (Plex inlineass pass-through)

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

This replaced the original "hybrid inline-ASS" path (subtitles= filter
on a staged sidecar file, with a setpts PTS-shift bracket for HLS+seek)
in 2026-05-12 once the fork's vf_inlineass landed and was live-validated
on Plex Android (HLS+seek + cold start) and Plex Windows direct-play.

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
- Filter shape doesn't match any known mode (plain / passthrough-inlineass /
  overlay-vaapi-bitmap / hdr-tonemap-vaapi)

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
| `-manifest_name` | `http://127.0.0.1:32400/.../manifest?...` | (DASH only — captured for `manifest_publish.go`, stripped from argv) |
| `-segment_list` | `http://127.0.0.1:32400/.../manifest?...` | `${SCALEPLEX_PMS_BASE_URL}/.../manifest?...&X-Plex-Token=...&scaleplex_seg_time=<N>` |

`SCALEPLEX_PMS_BASE_URL` is set in the worker DaemonSet env, e.g.
`http://<pms-service>.<namespace>.svc:32499`. The relay sidecar
listens on 32499 and forwards to 32400.

**Labels:** `progress:append-X-Plex-Token`, `progressurl:captured-for-reporter`,
`manifest_name:captured-for-publisher`, `hls:segment_list:rewrite-to-relay`.

ffmpeg's `-progress` doesn't attach Authorization headers, so the token
goes in the query string — the relay just forwards it through.

## DASH-specific rewrites

The rewriter detects DASH via `outputFormat == "dash"`.

- `-extra_window_size 999999` injected. Without it, ffmpeg's dashenc
  cleanup deletes chunks the moment they fall out of the rolling
  window, but PMS's NFS-readdir-driven serve is asynchronous and
  occasionally races the cleanup → 404 to client.
- `-format_options "movflags=+empty_moov+default_base_moof+separate_moof+cmaf"`
  injected before `-f dash`. Stock dashenc emits self-contained mp4
  segments by default (`ftyp+moov+styp+sidx+moof+mdat`) which MSE
  rejects as duplicate-init. The CMAF flags trim each chunk to
  `styp+sidx+moof+mdat`.
- `-skip_to_segment N` passes through to ffmpeg untouched. scaleplex-ffmpeg7
  (patch 0095) starts dashenc's segment_index at N natively so
  chunk-stream0-NNNNN.m4s aligns with PMS's `.mpd` `startNumber=`. The
  rewriter only emits a diagnostic change-tag (`skip_to_segment:passthrough=N`).
- `-ss <off>` captured into `RewriteResult.SeekOffsetSeconds`. The
  segwatch's `patchSeekChunkTimestamps` uses it to add
  `seekOffset*timescale` to each chunk's `tfdt.bmdt` and `sidx.ept`
  after the rename. See [SEEK.md#dash-seek](SEEK.md#dash-seek).
- `-manifest_name <url>` extracted; `manifest_publish.go` POSTs the
  `.mpd` body on each rewrite (Plex's ffmpeg fork does this natively;
  stock dashenc treats `-manifest_name` as a filename).

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
- **Strip `-copyts` ONLY on seek sessions** (`-ss <off>` set).
  Stock jellyfin 7.x segment muxer + `-ss` + `-copyts` produces
  zero chunks even though encoder runs (verified BH6 hevc_vaapi
  -ss 4801). Strip rebases PTS to 0, splits resume.
  Initial-play sessions KEEP `-copyts` — stripping there caused PS4
  chunk-0 PTS-offset loop. **Label:** `hls:drop:-copyts(seek)`.
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

- Drop `-loglevel_plex`, `-delete_removed`, `-strict_ts:N`, `-xioerror`
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

`scrubPlexFlagsOnBail` runs unconditionally on every bail. It drops:

- `-loglevel_plex <level>` — Plex-private log verbosity
- `-progressurl <url>` — Plex progress sink
- `-delete_removed <bool>` — Plex DASH muxer extension
- `-xioerror` — Plex-private boolean

Bails with `Applied=true` when the scrub mutated the argv, so the
worker spawns the cleaned copy.

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
- HLS argv must have `-copyts` stripped; DASH must keep it.
- VAAPI hwaccel injection must not duplicate when already present.
- Map-label updates must not corrupt graphs that already use shifted
  labels.
- Sidecar SRT discovery falls back to bail when no file is found
  (rather than producing a broken filter chain).
- `*_eae` audio swap accepts any `-codec:N` index (`rewriter_eae_test.go::TestRewriter_EAE_MultiStreamIndex`)
  + handles `truehd_eae` fallback to `eac3` (`TestRewriter_EAE_TrueHDFallback`).
- HLS + seek + sub-burn brackets `subtitles=` with `setpts` so libass
  reads absolute time while the encoder still emits 0-based PTS.

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
