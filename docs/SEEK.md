# Seek

> **Partly historical.** This documents how seek was solved during the
> *stock-ffmpeg era*. Most of the **DASH** machinery below — the
> `patchSeekChunkTimestamps` tfdt/sidx post-processor, the chunk
> renumber, the `manifest_publish.go` publisher — was **superseded**
> and removed once scaleplex-ffmpeg7 absorbed the Plex-fork behaviour:
> patch 0095 (native `-manifest_name` + `-skip_to_segment`), patch 0103
> (drops jellyfin's `reference_stream_first_pts` adjust), patch 0104
> (CMAF-strict movflags). DASH seek now works without worker-side
> chunk post-processing. The **HLS CSV-rewrite in the relay**
> (`scaleplex_seg_time`) is still live. The failure-mode diagnostics
> and the *Operational notes* section remain useful reading.

Seek was the hardest problem in the stock-ffmpeg build-up. Plex's
bundled ffmpeg has a custom dashenc fork and a custom `ssegment` muxer
that handle seek with internal state stock ffmpeg doesn't have — so
getting seek to behave on both DASH and HLS clients took multiple
layered fixes, captured here.

## DASH seek

**Client:** Plex Web (Chrome). MSE consumer of mp4 fragments.

### Failure mode

- Initial play works. Click seek to t=5000s.
- Player issues GET for chunk 632 (5000/8 ≈ 632) on the .mpd's URL pattern.
- Worker writes a chunk file. Client fetches it. **Spinner forever.**

`chrome://media-internals` shows `BUFFERING_HAVE_NOTHING`. The chunk
arrives, `appendBuffer` succeeds, buffered range never extends past
the initial play range.

### Diagnosis

Built a local MSE harness (`/tmp/mse-test/index.html` + `python3 -m
http.server 8765 --bind 127.0.0.1`). Loaded scaleplex's seek chunk into
a real MediaSource+SourceBuffer manually, observed `appendBuffer`
return OK with no buffered-range extension. Probed the chunk's mp4
boxes:

- Plex Transcoder.real seek chunk: `tfdt.baseMediaDecodeTime = 5000s × 11988 = 59940000`.
- scaleplex seek chunk: `tfdt.baseMediaDecodeTime = 0`.

`tfdt` (Track Fragment Decode Time) is the box MSE uses to place the
chunk on the media timeline. With `tfdt=0`, every seek chunk lands at
timeline 0..seg_dur, while the player's currentTime sits at the seek
offset → `BUFFERING_HAVE_NOTHING` forever.

Stock dashenc writes `tfdt=0` regardless of `-ss`/`-copyts`/`+cmaf`/
`-output_ts_offset`/`-start_at_zero` permutations. We tried all of them
in commits `cc2f2d9`..`9c9eb14` (six fixes layered, each solved a real
sub-bug, none fixed tfdt). Plex's fork patches dashenc to write
the correct `tfdt`; stock can't be coaxed into doing the same.

### Fix (commit `406c0e4`)

**Plan B: post-process every renumbered chunk file.** After
`os.Rename(...)`, the segwatch calls `patchSeekChunkTimestamps(target,
seekOffsetSeconds)`:

1. Read the chunk into memory.
2. Find the `sidx` box, read its `timescale` (video=11988, audio=48000
   for the typical Plex configs).
3. Find `sidx.earliest_presentation_time` (offset/size depends on box
   version), add `seekOffset * timescale`, write back.
4. Find `moof/traf/tfdt.baseMediaDecodeTime`, add the same, write back.
5. Atomic write (no one's reading the chunk yet — segwatch fires before
   PMS scans the dir).

Verified with curl: a chunk with `ept=0/bmdt=0` becomes
`ept=59940000/bmdt=59940000` for an offset-5000 seek. Matches PT.real
exactly.

### The six layered fixes that came before tfdt-patch

Each was a real bug; tfdt was the deepest of seven:

1. **`cc2f2d9`** — chunks deleted off NFS by ffmpeg's `window_size`
   cleanup before client could fetch. Inject `-extra_window_size 999999`.
2. **`53e0d31` → `0c2ab0a` → `90efa8f`** — seek chunks numbered wrong.
   Stock dashenc always counts segment_index from 1 regardless of
   `-ss`. We capture `-skip_to_segment N` from PMS's argv, strip it
   from ffmpeg's argv, then rename files via fsnotify. The
   skip-self-rename guard (`parsedNum >= startSeq → skip`) avoided an
   infinite rename loop where each rename target's CREATE event
   triggered another rename.
3. **`a6337ea` → `626fea2`** — manifest body's `startNumber=` must
   reflect renumbered file names. dashenc emits its internal counter
   K; PMS's "Transcoder segment range" log gates client-GET responses
   on that number. Final fix: `manifest_publish.go` regex-rewrites
   every `startNumber="K"` to `K + (skipToSegment - 1)` before POSTing.
4. **`019a335`** — *don't* strip `-copyts -start_at_zero
   -avoid_negative_ts disabled` (an earlier attempt rebased PTS to 0
   and produced 199-byte empty audio chunks because the AAC encoder
   got no primer samples).
5. **`954d284`** — force CMAF segment layout. Stock dashenc emits
   self-contained mp4 segments by default which MSE rejects as
   duplicate-init. Inject `-format_options
   "movflags=+empty_moov+default_base_moof+separate_moof"`.
6. **`487ce48`** — drop `+frag_keyframe` from movflags. Earlier add
   produced 2 fragments per chunk because h264_vaapi GOP < seg_duration.
7. **`406c0e4`** — tfdt + sidx.ept patch (the actual breakthrough).

The progression is documented in commit messages and
`project_scaleplex.md` memory. The lesson: when `chrome://media-internals`
reports `BUFFERING_HAVE_NOTHING` with no error, the chunk's metadata is
the next thing to check. The MSE harness was the diagnostic that paid
off — direct comparison with PT.real chunks pointed straight at tfdt.

## HLS seek

**Client:** Plex for Android. matroska-in-.ts container (used when
codec/audio combo can't fit mpegts — 4K HDR + 5.1 EAC3 source like
Balls Up). HLS playlist consumer.

### Failure mode (initial)

- Initial play works. Tap seek to 14:48 (888s).
- Player gives up after ~30s. **"Source: network connection failed"**
  on Android.

### Diagnosis

PMS log for the seek session:

```
GET /base/00111.ts → 200 1405ms 0 bytes
GET /base/00112.ts → 200  633ms 0 bytes
GET /base/00113.ts → 200  206ms 0 bytes
...
```

Every chunk: 200 OK with **0-byte body**. Player times out, surfaces
`network connection failed`.

But `media-00111.ts` exists on disk and is ~1 MB. PMS isn't serving
it.

### Three layered fixes

#### 1. force_key_frames keyframe burst (commit `3629b51`)

Before getting to the 0-byte issue, the first symptom was: **first
segment swallowed 23+ minutes of content (222 MB).**

Plex sends `-force_key_frames:0 "expr:gte(t,n_forced*8)"` on every
transcode. With `-copyts -ss 1384`, the encoder's `t` starts at 1384,
and the expression is true for every frame whose `n_forced*8 <= t`.
ffmpeg fires ~294 forced keyframes back-to-back at the start, then 8s
of drought, repeating. The HLS segment muxer waits for a keyframe to
close a segment; the keyframe burst followed by 8s drought meant the
first segment didn't close until much later (it eventually did, after
~23 min of content, when the next forced kf finally fired clean).

