# Lessons inherited from the clusterplex iteration (2026-04-15 → 2026-05-05)

Concrete pitfalls hit while running clusterplex on k8s-talos that scaleplex
should avoid by design.

## 1. Plex Transcoder is musl-compiled, blocks OpenCL

Plex's bundled `Plex Transcoder` is built with `--cc=x86_64-linux-musl-clang`.
Intel NEO OpenCL runtime (`libigdrcl.so`) is glibc. Plex returns
`CL_PLATFORM_NOT_FOUND_KHR` and falls back to SW tonemap. Cannot fix from
user space.

**scaleplex consequence**: stock-ffmpeg workers have no musl boundary.
`tonemap_vaapi` works directly; OpenCL not even needed.

## 2. `tonemap_vaapi` not in Plex's ffmpeg build

Plex compiles ffmpeg with `--disable-hwaccels` then re-enables a curated set
that excludes `tonemap_vaapi`. They keep `tonemap_cuda` and
`tonemap_opencl` only.

**scaleplex consequence**: build ffmpeg with the filters we actually want.

## 3. `inlineass` is a Plex-private filter

Plex uses `-map_inlineass 0:N` + `inlineass=...` filter to render subtitles
in-stream. Stock ffmpeg has only `subtitles=` and `ass=` filters which
expect a file path, not a stream index.

**scaleplex consequence**: when sidecar SRT exists, use stock `subtitles=`
on a transparent canvas + `overlay_vaapi`. When subs are embedded only,
pre-extract via a small ffprobe+ffmpeg step (worker-side, before main
encode).

## 4. dockermod's `LOCAL_RELAY_ENABLED=0` path is broken upstream

Setting it off doesn't actually disable the nginx relay; transcoder.js
silently aborts with "Work already sent". Forces every clusterplex install
to keep relay on, adding ~10s of HTTP-hop latency per segment cycle.

**scaleplex consequence**: workers write to NFS directly. PMS reads from
NFS directly. No relay. The shim only forwards task metadata, not segments.

## 5. EAE_ROOT path tied to PMS pod UUID

Plex regenerates `/run/plex-temp/pms-<UUID>/EasyAudioEncoder/` on every PMS
restart. Workers retain stale EAE process bound to old UUID; new tasks
fail with "EAE already running" → exit 187. Required PMS-postStart hook
to roll the worker DS on every PMS restart.

**scaleplex consequence**: don't use EAE on workers at all. Audio path
options:
1. Passthrough (`-c:a copy`) when client supports — most common case
2. libavcodec encoders (`-c:a eac3`/`ac3`/`aac`) — slight quality delta
3. Plex Transcoder as audio-only sidecar pod — last resort, brings musl/EAE
   issues back

## 6. Spegel caches mutable image tags

`pullPolicy: Always` doesn't refresh — Spegel serves the cached digest for
the tag. Burnt 3 hours debugging "why is my new code not running".

**scaleplex consequence**: bake `imagePullPolicy` documentation into the
chart; CI publishes both `:branch` (mutable, for discovery) and `:sha-XXXX`
(immutable, for HR). HR pins by sha.

## 7. NFS root_squash blocks lsio s6 init

`/codecs` chown'd by root in container maps to `nobody` on TrueNAS. lsio's
`4-setup-codecs-dir` exits 1, blocks `init-mods-end → svc-plex`. Required a
ConfigMap subPath to patch the s6 run script.

**scaleplex consequence**: workers run as a non-root user from the start
(no s6, no lsio init). Image's USER directive is `1000:1000`. Permissions
match NFS mount ownership. No chown needed.

## 8. bjw-s app-template SA naming defaults to release name

Set `serviceAccount.<id>: {}` and the SA gets named after the *release*,
not the *identifier*. Controllers reference SA by identifier, mismatch
breaks pod creation. Required `forceRename: <name>` to align.

**scaleplex consequence**: scaleplex deploys via bjw-s app-template
anyway — it's the homelab-standard chart and keeps storage / network /
scheduling in the operator's hands. The SA-naming gotcha is handled
explicitly with `serviceAccount.<id>.forceRename: <name>` where a
controller needs a named SA. (An earlier plan called for a first-party
chart; that was dropped — see `README.md` Deploy section.)

## 9. Orchestrator's `ROUND_ROBIN` strategy is broken

Sets the strategy in env, orchestrator never creates the task —
`Registered new job poster` fires but no `Creating single task` follows.
LOAD_RANK works but adds ~1.5s queue→run polling.

**scaleplex consequence**: simpler routing in our orchestrator. Random or
LB-by-load works; no socket.io strategy plumbing.

## 10. socket.io serialization quirks

Orchestrator received the env from PMS via socket.io. Many env vars went
through, but specific complex strings sometimes lost data. Required
diagnostic logging to confirm.

**scaleplex consequence**: HTTP/gRPC with explicit JSON schema. No
ambiguity.

## 11. Plex client profile gaps cause forced sub burn

LG WebOS Plex app reports `profile="Generic"` (empty XML) → Plex's default
capabilities don't declare HLS-with-text-track support → Plex burns subs
even when source is SRT and client could render natively.

**scaleplex consequence**: out of scope (PMS-side issue). Document as a
known constraint; users should patch client profiles or accept burn-in.

## 12. cold-start hitching at 4K

Encoder must build a buffer ahead of LG's playhead. Real-time encode at
4K HEVC + sub burn = ~10-30s of cold-start hitching before steady state.

**scaleplex consequence**: persistent worker daemon means warmup is
amortized across sessions. First session per worker still pays warmup;
subsequent sessions reuse the loaded image, libass cache, etc.

## 13. Stale transcoder orphans cause GPU contention

Plex sometimes spawns a new transcoder without killing the previous;
both contend for the Arc GPU's encode block, both hitch.

**scaleplex consequence**: worker agent keys on `session_id` and force-
kills any prior ffmpeg with the same ID before starting a new one.
