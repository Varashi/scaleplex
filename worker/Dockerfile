# scaleplex worker image — dual-backend (Intel VAAPI + NVIDIA NVENC).
#
# scaleplex-ffmpeg7 = jellyfin-ffmpeg + Plex-backport patch layer (see
# ../scaleplex-ffmpeg/). The deb compiles with BOTH backends enabled
# (VAAPI + iHD + opencl AND CUDA + NVENC + NVDEC + libnpp), so a
# single image carries both runtimes. At startup the worker agent's
# selectDialect() probes /dev/nvidia0 and /dev/dri/renderD* and picks
# automatically; WORKER_BACKEND env overrides. The runtime layer the
# host doesn't have sits unused (~200MB of CUDA libs on an Intel-only
# host; ~80MB of VAAPI userspace on an NVIDIA-only host) — accepted
# cost for one-image operational simplicity.
#
# Base: nvidia/cuda:12.6.3-runtime-ubuntu24.04 — ships CUDA runtime
# libs (libcuda, libcudart) needed by NVENC/NVDEC. NVENC/NVDEC encoder
# shared libs themselves (libnvidia-encode, libnvcuvid,
# libnvidia-opticalflow) come from the host at runtime via
# nvidia-container-toolkit's CDI spec. The Intel VAAPI stack is added
# on top via apt (intel-media-va-driver-non-free + libva + opencl).
#
# The deb is built by ../scaleplex-ffmpeg/build.sh (or the
# build-ffmpeg GHA workflow) and placed in ../scaleplex-ffmpeg/dist/
# before docker build. Build context root must be the scaleplex repo
# root (matches existing `worker/...` paths).

FROM golang:1.27-bookworm AS agent-build
WORKDIR /src
COPY worker/agent/go.mod worker/agent/go.sum ./
RUN go mod download
COPY worker/agent/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/scaleplex-agent ./...

FROM nvidia/cuda:12.6.3-runtime-ubuntu24.04
LABEL org.opencontainers.image.source=https://github.com/Varashi/scaleplex
LABEL org.opencontainers.image.description="scaleplex worker — scaleplex-ffmpeg + Intel VAAPI + NVIDIA NVENC + Go agent"
LABEL org.opencontainers.image.licenses=MIT