**Fix:** when `-ss > 0` is captured, rewrite the expression to subtract
the seek offset:

```
expr:gte(t,n_forced*8)  →  expr:gte(t-1384.000,n_forced*8)
```

Keyframes now fire at output time 0, 8, 16, … (segment boundaries).

#### 2. -copyts blocks segment splits (commit `b003ebf`)

After fixing keyframes the first segment was *still* huge — 317 MB.

Reproduced the issue locally on the worker pod (no PMS in the loop):
stock segment muxer with `-ss <off> -copyts` produces **one file**
across a 30s span. Without `-copyts`, splits resume every keyframe
past `-segment_time` (4 files for the same span).

Tested all four combinations of `-copyts` × `-output_ts_offset 888`:

| Flags | Files produced | CSV start_time |
|---|---|---|
| `-copyts` | 1 | 888.0 |
| `-copyts -output_ts_offset -888` | 4 ✓ | 0.04 |
| `-output_ts_offset 888` | 1 | 888.0 |
| (none) | 4 ✓ | 0.04 |

Splits happen only when output PTS starts near 0. CSV start_time is
0-based when splits work. There is no stock-ffmpeg combination that
gives both splits AND global-timeline CSV — Plex's ssegment fork
special-cases this; stock can't.

**Fix:** strip `-copyts` from HLS argv only. DASH path keeps it
(dashenc has separate split logic and DASH seek already worked with
`-copyts` in place).

