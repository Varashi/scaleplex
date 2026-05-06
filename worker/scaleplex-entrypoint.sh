#!/bin/sh
# Worker entrypoint wrapper.
#
# In production the agent baked into the image runs.
# In dev mode, drop a fresh binary at /transcode/_dev/scaleplex-agent
# (an existing NFS-shared path; all workers see the same file) and
# pkill the running agent — the kubelet restarts the container, this
# script picks up the overlay, no image rebuild needed.
#
# See worker/iterate.sh for the iteration helper.
set -e

DEV_BIN=/transcode/_dev/scaleplex-agent
PROD_BIN=/usr/local/bin/scaleplex-agent

if [ -x "$DEV_BIN" ]; then
    echo "[scaleplex-entrypoint] using dev overlay $DEV_BIN" >&2
    exec "$DEV_BIN" "$@"
fi
exec "$PROD_BIN" "$@"
