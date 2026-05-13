#!/bin/sh
# Worker entrypoint wrapper.
#
# In production the agent baked into the image runs.
# In dev mode, drop a fresh binary at /transcode/_dev/scaleplex-agent
# (an existing NFS-shared path; all workers see the same file) and
# pkill the running agent — the kubelet restarts the container, this
# script picks up the overlay, no image rebuild needed.
#
# See worker/iterate.sh + worker/iterate-ffmpeg.sh for iteration helpers.
set -e

DEV_BIN=/transcode/_dev/scaleplex-agent
PROD_BIN=/usr/local/bin/scaleplex-agent
DEV_LIB=/transcode/_dev/scaleplex-ffmpeg-lib

# ffmpeg .so overlay (worker/iterate-ffmpeg.sh) — prepend if .so files
# are present. ffmpeg children inherit LD_LIBRARY_PATH from
# scaleplex-agent.
if [ -d "$DEV_LIB" ] && ls "$DEV_LIB"/*.so.* >/dev/null 2>&1; then
    echo "[scaleplex-entrypoint] using dev ffmpeg overlay $DEV_LIB" >&2
    export LD_LIBRARY_PATH="$DEV_LIB:${LD_LIBRARY_PATH:-}"
fi

if [ -x "$DEV_BIN" ]; then
    echo "[scaleplex-entrypoint] using dev overlay $DEV_BIN" >&2
    exec "$DEV_BIN" "$@"
fi
exec "$PROD_BIN" "$@"
