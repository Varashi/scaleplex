# scaleplex-shim DOCKER_MOD image.
#
# Lays down three things on the lsio Plex container at start:
#
#   1. /usr/lib/plexmediaserver/scaleplex-shim
#      The Plex Transcoder replacement — POSTs to scaleplex orchestrator,
#      streams ffmpeg output back. Wired into Plex Transcoder by a
#      cont-init script.
#
#   2. /usr/local/bin/scaleplex-relay
#      Forward HTTP proxy listening on $LOCAL_RELAY_PORT (default 32499),
#      forwards to http://127.0.0.1:$PMS_PORT. Lets workers post Plex
#      Transcoder progress (and any other 127.0.0.1:32400 callbacks)
#      via the cluster Service IP without PMS rejecting them as
#      non-loopback. Exact same trick clusterplex used with nginx.
#      Runs as an lsio s6-v3 custom-services.d longrun.
#
#   3. /usr/local/bin/scaleplex-clientfix
#      Content-aware decision proxy (s6 longrun on :32401, forwards to
#      127.0.0.1:32400). When the k8s Service/LB targets :32401, it
#      transparently fronts PMS for client traffic: streams everything,
#      and on the matched Plex-for-Apple-TV-8.45 transcode DECISION it
#      rewrites container=mkv->mp4 only for re-encodes (fixing the
#      mkv-in-HLS bug, scaleplex#122) while passing copies through. In-pod
#      so PMS sees loopback -> honours X-Forwarded-For (real client IP).
#      Idle unless the Service targets :32401.
#
# lsio v3 paths (per the docker-mods script source we read 2026-05-05):
#   /custom-cont-init.d/ → user init scripts
#   /custom-services.d/  → user longrun services
# (the /etc/cont-init.d and /etc/services.d v2 paths are wiped by the
# mod loader before extraction.)

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY shim/go.mod ./
RUN go mod download
COPY shim/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/scaleplex-shim  ./cmd/shim && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/scaleplex-relay ./cmd/relay
# clientfix — content-aware decision proxy (cmd 3 below). Own stdlib-only
# module; build it here so it ships in the same dockermod image.
COPY clientfix/ /clientfix/
RUN cd /clientfix && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/scaleplex-clientfix ./cmd/clientfix

FROM alpine:3.23 AS layout
RUN mkdir -p /mod/usr/lib/plexmediaserver \
             /mod/usr/local/bin \
             /mod/custom-cont-init.d \
             /mod/etc/s6-overlay/s6-rc.d/scaleplex-relay/dependencies.d \
             /mod/etc/s6-overlay/s6-rc.d/scaleplex-clientfix/dependencies.d \
             /mod/etc/s6-overlay/s6-rc.d/user/contents.d
COPY --from=build /out/scaleplex-shim      /mod/usr/lib/plexmediaserver/scaleplex-shim
COPY --from=build /out/scaleplex-relay     /mod/usr/local/bin/scaleplex-relay
COPY --from=build /out/scaleplex-clientfix /mod/usr/local/bin/scaleplex-clientfix

# cont-init: idempotent binary swap of Plex Transcoder for our shim.
# Runs every container start; safe to re-run after Plex updates.
RUN cat > /mod/custom-cont-init.d/99-scaleplex-shim <<'EOF'
#!/usr/bin/with-contenv bash
PMS=/usr/lib/plexmediaserver
SHIM_BIN="$PMS/scaleplex-shim"
TARGET="$PMS/Plex Transcoder"
BACKUP="$PMS/Plex Transcoder.real"

if [ ! -x "$SHIM_BIN" ]; then
  echo "[scaleplex] shim missing at $SHIM_BIN — leaving Plex Transcoder alone" >&2
  exit 0
