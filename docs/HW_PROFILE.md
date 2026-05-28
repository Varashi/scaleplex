# Per-node HW profile + honor-Plex-HW/SW (future feature)

> **Status: PROPOSED (2026-05-24). Design target, not yet built.** Phase 1
> (honor-default + per-node `SCALEPLEX_FORCE_HW` override, single VAAPI
> backend) is the near-term, low-risk slice. Phases 2–3 (multi-backend
> node profiles, capability-aware dispatch) are deliberately deferred until
> cluster heterogeneity is real — building the abstraction speculatively
> would guess the wrong seams. See `REWRITER.md` for today's behaviour.

## Problem

Today the rewriter **always re-accelerates**: any session Plex emits as SW
(libx264/libx265, SW decode, SW filters) is reshaped to the worker's VAAPI
HW pipeline (decoder hwaccel inject, `libx264→h264_vaapi`, `scale→scale_vaapi`,
tonemap/sub-burn to HW). The HW backend is hardcoded to Intel iHD/VAAPI.

Two limits follow:

1. **No way to honor a genuine SW choice.** If a session should run SW
   (a codec/profile the node's HW can't do — e.g. 10-bit H.264 on Intel —
   or a deliberate quality choice), the rewriter has no path for it; it
   forces HW and either over-reshapes or fails.
2. **Single-backend assumption.** A non-Intel node (NVENC/QSV/AMF/RKMPP)
   cannot be added — the reshape emits vaapi graphs unconditionally.

### Why this is subtle here (corpus evidence, 2026-05-24)

1089 captured pre-rewrite PMS argv (`~/scaleplex-corpus`), 899 video
transcodes:

| What Plex emits | Count |
|---|---|
| HW-encode (vaapi) | 880 (98%) |
| **SW-encode (libx264/5) despite HW enabled** | **19 (2%)** |
| HW-decode (`-hwaccel:0`) | 834 |
| SW-decode | 65 (46 pair with HW-encode, 19 fully SW) |

The 19 fully-SW cases break down as: 8× subtitle burn-in (`inlineass`),
7× HDR tonemap (`tonemap=hable`), 5× plain AV1 downscale (`libdav1d`).
**Every one is a capability PMS's own container lacks but the Arc worker
has** (AV1 HW-decode, HW HDR-tonemap, HW sub-burn). So here the forced
re-acceleration *is* scaleplex's core value — honoring Plex's SW choice
would dump 4K HDR / AV1 / sub-burn onto the worker's 4-vCPU CPU.

**Conclusion:** honor-by-default is correct as a *framework default*, but
this homelab must run with force-HW on. The feature's local value is
correctness/portability, not behaviour change. (Local wins came from the
device/crf/preset fork patches, `0116`–`0118`.)

## Design

### Two orthogonal axes — do not conflate them

"Honor Plex argv" splits into:

1. **HW-vs-SW decision** — *default: honor Plex's choice.* Override:
   force-HW per node (capability-gated, below).
2. **Backend targeting** (vaapi / nvenc / qsv / device path) — **always
   node-local, never Plex's.**

The backend axis is load-bearing in a heterogeneous cluster: PMS emits
**vaapi** HW argv (corpus: 880×). Dispatched to an NVIDIA node, "honoring"
that vaapi graph *fails* — vaapi won't run on NVENC. So even the honor
path must remap the backend to the node's. **Honor the *decision* (HW vs
SW); the *implementation* (which GPU/backend) is always node-local.** This
generalises what patch `0116` already did for the device path (retarget at
device-open regardless of the path Plex baked in).

### Force-HW is capability-gated, not unconditional

"Force HW per node" must mean: *re-accelerate every session the node's HW
supports; honor-SW otherwise.* A node that can't do a given operation
(10-bit H.264 on Intel, an unsupported AV1 profile) keeps that session SW
even under force-HW. The corpus's 19 SW cases are all Arc-capable → force-HW
re-accelerates all; a genuine node-incapable case stays SW automatically.

### The new primitive: per-node HW profile

