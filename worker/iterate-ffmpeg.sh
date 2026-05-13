#!/bin/bash
# Push a locally-built scaleplex-ffmpeg7 .so onto the worker NFS dev
# overlay — same pattern as worker/iterate.sh but for the ffmpeg fork.
#
# Workflow:
#   1. extract /usr/lib/scaleplex-ffmpeg/lib/lib*.so.* from the deb
#   2. kubectl cp into /transcode/_dev/scaleplex-ffmpeg-lib/ on one
#      worker (NFS-shared — all workers see the same files)
#   3. pkill scaleplex-agent on every worker pod; kubelet restarts the
#      container, the entrypoint wrapper prepends the dev dir to
#      LD_LIBRARY_PATH, future ffmpeg spawns load the override .so
#
# To revert to the image's baked libs: `worker/iterate-ffmpeg.sh --revert`.
#
# Round-trip target: ~10 s once the deb already exists (build itself
# takes 5-10 min via scaleplex-ffmpeg/build.sh).

set -euo pipefail

NS=${NS:-clusterplex}
SELECTOR=${SELECTOR:-app.kubernetes.io/controller=worker}
DEV_LIB_DIR=${DEV_LIB_DIR:-/transcode/_dev/scaleplex-ffmpeg-lib}
DEB=${DEB:-$HOME/git/scaleplex/scaleplex-ffmpeg/dist/scaleplex-ffmpeg7_7.1.3-1-noble_amd64.deb}

case "${1:-}" in
  --revert)
    echo "→ removing dev ffmpeg overlay"
    POD=$(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | head -1 | sed s,pod/,,)
    kubectl -n "$NS" exec "$POD" -- bash -c "rm -rf '$DEV_LIB_DIR'"
    echo "→ restarting workers"
    for p in $(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | sed s,pod/,,); do
      kubectl -n "$NS" exec "$p" -- pkill scaleplex-agent || true &
    done
    wait
    echo "✓ workers reverted to image ffmpeg"
    exit 0
    ;;
esac

test -f "$DEB" || { echo "deb not found: $DEB"; exit 1; }

echo "→ extract .so files from $DEB"
STAGE=$(mktemp -d)
trap "rm -rf $STAGE" EXIT
cd "$STAGE"
ar x "$DEB"
tar --zstd -xf data.tar.zst ./usr/lib/scaleplex-ffmpeg/lib
SRC_LIB="$STAGE/usr/lib/scaleplex-ffmpeg/lib"
ls -la "$SRC_LIB" | grep -E '\.so\.[0-9]+$' | head

POD=$(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | head -1 | sed s,pod/,,)
echo "→ kubectl cp .so files via $POD into $DEV_LIB_DIR"
# Stage on `.new` then atomic rename to avoid ffmpeg loading a partial file.
kubectl -n "$NS" exec "$POD" -- bash -c "mkdir -p '$DEV_LIB_DIR.new'"
for f in "$SRC_LIB"/*.so.*[0-9]; do
  base=$(basename "$f")
  case "$base" in
    *.so.*[0-9].*[0-9].*[0-9]) continue ;;  # skip versioned libs, ffmpeg uses SONAME
  esac
  kubectl -n "$NS" cp "$f" "$POD:$DEV_LIB_DIR.new/$base"
done
kubectl -n "$NS" exec "$POD" -- bash -c "
  rm -rf '$DEV_LIB_DIR'
  mv '$DEV_LIB_DIR.new' '$DEV_LIB_DIR'
  ls '$DEV_LIB_DIR'
"

# Update entrypoint script on each worker so DEV_LIB_DIR gets prepended
# to LD_LIBRARY_PATH on container restart. The entrypoint script is
# IMAGE-baked; if the running image predates the LD_LIBRARY_PATH hook,
# kubectl-cp the updated wrapper in place. Subsequent images bake the
# hook so this becomes a no-op.
ENTRY_SRC="$(dirname "$0")/scaleplex-entrypoint.sh"
if [ -f "$ENTRY_SRC" ]; then
  for p in $(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | sed s,pod/,,); do
    kubectl -n "$NS" cp "$ENTRY_SRC" "$p:/usr/local/bin/scaleplex-entrypoint" &
  done
  wait
  for p in $(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | sed s,pod/,,); do
    kubectl -n "$NS" exec "$p" -- chmod +x /usr/local/bin/scaleplex-entrypoint || true &
  done
  wait
fi

echo "→ restarting workers"
for p in $(kubectl -n "$NS" get pod -l "$SELECTOR" -o name | sed s,pod/,,); do
  kubectl -n "$NS" exec "$p" -- pkill scaleplex-agent || true &
done
wait

echo "✓ done — entrypoint wrapper prepends $DEV_LIB_DIR to LD_LIBRARY_PATH"
echo "  next ffmpeg spawn on every worker uses the dev .so"
