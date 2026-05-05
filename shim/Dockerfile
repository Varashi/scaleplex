# scaleplex-shim DOCKER_MOD image.
# lsio's `with-contenv` mod loader does `cp -a /<image-root>/* /` in the
# running Plex container at startup. So everything we want installed
# needs to live at the right path inside this image.
#
# Layout we deposit:
#   /usr/lib/plexmediaserver/Plex Transcoder      ← our shim (replaces ffmpeg)
#   /usr/lib/plexmediaserver/Plex Transcoder.real ← the original, kept for fallback
#   /etc/cont-init.d/99-scaleplex-shim            ← s6 init that swaps the binary
# (lsio's mod system runs cont-init.d before svc-plex starts, so the
# swap is in place before the first transcode.)

FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY shim/go.mod ./
RUN go mod download
COPY shim/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/scaleplex-shim ./...

FROM alpine:3.20 AS layout
RUN mkdir -p /mod/usr/lib/plexmediaserver /mod/etc/cont-init.d
COPY --from=build /out/scaleplex-shim /mod/usr/lib/plexmediaserver/scaleplex-shim
RUN cat > /mod/etc/cont-init.d/99-scaleplex-shim <<'EOF'
#!/usr/bin/with-contenv bash
# Atomically swap Plex Transcoder for the scaleplex shim.
# - First run: rename original to .real, install shim
# - Subsequent runs: shim already in place, no-op
PMS=/usr/lib/plexmediaserver
SHIM_BIN="$PMS/scaleplex-shim"
TARGET="$PMS/Plex Transcoder"
BACKUP="$PMS/Plex Transcoder.real"

if [ ! -x "$SHIM_BIN" ]; then
  echo "[scaleplex] shim binary missing at $SHIM_BIN — leaving Plex Transcoder alone" >&2
  exit 0
fi

if [ -e "$TARGET" ] && [ ! -e "$BACKUP" ] && ! head -c4 "$TARGET" 2>/dev/null | grep -q '\x7fELF.*scaleplex'; then
  # Heuristic: if first 4 bytes are ELF magic and the file isn't already our shim,
  # back it up. (Our shim is also ELF; we tag the swap with .scaleplex marker file.)
  if [ ! -e "$PMS/.scaleplex-shim-installed" ]; then
    mv "$TARGET" "$BACKUP"
    echo "[scaleplex] backed up original Plex Transcoder → Plex Transcoder.real" >&2
  fi
fi

# Always (re)point Plex Transcoder at the shim — re-applies after a Plex update
# overwrites the binary.
ln -sf "$SHIM_BIN" "$TARGET"
touch "$PMS/.scaleplex-shim-installed"
echo "[scaleplex] Plex Transcoder → $SHIM_BIN" >&2
EOF
RUN chmod +x /mod/etc/cont-init.d/99-scaleplex-shim

# lsio mods convention: empty `FROM scratch` final stage holding only
# the layout files; lsio's mod loader copies everything from /.
FROM scratch
COPY --from=layout /mod/ /
