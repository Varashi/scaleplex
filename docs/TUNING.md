# Tuning

Operator-facing reference for the environment knobs that shape scaleplex
transcode quality and behaviour. All knobs are set on the **worker
DaemonSet** container env unless noted otherwise.

scaleplex ships with sensible defaults — a fresh deployment needs only
the two bootstrap variables in the last section. The tuning knobs below
exist for calibrating against hardware or content that differs from the
reference cluster (3× Intel Arc A310, iHD driver).

## Quality / behaviour tuning

| Env var | Default | Effect | When to tune |
|---|---|---|---|
| `-crf_qp_offset` (encoder argv option, not env) | `6` | Added to Plex's CRF value to land the VAAPI encoder's QP at perceptually-equivalent quality. Plex emits `-crf:0 N`; the fork's VAAPI encoder maps it to `QP = clamp(N + offset, 0, 51)` (patch 0117), then patch 0105 routes QP + `-maxrate` to QVBR. Replaces the retired `HW_QP_CRF_OFFSET` env knob. | Lower it if VAAPI output looks worse than Plex's libx264 at the same CRF. Raise it for lighter bitrates at the cost of quality. Calibrated for iHD on Arc — other GPU generations may want a different offset. |
| `-preset NAME` / `-compression_level N` (encoder argv) | (built-in Arc A310 bench map) | The fork's VAAPI encoder maps a libx264 `-preset` name → `compression_level` (iHD TargetUsage 1–7, where 7 = fastest) at init, unless `-compression_level` is set directly (patch 0118). Baked map: `ultrafast/superfast/veryfast=7/7/6`, `faster/fast/medium=5/4/4`, `slow/slower/veryslow=3/2/1`, unknown→7. Replaces the retired `SCALEPLEX_PRESET_MAP` env knob. | If a particular Plex preset gives unexpected throughput or quality on your GPU, override by injecting `-compression_level N` directly. When Plex omits a preset the encoder keeps the iHD driver's balanced default (~TU 4). |
| `SCALEPLEX_FFMPEG_LOGLEVEL` | `info` | Replaces the `-loglevel` value in Plex's argv (Plex sends `quiet`). `info` is required for the agent's stderr readiness signalling. Set to `debug` to expose per-cycle Phase-4 canThrottle diagnostics (`scaleplex/ct: PUT / avio_read / body`). | Throttle or stream-mapping debugging only. Leave at `info` in production — `debug` is verbose. |
| `SCALEPLEX_SUB_RENDER_HEIGHT` | `1080` | Caps the height at which the text sub-burn pre-render rasterises libass; the band is rendered at that resolution and the main graph HW-upscales it (`scale_vaapi`) back to the output band before `overlay_vaapi`. libass + qtrle cost scale ~linearly with rendered pixel area, so at 4K the `1080` default cuts pre-render CPU ~4.25× (and the FIFO + main-side hwupload bytes) for marginally softer glyph edges. Outputs ≤ the cap are unaffected (native). Tiers: `720` (~9× cheaper, visibly soft), `1080` (~4.25×, clean), `1440` (~2.25×, indistinguishable from native). Set `0` to opt out (native full-res render). | Raise to `1440`/`0` if 4K burned-in text looks too soft on your displays; lower to `720` only under heavy CPU pressure. Emits a `sub-prerender:render=WxH` change tag when active. |
| `SCALEPLEX_TONEMAP` | `opencl` | Backend for the HDR→SDR tonemap **Plex's argv asked for** — scaleplex never decides whether to tonemap, only how. `opencl` keeps Plex's algorithm-selectable `tonemap_opencl` chain (~15% slower than the fixed curve, still ~10× realtime at 4K HDR→1080p on an Arc A310). `vaapi` collapses it to iHD's fixed BT.2390 EETF `tonemap_vaapi`. | Set to `vaapi` to fall back to the fixed curve (no rebuild) if OpenCL misbehaves on a worker. |
| `SCALEPLEX_TONEMAP_ALGO` | `hable` | Fallback algorithm — one of `hable`, `mobius`, `reinhard`, `bt2390`, `linear`, `gamma`, `clip`. Used **only** by the `reFilterHDR` / `reFilterHDRAss` SW-chain reshapes, where the algorithm isn't carried through. When Plex sends a `tonemap_opencl` chain its algorithm is kept straight off the argv, so this knob doesn't apply. No effect under `SCALEPLEX_TONEMAP=vaapi`. | Rarely needs tuning. |

