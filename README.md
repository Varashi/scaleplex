# scaleplex

Distributed Plex Media Server transcoding fleet, without the `Plex Transcoder`
binary on workers.

[Why](#why) — [Status](#status) — [Architecture](#architecture) —
[Deploy](#deploy) — [Docs](#docs)

## Why

[clusterplex](https://github.com/pabloromeo/clusterplex) hits the limits of
running Plex's bundled `Plex Transcoder`: musl ffmpeg blocks Intel NEO OpenCL,
the Plex build excludes `tonemap_vaapi`, the `inlineass` filter is
Plex-private, and `LOCAL_RELAY` adds an HTTP hop on every segment. scaleplex
keeps the distributed-transcode shape but swaps workers to **stock ffmpeg**
(jellyfin-ffmpeg7) with full VAAPI HW filters.

Concretely this unlocks:

- HW HDR→SDR tonemap (`tonemap_vaapi`)
- HW subtitle burn-in (`overlay_vaapi` + stock `subtitles=` filter, replacing
  Plex's `inlineass`)
- HDR Main10 passthrough where the client supports it
- Direct NFS segment writes — no `LOCAL_RELAY` HTTP hop
- First-frame latency as a first-class design goal (see [docs/LATENCY.md](docs/LATENCY.md))
- Independence from Plex's bundled ffmpeg version

PMS still sees a normal local transcoder via a thin shim. Plex session
bookkeeping is unchanged.

## Status

**Working end-to-end as of 2026-05-06** on the test PMS pod
(`clusterplex-pms-*` with shim swapped via `DOCKER_MODS`):

| Streaming format | Initial play | Seek | Quality change | Notes |
|---|:-:|:-:|:-:|---|
| DASH (Plex Web Chrome) | ✓ | ✓ | ✓ | Real-time + Optimize jobs |
| HLS / mpegts (Plex Android) | ✓ | ✓ | — | |
| HLS / matroska (Plex Android, 4K HDR + 5.1 EAC3) | ✓ | ✓ | ✓ | mkv-in-.ts triggered when codec/audio combo can't fit mpegts |

**Source matrix tested:** AV1 + HEVC + H264 sources; SDR + HDR10; SRT sidecar
subs (burn-in via `subtitles=` filter chained through `overlay_vaapi`).

**Untested / pending:** LG WebOS, PS4, Apple TV, iOS clients;
sustained-load and concurrent-session benchmarks; full production cutover
(currently only the test PMS pod runs the shim, clusterplex on
production PMS is untouched).

Image tag tracked in `worker/deploy/worker.yaml` and the
`scaleplex_pms_dockermod` GHCR repo. Builds are sha-pinned (CI publishes
`sha-<short>` and Renovate/manual bumps update the deploy YAML).

## Architecture

```
┌────────────────────────────────────────────────┐
│  PMS pod                                       │
│  ┌──────────────────────────────────────────┐  │
│  │ Plex Media Server                        │  │
│  │ /usr/lib/plexmediaserver/Plex Transcoder │  │
│  │     ↓ symlinked to                       │  │
│  │ scaleplex-shim  (~5 MB Go binary)        │  │
│  │                                          │  │
│  │ scaleplex-relay (sidecar, 32499→32400)   │  │
│  │   POST→PUT for /progress                 │  │
│  │   CSV rewrite for /manifest (HLS seek)   │  │
│  └────────────────┬─────────────────────────┘  │
└───────────────────┼────────────────────────────┘
                    │ HTTP POST {args, env, cwd, session_id}
                    ▼
       ┌─────────────────────────────┐
       │ scaleplex-orchestrator      │
       │   - DNS-discovers workers   │
       │   - tracks active sessions  │
       │   - picks least-loaded      │
       └──────┬──────────────────────┘
              │ HTTP forward (verbatim)
   ┌──────────┼──────────────┐
   ▼          ▼              ▼
 ┌────────┐ ┌────────┐ ┌────────┐
 │Worker 1│ │Worker 2│ │Worker 3│   DaemonSet on gpu-worker nodes
 │        │ │        │ │        │
 │ scaleplex-agent (Go)         │   - Ubuntu 24.04
 │  - rewrites Plex argv → VAAPI│   - jellyfin-ffmpeg7
 │  - spawns ffmpeg              │   - intel-media-va-driver-non-free
 │  - watches segments, posts    │   - libass, fonts (DejaVu, Noto)
 │    progress + manifest        │   - render group access (568)
 │  - adaptive probesize         │
 └───┬────┘                      │
     │ writes segments
     ▼
 ┌──────────────────────────────────┐
 │ /transcode  (NFS, shared w/ PMS) │
 │   plex-transcode-<session>/      │
 │     header                       │
 │     media-NNNNN.ts (HLS)         │
 │     init-stream0.m4s (DASH)      │
 │     chunk-stream0-NNNNN.m4s      │
 │     dash                         │
 └──────────────────────────────────┘
```

**Boundary:** PMS only needs to see segments on disk and receive HTTP
callbacks (progress, manifest body). The relay sidecar gives ffmpeg a
loopback-equivalent endpoint to call back on (workers can't reach PMS's
127.0.0.1:32400 directly). Everything else flows over normal cluster Services.

## Repo layout

| Path | Purpose |
|---|---|
| `shim/cmd/shim/` | `Plex Transcoder` replacement. Static Go binary. |
| `shim/cmd/relay/` | Forward proxy on PMS pod (POST→PUT for `/progress`, CSV rewrite for HLS `/manifest`). |
| `shim/Dockerfile` | DOCKER_MOD image: drops shim into `/usr/lib/plexmediaserver/` + relay as s6-v3 longrun. |
| `orchestrator/` | Slim Go HTTP server. DNS-discovers workers, picks least-loaded. |
| `worker/agent/` | Worker-side daemon. Rewrites argv, spawns ffmpeg, posts progress, watches segments. |
| `worker/Dockerfile` | Ubuntu 24.04 + jellyfin-ffmpeg7 + iHD VAAPI + agent. |
| `worker/deploy/` | DaemonSet + namespace YAML. |
| `orchestrator/deploy/` | Deployment YAML. |
| `charts/scaleplex/` | Helm chart (placeholder; deploy via raw YAML for now). |
| `docs/` | Architecture, rewriter, seek, latency, lessons. |

## Deploy

Today scaleplex deploys via raw YAML manifests next to the existing
clusterplex namespace. The PMS pod is the existing
`clusterplex-pms-*` deployment with the `DOCKER_MODS` env var pointed at
`scaleplex_pms_dockermod`:

```yaml
env:
  - name: DOCKER_MODS
    value: ghcr.io/varashi/scaleplex_pms_dockermod:sha-<short>
  - name: LOCAL_RELAY_ENABLED
    value: "1"
  - name: LOCAL_RELAY_PORT
    value: "32499"
  - name: SCALEPLEX_ORCHESTRATOR_URL
    value: http://scaleplex-orchestrator.scaleplex.svc.cluster.local:3500
```

Worker DaemonSet runs in the `scaleplex` namespace, scheduled on
`gpu-worker` nodes. Orchestrator is a single Deployment.

Rollback is a one-liner: revert `DOCKER_MODS` on the PMS deploy, scale
clusterplex's orchestrator back up, `flux resume hr clusterplex -n
clusterplex`. The shim's cont-init script restores `Plex Transcoder.real`
on next PMS start.

A proper Helm chart is on the roadmap; the placeholder lives at
`charts/scaleplex/`.

## Docs

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — components, data flow, where state lives.
- [`docs/REWRITER.md`](docs/REWRITER.md) — every Plex-private argv quirk and its stock-ffmpeg translation.
- [`docs/SEEK.md`](docs/SEEK.md) — DASH and HLS seek deep-dive (the hardest problems we shipped).
- [`docs/LATENCY.md`](docs/LATENCY.md) — first-frame latency budget and design levers.
- [`docs/RESILIENCE.md`](docs/RESILIENCE.md) — PMS canThrottle pass-through, multi-engine GPU load, mid-stream worker recovery.
- [`docs/PLAN.md`](docs/PLAN.md) — original implementation plan (historical; mostly delivered).
- [`docs/LESSONS-FROM-CLUSTERPLEX.md`](docs/LESSONS-FROM-CLUSTERPLEX.md) — concrete pitfalls scaleplex avoids by design.

## Lineage

scaleplex inherits the lessons from
[Varashi/clusterplex#rewriter-plan](https://github.com/Varashi/clusterplex/tree/rewriter-plan).
clusterplex's `argRewriter.js` seeded `worker/agent/rewriter.go`, but the
Go port runs on the worker (where `/media` is locally mounted) instead of
on the orchestrator, so sidecar SRT/ASS lookups happen with direct fs
access rather than over a socket.io detour.