fi
# Back up the original on first install OR after a Plex update
# replaces the binary (size differs from our shim).
if [ -e "$TARGET" ] && [ "$(stat -c %s "$TARGET")" != "$(stat -c %s "$SHIM_BIN")" ] && [ ! -e "$BACKUP" ]; then
  cp "$TARGET" "$BACKUP"
  echo "[scaleplex] backed up original Plex Transcoder → Plex Transcoder.real" >&2
fi
cp "$SHIM_BIN" "$TARGET"
echo "[scaleplex] Plex Transcoder → scaleplex-shim ($(stat -c %s "$TARGET") bytes)" >&2
EOF

# s6-overlay v3 longrun for the relay. Direct install under
# /etc/s6-overlay/s6-rc.d/ because lsio's process_custom_services()
# (which would convert /custom-services.d/* into longruns) runs
# BEFORE mods are extracted, so anything we drop in /custom-services.d
# arrives too late.
RUN cat > /mod/etc/s6-overlay/s6-rc.d/scaleplex-relay/run <<'EOF'
#!/command/with-contenv bash
# Drop to abc (uid 1000, gid 1000) — the lsio Plex container's main
# user. The relay reads/writes matroska chunk files on the NFS
# /transcode volume (for in-line Cluster.Timecode patching before
# forwarding the segment_list CSV to PMS); chunks are written by
# ffmpeg-as-abc on the worker, and NFS root_squash strips uid 0 so
# root can't read them. Without dropping, "permission denied" on
# every chunk → no patch → mpv anchors timeline to chunk-0's
# unpatched Timecode=0 → Plex Windows playhead never moves.
exec s6-setuidgid abc /usr/local/bin/scaleplex-relay
EOF
RUN echo longrun > /mod/etc/s6-overlay/s6-rc.d/scaleplex-relay/type && \
    touch    /mod/etc/s6-overlay/s6-rc.d/scaleplex-relay/dependencies.d/init-services && \
    touch    /mod/etc/s6-overlay/s6-rc.d/user/contents.d/scaleplex-relay

# s6-overlay v3 longrun for clientfix — the content-aware decision proxy.
# Listens on :32401 (default) and forwards to PMS on 127.0.0.1:32400. The
# k8s Service / LB targets :32401 so clientfix is transparently in front of
# PMS for client traffic; PMS sees the loopback source so it honours
# X-Forwarded-For → real client IP. Idle (no traffic) unless the Service
# targets it. Env-overridable (CLIENTFIX_LISTEN_ADDR / CLIENTFIX_PMS_UPSTREAM).
RUN cat > /mod/etc/s6-overlay/s6-rc.d/scaleplex-clientfix/run <<'EOF'
#!/command/with-contenv bash
exec s6-setuidgid abc env \
  CLIENTFIX_LISTEN_ADDR="${CLIENTFIX_LISTEN_ADDR:-:32401}" \
  CLIENTFIX_PMS_UPSTREAM="${CLIENTFIX_PMS_UPSTREAM:-http://127.0.0.1:32400}" \
  /usr/local/bin/scaleplex-clientfix
EOF
RUN echo longrun > /mod/etc/s6-overlay/s6-rc.d/scaleplex-clientfix/type && \
    touch    /mod/etc/s6-overlay/s6-rc.d/scaleplex-clientfix/dependencies.d/init-services && \
    touch    /mod/etc/s6-overlay/s6-rc.d/user/contents.d/scaleplex-clientfix

RUN chmod +x /mod/custom-cont-init.d/99-scaleplex-shim \
             /mod/etc/s6-overlay/s6-rc.d/scaleplex-relay/run \
             /mod/etc/s6-overlay/s6-rc.d/scaleplex-clientfix/run

FROM scratch
LABEL org.opencontainers.image.source=https://github.com/Varashi/scaleplex
LABEL org.opencontainers.image.description="scaleplex PMS DOCKER_MOD — shim (Plex Transcoder replacement) + local-relay forward proxy + clientfix decision proxy"
LABEL org.opencontainers.image.licenses=MIT
COPY --from=layout /mod/ /