The real design object. Per node, **auto-detected at worker startup**
(probe VA / NVENC entrypoints during pre-warm — the worker already
pre-warms VAAPI), with config override for edge cases:

```
HWProfile {
  backend:     vaapi | nvenc | qsv | amf | rkmpp | ...
  device:      <node-local render node / CUDA index>
  encoder_map: { libx264 -> h264_<backend>, libx265 -> hevc_<backend>, av1 -> ... }
  filter_map:  { scale -> scale_<backend>, tonemap -> tonemap_<backend>,
                 overlay -> overlay_<backend>, hwupload/hwdownload semantics }
  caps:        [ hwdec_av1, hwdec_hevc, hwdec_vc1, enc_10bit_hevc,
                 enc_10bit_h264, hw_tonemap, hw_subburn, ... ]
  force_hw:    bool   # the per-node override
}
```

The rewriter's HW-reshape phases become **parameterised by this profile**
instead of hardcoded iHD/VAAPI. `force_hw` gates the HW-vs-SW decision;
`caps` gates whether a forced session can actually be HW'd.

### Capability-aware dispatch (phase 3)

Once nodes carry `caps`, the orchestrator can route by capability: send an
AV1 session only to AV1-decode-capable nodes; a 10-bit-H.264 session to a
node that can (or to SW). This also subsumes the parked CPU-fallback idea —
a node with no HW for a session honors SW → runs CPU.

## How this unifies parked threads