# multiverse for intel-media-va-driver-non-free; scaleplex-ffmpeg
# expects libva to come from the system (it doesn't bundle the VAAPI
# userspace, same as jellyfin-ffmpeg). CUDA runtime libs come from the
# base image; NVENC/NVDEC encoder libs (libnvidia-encode, libnvcuvid)
# are bound in at runtime via nvidia-container-toolkit's CDI spec.
#
# AMD radeonsi support (#123): mesa-va-drivers ships radeonsi_drv_video.so
# with the matching libva ABI under /usr/lib/x86_64-linux-gnu/dri/ so libva
# auto-discovers it from the default search path (no LIBVA_DRIVERS_PATH
# override). The scaleplex-ffmpeg deb bundles a (stale-ABI) copy under
# /usr/lib/scaleplex-ffmpeg/lib/dri/ — left in place but not on the default
# search path; the rewriter routes via the vendor-aware default driver
# (#124). mesa-opencl-icd registers the rusticl/Clover platform that
# `tonemap_opencl` needs on AMD (Intel keeps intel-opencl-icd; NVIDIA uses
# the CUDA OpenCL ICD from the base image).
COPY scaleplex-ffmpeg/dist/scaleplex-ffmpeg7_*.deb /tmp/
RUN sed -i 's|^Components: main universe$|Components: main universe multiverse|' \
      /etc/apt/sources.list.d/ubuntu.sources && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      /tmp/scaleplex-ffmpeg7_*.deb \
      intel-media-va-driver-non-free \
      intel-opencl-icd \
      mesa-va-drivers \
      mesa-opencl-icd \
      libva2 libva-drm2 \
      vainfo \
      fontconfig fonts-dejavu-core fonts-noto-core \
      strace \
      tini && \
    rm /tmp/scaleplex-ffmpeg7_*.deb && \
    rm -rf /var/lib/apt/lists/*

# Slim fontconfig config — restricts the font search universe to the
# DejaVu dir only. libass's `subtitles=` filter calls fontconfig once at
# filter init for fontselect; with the system-wide config (~100+ font
# faces from fonts-noto-core etc.) that scan dominates first-frame
# latency and pushes startup speed below realtime for the first ~1s of
# every transcode. Pinning to dejavu drops the corpus to ~14 files and
# matches Plex's own `fonts.conf` strategy (point at one private dir,
# nothing else). Combine with rewriter's `force_style='FontName=DejaVu
# Sans'` to skip fontconfig pattern matching entirely on the primary
# face.
RUN install -d -m 0755 /opt/scaleplex
COPY worker/fonts.conf /opt/scaleplex/fonts.conf

# /usr/lib/scaleplex-ffmpeg/{ffmpeg,ffprobe} is the install path;
# symlink into /usr/bin so the agent's hardcoded paths keep working.
RUN ln -sf /usr/lib/scaleplex-ffmpeg/ffmpeg  /usr/bin/ffmpeg && \
    ln -sf /usr/lib/scaleplex-ffmpeg/ffprobe /usr/bin/ffprobe

# render group (568) so container UID 1000 can open /dev/dri/renderD*
RUN groupadd -g 568 render2 || true && \
    useradd -u 1000 -g users -G video,render2 -m -s /bin/bash scaleplex || true

COPY --from=agent-build /out/scaleplex-agent /usr/local/bin/scaleplex-agent

# File-capability so the agent can perf_event_open() the i915 PMU even
# when running as uid 1000. K8s `capabilities.add` alone only seeds the
# container's bounding set; Linux drops effective caps on exec for
# non-root processes unless the binary carries cap bits via setcap.
# `cap_perfmon=ep` promotes them on exec — strictly narrower than the
# legacy SYS_ADMIN that the kernel split out in 5.8. Older Intel iGPU
# workers don't need this (sysfs reader path); harmless if unused.
# The pod manifest also adds PERFMON to the container's bounding set so
# the file cap actually has somewhere to land.
RUN apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libcap2-bin && \
    setcap cap_perfmon=ep /usr/local/bin/scaleplex-agent && \
    apt-get purge -y --auto-remove libcap2-bin && \
    rm -rf /var/lib/apt/lists/*

ENV LIBVA_DRIVER_NAME=iHD
# Backend selection — default `auto` probes /dev/nvidia0 first, falls
# back to VAAPI. Explicit "vaapi" / "nvidia" overrides the probe; the
# worker logs the resolved backend at startup. See worker/agent/dialect.go.
ENV WORKER_BACKEND=auto
# NVIDIA visibility — overridable by --gpus / nvidia-container-toolkit
# CDI request at runtime. Baked-in defaults so the binary's NVML probe
# works under a plain docker run --gpus=all without an explicit CDI
# device spec.
ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=video,compute,utility
# libass + fontconfig: point at the slim config so fontselect scans 14
# DejaVu files instead of the system corpus. The rewriter sets HOME=
# /home/ubuntu in the spawn env so libass finds the per-user cache the
# next stage builds.
ENV FONTCONFIG_FILE=/opt/scaleplex/fonts.conf
EXPOSE 3501

# Pre-populate the system fontconfig cache against the slim config.
# Runs as root so /var/cache/fontconfig is writeable; uid 1000 has
# read access at runtime, so the spawned ffmpeg/libass picks up the
# cache without any HOME tricks.
#
# The leading `rm -rf` is load-bearing: the fontconfig apt postinst already
# populated /var/cache/fontconfig in a LOWER image layer, and a later
# fc-cache cannot rewrite those existing cache files through overlayfs in a
# layered builder (BuildKit / buildah). It falls back to writing per-dir
# caches into the read-only /usr/share/fonts/truetype/* dirs, those writes
# fail, and fc-cache exits 2 — which broke the worker image build after
# v1.11.1 (root-caused 2026-06-03 by reproducing the layered build locally).
# Clearing the stale caches + creating the user cachedir up front lets
# fc-cache write everything fresh in this layer. On failure the captured log
# is dumped so a future regression is visible (the old `>log && tail` hid it).
#
# Without a populated cache libass's `subtitles=` filter triggers a
# fontselect on the first subtitle frame; on the original system-wide
# config that scan blocked for 2+ minutes on the worker pod, ffmpeg
# never produced a segment, and Plex's HLS client timed out with
# "Connection error" (live repro 2026-05-08: Plex Android, force-burn
# subs + seek, 2 min wall clock no segments).
#
# Cache dirs get chown'd to uid:1000 immediately after — libass's
# FcConfigParseAndLoad path still tries to write fallback cache
# entries on demand (e.g. when a FontName= override in inlineass=
# resolves to an unknown family and fontconfig has to synthesize a
# match). Without write access, fontconfig prints "No writable cache
# directories" to stderr — usually a benign warning, but with PMS's
# `-loglevel quiet` argv it becomes the only visible stderr and
# easily masks the real fatal (see 2026-05-31 Ghosts S2E1 diag where
# the warning was misread as the proximate cause of exit-145).
RUN rm -rf /var/cache/fontconfig/* /home/ubuntu/.cache/fontconfig && \
    install -d -o 1000 -g 1000 -m 0755 /home/ubuntu/.cache /home/ubuntu/.cache/fontconfig && \
    { FONTCONFIG_FILE=/opt/scaleplex/fonts.conf fc-cache -f -v >/tmp/fc-cache.log 2>&1 || { cat /tmp/fc-cache.log; exit 1; }; } && \
    tail -3 /tmp/fc-cache.log && \
    rm -f /tmp/fc-cache.log && \
    chown -R 1000:1000 /home/ubuntu/.cache /var/cache/fontconfig

# Default to the unprivileged uid 1000 (#68). Prod overrides this anyway
# (k8s `runAsUser: 1000`, compose `user: "1000:100"`), so this is defense-in-
# depth for external/ad-hoc invocations (`docker run ghcr.io/varashi/...`).
# The agent binary carries cap_perfmon=ep via setcap so the i915 PMU reader
# (Intel) still works for uid 1000; render2 (gid 568) + video group memberships
# carry /dev/dri/renderD* access; nvidia-container-toolkit handles /dev/nvidia*
# perms via CDI device specs.
USER scaleplex
ENTRYPOINT ["/usr/bin/tini","--","/usr/local/bin/scaleplex-agent"]
