#!/bin/bash
# Build the worker agent locally and push it to the dev overlay path
# on the running cluster — no Docker, no image build, no GH Actions.
#
# Workflow:
#   1. compile worker/agent → /tmp/scaleplex-agent
#   2. kubectl cp into /transcode/_dev/scaleplex-agent on one worker
#      (the path is NFS-shared so all workers see the same file)
#   3. pkill scaleplex-agent on every worker pod; kubelet restarts the
#      container, the entrypoint wrapper picks up the dev binary
#
# To revert to the image's baked binary: `worker/iterate.sh --revert`.
#
# Round-trip target: <15s (vs 5-10 min via the GH Actions image build).

set -euo pipefail

NS=${NS:-clusterplex}
SELECTOR=${SELECTOR:-app.kubernetes.io/controller=worker}
DEV_BIN_PATH=${DEV_BIN_PATH:-/transcode/_dev/scaleplex-agent}

case "${1:-}" in
  --revert)
    echo "→ removing dev overlay"
    POD=$(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | head -1 | sed s,pod/,,)
    kubectl -n "$NS" exec "$POD" -- rm -f "$DEV_BIN_PATH"
    echo "→ restarting workers"
    for p in $(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | sed s,pod/,,); do
      kubectl -n "$NS" exec "$p" -- pkill scaleplex-agent || true &
    done
    wait
    echo "✓ workers reverted to image binary"
    exit 0
    ;;
esac

cd "$(dirname "$0")/agent"
echo "→ go build"
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
  -o /tmp/scaleplex-agent ./...
SIZE=$(stat -c %s /tmp/scaleplex-agent)
echo "  built $(printf '%.1f' "$(echo "scale=1; $SIZE/1024/1024" | bc)") MiB"

POD=$(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | head -1 | sed s,pod/,,)
echo "→ kubectl cp via $POD"
kubectl -n "$NS" exec "$POD" -- mkdir -p "$(dirname "$DEV_BIN_PATH")"
# Atomic swap: copy to .new then rename. Avoids the wrapper picking up
# a half-written file mid-restart.
kubectl -n "$NS" cp /tmp/scaleplex-agent "$POD:${DEV_BIN_PATH}.new"
kubectl -n "$NS" exec "$POD" -- bash -c \
  "chmod +x '${DEV_BIN_PATH}.new' && mv '${DEV_BIN_PATH}.new' '${DEV_BIN_PATH}'"

echo "→ restarting workers"
for p in $(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | sed s,pod/,,); do
  kubectl -n "$NS" exec "$p" -- pkill scaleplex-agent || true &
done
wait

echo "✓ done — kubelet restarts each container in ~2-5s, /readyz flips after pre-warm (~5s)"
