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
      fontconfig fonts-dejavu-core fonts-noto-core \
      tini && \
    apt-get purge -y --auto-remove gpg && \
    rm -rf /var/lib/apt/lists/* && \
    fc-cache -f

# /usr/lib/jellyfin-ffmpeg/{ffmpeg,ffprobe} is the install path; symlink
# into /usr/bin so the agent's hardcoded paths keep working.
RUN ln -sf /usr/lib/jellyfin-ffmpeg/ffmpeg  /usr/bin/ffmpeg && \
    ln -sf /usr/lib/jellyfin-ffmpeg/ffprobe /usr/bin/ffprobe

# render group (568) so container UID 1000 can open /dev/dri/renderD*
RUN groupadd -g 568 render2 || true && \
    useradd -u 1000 -g users -G video,render2 -m -s /bin/bash scaleplex || true

COPY --from=agent-build /out/scaleplex-agent /usr/local/bin/scaleplex-agent

ENV LIBVA_DRIVER_NAME=iHD
EXPOSE 3501
USER 1000
ENTRYPOINT ["/usr/bin/tini","--","/usr/local/bin/scaleplex-agent"]
