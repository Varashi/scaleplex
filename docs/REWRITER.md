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
  hls:segment_list_size:5->99999, hls:drop:-copyts(seek),
  hls:segment_list:rewrite-to-relay, seek-offset:captured=888.000s,
  force_key_frames:offset-by-seek, progress:append-X-Plex-Token,
  progressurl:captured-for-reporter,
  inject:-canthrottleurl(scaleplex-ffmpeg7-canThrottle),
  loglevel:->info, drop:-nostats,
  env:strip:EAE_ROOT, env:strip:FFMPEG_EXTERNAL_LIBS, env:LIBVA
```

scaleplex-ffmpeg7 patches 0094–0098 absorb several rewrites that the
worker used to do at argv-time. The rewriter no longer emits these
tags (and ffmpeg accepts the originals natively):

- `drop:-loglevel_plex` — patch 0098 registers `-loglevel_plex` as an
  OPT_TYPE_STRING sink (value accepted + discarded).
- `hls:f=ssegment->segment` — patch 0098 adds `AVFMT_GLOBALHEADER` to
  `ff_stream_segment_muxer`; Plex's `-f ssegment` shape works without
  rewriting to `-f segment`.
- `hls:segment_format_options:live=1->live=0+per-frame-clusters` —
  patch 0094 makes `matroskaenc` write Duration regardless of
  `is_live`. Stock jellyfin's
  `IS_SEEKABLE = pb seekable && !is_live` macro naturally falls to the
  else-branch in cluster-default selection when `is_live=1` →
  1000 ms / 32 KB cluster cadence = per-frame clusters, matching Plex.
  Both behaviours the rewrite used to force are now defaults.
- `hls:drop:-segment_list_unfinished` / `-segment_list_separate_stream_times`
  — patch 0096 registers both as no-op `AVOptions` on the segment
  muxer (Phase 2a stub; functional emit logic deferred to Phase 2b).

Each label below maps to one of the transformations documented here.

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

### Hybrid inline-ASS (sidecar SRT/ASS subtitle burn-in)

Plex's `-map_inlineass 0:N` + `inlineass=...` filter is **Plex-private**.
Stock ffmpeg only has `subtitles=` and `ass=` which expect a file path,
not a stream index.

The rewriter detects this combo and:

1. Probes the source media's directory for a sibling SRT/ASS subtitle
   file. Probe order: `<base>.<lang>.srt`, `<base>.<lang>.ass`,
   `<base>.srt`, `<base>.ass`. (`findSidecarSubtitle()`).
2. If found, rewrites the filter chain to:

   ```
   [0:0]scale=w=W:h=H:force_divisible_by=4[0];
   [0]format=pix_fmts=yuv420p|nv12[12];
   [12]hwdownload,format=nv12[13];
   [13]subtitles=filename='<sidecar>':fontsdir=/usr/share/fonts[14];
   [14]hwupload[15]
   ```

3. Drops the `-map_inlineass` arg.

4. **HLS+seek PTS-shift bracket**: HLS path strips `-copyts` (stock
   ssegment muxer can't split chunks with `-copyts` + seek). Without
   the shift, fast-seek rebases frame PTS to 0, but `subtitles=`
   filter looks up SRT cues at absolute time — every cue lookup
   misses, subs render blank for the entire seek session. The
   rewriter brackets `subtitles=` with two `setpts` pieces only when
   `isHLS && seekOff > 0`:

   ```
   [12]hwdownload,format=nv12,setpts=PTS+<T>/TB[13];   # pre: feed libass absolute time
   [13]subtitles=filename=...[14];
   [14]setpts=PTS-<T>/TB,hwupload[15]                  # post: restore 0-based PTS for muxer
   ```

   The post-shift restores 0-based PTS so the segment muxer cuts
   cleanly and the relay sidecar's CSV rewrite produces the expected
   global-timeline window. DASH path keeps `-copyts` and doesn't need
   the shift. Plex's `inlineass` worked because Plex's fork supports
   `-copyts` + ssegment splits simultaneously; stock can't do both.

5. **`force_style` pin**: appended as `:force_style='FontName=DejaVu
   Sans'` (overridable via `HW_SUBTITLE_FORCE_STYLE`). Pins the
   primary face so libass skips fontconfig pattern matching at filter
   init. Mirrors Plex's `inlineass` `font_path=` direct-open trick
   within the bounds of stock ffmpeg's `subtitles=` filter (which
   doesn't expose `font_path=`). See
   [reference_libass_cold_start.md] in the user's auto-memory for the
   full cold-start mitigation analysis.

6. If no sidecar is found OR the source is `inlineass`-style (subs
   embedded in the main video stream), bails. PGS subs always force
   burn-in client-side; SRT-via-stream-index needs sidecar lookup to
   work in stock ffmpeg.

**Labels:** `filter:hybrid-inlineass`, `drop:-map_inlineass`,
`subtitle:pts-shift=<T>s` (HLS+seek).

This is the path that bottlenecked clusterplex (Plex's `inlineass` was
the only filter that worked, but it forced the whole pipeline through
Plex Transcoder).

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

