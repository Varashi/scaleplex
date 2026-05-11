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
WORKDIR="$(pwd)/.build"
DIST="$(pwd)/dist"
PATCHES="$(pwd)/patches"

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
  docker build -t scaleplex-ffmpeg-builder:"$TAG" .
  mkdir -p ./dist
  docker run --rm -v "$PWD/dist:/dist" scaleplex-ffmpeg-builder:"$TAG"
)

echo "==> Copying artifacts to $DIST/"
cp -v "$WORKDIR"/dist/deb/scaleplex-ffmpeg7_*.deb "$DIST/"

echo
echo "Built deb(s):"
ls -lh "$DIST"/scaleplex-ffmpeg7_*.deb