## Deployment / bootstrap

| Env var | Default | Effect |
|---|---|---|
| `SCALEPLEX_PMS_BASE_URL` | (required) | Cluster URL of the PMS relay sidecar, e.g. `http://<pms-service>.<namespace>.svc:32499`. The rewriter uses it to retarget `-progressurl`, `-segment_list`, `-canthrottleurl`, and `-manifest_name` away from PMS loopback so worker pods can reach them. |
| `X_PLEX_TOKEN` | (required) | Per-session Plex auth token, appended as a query param to the relay URLs above so the relay can re-issue authenticated PUTs to PMS. Supplied per session by the shim. |
| `HW_RENDER_DEVICE` | `/dev/dri/renderD128` | VAAPI render node passed to `-init_hw_device`. |
| `HW_VAAPI_DRIVER` | `iHD` | libva driver name. `iHD` for Intel Gen9+ / Arc. |
| `HW_LIBVA_DRIVERS_PATH` | (libva auto-discovers) | Only needed to override the driver search path, e.g. when pointing at a bundled driver cache. |
| `WORKER_MAX_SESSIONS` | `0` (unlimited) | Soft cap on concurrent sessions per worker. At the cap the worker refuses new dispatch so the orchestrator routes elsewhere. |

## Diagnostics / development

These knobs are **off by default** and exist for developers and for
troubleshooting a specific session. None of them should be set on a
normal production deployment — argv capture in particular writes a
growing corpus of JSON files to shared storage.

| Env var | Where | Default | Effect |
|---|---|---|---|
| `WORKER_DUMP_ARGV` | worker DaemonSet | unset (off) | Set to `1` to capture every dispatched PMS argv. Logs the argv to worker stderr and writes a per-session JSON capture (argv in + rewritten argv + outcome) under `$WORKER_ARGV_CORPUS_DIR`. Only the literal value `1` enables it — `0`/`false`/unset all disable. Use when investigating new PMS argv shapes (HW-decode mode, Plex version bumps). |
| `WORKER_ARGV_CORPUS_DIR` | worker DaemonSet | `/transcode/_argv-corpus` | Target directory for `WORKER_DUMP_ARGV` JSON captures. The default lands on the shared transcode volume so captures survive pod restarts and are reachable from outside the cluster. No effect unless `WORKER_DUMP_ARGV=1`. |
| `SCALEPLEX_DUMP_ARGV` | PMS shim | unset (off) | Set to `1` (or `true`) to make the PMS-side shim log the original Plex transcoder argv before it is handed to the orchestrator. Pairs with `WORKER_DUMP_ARGV` to capture both ends of a session. |
| `SCALEPLEX_DEBUG` | PMS shim | unset (off) | Set to `1` (or `true`) for verbose shim decision logging (dispatch target, fallback reasons). |

For verbose **worker-side** ffmpeg / throttle logging use
`SCALEPLEX_FFMPEG_LOGLEVEL=debug` (see the quality/behaviour table) —
that is the per-cycle Phase-4 canThrottle diagnostic channel.

## Notes

- **No HEVC SW-encode path.** Plex's software encode only ever emits
  h264 / libx264; HEVC encode is gated behind Plex's "enable HW encode"
  server setting. The rewriter's libx265 → hevc_vaapi mapping therefore
  never fires in practice.
- **Encoder choice is Plex-driven.** Plex picks libx264 vs libx265 per
  session from server prefs + client capabilities; the rewriter maps
  1:1 to the VAAPI variant. There is no worker-side encoder override.
- **Removed knobs.** `HW_QP_HDR_SUB_BOOST`, `SCALEPLEX_RC_MODE`,
  `SCALEPLEX_KEEP_COPYTS_HLS_STRIP`, and `SCALEPLEX_INLINEASS_PASSTHROUGH`
  were retired as their behaviour moved into scaleplex-ffmpeg fork
  patches or was found unnecessary. They are no-ops if still set.
