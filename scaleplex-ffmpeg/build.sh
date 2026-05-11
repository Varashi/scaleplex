#!/usr/bin/env bash
# Build scaleplex-ffmpeg = jellyfin-ffmpeg7 + our patches.
#
# Steps:
#  1. Clone jellyfin-ffmpeg at the tag pinned in ./VERSION (or arg $1)
#     into a temp checkout dir.
#  2. Copy patches/*.patch into the checkout's debian/patches/ and
#     append filenames to debian/patches/series.
#  3. Run jellyfin's docker-build pipeline (Dockerfile.in + docker-build.sh)
#     which produces a fully-bundled .deb in dist/deb/.
#  4. Copy the resulting deb into ./dist/ at the repo root.
#
# Requires: docker (or podman-docker), git, ~6 GB free disk, ~30-60 min
# build time. The build container is ephemeral; no host pollution.

set -euo pipefail
cd "$(dirname "$0")"

TAG="${1:-$(tr -d '[:space:]' < VERSION)}"
# Workdir lives outside the source tree by default so the heavy
# checkout + build artifacts don't pollute git. Override with
# WORKDIR_BASE to keep it adjacent to the source.
WORKDIR_BASE="${WORKDIR_BASE:-/build/scaleplex-ffmpeg-build}"
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

echo "==> Cloning jellyfin-ffmpeg @ $TAG → $WORKDIR"
git clone --depth 1 --branch "$TAG" \
  https://github.com/jellyfin/jellyfin-ffmpeg.git "$WORKDIR"

echo "==> Renaming jellyfin-ffmpeg → scaleplex-ffmpeg in debian + build scripts"
# We re-namespace the deb so the resulting package, install path, and
# dpkg state are unambiguously ours and never conflict with an
# upstream jellyfin-ffmpeg install. URL references to jellyfin's git
# repo / homepage are preserved (origin tracking).
(
  cd "$WORKDIR"
  # Stash GitHub repo URL behind a temp token so the mass sed below
  # doesn't rewrite our own origin pointer.
  FILES=$(find debian builder Dockerfile.in docker-build.sh -type f 2>/dev/null)
  # shellcheck disable=SC2086
  sed -i 's|github.com/jellyfin/jellyfin-ffmpeg|__SCALEPLEX_JF_URL__|g' $FILES
  # Mass rename
  # shellcheck disable=SC2086
  sed -i 's|jellyfin-ffmpeg|scaleplex-ffmpeg|g' $FILES
  # Restore origin URL
  # shellcheck disable=SC2086
  sed -i 's|__SCALEPLEX_JF_URL__|github.com/jellyfin/jellyfin-ffmpeg|g' $FILES

  # Rename the per-binary-package debian helper files. After the sed
  # above their *content* refers to scaleplex-ffmpeg7, but the filenames
  # still carry the jellyfin- prefix. dpkg-buildpackage looks them up
  # by `<binary-package>.<role>` so file names must match the new name.
  for f in debian/jellyfin-ffmpeg7.*; do
    mv "$f" "${f/jellyfin-ffmpeg7/scaleplex-ffmpeg7}"
  done
)

echo "==> Layering scaleplex patches into debian/patches/"
for patch in "$PATCHES"/*.patch; do
  name="$(basename "$patch")"
  cp -v "$patch" "$WORKDIR/debian/patches/$name"
  echo "$name" >> "$WORKDIR/debian/patches/series"
done
echo "--- new series tail:"
tail -10 "$WORKDIR/debian/patches/series"

echo "==> Generating Dockerfile + building deb"
(
  cd "$WORKDIR"
  make -f Dockerfile.make Dockerfile
  # podman won't resolve bare distro names without a TTY prompt; rewrite
  # `FROM noble` (templated from Dockerfile.in's `FROM DISTRO`) to a
  # fully-qualified reference. Harmless on real Docker.
  sed -i 's|^FROM noble$|FROM docker.io/library/ubuntu:noble|' Dockerfile
  # Use buildx + GHA layer cache when available (CI). Locally we fall
  # back to plain `docker build` against podman, which has no buildx
  # but caches layers natively in its own graphroot.
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
  # `:z` tells podman to relabel the host bind-mount with a shared
  # container SELinux context so the in-container artifact write
  # doesn't get blocked by host-side `unlabeled_t`. No-op on plain Docker.
  docker run --rm -v "$PWD/dist:/dist:z" "$IMG"
)

echo "==> Copying artifacts to $DIST/"
cp -v "$WORKDIR"/dist/deb/scaleplex-ffmpeg7_*.deb "$DIST/"

echo
echo "Built deb(s):"
ls -lh "$DIST"/scaleplex-ffmpeg7_*.deb
