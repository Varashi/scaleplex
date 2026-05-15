# scaleplex worker image
#
# Ubuntu 24.04 + scaleplex-ffmpeg7 + Intel iHD VAAPI driver + fonts.
# scaleplex-ffmpeg7 = jellyfin-ffmpeg + a small Plex-backport patch
# layer (see ../scaleplex-ffmpeg/). Same upstream lineage as
# jellyfin-ffmpeg, same featureset (VAAPI HW tonemap, overlay_vaapi,
# sub2video etc.), plus four DASH muxer extensions that let the worker
# rewriter drop a lot of seek-resume scaffolding.
#
# The deb is built by ../scaleplex-ffmpeg/build.sh (or the
# build-ffmpeg GHA workflow) and placed in ../scaleplex-ffmpeg/dist/
# before docker build. Build context root must be the scaleplex repo
# root (matches existing `worker/...` paths).

FROM golang:1.23-bookworm AS agent-build
WORKDIR /src
COPY worker/agent/go.mod worker/agent/go.sum ./
RUN go mod download
COPY worker/agent/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/scaleplex-agent ./...

FROM ubuntu:24.04
LABEL org.opencontainers.image.source=https://github.com/Varashi/scaleplex
LABEL org.opencontainers.image.description="scaleplex worker — scaleplex-ffmpeg + Intel iHD VAAPI + Go agent"
LABEL org.opencontainers.image.licenses=MIT

# multiverse for intel-media-va-driver-non-free; scaleplex-ffmpeg
# expects libva to come from the system (it doesn't bundle the VAAPI
# userspace, same as jellyfin-ffmpeg).
COPY scaleplex-ffmpeg/dist/scaleplex-ffmpeg7_*.deb /tmp/
RUN sed -i 's|^Components: main universe$|Components: main universe multiverse|' \
      /etc/apt/sources.list.d/ubuntu.sources && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      /tmp/scaleplex-ffmpeg7_*.deb \
      intel-media-va-driver-non-free \
      intel-opencl-icd \
      libva2 libva-drm2 \
      vainfo \
      fontconfig fonts-dejavu-core \
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
# libass + fontconfig: point at the slim config so fontselect scans 14
# DejaVu files instead of the system corpus. The rewriter sets HOME=
# /home/ubuntu in the spawn env so libass finds the per-user cache the
# next stage builds.
ENV FONTCONFIG_FILE=/opt/scaleplex/fonts.conf
EXPOSE 3501

# Pre-populate the system fontconfig cache against the slim config.
# Runs as root so /var/cache/fontconfig is writeable; uid 1000 has
# read access at runtime, so the spawned ffmpeg/libass picks up the
# cache without any HOME tricks. Avoids the per-user .cache/fontconfig
# layer that broke under BuildKit cache refreshes (fc-cache fell back
# to writing per-dir caches into /usr/share/fonts/truetype/dejavu and
# failed there).
#
# Without a populated cache libass's `subtitles=` filter triggers a
# fontselect on the first subtitle frame; on the original system-wide
# config that scan blocked for 2+ minutes on the worker pod, ffmpeg
# never produced a segment, and Plex's HLS client timed out with
# "Connection error" (live repro 2026-05-08: Plex Android, force-burn
# subs + seek, 2 min wall clock no segments).
RUN FONTCONFIG_FILE=/opt/scaleplex/fonts.conf fc-cache -f -v 2>&1 | tail -3
ENTRYPOINT ["/usr/bin/tini","--","/usr/local/bin/scaleplex-agent"]
