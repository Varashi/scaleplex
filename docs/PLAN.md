# scaleplex implementation plan

## Phase 1 — Worker image (3 days)

Build `ghcr.io/varashi/scaleplex_worker:<tag>`:
- Base: Ubuntu 24.04
- ffmpeg compiled or apt-installed with `--enable-vaapi --enable-libdrm
  --enable-libass --enable-libfreetype --enable-libfontconfig`
- Verify filters present: `tonemap_vaapi`, `overlay_vaapi`, `scale_vaapi`,
  `subtitles`, `ass`, `format`, `hwupload`, `hwdownload`
- intel-media-driver-non-free (for iHD VAAPI)
- libva2 + drivers
- A Go (preferred for size) or Node agent stub at `/app/agent`
- Healthcheck on agent's HTTP port

Test rig: launch container interactively, exec a test transcode of an HDR AV1
4K MKV with sidecar SRT through the full HW chain, confirm output plays.

Concrete filter recipes to validate before moving on:

```bash
# HDR→SDR no subs (tonemap_vaapi)
ffmpeg -hwaccel vaapi -hwaccel_output_format vaapi \
  -hwaccel_device /dev/dri/renderD128 \
  -i hdr.mkv \
  -filter_complex "[0:v]hwupload,scale_vaapi=w=1920:h=1080:format=p010,tonemap_vaapi=transfer=bt709:format=nv12[v]" \
  -map "[v]" -c:v hevc_vaapi -qp 22 -t 10 out.mp4

# SDR + SRT subs via overlay_vaapi
ffmpeg -hwaccel vaapi -hwaccel_output_format vaapi \
  -hwaccel_device /dev/dri/renderD128 \
  -i video.mkv \
  -filter_complex "
    [0:v]hwupload,scale_vaapi=w=1920:h=1080:format=nv12[main];
    movie=subs.srt,ass=fontsdir=/usr/share/fonts/truetype/dejavu,format=bgra,
      hwupload=extra_hw_frames=64[sub_hw];
    [main][sub_hw]overlay_vaapi=eof_action=pass[v]
  " \
  -map "[v]" -c:v h264_vaapi -qp 18 -t 10 out.mp4

# HDR + sub burn (combined)
# HDR pass tonemap then overlay sub at SDR
```

## Phase 2 — Worker agent (3 days)

`worker/agent/` (Go preferred):
- HTTP server on `:3501` with endpoints:
  - `POST /task` — receive `{session_id, args[], env{}, cwd}` from orchestrator
  - `POST /task/:id/kill` — SIGTERM running ffmpeg
  - `GET /healthz` — readiness/liveness
  - `GET /capability` — declare HW codecs/filters available
- Local arg translator (port `argRewriter.js` from clusterplex fork to Go)
  - Refine for stock ffmpeg targets (`tonemap_vaapi`, `overlay_vaapi` cleanly)
  - File system access for sidecar SRT lookup is local (no NFS RO mount on
    orchestrator anymore)
- ffmpeg spawn manager:
  - Track PID, stream stdout/stderr, parse progress lines
  - Heartbeat back to orchestrator over HTTP/gRPC
  - On task kill or orchestrator-disconnect: SIGTERM, then SIGKILL after 5s
- Session cleanup:
  - On new task receipt with same session_id, kill prior ffmpeg first
    (avoids stale-orphan problem we hit in clusterplex)
- **Latency-reduction features (see `docs/LATENCY.md`):**
  - Pre-warm at startup: hold `/dev/dri` fd, mmap codec libs, init libass +
    fontconfig, JIT the iHD VPP/encoder programs via a 1s dummy transcode
  - Adaptive probesize: ffprobe source once with progressive sizes (1MB →
    20MB), pick minimum that yields a full stream listing, cache by
    inode+mtime, replace Plex's `-probesize 20MB / -analyzeduration 20MB`
  - GOP-aligned segments: rewrite `-force_key_frames` to align with
    `-segment_time`
  - inotify watch on `/transcode` session dir; push "segment 0 ready" the
    moment it lands

## Phase 3 — Orchestrator (1 day)

Fork of current clusterplex orchestrator, stripped:
- Remove `argRewriter.js` (logic moved to worker)
- Keep socket.io OR migrate to HTTP/gRPC
- Keep worker registry, load tracking, PMS-restart hook (Role+RoleBinding
  reused from clusterplex)
- Forward task envelope verbatim to selected worker

## Phase 4 — PMS-side shim (1 day)

`shim/`:
- Reuse clusterplex's transcoder.js skeleton minus LOCAL_RELAY logic
- Or rewrite as a static Go binary for image-size minimisation
- Talks to orchestrator via the same protocol the worker talks back

## Phase 5 — Helm chart (1 day)

`charts/scaleplex/`:
- StatefulSet for orchestrator (1 replica) OR Deployment
- DaemonSet for workers (gpu-worker selector)
- Deployment for PMS (CPU worker, vsan PVC, shim DOCKER_MOD)
- ServiceAccount + Role + RoleBinding for worker-restart hook
- ConfigMap for worker arg-translator overrides

## Phase 6 — End-to-end tests (3-5 days)

Test matrix:
- Codecs: AV1, HEVC, H264, VC-1 sources × H264, HEVC targets
- HDR: SDR, HDR10, Dolby Vision sources
- Subs: none, SRT sidecar, embedded SRT, PGS (which forces burn anyway)
- Clients: LG WebOS, Plex Web (Chrome), Plex Web (Firefox), Apple TV
- Optimize jobs (vs live transcode) — same args path?
- Concurrent sessions (3 workers, each transcoding)

Each combo: validate playback works end-to-end + measure first-frame latency,
sustained CPU%, GPU encode utilization, memory footprint, segment write
rate vs real-time.

## Phase 7 — Parallel deployment (2 days)

- Deploy scaleplex in a NEW namespace (`scaleplex`) alongside `clusterplex`
- Move one Plex library to scaleplex via a separate scaleplex-pms pod
- Soak for 1+ week
- Cut over remaining libraries
- Decommission clusterplex namespace

## Risks

| risk | mitigation |
|---|---|
| Plex session state machine breaks if shim doesn't faithfully proxy | Phase 6 testing, fallback to running stock ffmpeg with original args verbatim if our translator can't match a pattern |
| EAE (EAC3 transcode) needs Plex's licensed binary | Bundle Plex Transcoder as an audio-only sidecar; or use libavcodec eac3 (slight quality difference); or force passthrough where client supports |
| Plex updates break shim assumptions | Renovate watches Plex image tag; CI smoke-tests shim against new args |
| Worker image size grows ~200MB | Spegel handles in-cluster distribution; first pull is on a single node |

## Total effort

2-3 weeks focused. Can be parallel with continued clusterplex usage during
development.

## Open questions

- Go vs Node for the agent? Go = smaller image, easier static binary. Node =
  reuse argRewriter.js directly. Lean Go.
- gRPC vs HTTP for orchestrator↔worker? gRPC has streaming for progress
  heartbeats. HTTP is simpler. Lean HTTP+SSE for now.
- Keep orchestrator as a separate component, or bake routing into the shim?
  Separate is cleaner for multi-PMS future scaling.