#### 3. CSV rewrite in the relay (commit `64ae74b`)

Splits now work, segments are normal-sized — but PMS still serves 0
bytes for every seek chunk.

PMS reads `-segment_list` CSV rows to learn each chunk's playlist
window:

```
media-00111.ts,0.041667,13.041667
media-00112.ts,13.041667,21.041667
media-00113.ts,21.041667,29.041667
```

With `-copyts` stripped, stock ffmpeg writes 0-based start_times. PMS's
m3u8 says chunk 111 should be at 888..896s; CSV says 0..13s. Mismatch
→ PMS returns 200/0-bytes. (Confirmed by tracing: the `-segment_list`
POST itself reaches PMS fine — return code 200 — but PMS's chunk-serve
path inspects the row's start_time before returning the file body.)

**Fix:** the relay sidecar rewrites each CSV row.

Worker side: append `&scaleplex_seg_time=<N>` to the segment_list URL
when rewriting it for HLS sessions. The N is the value of
`-segment_time` from the argv.

Relay side: when a POST hits `^/video/:/transcode/session/[^/]+/[^/]+/manifest$`
with that param set:

1. Strip the param from the upstream query.
2. Read the body.
3. For each line matching `^(media-(\d+)\.ts),([0-9.]+),([0-9.]+)\s*$`,
   compute `start = N*segTime`, `end = (N+1)*segTime`, replace.
4. Forward with corrected Content-Length.

CSV in:
```
media-00111.ts,0.041667,13.041667
media-00112.ts,13.041667,21.041667
```

CSV out:
```
media-00111.ts,888.000000,896.000000
media-00112.ts,896.000000,904.000000
```

Initial-play sessions (offset 0) rewrite to identical values, so it's
a no-op there. DASH and progress traffic skip the branch entirely.
Tests in `shim/cmd/relay/main_test.go`.

### Why three fixes and not one

Each fix solves a real, distinct bug. None of them masks another:

- Without (1) the encoder fires bad keyframes regardless of split flags.
- Without (2) the segment muxer never splits regardless of CSV.
- Without (3) PMS won't serve chunks regardless of how they're produced.

You can verify each independently by reverting that commit and observing
the matching failure mode (huge first segment / no splits / 0-byte
serves).

## Things that didn't work

For posterity, dead ends explored before each working fix:

**DASH seek dead ends:**
- `-output_ts_offset <off>` (only shifts PTS, not DTS / tfdt)
- Drop `-start_at_zero` (no effect on tfdt with stock dashenc)
- `+cmaf` movflag (no effect on tfdt either)
- Various `-avoid_negative_ts` modes
- `-output_ts_offset 0` injection rebased PTS but blanked AAC primer samples

**HLS seek dead ends:**
- `-output_ts_offset 888` (tested with and without `-copyts`; either
  produces 1 file or 4 files but doesn't bridge the splits-vs-global-CSV
  trade-off)
- Stripping `-segment_format_options output_ts_offset=10` (no effect on
  splits; that option targets the inner matroska muxer, not segment
  decisions)
- `-reset_timestamps 1` (segment muxer ignores it when `-copyts` is
  set)
- Strip `-segment_list` URL entirely so PMS falls back to filesystem
  scan (PMS doesn't actually fall back; CSV is required for serve)

## Operational notes

**To verify seek works on a new client class:**

1. Watch worker logs while seeking. The change list should include
   `seek-offset:captured=<N>s` for HLS and DASH paths. Absent → seek
   wasn't detected and the rewriter's seek codepath isn't running.
   (Note: `force_key_frames:offset-by-seek` retired 2026-05-14;
   jellyfin-ffmpeg 7.1 handles the IDR storm natively.)

2. For HLS specifically, check disk: `ls -la /transcode/Transcode/Sessions/*<job-uuid>*/`
   on a worker. Healthy = `media-NNNNN.ts` files of normal size
   (~0.5–2 MB each). Pathological = one giant file followed by normal
   files (= force_key_frames or copyts regression).

3. For DASH, probe a chunk: `cat header chunk-stream0-00632.m4s | ffprobe -v error -show_entries packet=pts_time -select_streams v -of csv -`.
   First packet `pts_time` should match the chunk's place in global
   timeline (e.g. 5048.000 for chunk 632 at 8s segments).

4. Check PMS log (`/config/Library/Application Support/Plex Media Server/Logs/Plex Media Server.log`)
   for `Completed: 200 GET /base/00NNN.ts ... <bytes> bytes`. **`0 bytes`
   on seek chunks** → CSV rewrite not happening (relay
   misconfig or `scaleplex_seg_time=` not appended).

## Seek on HW-decode PGS sessions (FIFO PTS shift)

For HW-decode bitmap sub burn-in the rewriter routes the PGS through a
separate sub pre-render and feeds the main `overlay_vaapi` from a FIFO
(see `REWRITER.md` "Bitmap sub burn-in (HW-decode pre-render path)").
On a seek session there is one extra correction that has nothing to do
with the DASH/HLS machinery above:

- Plex's bitmap HW-decode argv carries `-start_at_zero -copyts`.
  `-start_at_zero` zeroes the **muxer-side** PTS only — it does NOT
  shift the filter input. Verified offline against Avatar Fire and
  Ash AV1: at `-ss 540` the source's first frame arrives in
  `-filter_complex` at `pts_time:540.003`. The FIFO,
  meanwhile, arrives at `pts_time:0+` (its overlay canvas is
  necessarily 0-based and the sub branch is rebased by
  `setpts=PTS-N/TB`). Without compensation `overlay_vaapi` framesync
  pairs FIFO PTS T with main PTS T — cues drift forward by exactly
  `seekOff` seconds.

- The rewriter splices `setpts=PTS+<seekOff>/TB` onto the FIFO branch
  ahead of `format=bgra,hwupload[0]` so both meet on the absolute-PTS
  timeline. **The shift belongs on the FIFO branch, not the main**;
  the `sha-a451466` attempt added `setpts=PTS-STARTPTS` on both the
  `[0:0]hwupload[1]` main branch and the FIFO branch and caused
  constant forward-skipping. Reverted in `sha-3d3efb5`.

**Symptom of the bug, if it ever regresses:** seek into a PGS title,
the displayed cue is a future cue — the offset from dialogue equals
the seek offset.

**Verification:** the rewriter log line for the session must include
`seek-offset:captured=<T>s` AND the live `-filter_complex` must show
`setpts=PTS+<T>.000/TB,format=bgra,hwupload[0]` on the FIFO branch.
