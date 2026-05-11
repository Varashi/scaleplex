#!/bin/bash
# Build the relay locally and hot-swap it on the running PMS pod —
# no Docker, no GH Actions, no DOCKER_MOD reinstall.
#
# The relay is an s6-rc longrun on the PMS pod; pkill triggers s6 to
# restart it with the new binary. Round-trip ~10s.
#
# Workflow:
#   1. compile shim/cmd/relay → /tmp/scaleplex-relay
#   2. kubectl cp into /usr/local/bin/scaleplex-relay on the PMS pod
#      (atomic via .new + mv)
#   3. pkill scaleplex-relay; s6-supervise restarts it
#
# To restore the image binary: just bump the helmrelease DOCKER_MODS
# tag — k8s rolls a new PMS pod with the original binary baked in.

set -euo pipefail

NS=${NS:-clusterplex}
SELECTOR=${SELECTOR:-app.kubernetes.io/controller=pms}
BIN_PATH=${BIN_PATH:-/usr/local/bin/scaleplex-relay}

cd "$(dirname "$0")/cmd/relay"
echo "→ go build"
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
  -o /tmp/scaleplex-relay .
SIZE=$(stat -c %s /tmp/scaleplex-relay)
echo "  built $(printf '%.1f' "$(echo "scale=1; $SIZE/1024/1024" | bc)") MiB"

POD=$(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | head -1 | sed s,pod/,,)
echo "→ kubectl cp via $POD"
kubectl -n "$NS" cp /tmp/scaleplex-relay "$POD:${BIN_PATH}.new"
kubectl -n "$NS" exec "$POD" -- bash -c \
  "chmod +x '${BIN_PATH}.new' && mv '${BIN_PATH}.new' '${BIN_PATH}'"

echo "→ restarting relay (s6 supervises, auto-restarts)"
kubectl -n "$NS" exec "$POD" -- pkill -f /usr/local/bin/scaleplex-relay || true

echo "✓ done — relay back up in ~1s"
