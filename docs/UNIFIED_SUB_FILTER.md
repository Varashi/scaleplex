# Unified subtitle burn-in — merged `inlineass` VAAPI branch

> **Status: SHIPPED v1.3.0 (2026-05-24).** Subtitle burn-in moved out of the
> Go agent orchestration (2nd ffmpeg + qtrle FIFO + `overlay_vaapi` framesync)
> into the scaleplex-ffmpeg fork. The prototype was a standalone
> `overlay_sub_vaapi` filter (patches 0112/0113, since dropped); it shipped
> **merged into `vf_inlineass`** as a format-adaptive HW (VAAPI VPP) branch
> alongside the existing SW (FFDraw) path — one Plex-native `inlineass` node,
> HW/SW chosen by the negotiated input frame format (patch **0115**). The
> rewriter emits `inlineass=…:render_height=N[:animated_tier_down=1]` on the
> VAAPI surface for HW sub burn (text + PGS), keeping SW `inlineass` as the
> CPU fallback. The sections below are the design/phasing history that led
> here; see `CHANGELOG.md` (v1.3.0) and `REWRITER.md` for the shipped shape.

## Why

Two motivations converge:

1. **Latency.** The `LATENCY.md` baseline found the *only* meaningful
   time-to-first-segment cost is **embedded-subtitle extraction**
   (9–28s on long multi-sub 4K remuxes, re-paid on every seek). That
   cost is an artifact of the pre-render path's architecture: a separate
   `-c:s srt` extraction → `vf_subtitles` (libass loads the whole file
   once at init) → qtrle FIFO. A Go-side windowed-extraction fix is
   fragile (the canvas runs forever + the main holds the FIFO read end,
   so extending mid-session needs a seamless prerender hand-off that
   touches the hard-won overlay/throughput path).

2. **Maintenance surface.** Subtitle optimisations today are mostly *Go
   orchestration of stock ffmpeg filters* (2nd process, FIFO splice,
   `-copyts`/`-probesize 32`/PTS-rebase seek hacks, band parsing,
   render-res math, text/bitmap/animated branch sprawl). One fork filter
   collapses all of it.

## The two existing paths and their opposite weaknesses

| path | extraction latency | throughput | lives in |
|---|---|---|---|
| **inlineass** (animated/fallback) | **none** — streams cues incrementally | ~2 streams (per-frame CPU blend) | fork `vf_inlineass` 0099-0101 |
| **pre-render overlay** (static SRT/ASS/PGS) | **9–28s** (extract + load-once) | ~5 streams (GPU composite, rate-bounded) | Go agent `subprerender.go` + rewriter |

**Key fact (verified in the fork source):** `vf_inlineass` already feeds
libass **incrementally** via `ass_process_chunk` from the main demuxer on
the decoder thread (`avfilter_inlineass_append_data`, patch 0100). It has
zero extraction latency *by construction*. Its only weakness is that
`filter_frame` calls `ass_render_frame()` + CPU `ff_blend_mask` **per
video frame** — that per-frame cost is the ~2-stream ceiling.

The pre-render path is the mirror image: it gets ~5 streams precisely
because it renders the cue once and composites a rate-bounded qtrle
overlay on the GPU — but it pays the extraction + load-once latency to do
so.

**The unified filter takes the good half of each:** inlineass's
incremental feeding (already built) + the pre-render's render-once +
rate-bounded GPU composite. Extraction latency gone by construction;
throughput kept.

## Architecture: `overlay_sub_vaapi`

A VAAPI sub-burn filter, fed by the existing inlineass `append_data`
plumbing, that internalises the whole dance:

- **Input:** the decoded **video** on VAAPI surfaces (HW frames), same as
  the main transcode already produces (`hwupload`/HW decode output).
- **Subtitle feed:** the embedded/sidecar sub stream, mapped via the
  existing `-map_inlineass` path (0100) and fed incrementally with
  `ass_process_chunk`. **No `-c:s srt` extraction step. No `.srt` file.
  No `vf_subtitles` load-once. No FIFO. No second ffmpeg process.**
