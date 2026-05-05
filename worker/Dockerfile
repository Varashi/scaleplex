# scaleplex worker image — phase 1
#
# Ubuntu 24.04 + stock ffmpeg (apt) + Intel iHD VAAPI driver + libass fonts +
# the Go agent binary. Stock ffmpeg in 24.04 is 6.1.1 with vaapi+libass+libdrm
# enabled, which is enough for tonemap_vaapi / overlay_vaapi / scale_vaapi.

FROM golang:1.23-bookworm AS agent-build
WORKDIR /src
COPY worker/agent/go.mod ./
RUN go mod download
COPY worker/agent/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/scaleplex-agent ./...

FROM ubuntu:24.04

# multiverse needed for intel-media-va-driver-non-free (deb822 sources in 24.04)
RUN sed -i 's|^Components: main universe$|Components: main universe multiverse|' \
      /etc/apt/sources.list.d/ubuntu.sources && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ffmpeg \
      intel-media-va-driver-non-free \
      libva2 libva-drm2 \
      vainfo \
      fontconfig fonts-dejavu-core fonts-noto-core \
      ca-certificates \
      tini && \
    rm -rf /var/lib/apt/lists/* && \
    fc-cache -f

# render group (568) so the container UID 1000 can open /dev/dri/renderD*
RUN groupadd -g 568 render2 || true && \
    useradd -u 1000 -g users -G video,render2 -m -s /bin/bash scaleplex || true

COPY --from=agent-build /out/scaleplex-agent /usr/local/bin/scaleplex-agent

ENV LIBVA_DRIVER_NAME=iHD
EXPOSE 3501
USER 1000
ENTRYPOINT ["/usr/bin/tini","--","/usr/local/bin/scaleplex-agent"]
