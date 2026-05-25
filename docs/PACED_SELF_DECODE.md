# Paced self-decode for `-map_inlineass` (v1.5.0)

Status: DESIGN + IMPLEMENTATION (2026-05-25). Fork patch `0120`.

## Problem

`vf_inlineass` is fed decoded subtitle cues through a side-channel:
`scaleplex_inlineass_handle_subtitle()` fires from `transcode_subtitles()` in
`ffmpeg_dec.c` whenever the subtitle **decoder runs**, and forwards the
`AVSubtitle` to the bound filter (patch 0100). So cues only flow while the
subtitle stream is actively decoded.

Nothing in the `-map_inlineass` binding makes the stream decode. Today the
rewriter forces it by appending a throwaway **output**:

```
-map <spec> -f null -codec ass    nullfile   # text
-map <spec> -f null -codec dvdsub nullfile   # bitmap/PGS
```

That output sets up a full `demux → subdec → subenc → null-mux` chain on its
own scheduler threads. It is an **independent consumer** of the input with no
throttle gate:

- The canThrottle sleep (patch 0097) lands only in the **video encoder
  thread**. Once PMS asserts throttle the video path backs up, the single
  demux thread blocks on the video queue, and everything (sub included) paces
  to ~realtime — fine.
- But during the **pre-throttle flat-out buffer fill**, the redundant sub
  decode + ass/dvdsub re-encode + null-mux threads compete with the video path
  for the NFS read and CPU at exactly the moment the client buffer is being
  filled. On long embedded-sub 4K titles this shows up as **regular playback
  skips during fill, gone once throttle equilibrates** (Frank, 2026-05-24).
  Sidecar subs (small separate file) don't trigger it; embedded subs (in the
  big mkv) do.

The redundant subtitle *re-encode* to ass/dvdsub is pure waste — its output
goes to `/dev/null`. We only ever wanted the decoder to **run** so the
side-channel hook fires.

## Fix

Decode the `-map_inlineass` stream with a **sink-less decoder** created
directly from the binding — no output stream, no encoder, no muxer. Then drop
the rewriter's null-sink entirely.

Pacing is correct *by construction*:

- The decoder has no downstream sched node, so `sch_dec_send` discards each
  decoded frame immediately; its input queue drains fast and never blocks the
  demux.
- The demuxer is a **single sequential thread**; `demux_send_for_stream` →
  `tq_send` blocks the whole demux when *any* destination queue is full
  (`ffmpeg_sched.c`). The only stream that ever blocks it is video (gated by
  the canThrottle sleep in the video encoder). So the demux advances in file
  order at video pace, and the sub decoder only sees packets up to that point.

No independent consumer ⇒ no extra read-ahead, no competing threads during
fill. This mirrors Plex 6.x's single-threaded loop (one `process_input_packet`
per iter, sub decoded inline with video) within jellyfin 7.x's split-thread
scheduler.

## Why a *sink-less* decoder needs scheduler support

`sch_add_dec()` auto-creates output #0 for every decoder. Two places then
assume an output has a sink:

1. `start_prepare()` rejects any decoder output with `nb_dst == 0`
   ("Decoder output %u not connected to any sink").
2. `sch_dec_send()` returns `AVERROR_EOF` when `o->nb_dst == 0`
   (`(nb_done == o->nb_dst)` with both 0). `process_subtitle()` maps that to
   `AVERROR_EXIT`, which would terminate the decoder thread **after the first
   decoded subtitle**.

So the scheduler gains an explicit *sink-less* concept.

## Implementation (patch 0120)

### `fftools/ffmpeg_sched.{c,h}`
- `SchDec` gains `int sink_less;`.
- New API `void sch_dec_set_sinkless(Scheduler *sch, unsigned dec_idx);`.
- `start_prepare()`: skip the per-output "not connected to any sink" check when
  `dec->sink_less` (the source check still applies — a sink-less decoder must
  still be fed by the demuxer).
- `sch_dec_send()`: when the target output has `nb_dst == 0`, `av_frame_unref`
  the frame and `return 0` (consumed, discarded) instead of falling through to
  the `AVERROR_EOF` result. Universally safe — only sink-less outputs ever have
  `nb_dst == 0` at runtime.

### `fftools/ffmpeg_demux.c` (+ decl in `ffmpeg.h`)
- New public `int ist_inlineass_add(InputStream *ist)`:
  - `ist_use(ist, DECODING_FOR_FILTER, NULL, &src)` — idempotent; creates the
    decoder + `sch_connect(demux_stream → dec_in)` exactly like the normal
    decode path. `src` (the dec output node) is intentionally **dropped** — we
    connect it to nothing.
  - `sch_dec_set_sinkless(demuxer_from_ifile(ist->file)->sch, ds->sch_idx_dec)`.
  - returns `ds->sch_idx_dec`.

### `fftools/scaleplex_inlineass.{c,h}`
- New `int scaleplex_inlineass_setup_decoders(void)`: iterate the binding
  registry, `ist_inlineass_add(b->ist)` for each. Resolving `b->dec_ctx`
  continues to happen in `scaleplex_inlineass_link_graph()` at filtergraph
  config time (the decoder + its `dec_ctx` now exist before that runs).

### `fftools/ffmpeg.c`
- Call `scaleplex_inlineass_setup_decoders()` in `transcode()` immediately
  before `sch_start()` — all inputs/outputs/filtergraphs are parsed by then, so
  every `b->ist` exists with its codec resolved.

### `worker/agent/rewriter.go`
- Stop emitting the `-map <spec> -f null -codec ass|dvdsub nullfile` sink in
  every path that adds it (HW text branch, bitmap branch, honor-HW/SW paths).
  Keep `-map_inlineass <spec>` — that now drives the decode on its own.
- Update tests that assert the null-sink argv.

## Validation (plex-test)

Embedded-sub 4K session (SRT / ASS / PGS, + SW fallback):
- subtitles still render + seek correctly (parity with v1.4.0);
- **no startup I/O burst / playback skips during buffer fill**;
- no extra ass/dvdsub encoder or null-mux in the process's thread list;
- clean exit / no decoder-thread early-termination in logs.

Reference: Plex `plex.c` self-decode wiring; memory
`project_scaleplex_latency_parity_roadmap` (residual decode-sink item).
