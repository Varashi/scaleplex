# First-frame latency budget

> **HISTORICAL / design doc.** This was the pre-implementation latency
> design — targets, levers, and a v1/v2 split. It's kept for the
> rationale behind the worker pre-warm, adaptive probesize, no-LOCAL_RELAY
> and pod-startup design. Levers 1-5 + 7 shipped in v1.0; levers 6 + 8
> (partial-segment HLS, pre-rendered hot content) were deferred and not
> built. The pod-startup rules below are still current design constraints;
> the rest is intent, not a description of the shipped system.

**Goal:** play within **2-3 seconds** of click on LAN clients. Stretch
target: <1.5s for sessions where the worker is warm.

**Companion goal — fast pod startup for every scaleplex pod.** Worker,
orchestrator, and shim images all aim for a 2-second `Pending → Ready`
budget on a node that has the image cached. Levers used everywhere:

- **All deps baked into the image.** No initContainer downloads (the
  clusterplex iHD-driver init container was a 3-8s startup tax we will not
  reintroduce). This includes ffmpeg, iHD VAAPI driver, libass, fonts.
- **Spegel mirrors images cluster-wide.** First pull on a node hits ghcr,
  every subsequent pod pulls from a peer node — sub-second pulls cluster-
  wide once one node has the layer.
- **Tagged by digest / sha-XXX, never `:latest`.** Mutable tags + Spegel
  is a footgun (see `feedback_spegel_stale_tag_digest_pin.md`).
- **Liveness vs readiness split.** `/healthz` is up the moment HTTP binds;
  pre-warm runs in the background and `/readyz` only flips green after it
  finishes. The kubelet won't kill us mid-warm and Service endpoints don't
  receive traffic until we're truly hot.
- **Static Go binary, no CGO.** `scaleplex-agent` is `CGO_ENABLED=0` so
  process startup is single-syscall ELF load, no glibc/musl version dance.
- **`tini` as PID 1** to reap zombies cleanly without a full init system.

## Measured baseline — scaleplex in prod (2026-05-22)

> **CURRENT, measured.** This supersedes the speculative tables below for
> the shipped system. Method: harvested the unconditional worker log
> markers (`rewriter applied` → `agent band resolve` / `spawned subtitle
> pre-render` → `spawned ffmpeg pid` → `first segment ready`) across all 3
> prod + 3 plex-test workers, plus controlled extraction timing on the
> idle media-toolkit pod (NFS media, jellyfin-ffmpeg).

Time-to-first-segment (TTFS) splits into **two** parts:

1. **Extraction (pre-spawn)** — `rewriter applied` → `spawned ffmpeg`. This
   is the embedded-subtitle extraction step in `resolveSubFile`
   (`-vn -an -i SRC -map SPEC -c:s srt`). It is the ONLY part that varies,
   and it dominates the embedded-subtitle paths.
2. **Pipeline fill** — `spawned ffmpeg` → first segment file. This is the
   `scaleplex_worker_first_segment_seconds` metric. **≈ 1s (0.5–2s) for
   every path**, with libass first-render folded in. Not a target.

| sub type | extraction (pre-spawn) | pipeline fill | cold TTFS | seek |
|---|---|---|---|---|
| no-sub | 0 | ~1s | **~1s** | ~1s |
| sidecar SRT | 0 (file on disk) | ~1s | **~1s** | ~1s |
| PGS / bitmap | 0 (read source direct, no extract) | ~1s | **~1s** | ~1s |
| embedded SRT — TV episode | 1–5s | ~1s | **2–6s** | re-pays extraction |
| embedded SRT — long multi-sub 4K | **8–27s** | ~1s | **9–28s** | **re-pays FULL extraction every seek** |
| embedded ASS | = embedded SRT (flattened `-c:s srt` first) | ~1s | same | same |

**The bottleneck is embedded-subtitle extraction**, and its cost is
`∝ duration × sub-stream-count × interleave sparseness` — **not** raw file
size. Examples (controlled, idle worker):

| file | size | embedded subrip streams | duration | full extract |
|---|---|---|---|---|
| Ghosts (US) S05E21 | 2.1 GB | 2 | 22 min | 1–5s (live) |
| F1 The Movie (2025) | 3.4 GB | 61 | ~2.5 h | 8.3s (warm) |
| Avatar: Fire and Ash | 8.7 GB | 36 | ~2.5 h | 27.6s (cold), 23s (live) |

Long movies with many embedded SRT tracks are the worst case (a single
stream's packets are sparsely interleaved across the whole container, so
the demuxer walks every cluster). PGS, sidecar, no-sub, and libass-init
are all ~1s and are **not** worth optimising.

### Fix: windowed extraction (validated, not yet built)