- **Render-once:** call `ass_render_frame(renderer, track, time_ms,
  &detect_change)`. The 4th arg (currently passed `NULL` in
  `vf_inlineass`) reports whether the rendered image changed since the
  last call. Only when it changes do we (a) re-rasterise the cue to an
  ARGB staging buffer and (b) re-upload it to a **cached VAAPI overlay
  surface**. Between changes the cached surface is reused — this is the
  render-once-per-cue that gives the pre-render path its throughput.
- **Composite:** blend the cached overlay surface onto the video surface
  with VAAPI (the same VPP blend `overlay_vaapi` uses), rate-bounded to
  the video frame rate (not the libass cadence).
- **Band auto-crop:** libass's `ASS_Image` list carries each cue's
  bounding box. The filter knows the true cue extent per frame, so it can
  upload/composite only the dirty band — **kills `subparse.go` cue
  parsing, the agent-resolve band machinery, and the `__SP_BAND*`
  sentinels** (band falls out of the data, no static fallback needed).
- **Render-res cap:** rasterise libass at a capped height and let the
  VAAPI blend scale — folds the `SCALEPLEX_SUB_RENDER_HEIGHT` knob in.
- **Seek:** the filter sees real frame PTS; `ass_flush_events` on a seek
  discontinuity. **Deletes the two-timeline PTS-rebase seek hacks**
  (`setpts=PTS-N`, the `-copyts` FIFO dance — see `SEEK.md`).
- **Text + bitmap + animated in one place:** route by stream codec inside
  the filter. Animated ASS (`\k`/`\t`/`\move`/`\fad`) simply lets
  `detect_change` fire every frame (degrades to the inlineass per-frame
  behaviour automatically — no separate code path). Bitmap (PGS) feeds
  the sub2video bitmap as the overlay surface instead of libass.

### The hard part — the surface-pool overrun (why the split exists)

The pre-render path was split into a separate process specifically
because feeding a sparse overlay into `overlay_vaapi`'s framesync in the
main graph let framesync **hold decoded main-video VAAPI surfaces** while
hunting for an overlay frame to pair, overrunning the AV1 HW decoder's
small fixed surface pool → "Failed to upload decode parameters: 18"
(see `project_scaleplex_av1_decode_corruption`). That is the one genuine
unknown for Phase B and the thing to de-risk first.

`overlay_sub_vaapi` avoids framesync entirely: it is a **single-input
VPP filter** (video in, video out) that owns the overlay surface
internally. There is no second timeline to sync, so no framesync
surface-holding. Each video frame is composited against the *current*
cached overlay surface (whatever cue is active at that PTS) and released
immediately. The overlay surface is a single filter-owned VAAPI surface
(plus a small ring for in-flight composites), not drawn from the
decoder's pool. **This is the core thing the Phase B spike must prove:**
that an internally-owned, render-once overlay surface composited per
frame holds throughput at 4K without touching the decoder pool.

## Phasing

The full Phase B is multi-week C with the surface-pool unknown. Phase A
ships the *latency* win sooner with no risk to the throughput path, and
exercises the incremental-feed-into-the-pre-render plumbing that B also
needs.

### Phase A — incremental-feed pre-render (latency fix, low risk)

Keep the existing 2-process + FIFO + `overlay_vaapi` + qtrle throughput
path **unchanged**. Only replace the pre-render's subtitle *source*:
swap stock `subtitles=<extracted.srt>` (extract + load-once) for a
fork filter that renders onto the transparent `color` canvas while being
fed the embedded sub stream **incrementally** via the 0100 plumbing.

- New/extended filter: a transparent-canvas variant of `vf_inlineass`
  (alpha-preserving overlay onto the `color` canvas instead of opaque
  video; reuse `append_data` for the feed).
- Pre-render argv: `-f lavfi -i color=... -i <source> -discard:v all
  -discard:a all -map_inlineass <sub spec>` instead of reading an
  extracted `.srt`. `-ss <playhead>` jumps the demuxer; cues stream from
  there.
- **Deletes:** `resolveSubFile`'s `-c:s srt` extraction step (the 9–28s),
  the load-once behaviour, and the whole windowed-extraction/hand-off
  problem — the demuxer delivers cues just-in-time at the rate-bounded
  canvas pace.
- **Untouched:** FIFO, `overlay_vaapi`, qtrle, band crop, render-res,
  seek-PTS rebase. Throughput identical to today; only latency improves.
