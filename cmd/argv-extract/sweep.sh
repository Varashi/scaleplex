#!/bin/bash
# Sweep all argv-corpus sources into ~/scaleplex-corpus/.
#
# Pulls:
#   1. Worker NFS captures from the prod `plex` PMS pod
#      (/transcode/_argv-corpus/<sid>.json — written by the worker's
#      persistArgvCapture when WORKER_DUMP_ARGV=1 is set on the DS).
#      The PMS pod and worker DS share the /transcode PVC, so reading
#      via the PMS pod sees worker-written files.
#   2. Worker NFS captures from the `plex-test` PMS pod (same model).
#   3. Worker stderr lines from kubectl logs (Go %q-printed argv
#      slices + rewriter changes + outcome). Captures both `plex` and
#      `plex-test` worker DSes.
#
# Auto-detects each file's format (JSON vs NUL-args). Idempotent —
# only adds new captures to the corpus.
#
# Run anytime to refresh the corpus from current cluster state. Logs
# rotate after a few hours, NFS captures persist longer; sweep
# regularly if you care about the log-only outcome data
# (segments_created, exit_reason, encode_speed).

set -euo pipefail

CORPUS_DIR="${CORPUS_DIR:-$HOME/scaleplex-corpus}"
TMP="$(mktemp -d -t argv-sweep.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$CORPUS_DIR"

EXTRACT_BIN="$(dirname "$(readlink -f "$0")")/argv-extract"
if [ ! -x "$EXTRACT_BIN" ]; then
  EXTRACT_BIN="$(go env GOPATH)/bin/argv-extract"
fi
if [ ! -x "$EXTRACT_BIN" ]; then
  # Fall back to building on-the-fly.
  EXTRACT_BIN="$TMP/argv-extract"
  go build -o "$EXTRACT_BIN" "$(dirname "$(readlink -f "$0")")"
fi

SWEEP_ARGS=()

# Sweep a /transcode/_argv-corpus dir via a PMS pod in $ns.
# Files are owned by uid 1000 (abc) inside the LSIO PMS container;
# root-in-pod can't read them under NFS root_squash, so stream as
# abc via runuser.
sweep_ns() {
  local ns="$1"
  local pms_label="$2"
  local pod
  pod="$(kubectl -n "$ns" get pod -l "$pms_label" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [ -z "$pod" ] && { echo "→ $ns: no PMS pod, skipping"; return; }
  echo "→ sweeping $ns NFS via $pod"
  mkdir -p "$TMP/$ns"
  kubectl -n "$ns" exec "$pod" -c app -- \
    runuser -u abc -- bash -c 'cd /transcode && tar cf - _argv-corpus 2>/dev/null' \
    | tar xf - -C "$TMP/$ns" 2>/dev/null || true
  [ -d "$TMP/$ns/_argv-corpus" ] && SWEEP_ARGS+=(-sweep "$TMP/$ns/_argv-corpus")
}

# PMS label keys differ per ns (controller=plex on prod, controller=pms
# on plex-test); using app.kubernetes.io/name= would also match the
# orchestrator pod.

# 1. Prod plex NFS.
sweep_ns plex      app.kubernetes.io/controller=plex

# 2. Plex-test NFS.
sweep_ns plex-test app.kubernetes.io/controller=pms

# 3. Worker logs — argv lines + outcome metadata (rewriter changes,
# segments_created, exit_reason, encode_speed). Tail both ns worker DSes.
echo "→ extracting from worker logs + sweep dirs"
{
  for ns in plex plex-test; do
    kubectl -n "$ns" logs -l app.kubernetes.io/controller=worker \
      --tail=99999 --since="${SINCE:-24h}" --prefix=true 2>/dev/null || true
  done
} | "$EXTRACT_BIN" -corpus "$CORPUS_DIR" "${SWEEP_ARGS[@]}"

echo "→ corpus at $CORPUS_DIR ($(ls "$CORPUS_DIR" | wc -l) entries)"
