# Plex Optimize argv corpus

What ffmpeg argv shapes does Plex generate for Optimize jobs? — and what
does that tell us about whether the rewriter's `tryOptimizeRemux` fast-
path should be folded into the main pipeline?

This doc summarizes the first full corpus sweep (2026-05-26, 1824 cells,
plex-test PMS 1.43.2) and what we learned. The tool that built it is
[`cmd/optimize-corpus-gen`](../cmd/optimize-corpus-gen/) — its README
documents the subcommands, flags, and per-quirk Plex API gotchas.

## Why we need a separate Optimize corpus

The organic argv corpus at `~/scaleplex-corpus/` (populated by
`cmd/argv-extract/sweep.sh`) carries ~1583 capture entries — almost all
of them from live playback. Plex Optimize jobs are under-represented
because they fire only when a user explicitly requests "Convert" / queues
a target preset. So evaluating refactors that touch the Optimize fast-
path (`tryOptimizeRemux` in `worker/agent/rewriter.go`) with the
organic corpus alone gives weak coverage of the very shapes that path
exists to handle.

The Optimize-corpus generator drives plex-test through a full Cartesian
of `{sources × targets × prefs}`, so the resulting corpus is a designed
sample, not a found one. `cmd/optimize-corpus-gen/README.md` covers the
how; this doc covers the what.

## Matrix (2026-05-26 sweep)

- **19 synthetic source clips** (`cmd/optimize-corpus-gen/synth/spec.go::DefaultMatrix`)
  covering h264 / hevc / av1 × profile × bit-depth × transfer × audio × sub.
  Filename encodes the cell axes (e.g.
  `hevc__main10__2160p__10bit__hdr10__eac3-5.1__sub-srt.mkv`).
- **3 Optimize targets** (Plex built-ins): Mobile (720p / 4 Mbps),
  TV (1080p / 8 Mbps), Original Quality (no transcode — remux-only).
- **32 pref combinations** (2⁵ Cartesian):
  `HardwareAcceleratedCodecs × HardwareAcceleratedEncoders ×
  TranscoderToneMapping × TranscoderHEVCEncodingMode × TranscoderHEVCOptimize`.
  The HEVC pair is per-axis — `TranscoderHEVCOptimize` gates Optimize's
  HEVC choice independently of `TranscoderHEVCEncodingMode` (which gates
  standard transcoding).

Cartesian: **19 × 3 × 32 = 1824 cells**. Runtime: ~48 min at ~1.5s/cell.
Capture rate: **1824 / 1824 (100%), zero timeouts, zero errors.**

## Result: 53 distinct argv shapes

The 1824 captures cluster into **53 distinct argv shapes** under the
analyzer's fingerprint (decode codec, hwaccel, encode codec, audio
codec, canonicalized filter graph, sub-burn class, tonemap class,
`-map` count). Top 25 by cell count:

| # | cells | %    | decode      | hwaccel | encode      | audio | maps | notes                                       |
|---|------:|-----:|-------------|---------|-------------|-------|-----:|---------------------------------------------|
|  1|  288  | 15.8 | hevc        | —       | copy        | aac   |  2   | HEVC video copy + audio re-encode to AAC    |
|  2|  288  | 15.8 | h264        | —       | copy        | copy  |  2   | **`tryOptimizeRemux`'s shape** — pure remux |
|  3|  160  |  8.8 | hevc        | —       | libx264     | aac   |  2   | SW HEVC→H264 transcode                      |
|  4|  120  |  6.6 | libdav1d    | —       | libx264     | aac   |  2   | SW AV1→H264 transcode                       |
|  5|   80  |  4.4 | hevc        | vaapi   | libx264     | aac   |  2   | Hybrid HW-decode + SW-encode                |
|  6|   64  |  3.5 | h264        | —       | copy        | copy  |  3   | Pure h264 remux + sub sidecar               |
|  7|   60  |  3.3 | av1         | vaapi   | libx264     | aac   |  2   | Hybrid HW-decode + SW-encode                |
|  8|   48  |  2.6 | hevc        | —       | libx264     | aac   |  3   | SW reshape + inlineass sub-burn             |
|  9|   48  |  2.6 | h264        | —       | libx264     | copy  |  3   | SW reshape + inlineass sub-burn (audio copy)|
| 10|   40  |  2.2 | hevc        | vaapi   | h264_vaapi  | aac   |  2   | Full-HW transcode (HEVCOptimize=false)      |
| 11|   36  |  2.0 | hevc        | vaapi   | hevc_vaapi  | aac   |  2   | Full-HW transcode (HEVCOptimize=true)       |
| 12|   32  |  1.8 | h264        | —       | libx264     | aac   |  2   | SW h264 7.1→stereo transcode (Mobile)       |
| 13|   32  |  1.8 | hevc        | —       | copy        | copy  |  2   | HEVC pure remux (Original Quality)          |
| 14|   32  |  1.8 | hevc        | —       | copy        | aac   |  3   | HEVC remux + audio re-encode + sub sidecar  |
| 15|   32  |  1.8 | hevc        | —       | libx264     | aac   |  3   | SW HEVC→H264 + inlineass                    |
| 16|   30  |  1.6 | av1         | vaapi   | h264_vaapi  | aac   |  2   | Full-HW transcode                           |
| 17|   24  |  1.3 | av1         | vaapi   | hevc_vaapi  | aac   |  2   | Full-HW (HEVCOptimize=true)                 |
| 18|   24  |  1.3 | h264        | vaapi   | libx264     | copy  |  3   | Hybrid + inlineass + audio copy             |
| 19|   24  |  1.3 | hevc        | vaapi   | libx264     | aac   |  3   | Hybrid + inlineass                          |
| 20|   24  |  1.3 | libdav1d    | —       | libx264     | aac   |  3   | SW av1→libx264 + sub                        |
| 21|   24  |  1.3 | libdav1d    | —       | libx264     | aac   |  2   | SW av1 HDR→libx264 + **SW tonemap=mobius**  |
| 22|   24  |  1.3 | libdav1d    | —       | libx264     | aac   |  3   | Same with sub                               |
| 23|   16  |  0.9 | hevc        | —       | libx264     | copy  |  2   | SW HEVC→H264, audio copy                    |
| 24|   16  |  0.9 | h264        | vaapi   | libx264     | aac   |  2   | Hybrid h264                                 |
| 25|   16  |  0.9 | h264        | vaapi   | libx264     | copy  |  3   | Hybrid h264 + sub                           |

