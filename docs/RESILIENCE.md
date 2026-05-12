# Resilience layer

Three independent mechanisms that keep transcodes well-behaved when
something goes wrong: PMS throttling, GPU-aware routing, and worker
crash recovery. Shipped 2026-05-10.

## 1. PMS canThrottle pass-through

### Why

Plex's transcoder supervisor sets `canThrottle` in progress responses
once the client buffer is far enough ahead of playback that further
GPU/disk burn is wasted (skipped or abandoned segments). Plex's own
ffmpeg fork honours this with a 100 ms-per-packet `usleep` in
`fftools/ffmpeg.c`. The rewriter strips Plex's `-progressurl` and
substitutes `-progress pipe:N` for our stdlib parser, but we re-inject
`-canthrottleurl <relay-url>` pointing at the same endpoint so the
in-binary sleep keeps working.

### How

scaleplex-ffmpeg7 patch 0097 (refined 2026-05-12) ports Plex's
in-binary throttle to jellyfin-ffmpeg 7.x:

- A new global atomic `scaleplex_throttle_delay_us` plus
  `-canthrottleurl <url>` AVOption. `print_report()` fires a one-shot
  HTTP PUT to the URL after each progress block (sub-second cadence),
  parses the response body for the literal `canThrottle`, sets the
  atomic to 100 ms or 0.
- The sleep itself lives in `fftools/ffmpeg_enc.c` `encoder_thread()`,
  after `frame_encode()` returns, gated by
  `ost->type == AVMEDIA_TYPE_VIDEO`. One sleep per encoded video frame
  on the video stream's thread only — the audio + subtitle encoder
  threads run in parallel and DON'T sleep (compounding would over-
  throttle multiplicatively). At 23.976 fps × 100 ms ≈ 2.4 s/wall-s =
  ~28 % throughput when asserted, matching Plex's documented rate.
  Equilibrium hovers ~1.3 × sustained as PMS oscillates canThrottle by
  buffer level.
- The original 2026-05-11 cut placed the sleep in the demuxer thread
  (`ffmpeg_demux.c input_thread`), which fires at queue-drain rate —
  ~1.5–3 × encoder consumption rate. Net throughput collapsed to
  0.1–0.28 × sustained, Android + LG webOS playback skipped because
  the client drained faster than the encoder filled. The encoder-
  thread placement is the correct jellyfin-7.x analogue of Plex's
  single-threaded 6.x sleep.

Worker side (`worker/agent/progress_report.go`):

- Still reads up to 4 KB of each PUT response and looks for the
  `canThrottle` substring. Match flips a per-session `*throttleSignal`
  (atomic int). The Go agent doesn't pause anything anymore — the flag
  exists only so progress PUTs can suppress `&speed=` while asserted
  (Plex Transcoder protocol: "Only pass back speed if we're not
  throttled". Reporting fast progress while throttled would prevent
  PMS from ever clearing the flag).
- Fail-open: any PUT transport / 4xx / 5xx error clears the throttle
  so a flapping PMS doesn't strand a session.

The external SIGSTOP/SIGCONT duty-cycle controller that scaleplex used
to ship before patch 0097 (`worker/agent/throttle.go`) was retired in
commit `a23cebb`. The in-binary sleep is finer-grained, doesn't burn
context switches, and self-paces with the encoder. No more depth-
based tier escalation needed.

## 2. Multi-engine GPU load

### Why

Arc cards (and modern Intel iGPUs) have multiple parallel video
engines (A310: 2× `vcs` + 2× `vecs`). One transcode session uses one
engine; a second session can land on the next without contention.
Routing by session-count alone treats a 2-engine card the same as a
1-engine one, missing real headroom.

### How

`worker/agent/load.go` exposes mean busy fraction across video engines.
Two readers, auto-detected at startup:

- **sysfs** — `/sys/class/drm/card*/engine/{vcs,vecs}*/busy`. Used on
  older Intel iGPUs (Skylake..Tigerlake) and i915 < ~6.5. No
  capabilities required.
- **PMU** — `perf_event_open(2)` against
  `/sys/bus/event_source/devices/i915_*`. Used on Arc + recent kernels
  that dropped the sysfs busy file. Requires `CAP_PERFMON`; the
  Dockerfile applies `setcap cap_perfmon=ep` to the agent so the cap
  lands on a uid-1000 process. Pod manifest must allow PERFMON in the
  bounding set (PSA `privileged` namespace label) and
  `allowPrivilegeEscalation: true` (no_new_privs would block file caps).

`detectReader()` tries sysfs first; falls back to PMU; falls back to
`none` (gpu_load=0, no regression vs pre-Phase-3 routing).

### Routing

`/capability` reports `{gpu_load, gpu_engines}`. Orchestrator
`worker.load()` returns `max(session_saturation, gpu_load)` —
whichever resource is closer to 1.0 drives picks. A 2-engine worker
running 1 session reports gpu_load=0.5 and outranks (loses to) a
0-session worker reporting 0.

