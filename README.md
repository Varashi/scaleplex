# scaleplex

Distributed Plex Media Server transcoding fleet, without `Plex Transcoder`
coupling on workers.

## Why

[clusterplex](https://github.com/pabloromeo/clusterplex) hits the limits of
running Plex's bundled `Plex Transcoder` (musl ffmpeg, no `tonemap_vaapi`,
custom `inlineass` filter, mandatory `LOCAL_RELAY` proxy). scaleplex keeps the
distributed-transcode shape but swaps workers to **stock ffmpeg** with full
VAAPI HW filters, unlocking:

- HW HDR→SDR tonemap (`tonemap_vaapi`)
- Full HW subtitle burn (`overlay_vaapi`)
- HDR Main10 passthrough (no tonemap loss for HDR clients)
- Direct NFS segment writes (no `LOCAL_RELAY` HTTP hop)
- **First-frame latency as a first-class design goal** (see docs/LATENCY.md)
- Independent of Plex version internals

PMS still sees a normal local transcoder via a thin shim. Plex session
bookkeeping unchanged.

## Architecture

```
┌─────────────────────────────────────┐
│ PMS (no GPU, vsan /config)          │
│  ┌───────────────────────────────┐  │
│  │ /usr/lib/plexmediaserver/     │  │
│  │ "Plex Transcoder" → SHIM      │  │
│  └──────────────┬────────────────┘  │
└─────────────────┼───────────────────┘
                  │ HTTP/gRPC: {args, env, session_id}
                  ▼
        ┌─────────────────────┐
        │ Orchestrator (slim) │
        │ - worker registry   │
        │ - LB strategy       │
        │ - PMS-restart hook  │
        └──────┬──────────────┘
               │
   ┌───────────┼───────────┐
   ▼           ▼           ▼
 ┌────────┐ ┌────────┐ ┌────────┐
 │Worker 1│ │Worker 2│ │Worker 3│   DaemonSet (1/GPU node)
 │        │ │        │ │        │
 │stock   │ │        │ │        │   - Ubuntu 24.04
 │ffmpeg  │ │        │ │        │   - ffmpeg w/ vaapi+libass
 │+ libass│ │        │ │        │   - tonemap_vaapi
 │+ iHD   │ │        │ │        │   - overlay_vaapi
 │+ Go/Node│ │        │ │        │   - Go/Node agent
 │ agent  │ │        │ │        │     (HTTP server,
 │        │ │        │ │        │      arg translator,
 │        │ │        │ │        │      ffmpeg spawner)
 └───┬────┘ └────────┘ └────────┘
     │ writes segments
     ▼
 ┌──────────────────────────────────┐
 │ /transcode (NFS, shared with PMS)│
 │  plex-transcode-<session>/       │
 │   media-NNNNN.ts                 │
 └──────────────────────────────────┘
```

## Repo layout

- `shim/` — PMS-side `Plex Transcoder` replacement. Static binary or thin
  script that POSTs the task to the orchestrator and exits with worker's
  status.
- `orchestrator/` — task router. ~current clusterplex orchestrator minus
  arg-rewriting (which moves to the worker side, where it has direct fs
  access and no socket.io escape gymnastics).
- `worker/` — Dockerfile + agent. Custom Ubuntu image, stock ffmpeg with
  `--enable-vaapi --enable-libass`, libass, intel-media-driver, agent that
  receives task envelopes, translates Plex args → optimal HW chain, runs
  ffmpeg, writes NFS segments.
- `charts/scaleplex/` — Helm chart for k8s deployment (single chart, replaces
  clusterplex's bjw-s app-template usage).
- `docs/` — design notes, lessons inherited from the clusterplex iteration.

## Status

Bootstrap. Phase 1 (worker image) not yet started.

## Lineage

scaleplex inherits the lessons from
[Varashi/clusterplex#rewriter-plan](https://github.com/Varashi/clusterplex/tree/rewriter-plan)
and the live deployment in `Varashi/k8s` clusterplex namespace. The
`argRewriter.js` module from that fork is the seed for the worker-side arg
translator.
