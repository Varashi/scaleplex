#!/usr/bin/env bash
# Build the scaleplex-ffmpeg DEPS base image.
#
# The deps image bakes:
#  - a jellyfin-ffmpeg checkout at the pinned tag, re-namespaced to
#    scaleplex-ffmpeg (so install paths under /usr/lib/scaleplex-ffmpeg/
#    are correct)
#  - the dpkg-buildpackage build deps (apt) — installed by Dockerfile.in
#  - every bundled lib (iconv, libxml2, freetype, libass, x264, x265,
#    dav1d, SVT-AV1, libdrm, libva, libplacebo, intel media-driver,
#    oneVPL, NVENC headers, etc.) compiled into ${TARGET_DIR} =
#    /usr/lib/scaleplex-ffmpeg/
#
# What it does NOT bake: our /patches/*.patch series. The whole point of
# the split is to keep deps stable across patch iterations.
#
# Image:  ghcr.io/varashi/scaleplex-ffmpeg-deps:<jellyfin-tag>
# Trigger: jellyfin-ffmpeg VERSION bump (rare, ~3-5 GB image, ~30-40 min
#          to rebuild). The patches+deb build.sh consumes this as a FROM.
#
# Local usage:
#   ./build-deps.sh                     # uses VERSION
#   ./build-deps.sh v7.1.3-1            # explicit tag
#   PUSH=1 ./build-deps.sh              # push to GHCR after build
#   DEPS_IMAGE=foo/bar ./build-deps.sh  # override registry/repo

set -euo pipefail
cd "$(dirname "$0")"

# shellcheck source=./lib/rename.sh
source ./lib/rename.sh
# shellcheck source=./lib/split-docker-build.sh
source ./lib/split-docker-build.sh

TAG="${1:-$(tr -d '[:space:]' < VERSION)}"
DEPS_IMAGE="${DEPS_IMAGE:-ghcr.io/varashi/scaleplex-ffmpeg-deps}"
IMG="${DEPS_IMAGE}:${TAG}"

# Workdir: same selection logic as build.sh.
if [[ -n "${WORKDIR_BASE:-}" ]]; then
  :
elif [[ -d /build && -w /build ]]; then
  WORKDIR_BASE=/build/scaleplex-ffmpeg-deps-build
elif [[ -n "${RUNNER_TEMP:-}" ]]; then
  WORKDIR_BASE="${RUNNER_TEMP}/scaleplex-ffmpeg-deps-build"
else
  WORKDIR_BASE="$(pwd)"
fi
WORKDIR="${WORKDIR_BASE}/.build-deps"
mkdir -p "$WORKDIR_BASE"
rm -rf "$WORKDIR"

echo "==> Cloning jellyfin-ffmpeg @ $TAG → $WORKDIR"
git clone --depth 1 --branch "$TAG" \
  https://github.com/jellyfin/jellyfin-ffmpeg.git "$WORKDIR"

(
  cd "$WORKDIR"
  echo "==> Re-namespacing jellyfin-ffmpeg → scaleplex-ffmpeg"
  scaleplex_ffmpeg_rename_in_place

  echo "==> Injecting --deps-only / --build-only mode into docker-build.sh"
  scaleplex_ffmpeg_split_docker_build

  echo "==> Templating Dockerfile from Dockerfile.in"
  make -f Dockerfile.make Dockerfile
  # podman: bare distro names don't resolve without a registry prefix.
  sed -i 's|^FROM noble$|FROM docker.io/library/ubuntu:noble|' Dockerfile

  echo "==> Appending deps-only build step into Dockerfile"
  # Inject a RUN step that compiles every bundled dep into ${TARGET_DIR}
  # via the patched docker-build.sh. Goes right before the ENTRYPOINT so
  # the layered apt+pip prep stays cacheable.
  #
  # We also drop apt lists at the end to keep the image as small as
  # reasonable — deps image still lands ~3-5 GB but every MB shaved off
  # cuts CI pull time.
  python3 - <<'PYEOF'
with open('Dockerfile') as fh:
    df = fh.read()

inject = (
    '\n# scaleplex: pre-compile every bundled dep into /usr/lib/scaleplex-ffmpeg/.\n'
    '# This RUN is the entire point of the deps image — caching it across\n'
    '# patch iterations is what drops worker rebuilds from ~60 min to ~5-10.\n'
    '# We deliberately keep the per-dep source + DESTDIR staging dirs under\n'
    '# ${SOURCE_DIR}/<name>/ because debian/scaleplex-ffmpeg7.install lists\n'
    '# them by relative path; dpkg-buildpackage looks them up later in\n'
    '# --build-only mode. Dropping them would re-break the deb pack.\n'
    'RUN SCALEPLEX_PHASE=deps /docker-build.sh \\\n'
    ' && rm -rf /var/lib/apt/lists/*\n\n'
)

if 'ENTRYPOINT' not in df:
    raise SystemExit('Dockerfile: ENTRYPOINT marker not found')
df = df.replace('ENTRYPOINT', inject + 'ENTRYPOINT', 1)

with open('Dockerfile', 'w') as fh:
    fh.write(df)
PYEOF

  echo "==> Building deps image: $IMG"
  if [[ "${BUILDX:-}" == "1" ]] && docker buildx version >/dev/null 2>&1; then
    PUSH_ARG="--load"
    if [[ "${PUSH:-}" == "1" ]]; then
      PUSH_ARG="--push"
    fi
    docker buildx build \
      $PUSH_ARG \
      --cache-from="${BUILDX_CACHE_FROM:-type=gha,scope=ffmpeg-deps}" \
      --cache-to="${BUILDX_CACHE_TO:-type=gha,mode=max,scope=ffmpeg-deps}" \
      -t "$IMG" .
  else
    docker build -t "$IMG" .
    if [[ "${PUSH:-}" == "1" ]]; then
      docker push "$IMG"
    fi
  fi
)

echo
echo "Built deps image: $IMG"
echo "Patches+deb build.sh will consume this via FROM."
