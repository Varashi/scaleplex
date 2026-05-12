#!/usr/bin/env bash
# Shared helper: re-namespace a jellyfin-ffmpeg checkout to scaleplex-ffmpeg.
#
# Sourced by ../build.sh and ../build-deps.sh. Must run with `set -euo pipefail`
# already in effect. Operates on $PWD (cd into the checkout root first).
#
# What it does:
#  1. Stashes the upstream github URL behind a placeholder token so the
#     mass sed doesn't rewrite our origin pointer.
#  2. Renames every occurrence of "jellyfin-ffmpeg" → "scaleplex-ffmpeg"
#     across debian/, builder/, Dockerfile.in, docker-build.sh.
#  3. Restores the upstream URL placeholder.
#  4. Renames the per-binary-package debian helper files (control script
#     basenames must match the new binary package name for
#     dpkg-buildpackage to find them).
#
# Result: install path /usr/lib/scaleplex-ffmpeg/, deb package name
# scaleplex-ffmpeg7. Both deps-image and patches+deb build see an
# identically renamed tree, so the bundled-deps' baked-in install
# locations match what dpkg-buildpackage expects later.

scaleplex_ffmpeg_rename_in_place() {
  local files
  files=$(find debian builder Dockerfile.in docker-build.sh -type f 2>/dev/null)

  # shellcheck disable=SC2086
  sed -i 's|github.com/jellyfin/jellyfin-ffmpeg|__SCALEPLEX_JF_URL__|g' $files
  # shellcheck disable=SC2086
  sed -i 's|jellyfin-ffmpeg|scaleplex-ffmpeg|g' $files
  # shellcheck disable=SC2086
  sed -i 's|__SCALEPLEX_JF_URL__|github.com/jellyfin/jellyfin-ffmpeg|g' $files

  for f in debian/jellyfin-ffmpeg7.*; do
    [[ -e $f ]] || continue
    mv "$f" "${f/jellyfin-ffmpeg7/scaleplex-ffmpeg7}"
  done
}
