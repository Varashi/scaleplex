#!/usr/bin/env bash
# Build scaleplex-ffmpeg = jellyfin-ffmpeg7 + our patches.
#
# Two-image pipeline:
#   1. scaleplex-ffmpeg-deps:<tag>  — slow-changing base. Re-namespaced
#      jellyfin checkout + every bundled dep pre-compiled into
#      /usr/lib/scaleplex-ffmpeg/. Built by ./build-deps.sh, only re-built
#      when VERSION (jellyfin tag) bumps.
#   2. scaleplex-ffmpeg-builder:<tag>  — fast-changing patches-on-top
#      layer. Copies our debian/patches/*.patch over, appends to series,
#      runs dpkg-buildpackage. This is the per-iteration build.
#
# Per-iteration cost drops from ~60 min (compile every dep + ffmpeg) to
# ~5-10 min (just ffmpeg + linking against the prebaked deps).
#
# The deps image is auto-pulled from GHCR. If you haven't yet built the
# deps image for the current VERSION, run build-deps.sh first (or set
# AUTO_BUILD_DEPS=1).
#
# Steps:
#  1. docker pull ghcr.io/varashi/scaleplex-ffmpeg-deps:<tag>
#  2. Copy patches/*.patch into a temp build context.
#  3. Build a tiny patches-on-top image that overlays patches and
#     compiles ffmpeg via docker-build.sh --build-only.
#  4. docker run extracts the deb into ./dist/.
#
# Requires: docker (or podman-docker), git, ~2 GB transient. Build time
# dominated by ffmpeg's own ~5-10 min compile.

set -euo pipefail
cd "$(dirname "$0")"

TAG="${1:-$(tr -d '[:space:]' < VERSION)}"
DEPS_IMAGE="${DEPS_IMAGE:-ghcr.io/varashi/scaleplex-ffmpeg-deps}"
DEPS_REF="${DEPS_IMAGE}:${TAG}"

# Workdir: same selection logic as build-deps.sh.
if [[ -n "${WORKDIR_BASE:-}" ]]; then
  :
elif [[ -d /build && -w /build ]]; then
  WORKDIR_BASE=/build/scaleplex-ffmpeg-build
elif [[ -n "${RUNNER_TEMP:-}" ]]; then
  WORKDIR_BASE="${RUNNER_TEMP}/scaleplex-ffmpeg-build"
else
  WORKDIR_BASE="$(pwd)"
fi
WORKDIR="${WORKDIR_BASE}/.build"
DIST="$(pwd)/dist"
PATCHES="$(pwd)/patches"
mkdir -p "$WORKDIR_BASE"

if [[ ! -d "$PATCHES" ]] || ! ls "$PATCHES"/*.patch >/dev/null 2>&1; then
  echo "no patches in $PATCHES — nothing to do" >&2
  exit 1
fi

mkdir -p "$DIST"
rm -rf "$WORKDIR"
mkdir -p "$WORKDIR"

echo "==> Resolving deps base image: $DEPS_REF"
if ! docker image inspect "$DEPS_REF" >/dev/null 2>&1; then
  if docker pull "$DEPS_REF"; then
    :
  elif [[ "${AUTO_BUILD_DEPS:-0}" == "1" ]]; then
    echo "==> Deps image not in registry — building locally via build-deps.sh"
    ./build-deps.sh "$TAG"
  else
    cat >&2 <<EOF
==> Deps image $DEPS_REF is not available locally or on GHCR.

   Either:
     - Run ./build-deps.sh $TAG to build it locally (~30-40 min)
     - Set AUTO_BUILD_DEPS=1 to do that automatically
     - Push the deps image from the build-ffmpeg-deps GHA workflow
EOF
    exit 1
  fi
fi

echo "==> Staging patches+deb build context"
# Tiny build context — just our patches + a wrapper.
cp -r "$PATCHES" "$WORKDIR/patches"

cat > "$WORKDIR/Dockerfile" <<EOF
# Patches-on-top layer. The deps image already has:
#   - the re-namespaced ffmpeg source at \${SOURCE_DIR} = /ffmpeg
#   - every bundled dep installed under /usr/lib/scaleplex-ffmpeg/
#   - all apt+pip build deps installed
#   - docker-build.sh patched with --deps-only / --build-only flags
#
# We layer in our debian/patches/*.patch, append them to the series,
# then dpkg-buildpackage picks them up at run time. Patches apply via
# quilt during dpkg-buildpackage's source-prep — no need to apply here.
FROM ${DEPS_REF}

# The deps image cleans /var/lib/apt/lists/* in its final RUN to keep
# size down. mk-build-deps needs a populated apt cache to verify the
# Build-Depends from debian/control are present. Refresh lists once
# here — cheap and lets the build-only phase resolve packages.
RUN apt-get update

COPY patches/ /scaleplex-patches/

# Drop patches into debian/patches/ + append filenames to series. Done at
# build time so the patches layer is its own cacheable image step (each
# patch tweak invalidates this one ~kilobyte layer, nothing more).
RUN set -eux; \\
    SERIES=/ffmpeg/debian/patches/series; \\
    test -f "\${SERIES}"; \\
    for p in /scaleplex-patches/*.patch; do \\
      name="\$(basename "\$p")"; \\
      cp "\$p" /ffmpeg/debian/patches/"\$name"; \\
      grep -qxF "\$name" "\${SERIES}" || echo "\$name" >> "\${SERIES}"; \\
    done; \\
    echo "--- new series tail:"; \\
    tail -10 "\${SERIES}"

# Inherited ENTRYPOINT is /docker-build.sh from the deps image. We pass
# --build-only at run time so it skips the prepare_extra_* phase and
# jumps straight to mk-build-deps + dpkg-buildpackage. Override here so
# default \`docker run\` does the right thing without extra argv.
ENTRYPOINT ["/docker-build.sh", "--build-only"]
EOF

(
  cd "$WORKDIR"
  IMG="scaleplex-ffmpeg-builder:$TAG"
  if [[ "${BUILDX:-}" == "1" ]] && docker buildx version >/dev/null 2>&1; then
    docker buildx build \
      --load \
      --cache-from="${BUILDX_CACHE_FROM:-type=gha,scope=ffmpeg-builder}" \
      --cache-to="${BUILDX_CACHE_TO:-type=gha,mode=max,scope=ffmpeg-builder}" \
      -t "$IMG" .
  else
    docker build -t "$IMG" .
  fi
  mkdir -p ./dist
  # `:z` relabels the host bind for podman SELinux; no-op on Docker.
  docker run --rm -v "$PWD/dist:/dist:z" "$IMG"
)

echo "==> Copying artifacts to $DIST/"
cp -v "$WORKDIR"/dist/deb/scaleplex-ffmpeg7_*.deb "$DIST/"

echo
echo "Built deb(s):"
ls -lh "$DIST"/scaleplex-ffmpeg7_*.deb