The remaining 28 shapes account for ~12% of cells combined; full list
available via `optimize-corpus-gen analyze -corpus-dir ~/scaleplex-corpus/optimize-sweep`.

## Key findings

### 1. `tryOptimizeRemux` handles ~32% of Optimize traffic — not 2%

Buckets #2 (288, 15.8%), #6 (64, 3.5%), #13 (32, 1.8%) and similar
copy-encode + copy-audio shapes total ~21% pure-passthrough by cell
count. Add copy-encode + audio-transcode shapes (#1 + #14, another
~17.6%) and **copy-encode covers ~32% of Optimize traffic** — buckets
the existing fast-path is meant to short-circuit.

A mid-sweep snapshot at 392 cells (mostly AV1 + Mobile band) made the
fast-path look like a 2% edge case. The full corpus disproves that —
the pure-passthrough shape is one of the two largest buckets in the
entire matrix.

### 2. SW tonemap is real

Buckets #21 and #22 (48 cells, ~2.6% combined, plus smaller cousins for
hevc/h264 SW HDR sources) carry an explicit **SW tonemap filter**:

```
[0]format=p010,tonemap=mobius[N];[N]format=pix_fmts=yuv420p|nv12[N]
```

This fires when source is HDR, `TranscoderToneMapping=true`, AND
`HardwareAcceleratedCodecs=false`. PMS falls back to a SW pipeline and
**does emit a SW tonemap node** — refuting the earlier "Plex tonemaps
HW-only" claim in [`docs/REWRITER.md`](REWRITER.md) and the
[`project_scaleplex_libplacebo_tonemap`](../../../../.claude/projects/-home-frank-boeye-net/memory/project_scaleplex_libplacebo_tonemap.md)
project memory (both corrected 2026-05-26). scaleplex already handles
this shape — the orthogonal `extractGraphFacts` detector captures the
algo from a bare `tonemap=<algo>` node and `composeBurn` re-emits via
`tm.stage(algo)`.

### 3. Sub-burn uses `inlineass` exclusively

Buckets #8, #9, #18, #19, #20, #22 (~10% of corpus combined) carry sub
burn-in. Every one uses Plex's `inlineass=` filter; **none** use
`subtitles=` or `overlay_vaapi`. Confirms that the rewriter's unified-
inlineass approach (v1.3.0 / v1.6.1) matches what Optimize emits, not
just what live playback emits.

### 4. Audio is target-driven

For **Mobile target**, every single capture shows `audio=aac` regardless
of source codec (eac3-5.1, ac3-5.1, aac-7.1 → all become aac). PMS
unconditionally downmixes-to-AAC for Mobile.

For **TV target**, audio varies (aac vs copy) depending on whether the
source's bitrate fits the target preset.

For **Original Quality target**, audio is mostly `copy` (passthrough),
matching the no-transcode intent of that preset.

So the audio dimension of the matrix produces less variety than
expected on Mobile cells, more on TV, most on Original.

### 5. OpenCL HW tonemap chain appears (~3%)