Extract only the cues near the playhead instead of the whole file. The
working recipe (measured): **input-side `-ss <playhead>` (fast Cues-index
seek) + OUTPUT-side `-to`/`-t <window>` placed AFTER `-map` (bounds the
write).** Output-side placement is required — an input-side `-to` (before
`-i`) does NOT early-stop subtitle extraction.

| pattern | time | notes |
|---|---|---|
| full extract (today) | 8–27s | reads/walks whole container |
| `-to 120` output-side (cold start) | **0.40–0.53s** | reads only file front |
| `-ss <T>` + `-t 120` output-side (seek) | **0.34–0.55s** | Cues-index jump |

~16–65× faster, cache-independent (window emits only that window's cues —
verified by cue counts; F1 cold-window 0.34s beat warm-full 8.3s).

### Simplest fix (SHIPPED in the agent): `-discard` extraction

`resolveSubFile` extracted with `-vn -an`, which are OUTPUT options — the
demuxer still parses every video/audio packet while walking to the
interleaved subtitle blocks. Switching to input-level `-discard:v all
-discard:a all` (skip A/V at the demuxer) cut extraction **WARM** from
32.6 s → 1.4 s (identical 2465 cues), and the bitmap pre-render already
used `-discard` for this reason.

> **CORRECTION (plex-test, 2026-05-23): `-discard` does NOT fix
> cold-start.** On the worker with a COLD NFS cache the same Avatar
> extraction took **18 s**, not 1.4 s. `-discard` only saves the A/V
> *decode* CPU — the demuxer still has to READ the whole file's clusters
> (8.7 GB ÷ ~480 MB/s NFS ≈ 18 s) to collect every interleaved subtitle
> packet, because one stream's packets span the whole timeline. So
> `-discard` ~halves cold (18 vs 32 s) and is a big win on warm/re-reads,
> but the *synchronous whole-file* extraction still blocks first-segment
> ~18 s cold on a big multi-sub remux. The 1.4 s was a warm-cache
> artifact.

