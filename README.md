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
(scaleplex-ffmpeg7 — jellyfin-ffmpeg + a small Plex-backport patch layer in
[`scaleplex-ffmpeg/`](scaleplex-ffmpeg/)) with full VAAPI HW filters.

Concretely this unlocks:

- HW HDR→SDR tonemap (`tonemap_vaapi`)
- HW subtitle burn-in: bitmap subs via `overlay_vaapi`, text subs via a
  fork-native port of Plex's `inlineass` filter (scaleplex-ffmpeg7 patches
  0099-0101)
- HDR Main10 passthrough where the client supports it
- Direct NFS segment writes — no `LOCAL_RELAY` HTTP hop
- First-frame latency as a first-class design goal (see [docs/LATENCY.md](docs/LATENCY.md))
- Independence from Plex's bundled ffmpeg version

PMS still sees a normal local transcoder via a thin shim. Plex session
bookkeeping is unchanged.

## Status

**v1.0 — feature-complete and validated.** Every client/format cell in
the matrix below has been exercised end-to-end (initial play, seek,
quality change, subtitle burn-in as applicable) on the scaleplex PMS
deployment:

| Client / format | Play | Seek | Subs | Notes |
|---|:-:|:-:|:-:|---|
| Plex Web — DASH (Chrome / Firefox) | ✓ | ✓ | ✓ | Burn-in + text-sub side-channel (`-segment_format ass`) |
| Plex Android — HLS mpegts | ✓ | ✓ | ✓ | |
| Plex Android — HLS matroska (4K HDR + 5.1 EAC3) | ✓ | ✓ | ✓ | mkv-in-`.ts` when codec/audio can't fit mpegts |
| Plex Windows desktop — segmented matroska | ✓ | ✓ | ✓ | Cosmetic playhead-reset on seek — see [`docs/KNOWN_ISSUES.md`](docs/KNOWN_ISSUES.md) |
| LG webOS — HLS (4K HEVC HDR) | ✓ | ✓ | ✓ | PGS overlay + SRT burn-in |
| Plex Optimize (HW-decode + remux fast-path) | ✓ | n/a | ✓ | mp4 + faststart, multi-track audio, sidecar SRT copy |
| PMS Detection / ML pre-pass | ✓ | n/a | n/a | bail-path scrub — ffmpeg runs the original argv cleaned of Plex-private flags |

**Source matrix:** AV1 + HEVC + H264; SDR + HDR10; embedded and sidecar
SRT / ASS text subs (burn-in via the fork-native `inlineass=` filter);
embedded PGS bitmap subs (`overlay_vaapi`). HDR→SDR via `tonemap_vaapi`.

**Resilience:** PMS `canThrottle` pass-through, multi-engine GPU load
reporting, transparent mid-stream worker recovery across DaemonSet rolls
(see [`docs/RESILIENCE.md`](docs/RESILIENCE.md)).

**Deployment scope.** v1.0 is a code milestone — the software is
release-ready. Pointing any particular PMS instance at scaleplex is an
independent operational decision, not gated on this tag.

**Images** are sha-pinned — CI publishes `ghcr.io/varashi/scaleplex_worker`,
`scaleplex_orchestrator`, and `scaleplex_pms_dockermod` as `sha-<short>`;
the Helm release pins each tag explicitly.

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
 │  - rewrites Plex argv → VAAPI│   - scaleplex-ffmpeg7
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
| `worker/Dockerfile` | Ubuntu 24.04 + scaleplex-ffmpeg7 + iHD VAAPI + agent. |
| `worker/deploy/` | DaemonSet + namespace YAML. |
| `orchestrator/deploy/` | Deployment YAML. |
| `scaleplex-ffmpeg/` | Patch layer + Debian build pipeline for `scaleplex-ffmpeg7` (jellyfin-ffmpeg + Plex backports). |
| `charts/scaleplex/` | Helm chart (placeholder; deploy via raw YAML for now). |
| `docs/` | Architecture, rewriter, seek, latency, lessons. |

## Deploy

scaleplex is Kubernetes-native. It adds three things to a cluster and
**does not own the PMS pod** — PMS stays whatever you already run, which
keeps rollback a one-line revert.

1. **Worker** — a DaemonSet, one pod per GPU node (Intel iGPU / Arc,
   `/dev/dri/render*`). Pre-warms VAAPI; `/readyz` gates on warm-up.
2. **Orchestrator** — a stateless Deployment. DNS-discovers workers via
   a headless Service and routes each task to the least-loaded one.
