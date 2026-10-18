#!/usr/bin/env bash
# Prints the content key of the scaleplex-ffmpeg deb: a hash over every
# committed build input under scaleplex-ffmpeg/ (patches, VERSION,
# build scripts, lib/), excluding docs. build-ffmpeg publishes the deb as
# ghcr.io/varashi/scaleplex-ffmpeg-deb:<key>; build-worker pulls it by
# the same key. Uses committed blob hashes (git ls-tree), so the result
# depends only on HEAD, not on the working tree.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
git ls-tree -r HEAD -- scaleplex-ffmpeg \
  | grep -v '\.md$' \
  | sha256sum | cut -c1-40
