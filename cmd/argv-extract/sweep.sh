#!/bin/bash
# Sweep all argv-corpus sources into ~/scaleplex-corpus/.
#
# Pulls:
#   1. Worker NFS captures from clusterplex-worker pods
#      (/transcode/_argv-corpus/<sid>.json — written by
#      persistArgvCapture).
#   2. Plex production wrapper captures from the plex pod
#      (/transcode/_argv-corpus/<session>-<job>.argv — written by the
#      bash tee-wrapper installed via custom-cont-init.d).
#   3. Worker stderr lines from kubectl logs (Go %q-printed argv
#      slices + rewriter changes + outcome).
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

# 1. clusterplex worker NFS — written by persistArgvCapture.
CP_POD="$(kubectl -n clusterplex get pod -l app.kubernetes.io/controller=worker \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
if [ -n "$CP_POD" ]; then
  echo "→ sweeping clusterplex worker NFS via $CP_POD"
  mkdir -p "$TMP/clusterplex"
  # kubectl cp is finicky with - and / in paths; use exec+tar instead.
  kubectl -n clusterplex exec "$CP_POD" -- \
    bash -c 'cd /transcode && tar cf - _argv-corpus 2>/dev/null' \
    | tar xf - -C "$TMP/clusterplex" 2>/dev/null || true
fi

# 2. Production plex NFS — written by bash wrapper. Files owned by
# uid 1000 (abc) inside the pod; root-in-pod can't read them due to
# NFS root_squash. Stream as abc via runuser.
PLEX_POD="$(kubectl -n plex get pod -l app.kubernetes.io/name=plex \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
if [ -n "$PLEX_POD" ]; then
  echo "→ sweeping plex prod NFS via $PLEX_POD"
  mkdir -p "$TMP/plex"
  kubectl -n plex exec "$PLEX_POD" -c app -- \
    runuser -u abc -- bash -c 'cd /transcode && tar cf - _argv-corpus 2>/dev/null' \
    | tar xf - -C "$TMP/plex" 2>/dev/null || true
fi

# 3. Worker logs — kubectl tail on stdin to extractor. Always include;
# argv lines are valuable when WORKER_DUMP_ARGV=1 is set on the DS.
echo "→ extracting from worker logs + sweep dirs"
{
  kubectl -n clusterplex logs -l app.kubernetes.io/controller=worker \
    --tail=99999 --since="${SINCE:-24h}" --prefix=true 2>/dev/null || true
} | "$EXTRACT_BIN" \
  -corpus "$CORPUS_DIR" \
  ${CP_POD:+-sweep "$TMP/clusterplex/_argv-corpus"} \
  ${PLEX_POD:+-sweep "$TMP/plex/_argv-corpus"}

echo "→ corpus at $CORPUS_DIR ($(ls "$CORPUS_DIR" | wc -l) entries)"