### Metrics

- `scaleplex_worker_gpu_engine_load` — gauge, updated every 2 s

## 3. Checkpoint cache + mid-stream recovery

### Why

Worker pod death (k8s rolling update, OOM, network partition) used to
drop every active session — the proxy stream from worker → PMS just
ended, the shim saw ffmpeg "exit", Plex re-spawned the transcode from
the start. The buffered chunks the client had already fetched were
re-encoded. The visible symptom was a several-second stall plus
backwards motion on the seek bar.

### How

Two cooperating pieces:

#### Worker-side checkpoint endpoint

`worker/agent/checkpoint.go` exposes `GET /task/<id>/checkpoint`.
Returns the post-rewrite argv, env, cwd, source path, progress and
manifest URLs, original session-start seek offset, and the highest
segment seq emitted across streams. Pure introspection — read-only,
safe to poll.

`runningTask` carries the spawn state plus an `atomic.Int64 lastSeq`
that `watchAndRenumberChunks` bumps on each successful chunk rename.

#### Orchestrator-side cache + injection

`orchestrator/checkpoint.go`:

- A goroutine spawned alongside each dispatch polls
  `/task/<id>/checkpoint` every 2 s while the PMS-facing request is
  alive.
- Cache key = the Plex transcode-session UUID parsed from
  `-progressurl` (Plex keeps that UUID stable across ffmpeg restarts
  within a session; the per-spawn job UUID changes).
- Cached value: `{original_ss, segment_time, last_seq, updated_at}`.
- 5-minute TTL covers Plex's supervisor restart latency with margin.

On every fresh `POST /task` whose argv carries a known UUID:

- Read `original_ss` from the new request's argv (input-side `-ss`
  before `-i`).
- If it differs from the cached value (user re-seeked deliberately),
  drop the stale hint — no resume.
- Otherwise inject `-ss <last_seq * segment_time>` and `-copyts`
  before `-i`, plus `-segment_start_number <last_seq + 1>` (replacing
  any existing values). The worker resumes exactly where the previous
  one stopped.

`orchestrator/main.go`'s `proxyToWorker` is split:

- `streamFromUpstream` — opens one connection, writes PMS-facing
  status+headers on the first call (gated by `headersWritten`), pipes
  body bytes through, returns when either side errors.
- `proxyToWorker` — outer loop. If `streamFromUpstream` reports a
  failure AFTER any bytes flowed (mid-stream failure), pick a healthy
  alternative worker via `pickRecoveryWorker`, inject resume flags
  from the cache, reopen with headers already sent. PMS-facing
  connection stays open across the swap. Up to 3 attempts.

### Recovery vs initial 503

- 503 from the first dial → caller falls through to next worker
  (existing behaviour).
- Mid-stream failure → recovery loop kicks in, picks alternative
  worker, injects resume flags, reuses headers.
- PMS write failure → terminal, no recovery (client already gone).

### Metrics

- `scaleplex_orch_dispatch_total{outcome="resumed"}` — request landed
  on a cached resume hint
- `scaleplex_orch_dispatch_total{outcome="recovered"}` — mid-stream
  swap fired
- (throttle state is no longer exposed as a worker metric — ffmpeg-
  internal sleep doesn't surface through the agent. Look at the
  agent's per-session log lines `throttle ON/OFF` (debug-level) or
  ffmpeg stderr `speed=` if you need to see it.)

## 4. Mid-stream rebalance — PARKED

Phase 4c (proactive worker swap when load delta exceeds a threshold)
was scoped but not built. The infrastructure to ship it is tractable
(per-session swap-signal channel + rebalancer goroutine, ~200 LOC) but
the value is marginal at homelab concurrency: with 1–3 concurrent
sessions on 3 GPU workers, the rebalancer almost never has anything to
do, and a misfire causes a ~1–3 s playback stall for nothing.

Reconsider when concurrent-session count routinely exceeds ~6 or when
a real contention scenario surfaces in Plex playback metrics. If
re-opened, build the manual `POST /task/<id>/swap?target=<url>`
debug endpoint first, validate against a real session, then layer
the auto-rebalancer on top.

## Operational caveats

- **PSA bump for the worker namespace**: `pod-security.kubernetes.io/enforce`
  must be `privileged` so PERFMON can be granted. Documented in
  `cluster-talos/.../clusterplex/app/namespace.yaml`.
- **`allowPrivilegeEscalation: true`** on the worker container — file
  capabilities don't land if `no_new_privs` is set.
- **Sysfs reader requires no caps**; the privileged-bump only matters
  on Arc / recent-kernel hardware that needs the PMU path.
- **Recovery doesn't re-establish progress reporters from scratch** —
  the new worker starts its own (it's the worker's `progress_report.go`
  goroutine), and PMS sees the same `progressurl` it always saw.
