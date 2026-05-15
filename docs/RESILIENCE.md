# Resilience layer

Three independent mechanisms that keep transcodes well-behaved when
something goes wrong: PMS throttling, GPU-aware routing, and worker
crash recovery. Shipped 2026-05-10.

## 1. PMS canThrottle pass-through

### Why

Plex's transcoder supervisor sets `canThrottle` in progress responses
once the client buffer is far enough ahead of playback that further
GPU/disk burn is wasted (skipped or abandoned segments). Plex's own
ffmpeg fork honours this with a 100 ms-per-frame `usleep` in
`fftools/ffmpeg.c`. The worker rewriter strips Plex's `-progressurl`
and substitutes `-progress pipe:N` for the agent's stdlib parser,
but re-injects `-canthrottleurl <relay-url>` pointing at the same
endpoint so the in-binary sleep keeps working.

### How

scaleplex-ffmpeg7 patch 0097 ports Plex's in-binary throttle from
Plex Transcoder 1.12.3's `fftools/ffmpeg.c` (sourced via the
Diagonactic mirror — the official GPL tarball omits `fftools/`).

- New `-canthrottleurl <url>` AVOption + globals
  `canthrottle_url` (string) and `scaleplex_throttle_delay_us`
  (`_Atomic int`).
- `print_report()` fires a one-shot HTTP PUT to the URL after every
  progress block (~0.5 s cadence). The PUT body is empty; the
  response carries Plex's `<MediaContainer>` with `canThrottle="1"`
  attribute when the buffer is full enough.
- Mirrors Plex's `PMS_IssueHttpRequest` helper at
  `fftools/plex.c` L63-114: `AVIO_FLAG_READ` (the `method=PUT`
  AVDict option triggers PUT semantics; the avio descriptor only
  reads the response), `avio_size()` to learn Content-Length, then
  `avio_read(ctx, buf, size)` for exactly that many bytes. Asking
  for more bytes than the server sent blocks `avio_read` forever —
  shipped + caught + fixed 2026-05-12.
- Response body parsed for the literal substring `canThrottle`.
  Hit sets the atomic to `100 * 1000` (100 ms in µs). Miss sets it
  to 0. Errors fail-open.
- The video encoder thread (`fftools/ffmpeg_enc.c encoder_thread`)
  reads the atomic after each `frame_encode()` call and
  `av_usleep`s when nonzero. Per-output-video-frame cadence, ~28%
  throughput when asserted at 23.976 fps. AUDIO and SUB encoder
  threads run in parallel and DO NOT sleep — gating by
  `ost->type == AVMEDIA_TYPE_VIDEO` is load-bearing (sleeping in
  parallel threads would compound multiplicatively: 0.28× video ×
  0.28× audio = 0.08× net).
- Speed reporting: worker agent's `progress_report.go` still parses
  every progress PUT response for `canThrottle` and flips a
  per-session `throttleSignal` (atomic int). Used only to suppress
  `&speed=` on outbound progress PUTs while asserted — Plex
  Transcoder protocol: "Only pass back speed if we're not
  throttled" (PMS uses speed to estimate buffer-fill and would
  never clear canThrottle if we reported a fast rate while
  throttled). No SIGSTOP / SIGCONT pulsing anymore — that
  `worker/agent/throttle.go` controller existed pre-Phase-4 and
  was retired in commit `a23cebb` once the in-binary sleep landed.

### Equilibrium behaviour

Burst ~5× for the first 30–60 s of a fresh session while PMS sees
buffer below `TranscoderThrottleBuffer`. Then PMS asserts
canThrottle, encoder rate drops, buffer drains, PMS clears
canThrottle, encoder catches up, repeat. Steady state hovers
~1.3–1.6× as PMS oscillates the flag. Validated 2026-05-12 on
Android BH6 4K HEVC HDR (downgraded to 720x404 stereo AAC via
indirect-connection bandwidth cap): cold start + 1:20:00 seek both
smooth, ~720 successful PUT cycles in 2 minutes.

### Metrics

In-binary throttle state isn't exposed as a worker metric. ffmpeg
emits:

- `scaleplex/ct: throttle ON|OFF` at AV_LOG_INFO on state
  transitions (visible at the default `-loglevel info`). Use this
  to see when PMS first asserts canThrottle on a session.
- `scaleplex/ct: PUT/avio_read/body` at AV_LOG_DEBUG for per-cycle
  diagnostics (hidden by default). Bump the worker DaemonSet env
  `SCALEPLEX_FFMPEG_LOGLEVEL=debug` to expose them; the rewriter
  threads the value through the `-loglevel quiet → <level>` rewrite
  it already does for ffmpeg output.

Logs flow worker stderr → orchestrator stream → PMS shim → PMS pod
log. `kubectl logs <pms-pod> -n <namespace> | grep scaleplex/ct` works
as long as the session's worker is healthy.

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
- `scaleplex_worker_throttle_*` (above) — orthogonal but observed in
  the same session graphs

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
  must be `privileged` so PERFMON can be granted.
- **`allowPrivilegeEscalation: true`** on the worker container — file
  capabilities don't land if `no_new_privs` is set.
- **Sysfs reader requires no caps**; the privileged-bump only matters
  on Arc / recent-kernel hardware that needs the PMU path.
- **Recovery doesn't re-establish progress reporters from scratch** —
  the new worker starts its own (it's the worker's `progress_report.go`
  goroutine), and PMS sees the same `progressurl` it always saw.
