# scaleplex worker image
#
# Ubuntu 24.04 + jellyfin-ffmpeg7 + Intel iHD VAAPI driver + fonts.
# We use jellyfin-ffmpeg (instead of stock Ubuntu ffmpeg) for:
#   - subtitles=...:sub2video=1 → renders only sub pixels with alpha,
#     no 4K canvas allocation per frame (drops the bandwidth wall we hit
#     in the 2026-05-05 sub-burn bench)
#   - alpha_format option on overlay_vaapi (no separate format=bgra)
#   - PGS HW overlay support (not used today but free)
#   - vaapi scaler default to "fast"
#   - tonemapx SIMD CPU tonemap as a soft-fallback
#
# Pinned to the version channel (no per-version tag in the apt repo —
# they ship rolling updates inside `noble main`); CI captures the
# resolved version in the image SHA.

FROM golang:1.23-bookworm AS agent-build
WORKDIR /src
COPY worker/agent/go.mod worker/agent/go.sum ./
RUN go mod download
COPY worker/agent/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/scaleplex-agent ./...

FROM ubuntu:24.04
LABEL org.opencontainers.image.source=https://github.com/Varashi/scaleplex
LABEL org.opencontainers.image.description="scaleplex worker — jellyfin-ffmpeg + Intel iHD VAAPI + Go agent"
LABEL org.opencontainers.image.licenses=MIT

# multiverse for intel-media-va-driver-non-free; jellyfin-ffmpeg expects
# libva to come from the system (it doesn't bundle the VAAPI userspace).
RUN sed -i 's|^Components: main universe$|Components: main universe multiverse|' \
      /etc/apt/sources.list.d/ubuntu.sources && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      curl gpg ca-certificates && \
    install -d -m 0755 /etc/apt/keyrings && \
    curl -fsSL https://repo.jellyfin.org/jellyfin_team.gpg.key \
      | gpg --dearmor -o /etc/apt/keyrings/jellyfin.gpg && \
    echo "deb [signed-by=/etc/apt/keyrings/jellyfin.gpg] https://repo.jellyfin.org/ubuntu noble main" \
      > /etc/apt/sources.list.d/jellyfin.list && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      jellyfin-ffmpeg7 \
      intel-media-va-driver-non-free \
      libva2 libva-drm2 \
      vainfo \
      fontconfig fonts-dejavu-core \
      tini && \
    apt-get purge -y --auto-remove gpg && \
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

# /usr/lib/jellyfin-ffmpeg/{ffmpeg,ffprobe} is the install path; symlink
# into /usr/bin so the agent's hardcoded paths keep working.
RUN ln -sf /usr/lib/jellyfin-ffmpeg/ffmpeg  /usr/bin/ffmpeg && \
    ln -sf /usr/lib/jellyfin-ffmpeg/ffprobe /usr/bin/ffprobe

# render group (568) so container UID 1000 can open /dev/dri/renderD*
RUN groupadd -g 568 render2 || true && \
    useradd -u 1000 -g users -G video,render2 -m -s /bin/bash scaleplex || true

COPY --from=agent-build /out/scaleplex-agent /usr/local/bin/scaleplex-agent
COPY worker/scaleplex-entrypoint.sh /usr/local/bin/scaleplex-entrypoint
RUN chmod +x /usr/local/bin/scaleplex-entrypoint

ENV LIBVA_DRIVER_NAME=iHD
# libass + fontconfig: point at the slim config so fontselect scans 14
# DejaVu files instead of the system corpus. The rewriter sets HOME=
# /home/ubuntu in the spawn env so libass finds the per-user cache the
# next stage builds.
ENV FONTCONFIG_FILE=/opt/scaleplex/fonts.conf
EXPOSE 3501

# Pre-populate fontconfig cache for the runtime user (uid 1000 = ubuntu
# in the Ubuntu 24.04 base image). Plex's transcoder env carries
# HOME=/config/Library/Application Support/Plex Media Server, which
# the worker forwards to the spawned ffmpeg. Fontconfig reads HOME to
# locate the per-user cache dir; with HOME pointing at a path that
# doesn't exist on the worker pod and /var/cache/fontconfig owned by
# root (read-only for uid 1000), fontconfig errors with "No writable
# cache directories" and falls back to a non-cached scan-on-every-call
# mode. libass's `subtitles=` filter triggers fontselect at the first
# subtitle frame; without a cache that call blocks for 2+ minutes on
# the worker pod, ffmpeg never produces a segment, and Plex's HLS
# client times out with "Connection error" (live repro 2026-05-08:
# Plex Android, force-burn subs + seek, 2 min wall clock no segments).
#
# Building the cache here as uid 1000 with HOME=/home/ubuntu lands the
# *.cache-N files in /home/ubuntu/.cache/fontconfig. The rewriter sets
# HOME=/home/ubuntu in the spawn env so libass finds the cache.
USER 1000
# Cache built against /opt/scaleplex/fonts.conf (slim) — produces a
# tiny cache that mmaps in microseconds vs the system-corpus cache.
RUN HOME=/home/ubuntu FONTCONFIG_FILE=/opt/scaleplex/fonts.conf fc-cache -f
# Wrapper: prefers /transcode/_dev/scaleplex-agent if present (NFS-shared
# dev overlay so a `go build && cp && pkill` round-trip swaps the binary
# in seconds without rebuilding the image). Falls through to the baked
# binary in production.
ENTRYPOINT ["/usr/bin/tini","--","/usr/local/bin/scaleplex-entrypoint"]