Smaller buckets (not in the top 25 above) carry the full
`hwmap=opencl → tonemap_opencl=tonemap=mobius → hwmap=vaapi:reverse=1`
chain — Plex's HW-tonemap output when HDR source + HW codecs on +
HDRtm on. The rewriter's `gpuResidentOpenCLTonemap` path gets Optimize
traffic too, not just live playback.

### 6. Hybrid HW-decode + SW-encode is common

Buckets #5 (80), #7 (60), #18 (24), plus smaller variants ~150 cells
total — `decode=<codec> hwaccel=vaapi encode=libx264` shape. Fires when
`HardwareAcceleratedCodecs=true` but `HardwareAcceleratedEncoders=false`.
The rewriter has an explicit `honor:plex-hwdec-swenc` path that handles
this — Optimize exercises it heavily.

## Implication for the fast-path fold question

The architecture question from earlier in the design discussion was
whether to fold `tryOptimizeRemux` into the main `Rewrite()` pipeline.
The corpus updates the answer:

- **The fast-path catches a substantial real shape category** (~32% of
  Optimize traffic), not an edge case. It cleanly expresses "video is
  copy-encode; no decode/encode/filter work" as a distinct intent.
- **Folding would add two new entry detectors** to the main pipeline:
  - bare-codec + no-hwaccel + `-codec:0 copy` → pure passthrough
  - bare-codec + no-hwaccel + `-codec:0 copy` + audio re-encode → audio-only-transcode passthrough
- **It's not a complexity win.** The fast-path's existence neither
  duplicates the main pipeline's logic (the cross-cutting scrubs — EAE
  swap, URL rewrite, env strip — are already shared via common helpers)
  nor masks shape branching the orthogonal core would subsume more
  cleanly (the cleanly-orthogonal filter-graph reshape lives in the
  main pipeline; pure-passthrough has no filter graph to reshape).

**Tentative conclusion: keep the fast-path separate.** It is not
analogous to the regex-zoo refactor (#39, #41) — that collapsed N
input shapes for the *same* operation into one extractor + one composer.
Pure passthrough is a *different* operation (no encode work), and
expressing that distinction as a sibling fast-path is clearer than
encoding it as a flag in the main pipeline.

The fold remains tenable on aesthetic grounds (one entry point, fewer
pieces to remember), but the data doesn't make it the obviously right
call.

## Standalone value of the corpus

Beyond the fold question, the corpus has ongoing value:

1. **Regression coverage for rewriter changes.** Any change to
   `worker/agent/rewriter.go` can be parity-tested against the 1824
   Optimize-shaped argvs in addition to the 1583 playback-shaped argvs
   in the existing corpus. The `replay_test.go` harness reads the same
   capture-JSON format; pointing it at `~/scaleplex-corpus/optimize-sweep`
   gives Optimize coverage. (Replay tests currently default to
   `~/scaleplex-corpus`; either add a separate test run or merge the
   captures into a single corpus dir.)
2. **PMS upgrade regression.** Re-run `optimize-corpus-gen sweep` after
   a PMS upgrade and diff the new captures against this run's. Argv
   shape changes from a PMS update show up immediately instead of weeks
   later when a user hits the bad shape.
3. **Reusable pattern for other under-represented surfaces.** Detection
   jobs, intro/credits detection, audio-only voice-activity passes, sub
   side-channels — all are triggerable via API and under-represented in
   the playback corpus. The generator's structure (Plex API + matrix
   expander + remote watcher + sidecar tagger) ports to any of them.

## Corpus on disk

- `~/scaleplex-corpus/optimize-sweep/` — sibling subdir of the playback
  corpus `~/scaleplex-corpus/`.
- 1824 `<basename>.json` capture files (worker's `persistArgvCapture`
  output, copied locally via kubectl exec).
- 1824 `<basename>.optimize-cell.json` sidecars (generator-written;
  carry the cell tag: source ratingKey + title + ffprobe profile,
  target tagID + title, pref combination, cell ID fingerprint).
- 1 `manifest.json` (per-cell status: captured / timeout / error /
  skipped; used by `sweep -resume` to skip cells already complete).
- Existing playback-corpus tools (`replay_test.go`, `dedupe.py`,
  `argv-extract`) ignore the subdir, so the playback corpus stays
  untouched.

## Re-running

```bash
# Quick clean-up if a previous run was interrupted.
optimize-corpus-gen clean -plex "$PLEX_TEST_URL" -token "$PLEX_TOKEN"

# Resume a partial sweep (default: skip cells already captured).
optimize-corpus-gen sweep ... -resume

# Re-analyze without re-sweeping.
optimize-corpus-gen analyze -corpus-dir ~/scaleplex-corpus/optimize-sweep
```

See [`cmd/optimize-corpus-gen/README.md`](../cmd/optimize-corpus-gen/README.md)
for the full workflow and per-quirk Plex API documentation.