- **CPU fallback** (`project_scaleplex_cpu_fallback`, parked "no driver
  while all nodes identical"): honor-SW + caps gating *is* the CPU-fallback
  mechanism. The hetero-GPU plan is its driver.
- **Blanket-trust Plex graph** (`hwdecode_blanket_trust`, "future general
  SW-filter detection"): honor-default is exactly that.
- **Drop-in direction** (rewriter→fork migration, `0116`–`0118`): honor by
  default = the worker does less argv surgery; the binary already
  self-targets the device.

## Phasing

- **Phase 1 — honor-default + `SCALEPLEX_FORCE_HW`, single backend (vaapi).**
  Near-term. Gate the existing SW→HW reshape: when the session is fully-SW
  and `!force_hw`, skip the HW-reshape phases (decoder hwaccel inject,
  encoder swap, filter→vaapi, device inject) but keep the transport/audio
  scrubs (progressurl/segment relay, eae→eac3, Plex-private flag strip) so
  the SW transcode still runs correctly on the worker. **Deploy gotcha:
  ship the default-flip and the homelab worker DS `SCALEPLEX_FORCE_HW=1` in
  the same change — otherwise all 19 HDR/AV1/sub-burn jobs (+46 SW-decode
  partials) regress to CPU.** (Historical: also sidestepped the SW-HDR
  reshape's hardcoded-`hable` exit-8 bug — fixed in v1.5.0 by capturing any
  algo via the rewriter, then made moot in v1.6.1 when the SW-HDR reshape was
  collapsed onto `extractGraphFacts → composeBurn`.)

- **Phase 2 — node HW profile abstraction + multi-backend reshape.**
  Parameterise encoder_map / filter_map / device by backend; auto-detect at
  startup. Build **when a non-Arc node actually exists**, not before.

- **Phase 3 — capability-aware dispatch + CPU-fallback unification.**

## Override semantics (phase 1)

- `SCALEPLEX_FORCE_HW=1` (worker DS env, per-node) → re-accelerate every
  node-HW-supported session (today's behaviour). The homelab sets this.
- unset / `0` → honor Plex's HW-vs-SW decision; SW stays SW (CPU on worker).
- Backend is always the node's (device already via `SCALEPLEX_RENDER_DEVICE`
  / patch `0116`).

## Heterogeneous fleet + scheduling (PR4, scaleplex#77)

The worker self-detects its backend; the admin's only real lever is **which
hardware each worker gets**. Same image everywhere.

### Per-worker (deployment) config
- Assign hardware → backend is auto-detected (`WORKER_BACKEND=auto`, the default):
  - NVIDIA: `runtime: nvidia` / `gpus: all` (docker) or NVIDIA device-plugin +
    runtimeClass (k8s) → `/dev/nvidia0` present → **nvenc**.
  - Intel/AMD: `/dev/dri` (docker `devices:` / Intel GPU device-plugin) →
    a `renderD*` node → **vaapi**.
  - No GPU assigned → **sw** (CPU).
- Pin explicitly with `WORKER_BACKEND=vaapi|nvenc|sw` only for edge cases
  (dual-GPU host, force-SW for testing).
- `WORKER_MAX_SESSIONS=N` — soft concurrency cap; worth setting low on a CPU
  worker. `SCALEPLEX_FORCE_HW` is meaningful only on HW workers (forced off on sw).
- The worker reports `backend` + `gpu_load` on `/capability` (NVIDIA `gpu_load`
  via an `nvidia-smi` utilization reader; Intel via i915 sysfs/PMU).

k8s shape: one worker Deployment/DaemonSet **per hardware class** (its own
nodeSelector + device-plugin), all the same image + `WORKER_BACKEND=auto`. The
orchestrator's DNS/LIST/PUSH discovery sweeps them into one pool. Adding a
NVIDIA/CPU tier = deploy another worker set — **no orchestrator reconfig**.

### Orchestrator scheduling knobs (env)
- `SCALEPLEX_LB_STRATEGY` = `load` (default) | `round-robin` | `least-sessions`
  | `random` — the within-tier ordering.
- `SCALEPLEX_PREFER_HW` = `1` (default) | `0`. On → tier `[HW, SW]`, order each
  by the strategy, HW first (CPU node is overflow). `0` → one flat pool.

Both default to the pre-PR4 behavior (`load`, HW-preferred). On a homogeneous
fleet the tiering collapses to a single tier — no-op. The orchestrator needs no
per-worker config; it tiers + ranks from the reported `backend` + load.
`/workers` JSON gains a per-worker `backend` for observability.

## Plex-Pass gate (scaleplex#78)

Plex HW transcoding is a Plex-Pass-only feature: without an active Pass, PMS's TPU
emits SW-only argv. scaleplex's HW *re-acceleration* paths — `SCALEPLEX_FORCE_HW=1`
(force HW on a SW argv) and the cross-backend reshape (HW→HW retarget) — would
otherwise hand a non-Pass account HW transcoding it isn't entitled to (a TOS
issue). The gate confirms an active Pass before either path runs; honor-source
(running what Plex itself emitted) is never gated.

Three layers, all shipped, fail-closed:

- **L1 — startup warning.** The worker logs a WARN when `SCALEPLEX_FORCE_HW=1`,
  flagging it as a Pass-only feature.
- **L2 — dockermod reads Preferences.xml.** The PMS dockermod/shim (in the PMS
  container) reads `myPlexSubscription` from `Preferences.xml` per spawn and
  forwards `SCALEPLEX_PASS_ACTIVE=0|1` in the session env. Local + always fresh —
  no per-session HTTP probe to flake. Path override: `SCALEPLEX_PMS_PREFS`.
- **L3 — worker self-gate.** `hwAccelAllowed()` prefers the per-session
  `SCALEPLEX_PASS_ACTIVE` when present; otherwise it falls back to a cached
  (5 min) HTTPS query of `myPlexSubscription` via the session's PMS base + token.
  Fail-CLOSED: a missing/`0` subscription, query error, or timeout DENIES
  re-acceleration and the session falls back to Plex's emitted (SW) pipeline.
  Inert (allow) only when no PMS base/token is wired at all (a bare/test worker).

A SW (CPU) worker downgrades everything to software and grants no entitlement, so
it is never Pass-gated (`isForeignHWSource` reports not-foreign for `sw`, and
`SCALEPLEX_FORCE_HW` is forced off there).

`SCALEPLEX_PASS_ACTIVE` is read from the per-session env only (not the worker
process env), so a static worker-level value can't override the per-spawn freshness.