- Risk: low — the fragile VAAPI composite path is not modified.

#### Phase A — first build + test results (2026-05-22, on SKW-Build, CPU-only)

Built `scaleplex-ffmpeg7` with 0108, installed on SKW-Build, ran the
inlineass canvas pre-render against Avatar Fire & Ash (4K, 36 embedded
subs — the 27 s extraction case). Findings:

1. **Core mechanism PROVEN.** `inlineass=alpha=1` fed via `-map_inlineass`
   produces 120 s of pre-render in **1.8 s** (vs 27 s extraction) and the
   cues render correctly (content frames match REF `subtitles=` timing,
   e.g. the 95.8–97.6 s cue). The `alpha` option works (transparent
   canvas, text composited). **The latency win is real and the incremental
   feed is viable.**
2. **Decode-trigger required.** `-map_inlineass` alone produces a BLANK
   output — the fork's port records the binding but does NOT mark the
   bound stream `decoding_needed`, so nothing decodes it → `handle_subtitle`
   /`append_data` never fire. Prod works only because Plex's argv co-emits
   a decode sink: a 3rd output `-map 1:s:0 -c:s ass -f null <nullfile>`
   that transcodes the sub, forcing the decode that feeds inlineass. The
   pre-render argv must replicate that sink (OR — cleaner — fix the fork so
   `-map_inlineass` self-marks `decoding_needed`; the omitted part of
   Plex's fftools/plex.c).
3. **BLOCKER — libass assertion abort.** With the decode sink, it renders
   ~25 correct frames then SIGABRTs:
   `libass/ass.c:127: ass_alloc_event: Assertion 'track->n_events <=
   track->max_events' failed`, via `avfilter_inlineass_replay_chunk`
   (0101 pending-buffer) → `ass_process_chunk` → `ass_alloc_event`, on the
   `dec1:3:srt` decoder thread. Classic unsynchronized `ASS_Track`
   mutation. `replay_chunk` + `append_data` both take `inlineass_lock`, so
   the race is subtler — likely the pending-buffer replay path (triggered
   because the sub decoder starts before the filtergraph links in the
   canvas+decode-sink layout) double-touches the track, or another libass
   entry (render/encoder-side ass) isn't under the same lock. Not
   reconfig-related (persists with an argb canvas, no auto_scale).
   **Must fix before plex-test** (deploying would crash identically).

   **Diagnosis (2026-05-22):** the trigger is the **unbounded decode
   sink**. `-map 1:s:0 -c:s ass -f null` has nothing rate-limiting it, so
   the sub decoder drains the ENTIRE movie's cues instantly (logs: canvas
   stuck at `frame=50` while the decoder's `time=` climbs to 1:10:00+),
   flooding the inlineass `ASS_Track` with ~2465 events VIA `replay_chunk`
   while `filter_frame`/`ass_render_frame` runs concurrently on the same
   track for the longer `-t 120` render. `-t 10` (filter idle after 50
   frames) never crashes; `-t 120` (long overlap) does.
   **Workaround confirmed:** bounding the decode sink to the output window
   (`-t 120` on BOTH outputs) → exit 0, correct output. Decoder stays in
   step with the canvas → no flood → no crash.
   **But the underlying race remains** (all libass calls *appear* to take
   `inlineass_lock` — filter_frame, append_data, replay_chunk all lock —
   so the exact unlocked path isn't yet identified; needs a tsan/instrumented
   build, or there's a libass realloc-vs-render hazard the lock doesn't
   cover). And the unbounded sink would flood in a REAL session too (the
   `-f null` sink isn't rate-limited by the FIFO consumer), so "just bound
   it" isn't a full fix — the sub decode must be paced with the canvas.
   **Proper fix direction:** make `-map_inlineass` self-decode the bound
   stream, paced by the filter's consumption (no separate unbounded null
   sink) — the omitted part of Plex's fftools/plex.c — which also removes
   finding #2's decode-sink requirement. That's the real Phase A fork work.
   Test GPU-free on SKW-Build; VAAPI parts via media-toolkit
   ([[reference_skw_build_node]]).

   **DEEP DIAGNOSIS (2026-05-23) — it's HEAP CORRUPTION, not a lock race.**
   Built `0108` (alpha) + `0109` (lock config_input/uninit — 0100 had
   missed those libass entries; correct hardening, KEPT) + `0110` (temp
   instrumentation, since removed). The instrumentation logged thread +
   track ptr + n_events/max_events at every `replay_chunk` and
   `filter_frame`:
   - Same track throughout; FF (filter thread) + RC (decoder thread)
     alternate cleanly UNDER the lock — `n` grows 2→3→…→9 normally — then
     the next RC reads `n=-320676469 max=-532588060` = the track struct's
     memory is **garbage**. So the lock IS serialising the logged calls;
     the corruption comes from OUTSIDE them.
   - Two filter instances exist (probe `0x560f…` + live `0x7a9d…`, the
     normal probe→live graph reconfig that 0101 handles). Only the live
     instance ever renders/feeds; the live track gets clobbered.
   - `0109` (locking uninit) did NOT fix it → not the uninit-vs-feed
     concurrency. Likely mechanism: the **probe instance's libass teardown
     (`ass_library_done`) frees process-global libass state**
     (fontconfig/FreeType/font-provider — exactly what 0100's comment
     flagged as non-reentrant) that the LIVE instance still uses; a lock
     serialises but can't fix a logical free-then-use ACROSS the two
     instances. The corruption lands inside `libass.so`.
   - storage_size mismatch ruled out (subrip reports 0 dims →
     `set_storage_size` is guarded off).
   **To root-cause definitively needs ASAN of libass itself** (the OOB is
   inside libass.so; ffmpeg-only ASAN won't see it) — a big detour with
   uncertain payoff. **The trigger is the two-instance double-config**
   inherent to the canvas + separate-sub-input + decode-sink layout, which
   prod's main-video inlineass path never exercises.

   **RESOLVED 2026-05-23 — it was a use-after-free; fixed by `0111`.**
   Valgrind memcheck (no rebuild needed — the tiny sub-only repro) caught it
   precisely: `avfilter_inlineass_replay_chunk` (decoder thread) reads a
   binding's `flt_ctx` that was **freed by `avfilter_graph_free`** — the
   PROBE filter context, torn down during the probe→live reconfig — in the
   window before the live `link_graph` resets `flt_ctx`. (Natively this
   surfaced as the libass `n_events` abort because the freed context's
   track read as garbage; hence the earlier wrong "heap corruption / lock"
   guesses.) `0109`'s uninit lock couldn't help — the lock doesn't clear a
   stale pointer.
   **Fix `0111-inlineass-fix-uaf-reconfig`:** `vf_inlineass` gains an uninit
   callback (`avfilter_inlineass_set_uninit_cb`); `scaleplex_inlineass`
   registers it at `link_graph` time; `uninit()` invokes it BEFORE teardown
   to NULL any binding whose `flt_ctx == ctx` (under `bindings_lock`,
   serialising with `handle_subtitle`; lock order safe — uninit takes
   bindings_lock then releases before inlineass_lock). After the clear,
   `handle_subtitle` buffers instead of touching freed memory; the live
   `link_graph` drains the buffer on rebind.
   **Validated 2026-05-23:** original crashing repro exit 0; valgrind
   `ERROR SUMMARY: 0 errors`; cold-start 2.9 s (content matches the
   `subtitles=` REF exactly, 10/600) vs ~27 s extraction; seek (`-ss 1800`)
   2.4 s, exit 0. **NOTE: 0111 likely fixes a latent bug on the PROD
   main-video inlineass path too** (it also hits probe→live reconfigs).

   **Phase A patch set (all KEEP): `0108` (alpha), `0109` (lifecycle
   locks), `0111` (UAF fix).** Remaining Phase A work: wire it into
   `subprerender.go` (inlineass canvas + decode-sink, drop the `-c:s srt`
   extraction), optionally bound/replace the decode-sink (self-decode), then
   end-to-end on plex-test.

#### Phase A implementation status (2026-05-22)

- **Filter:** `patches/0108-inlineass-alpha-canvas.patch` adds an `alpha`
  option to `vf_inlineass` (selects alpha pixfmts + `FF_DRAW_PROCESS_ALPHA`
  so `ff_blend_mask` writes the alpha plane). Needed because the stock
  filter was built to overlay opaque video; the transparent-canvas
  pre-render needs alpha (the same reason `subtitles=` needs `alpha=1`).
  Default 0 → per-frame inlineass path unchanged. NOT yet built/tested.
- **Agent argv (planned):** replace, in `subprerender.go`'s text path,
  `-f lavfi -i color=... -vf "...subtitles=<extracted.srt>:alpha=1..."`
  with `-f lavfi -i color=... -i <source> -discard:v all -discard:a all
  -map_inlineass <subspec> -vf "...inlineass=alpha=1:...,format=argb"`,
  and **delete `resolveSubFile`'s `-c:s srt` extraction** entirely. The
  embedded sub stream is decoded + fed incrementally (0100 plumbing),
  `-ss <playhead>` jumps the source demuxer → cues stream just-in-time,
  no full-file walk, no load-once, no FIFO hand-off.

> **OPEN RISK to validate on first test — feed-timing race.** Today's
> pre-render burst-fills the lavfi canvas at 15–30× realtime to pre-fill
> the FIFO; that is only safe because `subtitles=` has ALL cues loaded up
> front. With incremental feed (sub decoded on the decoder thread, canvas
> rendered on the filter thread, decoupled by the scheduler queue), a
> burst-filling canvas can render frames at PTS T before the cue at T has
> been demuxed/fed → missing/late subtitles in the pre-filled segments.
> First test must check, on a 4K embedded-sub session, whether the sub
> decoder keeps ahead of the burst. If not, options: (a) cap the
> pre-render rate (e.g. modest read-ahead via `-re`-like pacing on the
> canvas) so it can't outrun the feed — costs some first-segment speed but
> the near-playhead cues with `-ss` are fast; (b) gate the canvas on a
> small "cues fed up to PTS+lookahead" watermark inside the filter. Decide
> empirically; do not ship Phase A until cues are verified present across
> a seek + cold start at 4K.

### Phase B IMPLEMENTATION PLAN (chosen 2026-05-23 — the build target)

Decided to build the purpose-built filter after the inline-feed-on-canvas
validation showed it fixes cold-start but accumulates bugs (UAF [fixed
0111], red color, startup skips) because `inlineass` is built for opaque
YUV video overlay, not a transparent canvas. `overlay_sub_vaapi` does it
right and folds in every learning below.

**Base:** upstream `libavfilter/vf_overlay_vaapi.c` + `vaapi_vpp.c`
(FFmpeg 7.1.3, in the deps image). It already VPP-blends an overlay VAAPI
surface onto a main VAAPI surface — but as a TWO-input framesync filter
(the surface-pool-overrun trap). Our variant is **single-input**.

**Architecture (`vf_overlay_sub_vaapi`, single video in → video out):**
- Input: the HW-decoded video on VAAPI surfaces (what PMS already produces
  in HW-decode mode). No second filtergraph input → **no framesync** → no
  holding the AV1 decoder's surface pool (the error-18 lineage,
  [[project_scaleplex_av1_decode_corruption]]).
- Subtitle feed: the existing `-map_inlineass` plumbing (patch 0100) +
  `ass_process_chunk` incremental feed + the process-wide libass lock
  (0100/0109). **No `.srt` extraction** → cold-start fixed (proven by the
  inline-feed test: ~1 s vs 18 s). **Paced by construction**: the sub is
  decoded from the SAME demuxer as the video (like the prod inlineass
  main-video path), so it advances at the video's pace — **no unbounded
  `-f null` decode sink** → no startup I/O burst (fixes the skips).
- Per output frame: `ass_render_frame(renderer, track, time_ms, &change)`.
  On `change`: rasterise the cue to an **ARGB CPU buffer with a plain RGBA
  blend — NO vsfilter/`calculate_mangle_table` colour mangle** (that mangle
  on a non-YUV target is what made the inline-feed subs RED), then upload
  to a **filter-owned, cached VAAPI overlay surface**. On no-change: reuse
  the cached surface (render-once-per-cue → keeps throughput).
- Composite: VAAPI VPP blend (vaapi_vpp, as `overlay_vaapi` does) the
  cached overlay surface onto the video surface; output + release the video
  surface immediately. Band auto-crop from the libass `ASS_Image` bbox
  (kills `subparse.go`); render-res cap folds into the rasterise size.

**This single filter solves all three issues from the inline-feed test:**
latency (incremental, no extraction), colour (controlled RGBA rasterise →
upload, no mangle), throughput (render-once + cached surface + single-input
VPP, no framesync, no decode-sink burst).

**Phasing (each a build→media-toolkit GPU test→plex-test cycle):**
1. **DE-RISK SPIKE — DONE 2026-05-23, PASSED (patch `0112`).** A minimal
   `vf_overlay_sub_vaapi` composites a filter-owned STATIC green band
   (BGRA, uploaded via `av_hwframe_transfer_data`) onto 4K HW video,
   single-input VPP, no framesync. Tested on a plex-test gpu-worker
   (worker image `vaapi-spike`): 4K AV1→HEVC WITH the filter **~5.2×**,
   rc=0, **ZERO error-18 / surface-pool / Invalid**; baseline (no filter)
   5.39× → composite costs ~3–4% (essentially free); composite verified
   (bottom band avg BGRA `(30,202,26)` green, top unaffected). **The
   single-input VPP model holds 4K throughput with no decoder surface-pool
   overrun — the one real unknown is cleared; the rest is assembly.**
2. **libass render → cached VAAPI surface — DONE 2026-05-23, PASSED
   (worker image `vaapi-step2b`).** `filename=<.ass>` load; `ass_render_frame`
   once-per-cue-change → premultiplied **BGRA** (NO vsfilter mangle — fixes
   the red-subs bug: `p=(c*a + p*ia)/255`, `a=op*src/255`, `op=255-(color&0xFF)`)
   → `av_hwframe_transfer_data` into a cached VAAPI surface → single-input VPP
   composite (`VA_BLEND_PREMULTIPLIED_ALPHA`, full-frame). Validated 4K
   AV1→HEVC (Avatar, embedded SRT→.ass):
   - **Colour correct** (white text, dark outline, bottom-centre — screenshot).
   - **Throughput ~5.2×** (5.16–5.23×), ≈free vs 5.39× baseline.
   - **Initial play (`-copyts`, no `-ss`) rc=0 end-to-end** through hevc_vaapi,
     incl. a real cue render at 95.8s.
   - Filter output itself is sound: `filter→hwdownload→null` is rc=0 even on
     seek.
   - ~~KNOWN EDGE: `-copyts -ss N` segfaults hevc_vaapi~~ **RESOLVED
     2026-05-23 — TEST ARTIFACT, not a bug.** The crash was `-t` combined
     with `-copyts -ss N` (a duration that predates the 555s copyts timeline
     → ffmpeg feeds the encoder garbage). It reproduces with **no sub filter
     at all** (`scale_vaapi` alone), any source codec, HW or SW decode — so
     it's neither this filter nor an AV1 issue. Swap `-t` → `-frames:v` or
     `-to` and every seek mode is rc=0 (file+seek, `-to`+seek, feed+seek+
     decode-sink), with the cue rendering correctly at the seek point
     (grab @555.7s "- Sully is still out there."). overlay_sub_vaapi is
     seek-correct; nothing to fix.
3. **Incremental `-map_inlineass` feed — DONE 2026-05-23, PASSED (patches
   `0112` step-3 + new `0113`, worker image `vaapi-step3`).** `filename=` now
   optional; with no file the filter creates an empty `ass_new_track` and is
   fed live by the existing fftools `-map_inlineass` plumbing. Implemented:
   filter exposes `avfilter_overlay_sub_vaapi_{replay_chunk,add_attachment,
   set_fonts,set_storage_size,set_uninit_cb}` (mirrors vf_inlineass 0099-0101/
   0111) in a new `vf_overlay_sub_vaapi.h`; a file-scope `osv_ass_lock`
   serialises the decoder-thread feed against the filter-thread render; `0113`
   teaches `fftools/scaleplex_inlineass.c` to also bind `overlay_sub_vaapi`
   filters and dispatch all 6 call sites by `InlineAssBinding.is_osv`. Shared
   `inlineass_filter_uninit_cb` (same signature). Validated 4K AV1 Avatar,
   embedded subrip 0:3:
   - **Feed works:** `handle_subtitle` delivers cues, buffer→drain across the
     probe→live reconfigure, `dec_ctx` binds. rc=0, no crash/UAF.
   - **Fed cue renders correctly** — frame grab at the first cue (48.99s)
     shows "Come on. Bro!" white + dark outline, bottom-centre (same render
     path as step-2 file mode, now sourced from `ass_process_chunk`).
   - **No separate extraction pass** — cues arrive just-in-time during the
     transcode; the pre-render path's 9-28s `-c:s srt` extract + load-once is
     gone. This is the latency win.
   - **KNOWN (decode-sink burst):** the fftools `-map_inlineass` mechanism
     still needs a `-map <sub> -c:s ass -f null` decode sink to trigger sub
     decoding; that sink reads ahead → a transient throughput stall
     (5.4×→0.04×→recovers ~2.5×). Same behaviour vf_inlineass has,
     throttle-bounded in prod. The decode-sink-free end-state (feed libass
     from the demuxer *inside* the filter, no fftools sink) is the remaining
     future item — that is what makes this filter beat inlineass on both
     latency AND throughput.
4. Band (libass bbox). Also: kill the decode sink (feed libass from the
   demuxer inside the filter) for the full throughput win.
   - **Bitmap / PGS route — DONE 2026-05-23, PASSED (patches `0112`+`0113`,
     image `vaapi-pgs`).** Additive to the libass text path (a stream is text
     XOR bitmap). `0113`: a bitmap-codec stream (PGS/DVD/DVB/XSUB) on an
     overlay_sub_vaapi binding routes the whole decoded presentation (incl.
     empty = clear) to `avfilter_overlay_sub_vaapi_replay_bitmap` instead of
     the per-rect text path. Filter: `replay_bitmap` (decoder thread) blits
     the palettised rects to **premultiplied BGRA** at the sub canvas size
     (decoder presentation dims, default 1920×1080) into a host staging buf
     under `osv_ass_lock`; `refresh_bitmap` (filter thread) uploads to a
     cached `bmp_overlay` surface and gates `have_bmp` by the display window;
     `filter_frame` composites bitmap-or-text and VPP-upscales canvas→frame.
     Validated 4K AV1 Inception, embedded PGS 0:2 (English SDH): 30
     presentations routed (show rects=1 / clear rects=0), `dec_ctx` bound,
     **burned cue renders correctly** (grab @56.5s "[CHILDREN LAUGHING]",
     bottom-centre, canvas→4K upscale), rc=0 no crash. Decode sink for bitmap
     = `-map <sub> -c:s dvdsub -f null` (PGS can't encode to ass). A
     presentation landing before the filter binds (probe→live) is dropped —
     PGS resends the next display set; only affects t≈0.
   - **Seek — DONE 2026-05-23, no code needed.** overlay_sub_vaapi is already
     seek-correct in file AND feed mode (`-copyts -ss` → rc=0, cue renders at
     the seek point). The earlier "seek crash" was a `-t`+`-copyts` test
     artifact (see step-2 note), not a real issue. No setpts dance needed —
     the filter just copies input PTS through `av_frame_copy_props`.
   - **Render-res cap — DONE 2026-05-23, PASSED (patch `0112`, image
     `vaapi-step4a`).** `render_height` option (default 1080): libass
     rasterises at `ow×oh` where `oh = render_height` (capped, even) and
     `ow = in_w·oh/in_h` (aspect-preserving, even); the small overlay surface
     is VPP-upscaled to the full frame in `filter_frame` (overlay
     `output_region` = full output, surface = capped). `render_height<=0` or
     `>= in_h` → no cap. Folds in the old `SCALEPLEX_SUB_RENDER_HEIGHT` knob.
     Validated 4K Avatar: log `render 1920x1080 -> output 3840x2160`; cue
     renders at correct bottom-centre position/size (matches uncapped); a
     cue-dense 13s window benched **−25% user CPU (0.507s vs 0.672s) and
     −70 MB maxrss (273 vs 344 MB)** vs full-4K render — compounds at
     concurrency, which is the point.
5. Rewriter: swap Plex's `inlineass`/sub-map for `overlay_sub_vaapi=…`;
   DELETE `subprerender.go` orchestration, `band.go`, `subparse.go`, FIFO
   splice, seek-PTS hacks, the inline-feed flag path, render-res argv math.

**Patches:** `0112-overlay-sub-vaapi.patch` (the filter + `vf_overlay_sub_vaapi.h`
+ register; steps 1-3) + `0113-overlay-sub-vaapi-feed.patch` (fftools
`scaleplex_inlineass.c` dispatch). KEEP `0111` (UAF — valuable for the existing
inlineass path regardless). `0108` (alpha) + the Go inline-feed path become
redundant once the filter lands (keep as fallback until then). All four
(0108/0109/0111/0112/0113) are uncommitted working-tree on `main`.

**Build/test loop:** SKW-Build builds the deb; **media-toolkit pod**
(`gpu.intel.com/i915` + `/dev/dri` + vainfo) runs ad-hoc 4K VAAPI tests
WITHOUT a plex-test deploy (drop the deb in); plex-test for e2e
([[reference_skw_build_node]]). Multi-session effort.

### Phase B — full `overlay_sub_vaapi` (original sketch)

Replace the entire 2-process orchestration with the single VPP filter
above. **Deletes:** `subprerender.go` orchestration, FIFO splice +
`-copyts`/`-probesize 32`, band parsing (`subparse.go`, `band.go`,
`__SP_BAND*`), render-res math, seek-PTS rebase, and the text/bitmap/
animated rewriter branch sprawl. Rewriter shrinks to "swap Plex's
`inlineass`/sub map for `overlay_sub_vaapi=...`".

Gate behind `SCALEPLEX_SUB_FILTER=vaapi` (default off) until soak-tested;
keep the Phase A / current path as the fallback.

## De-risk first (Phase B spike)

Before committing to the full filter, prove the surface-pool claim:
a minimal `overlay_sub_vaapi` that composites a *static* filter-owned
overlay surface onto 4K HEVC/AV1 HW-decoded video, single-input VPP, and
measure that it sustains ~5 streams flat-out with **zero** error-18 and
no decoder-pool pressure (per `feedback_validate_scaleplex_opts_at_4k` /
`feedback_flatout_is_cluster_capacity`). If that holds, the rest is
assembly; if not, Phase A still ships the latency win and we keep the
two-process composite.

## Options surface (target)

```
overlay_sub_vaapi=
  render_height=1080      # libass rasterise cap (folds SUB_RENDER_HEIGHT)
  : si=<idx>              # embedded sub stream index (or fed via map)
  : force_style=...       # passthrough of Plex inlineass force_style
  : fontsdir=... : fontconfig=...
```

## Build / test loop

- Fork builds a `scaleplex-ffmpeg7` deb: `scaleplex-ffmpeg/build.sh`
  consumes the GHCR deps image (`scaleplex-ffmpeg-deps:<tag>`) and runs
  `dpkg-buildpackage`; ~5–10 min per patch iteration.
- **Local build is currently disk-blocked** (deps image 3–5 GB; dev box
  `/` ~2 GB free). Iterate via CI (GHA build → deb → worker image →
  `plex-test` DaemonSet), then validate at 4K on an idle `plex-test`
  gpu-worker before prod.
- Worker image consumes the deb at `worker/Dockerfile`.

## What this deletes when done (Phase B)

`subprerender.go` (orchestration), `band.go`, `subparse.go`,
`subparse_test.go`, the `SubPrerenderSpec` band/render/seek fields, the
`__SP_BAND*` sentinels + `PatchMainArgsBand`, the FIFO/`-copyts`/
`-probesize 32` splice in `rewriter.go`, the SRT-vs-ASS-vs-PGS branch
sprawl, and the `SCALEPLEX_SUB_RENDER_HEIGHT` argv plumbing — replaced by
filter options. The rewriter keeps only "detect burn-in request → emit
`overlay_sub_vaapi`".

## Related memory

`project_scaleplex_unified_sub_filter`, `project_scaleplex_latency_parity_roadmap`,
`project_scaleplex_av1_decode_corruption` (the surface-pool lineage),
`project_scaleplex_inlineass_port`, `project_scaleplex_srt_to_pgs_gpu`,
`project_scaleplex_pgs_prerender`, `feedback_validate_scaleplex_opts_at_4k`.
