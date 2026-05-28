# Architecture

## Components

### scaleplex-shim (PMS pod)

A drop-in replacement for `/usr/lib/plexmediaserver/Plex Transcoder` (which
is just Plex's bundled ffmpeg under another name). The shim is a static Go
binary, ~5 MB, built from `shim/cmd/shim/`.

It is installed by a `cont-init` script (`/custom-cont-init.d/99-scaleplex-shim`)
that runs on every container start: backs up the original to
`Plex Transcoder.real` if not already backed up, then copies the shim over
the original. The script is idempotent and safe to re-run after a Plex
update replaces the binary.

When PMS spawns "Plex Transcoder", the shim:

1. Captures `argv`, `env`, and `cwd`.
2. POSTs `{session_id, args[], env{}, cwd}` to the orchestrator over HTTP.
3. Streams the orchestrator's chunked response back, demultiplexing
   stdout/stderr/progress lines so PMS sees what it expects.
4. Exits with the worker's status code.

`session_id` is derived from `cwd` (the per-session transcode dir PMS
creates). On a re-spawn for the same session (the seek/quality-change
flow), the agent on the worker side keys on this ID and force-kills the
prior ffmpeg before starting the new one — avoids the orphan-transcoder
problem clusterplex hit.

### scaleplex-relay (PMS pod sidecar)

Stock ffmpeg's `-progress <url>`, `-manifest_name <url>`, and
`-segment_list <url>` flags POST/PUT to URLs that PMS expects on
`http://127.0.0.1:32400/...`. Workers can't reach PMS's loopback. So the
worker's argv-rewriter rewrites those URLs to point at the cluster
Service IP — but PMS rejects requests that don't appear to come from
loopback (its transcode-session endpoints check the source IP).

The relay solves both halves: it listens on `LOCAL_RELAY_PORT` (default
32499) on the PMS pod and forwards every request to
`http://127.0.0.1:PMS_PORT`. From PMS's perspective the source IP is
loopback (the relay connects from the same pod's network namespace), so
the auth check passes.

It also fixes two protocol mismatches that stock ffmpeg can't be
configured around:

1. **`-progress` POST → PUT.** Plex's progress handler is registered for
   PUT only. Stock ffmpeg's progress writer always POSTs. The relay
   promotes POST to PUT when the path matches
   `^/video/:/transcode/session/[^/]+/[^/]+/progress$`.

2. **HLS `-segment_list` CSV rewrite.** PMS reads CSV rows to learn each
   chunk's playlist window and serves a 200/0-byte response when start_time
   doesn't match. Stock ffmpeg without `-copyts` writes 0-based CSV; we
   need global-timeline CSV. The relay rewrites each `media-NNNNN.ts,start,end`
   row to `N*segDur..(N+1)*segDur`, scoped to manifest POSTs that carry
   a `scaleplex_seg_time=<N>` query param the worker tags. See
   [SEEK.md](SEEK.md#hls-seek) for the full story.

The relay runs as an s6-overlay v3 longrun
(`/etc/s6-overlay/s6-rc.d/scaleplex-relay/`), installed by the same
DOCKER_MOD that lays down the shim.

### scaleplex-orchestrator

`orchestrator/main.go`. Single-file HTTP server, ~400 LOC.

**Worker discovery.** DNS lookup of `WORKERS_DNS` (a headless k8s
Service in front of the worker DaemonSet), re-resolved every
`WORKERS_REFRESH_SECONDS` — plus a static `WORKERS_LIST` and a PUSH
self-register path (workers `POST /register` + heartbeat) for non-k8s
deployments. Each known worker is polled with `GET /capability` every
`WORKERS_PROBE_SECONDS` to learn `(active_sessions, max_sessions,
healthy, gpu_load, backend)`.

**Selection (`schedule()`).** Computed once per dispatch. With
`SCALEPLEX_PREFER_HW=1` (default) the pool is split `[HW tier, SW tier]`
and ordered within each tier by `SCALEPLEX_LB_STRATEGY` — `load`
(default: `max(session-saturation, gpu_load)`) | `round-robin` |
`least-sessions` | `random` — so a heavy job prefers a GPU and only
spills onto a CPU worker when the GPU tier is saturated. A worker that
hasn't reported a backend yet (pre-PR4 image mid-rolling-upgrade) counts
as HW. On a homogeneous fleet the tiers collapse and it reduces to the
prior least-loaded ranking. A 503 falls through to the next candidate;
the in-flight counter keeps concurrent arrivals from colliding.

**Race protection.** The orchestrator combines its `/capability` cache
with a local in-flight counter, so two concurrent task arrivals don't
both pick the same worker just because the cache is 5s stale. The local
counter increments on dispatch, decrements when the worker's response
stream closes.

No socket.io, no arg rewriting (worker-side), no LOCAL_RELAY logic. The
clusterplex orchestrator's PMS-restart hook is gone too — workers no
longer hold per-PMS-UUID state (no EAE, see [LESSONS-FROM-CLUSTERPLEX.md](LESSONS-FROM-CLUSTERPLEX.md#5-eae_root-path-tied-to-pms-pod-uuid)).

### scaleplex-agent (per-worker)

`worker/agent/`. The substantive component. Listens on `:3501`.

**Endpoints:**
- `POST /task` — receive `{session_id, args[], env{}, cwd}`. Spawns ffmpeg.
  Streams stdout/stderr/exit code back as a chunked response.
- `GET /capability` — return `{active_sessions, max_sessions, healthy, gpu_load, gpu_engines, backend}` (`backend` ∈ `vaapi`/`nvenc`/`sw`).
- `GET /healthz` — bound the moment HTTP listens.
- `GET /readyz` — gated on pre-warm completion.
- `GET /metrics` — Prometheus.

**Pre-warm at startup** (gated by `/readyz`):
- Hold `/dev/dri/renderD128` fd so first VAAPI session doesn't pay
  device-init cost.
- Run `ffmpeg -version` to page-cache binary + dynamic deps.
- Run a 1-second `testsrc → h264_vaapi → null` transcode to JIT the iHD
  VPP/encoder programs. Cuts ~1s off cold-session latency.

**Per-task pipeline:**

```
POST /task ──┬─→ Rewrite(args, env)        worker/agent/rewriter.go
             │     - reshape ANY source argv → this worker's backend
             │       (cross-backend: VAAPI/NVENC/SW → activeDialect)
             │     - Plex codecs → backend HW chain (or SW on the cpu node)
             │     - Plex-private filters → stock equivalents
             │     - URL rewrites (progress, manifest, segment_list)
             │     - HLS / DASH path branches
             │     - Plex-Pass gate on HW re-acceleration
             │
             ├─→ Adaptive probesize        worker/agent/probesize.go
             │     - ffprobe with progressive sizes (1MB → 20MB)
             │     - cache by inode+mtime
             │     - replace Plex's 20MB with the smallest that worked
             │
             ├─→ Spawn ffmpeg              worker/agent/main.go
             │
             ├─→ Watch segments            worker/agent/segwatch.go
             │     - inotify on session dir
             │     - DASH: rename chunks to seek-target index, patch
             │       tfdt + sidx.ept on each chunk after rename
             │
             ├─→ Publish manifest          worker/agent/manifest_publish.go
             │     - DASH only
             │     - watches <cwd>/dash file via fsnotify
             │     - POSTs body to PMS on each rewrite, debounced 200ms
             │
             ├─→ Report progress           worker/agent/progress_report.go
             │     - parses ffmpeg's -progress key=value stream
             │     - prelude PUTs (duration, streamDetail per source
             │       stream, dimensions) match PT.real byte-for-byte
             │     - periodic /progress?progress=&size=&remaining=&speed=
             │
             └─→ Stream stderr → response   stdout/stderr to PMS
```

**Backend dialect + cross-backend reshape** (`worker/agent/dialect.go`,
`rewriter_sw.go`, `source_backend.go`). At startup `selectDialect()` picks the
worker's backend from `WORKER_BACKEND` (`auto` probes `/dev/nvidia0` → nvenc,
else a DRM render node → vaapi, else sw). The rewriter emits all backend-specific
tokens (encoder, hwaccel, scale/tonemap/sub-burn filters, device init) through
the active `dialect`, so one binary serves Intel, NVIDIA, or CPU. PMS may emit an
argv shaped for a *different* backend than the worker's (a VAAPI PMS dispatching
to an NVIDIA or CPU node); `detectSourceBackend` + `reshapeForeignHWArgv`
(→ `composeBurn`) / `reshapeToSoftware` (→ `composeBurnSW`) renormalize it to the
worker's backend first — decode flags, the orthogonal filter graph, and the
encoder — emitting Plex's own stock shapes so the fork needs no per-backend patch.
HW *re-acceleration* (`SCALEPLEX_FORCE_HW=1` SW→HW, or a cross-backend HW→HW
reshape) is gated by the Plex-Pass check (`pass_gate.go`, fail-closed); a SW
worker downgrades everything and is never Pass-gated. Pre-warm above is
backend-aware (VAAPI render-node JIT on Intel; NVIDIA/CPU skip it).

**Session cleanup:** on receipt of a new task with the same `session_id`,
the agent first SIGTERMs any running ffmpeg for that session (5s grace,
then SIGKILL), then starts the new one. Avoids the
two-transcoders-on-one-GPU contention clusterplex saw.

**Argv corpus capture (gated by `WORKER_DUMP_ARGV=1`):** every task
spawn writes `<session_id>.json` to `/transcode/_argv-corpus` with
`{argv, env, client, ...}` then merges `outcome` (exit_status, signal,
duration_ms, segments_created, stderr_tail, ended_at) post-spawn via
atomic tmp+rename. Client identification is extracted from the
`X_PLEX_*` env vars Plex Transcoder inherits from PMS — lets corpus
analysis cluster bugs by client class (PS4 vs LG WebOS vs Apple TV
etc.). The same enrichments are emitted by the production plex
tee-wrapper as `CLIENT:KEY=VALUE` headers + `OUTCOME:...` footer in
the `.argv` files. The corpus feeds `worker/agent/replay_test.go` for
regression detection: each entry's historical outcome is compared to
what the current rewriter produces; mismatches surface as test
failures. See `cmd/argv-extract/sweep.sh` to refresh the corpus from
both NFS surfaces.

## Data flow

1. **Client click → playback request.** PMS receives the request, decides
   transcode parameters, picks a transcode dir under
   `/transcode/Transcode/Sessions/plex-transcode-<sid>-<job>/`.

2. **PMS spawns "Plex Transcoder".** The shim sees the spawn. It POSTs
   `{session_id, args, env, cwd}` to the orchestrator.

3. **Orchestrator picks a worker.** Forwards the POST verbatim. The
   forwarded request returns a chunked response that the orchestrator
   passes back to the shim.

4. **Worker rewrites argv.** `Rewrite()` walks the argv, swapping codec
   names, filter graphs, URL targets. The result might add 5-15 changes
   per session for a typical 4K HDR + EAC3 source. Logged once at task
   start; the change list is the diagnostic channel for "did the
   translator do what I expect".

5. **Worker spawns ffmpeg.** Working directory is the per-session
   `/transcode/Transcode/Sessions/...` path PMS created. ffmpeg writes
   segments here directly (NFS shared with PMS).

6. **ffmpeg writes segments + posts progress.** As segments land, the
   agent's segwatch sees inotify events, performs DASH-specific
   post-processing (chunk rename + tfdt patch on seek), and the manifest
   publisher POSTs each .mpd refresh up to PMS.

7. **PMS serves segments to client.** PMS's HTTP serves `/base/NNNNN.ts`
   or `/chunk-streamN-NNNNN.m4s` directly off NFS — no LOCAL_RELAY hop.

8. **Session ends.** Client closes, PMS issues `DELETE
   /transcode/sessions/<sid>` (which the agent observes via the orchestrator
   notifying it; or the ffmpeg process itself completes). Agent kills
   ffmpeg (if still running), decrements active-session count.

## Where state lives

| Where | What | Lifetime |
|---|---|---|
| PMS `/config` PVC | Plex library DB, session bookkeeping, m3u8 / mpd templates | persistent (vsan PVC) |
| Worker `/dev/dri/renderD128` fd | VAAPI device handle held by agent | process lifetime |
| Worker probesize cache | inode+mtime → minimum probesize that yielded full stream listing | process lifetime |
| Orchestrator memory | worker registry, active-session counts | process lifetime |
| Shared NFS `/transcode` | session dirs, segments | bounded by PMS's session GC |
| Worker memory (per task) | rewriter inputs, ffmpeg PID, channel for stderr→response | task lifetime |
| Relay (PMS pod) | none — pure forward proxy with two body rewrites | per-request |

No persistent state on workers, no per-PMS-UUID coupling. A worker can
be drained and replaced at any time; the orchestrator notices via DNS
and reroutes new tasks. In-flight tasks on a draining worker complete
or get killed by SIGTERM through the pod terminationGracePeriod.

## Failure modes

**Worker dies mid-transcode.** Shim sees the chunked response close
unexpectedly, returns non-zero to PMS, PMS retries with a fresh
transcode (PMS already does this when its own bundled transcoder
crashes — same path). New task lands on a different worker via the
orchestrator's load balancing.

**Orchestrator dies.** Shim's POST fails. Shim returns non-zero. PMS
treats it as a transcode failure. Orchestrator restarts (Deployment),
shim's next attempt succeeds. Active sessions during the outage are
lost; the user sees a "playback error" and clicks play again.

**Relay dies.** ffmpeg's progress / manifest POSTs fail. The transcode
itself continues (segments are written to NFS regardless). PMS just
doesn't get progress updates; some clients show a stale buffer. Relay
restarts (s6 longrun), traffic resumes.

**NFS hiccup.** Both worker (writing segments) and PMS (serving them)
see EIO. ffmpeg may bail. PMS retries the transcode, see "worker dies"
above. The workers used to read from `/media` over NFS too; that path
needs the NFS mount option `hard` to retry rather than fail.

**iHD VAAPI corruption.** GPU enters a bad state (rare). Worker's
ffmpeg fails with VA-API errors. Agent reports failure, returns
non-zero. PMS retries on a different worker. Sustained failures on one
node would surface via the gpu-node controller; out of scope here.