3. **PMS DOCKER_MOD** — on your existing PMS container, point
   `DOCKER_MODS` at `scaleplex_pms_dockermod`. The mod lays down the
   shim as `Plex Transcoder` and runs the relay sidecar:

   ```yaml
   env:
     DOCKER_MODS: ghcr.io/varashi/scaleplex_pms_dockermod:sha-<short>
     LOCAL_RELAY_ENABLED: "1"
     LOCAL_RELAY_PORT: "32499"
     SCALEPLEX_ORCHESTRATOR_URL: http://<orchestrator-service>.<namespace>.svc:3500
   ```

The worker + PMS pods must share the NFS volumes PMS transcodes into
(`/transcode`) and reads media from (`/media`) — the worker writes
segments exactly where the PMS serves them.

### Namespace topology — pick one

The worker wants `CAP_PERFMON` to read the i915 hardware PMU for
GPU-busy load telemetry (needed on GPUs with no sysfs busy file, e.g.
Intel Arc). `PERFMON` is on Pod Security Admission's `privileged`-only
allowlist. That forces a choice:

- **A — fold into the PMS namespace.** Run the worker + orchestrator in
  the same namespace as your PMS. Simplest — the worker reuses the PMS's
  exact `/transcode` + `/media` volume definitions, so the paths cannot
  drift. Cost: that namespace must be PSA `privileged`. Fine for a
  single-operator cluster where you control every manifest.
- **B — dedicated `scaleplex` namespace.** Keeps your PMS namespace at
  PSA `baseline`; only the `scaleplex` namespace is `privileged`. You
  must configure the worker fleet to mount the **same** `/transcode` NFS
  export the PMS uses.

Either way the worker carries `cap_perfmon=ep` as a file capability so
only the agent binary gets the bits, not the whole container. If you'd
rather keep every namespace at `baseline`, drop the `PERFMON` capability
entirely — the worker falls back cleanly and the orchestrator
load-balances on session count instead of GPU-busy %.

**Rollback** — remove the `DOCKER_MODS` env from the PMS container. The
shim's cont-init script restores `Plex Transcoder.real` on next PMS
start. The worker DaemonSet and orchestrator can be left running or
removed independently; they are inert without the shim feeding them.

**Helm.** scaleplex is deployed in the reference setup as a
[bjw-s `app-template`](https://github.com/bjw-s-labs/helm-charts)
HelmRelease — homelab-familiar, and it keeps storage / networking /
scheduling fully in the operator's hands. A reference `values.yaml`
fragment carrying the scaleplex-structural pieces (worker DaemonSet
shape, headless discovery Service, PERFMON cap) is the planned
distribution artifact; a dedicated first-party chart is a possible
follow-up if the reference proves clumsy. The `charts/scaleplex/`
directory is a placeholder.

## Docs

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — components, data flow, where state lives.
- [`docs/REWRITER.md`](docs/REWRITER.md) — every Plex-private argv quirk and its stock-ffmpeg translation.
- [`docs/TUNING.md`](docs/TUNING.md) — operator env knobs for transcode quality + behaviour.
- [`docs/SEEK.md`](docs/SEEK.md) — DASH and HLS seek deep-dive (the hardest problems we shipped).
- [`docs/LATENCY.md`](docs/LATENCY.md) — first-frame latency budget and design levers.
- [`docs/RESILIENCE.md`](docs/RESILIENCE.md) — PMS canThrottle pass-through, multi-engine GPU load, mid-stream worker recovery.
- [`docs/KNOWN_ISSUES.md`](docs/KNOWN_ISSUES.md) — tracked limitations as of v1.0.
- [`CHANGELOG.md`](CHANGELOG.md) — release notes.
- [`docs/PLAN.md`](docs/PLAN.md) — original implementation plan (historical; mostly delivered).
- [`docs/LESSONS-FROM-CLUSTERPLEX.md`](docs/LESSONS-FROM-CLUSTERPLEX.md) — concrete pitfalls scaleplex avoids by design.

## Lineage

scaleplex inherits the lessons from
[Varashi/clusterplex#rewriter-plan](https://github.com/Varashi/clusterplex/tree/rewriter-plan).
clusterplex's `argRewriter.js` seeded `worker/agent/rewriter.go`, but the
Go port runs on the worker (where `/media` is locally mounted) instead of
on the orchestrator, so sidecar SRT/ASS lookups happen with direct fs
access rather than over a socket.io detour.