**The real cold-start fix is reading LESS of the file**, not skipping A/V
decode: either windowed extraction (`-ss`+output-`-to`) or the inline-feed
path (incremental — first cues from the file front, the rest in the
background, so first-segment doesn't wait for the full read). Validated on
plex-test: inline-feed spawns in ~0 s vs `-discard`'s 18 s.

### Inline-feed (fork) path — the strategic alternative

The non-trivial part is the **window lifecycle**: the pre-render consumes
one `.srt` for the whole session, so a finite window must be extended as
playback advances (extract a generous first window, e.g. 300–600s, and
extend lazily; a seek extracts a fresh window at the new playhead). Per
extract is now ~0.5s so re-extraction is cheap. Code: `resolveSubFile` /
`spawnSubPrerender` in `worker/agent/subprerender.go`.

## Where latency comes from in the current clusterplex stack

Measured during the 2026-05-05 sessions:

| stage | typical | notes |
|---|---|---|
| Plex client → PMS handshake | 300-800ms | TLS, ABR negotiation |
| PMS → shim spawn | 50-200ms | fork+exec, env wiring |
| shim → orchestrator | 100-300ms | socket.io connect |
| orchestrator → worker | 50-100ms | task forward |
| **ffmpeg cold start** | **1500-3000ms** | dynamic linker, codec libs, `iHD_drv_video.so` mmap, fontconfig init |
| **`-analyzeduration 20MB / -probesize 20MB`** | **1500-5000ms** | NFS read of source for format detection — biggest single offender on 4K AV1 |
| EAE spawn (audio paths) | 500-1500ms | spawning EasyAudioEncoder; only when audio is transcoded |
| libass + fontconfig + SRT load (sub paths) | 200-800ms | one-shot per session |
| First GOP encode | 200-1000ms | depends on source GOP size |
| NFS write of segment 0 + PMS HLS-style wrap | 200-400ms | with LOCAL_RELAY: + ~1-3s HTTP-hop |
| LG/client buffer fill before playback | 1000-3000ms | client-side; bounded by HLS playlist `target-duration` |

Cold-start total: **5-15 seconds** typical. We saw 19s on first frame
during testing.

## Design levers scaleplex can pull

### 1. Persistent worker daemon (warm libs)

Agent at boot:
- Open `/dev/dri/renderD128` once and hold the fd → first VAAPI session
  doesn't pay device-init cost.
- Run a no-op `ffmpeg -version` → page-cache the binary + dynamic
  dependencies (`libavcodec.so`, `libavfilter.so`, `libva.so.2`, etc.)
- Render a 1×1 px ASS overlay through libass once → fontconfig DB built,
  font files mmap'd, libass internal caches initialized.
- Optionally launch a 1-second dummy HW transcode (`testsrc → null`) so
  VAAPI VPP/encoder programs are JIT-compiled by the iHD driver and cached
  for the next real session.

Expected savings: **800-1500ms** on first session of each worker.

### 2. Skip `-probesize 20MB` / `-analyzeduration 20MB`

Plex sets these conservatively. For known-shape MKV/MP4 the first 1-2MB
is already enough.

The agent ffprobes the source once with progressive probesize (1MB, 4MB,
20MB) and picks the minimum that yields a complete stream listing.
Result cached per inode+mtime (cheap NFS stat). Forwards to ffmpeg with
the smallest that worked.

Expected savings: **1000-3000ms** per session, especially first segment of
large 4K AV1 remuxes.

### 3. No `LOCAL_RELAY`

Workers write to NFS at the path PMS expects. PMS reads from same NFS.
No HTTP relay. Eliminates a per-session connection setup round trip and
the per-segment HTTP/TCP buffering pause we measured causing hitching.

Expected savings: **500-3000ms** on first segment + smoother steady state.

### 4. Speculative ffmpeg spawn

The orchestrator gets the task envelope from the shim. Don't wait for
ack — kick off ffmpeg on the chosen worker immediately (within 100ms).
By the time the shim's response cycle completes and PMS notifies the
client, ffmpeg has already produced segment 0.

Expected savings: **200-500ms** of overlapped pipeline time.

### 5. GOP-aligned segments

Set `-force_key_frames "expr:gte(t,n_forced*<seg_time>)"` so each segment
starts on an I-frame. ffmpeg can flush a complete segment as soon as the
next keyframe boundary, without waiting for full GOP closure.

Expected savings: **300-800ms** on first-segment availability.

### 6. Shorter initial segment

Some HLS implementations support **partial segments** (`#EXT-X-PART`).
First "part" can be 200ms long, served before the full segment is ready.
Plex doesn't surface this through its HLS manifest natively, but if we
generate a Plex-compatible playlist with partial segments, supporting
clients (LG WebOS recent firmware, Apple TV) can start playing within
~500ms of segment 0 starting.

Expected savings: **500-1500ms** on supporting clients. Validation: test
each client class for compatibility.

### 7. Streaming progress notifications, not polling

Orchestrator → shim communication via SSE or websocket push. When ffmpeg
writes segment 0, agent inotify notices it, pushes a "first-segment-ready"
event up the chain. Shim returns to PMS immediately, PMS serves the
segment.

vs current clusterplex which polls ffmpeg's progress URL: cuts ~100-500ms
of polling latency off the critical path.

### 8. Pre-warmed dummy transcode pool (optional, aggressive)

For really hot media (most-recently-played, "currently watching" lists),
keep a pre-rendered first ~30s available. Plex itself can hint via its
recent activity API. When user clicks play on hot content, no transcode
needed for the first 30s — buys time for the real transcode to warm up.

Expected savings: **3-5 seconds** for hot content, zero for cold content.
Requires storage and a hit-rate analysis. Defer to v2.

### 9. HTTP/2 (or QUIC/HTTP-3) Plex ↔ client

Re-uses connection across segment requests, eliminates per-segment TLS
setup. Plex supports HTTP/2 today; LG WebOS Plex needs to honor it.
Out of scope for scaleplex (PMS-side concern), but worth noting.

## Implementation priorities for v1

1. **Persistent worker daemon with library pre-warm** (lever 1) — biggest
   win for the smallest amount of code.
2. **Adaptive probesize via agent-side ffprobe** (lever 2) — independent,
   cheap to implement.
3. **No `LOCAL_RELAY`** (lever 3) — already in the architecture.
4. **GOP-aligned segments** (lever 5) — one-line ffmpeg arg change.
5. **Speculative spawn** (lever 4) — minor orchestrator change.
6. **Streaming progress** (lever 7) — depends on agent ↔ orchestrator
   protocol choice; if HTTP+SSE, almost free.

Defer to v2: partial-segment HLS (lever 6), pre-rendered hot content
(lever 8).

## Measurement plan

Build a `scaleplex-latency` test harness:
- Trigger transcode via Plex API
- Measure timestamps at: shim spawn, orchestrator receipt, worker
  receipt, ffmpeg spawn, segment 0 written, PMS serves segment 0, client
  GET segment 0
- Repeat 10× per scenario (cold worker, warm worker, with/without subs,
  with/without HDR)
- Compare against same-scenario clusterplex baseline

Target metrics for v1:
- Cold worker, 4K HEVC, no subs: **first-frame ≤ 4s** (vs ~10s in clusterplex)
- Warm worker, 4K HEVC, no subs: **first-frame ≤ 2s**
- Warm worker, 4K HEVC + SRT burn: **first-frame ≤ 3s**

## Anti-goals

- Don't optimize for tail latency at the cost of typical latency
  (no 2-pod redundancy in v1).
- Don't break Plex's session bookkeeping for the sake of speed.
- Don't degrade quality (e.g., cap probesize so low ffmpeg picks wrong
  pixfmt) just to save 100ms.