- Filter graph contains a `subtitles=...` already (Plex's own SW path —
  not our hybrid)
- Decoder is unrecognised
- Filter shape doesn't match any known mode (plain / hybrid-inlineass /
  overlay-vaapi-sidecar / etc.)

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
`http://clusterplex-pms.clusterplex.svc:32499`. The relay sidecar
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
- `-skip_to_segment N` captured into `RewriteResult.SkipToSegment` and
  stripped from argv. `segwatch.watchAndRenumberChunks` uses N as the
  starting sequence so chunk filenames align with the `.mpd`'s
  `startNumber=`. See [SEEK.md#dash-seek](SEEK.md#dash-seek) for why.
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

- `-f ssegment` — now passthrough. scaleplex-ffmpeg7 patch 0098 adds
  `AVFMT_GLOBALHEADER` to `ff_stream_segment_muxer` so it accepts
  Plex's `-flags +global_header` / `-segment_header_filename header`
  combination natively. Before patch 0098 the rewriter translated to
  `-f segment` (which already had the flag); behaviour is identical.
- `-segment_format matroska` and `-segment_header_filename header` are
  **kept**. Plex uses mkv-in-.ts when the codec/audio combo can't fit
  mpegts (4K HDR + 5.1 EAC3, Atmos passthrough, etc.). Stock segment
  muxer supports this — verified locally.
- `-segment_list_separate_stream_times`, `-segment_list_unfinished`
  pass through; patch 0096 registers both as no-op `AVOptions` so the
  argv parser stops rejecting them. (Phase 2b will wire the actual
  per-stream end-time tracking + `#`-prefix CSV emit; the relay's CSV
  rewrite covers correctness in the meantime.)
- `-segment_format_options live=1` passthrough. Patch 0094 + jellyfin's
  `IS_SEEKABLE = pb seekable && !is_live` macro combine to give the
  desired Plex-Windows shape (Duration written in header, ~per-frame
  clusters from the cluster-default else-branch) without rewriting.
- `-segment_list_size 5` → `99999`. Plex's small CSV window drops
  older rows as ffmpeg outpaces playback; PMS then 200/0-bytes any
  chunk request that falls outside the window. 99999 retains every
  chunk's row for the lifetime of the session. **Label:**
  `hls:segment_list_size:5->99999`.
- **Strip `-copyts` ONLY on seek sessions** (`-ss <off>` set). Stock
  jellyfin 7.x segment muxer + `-ss` + `-copyts` produces zero chunks
  even though the encoder runs (verified 2026-05-11, BH6 hevc_vaapi
  -ss 4801). Strip rebases PTS to 0, splits resume.
  Initial-play sessions KEEP `-copyts` — stripping there caused PS4
  BH6 chunk-0 PTS-offset loop (2026-05-12); upstream
  `reference_stream_first_pts` in libavformat/segment.c (FFmpeg 7.1.3)
  handles non-zero start PTS for the split logic. **Label:**
  `hls:drop:-copyts(seek)`.
- Rewrite `-segment_list <PMS-loopback>` to `<relay>?...&scaleplex_seg_time=<N>`.
  The `scaleplex_seg_time` query param tells the relay to rewrite each
  CSV row's start_time to the global timeline. Without it, PMS would
  serve every seek chunk as 200/0-bytes (CSV rows say chunk N starts at
  PTS 0 instead of N×8s). **Label:** `hls:segment_list:rewrite-to-relay`.

## `-force_key_frames` rewrite (seek path)

Plex always emits `-force_key_frames:0 "expr:gte(t,n_forced*8)"`. With
`-copyts -ss 1384`, the encoder's `t` starts at 1384, the expression is
true for every frame whose `n_forced*8 <= t`, and ffmpeg fires ~`off/8`
forced keyframes back-to-back at the start, then nothing for 8s. The
HLS segment muxer needs a keyframe to close — first segment swallows
tens of minutes (observed: 222 MB / 23 min on Balls Up).

When seek is captured (`-ss > 0`), the rewriter rewrites the expression
to subtract the seek offset:

```
expr:gte(t,n_forced*8)  →  expr:gte(t-1384.000,n_forced*8)
```

The keyframe cadence then matches PMS's intent (kf at output 0, 8, 16,
…) and segments split every 8s. **Label:** `force_key_frames:offset-by-seek`.

## Other tweaks

- `-loglevel quiet` / `-loglevel <whatever>` → `-loglevel info`. We need
  ffmpeg's stream-mapping lines so the agent can identify input streams
  for the progress reporter. **Label:** `loglevel:->info`.
- `-loglevel_plex error` dropped. Plex-private flag. **Label:** `drop:-loglevel_plex`.
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
through untouched — the fast-path never calls `dropSidecarInput`.

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
The capture+outcome plumbing is identical on both surfaces:
clusterplex worker (Go agent — `persistArgvCapture` +
`persistArgvOutcome`) and production plex (bash tee-wrapper —
`CLIENT:KEY=VALUE` headers + `OUTCOME:exit_status=N ...` footer).

The bail path is exercised explicitly in tests
(`TestRewriter_Bail_*`) — a regression that quietly succeeded would be
worse than one that bailed.
