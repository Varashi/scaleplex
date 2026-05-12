#!/usr/bin/env bash
# Inject two-phase mode into the renamed docker-build.sh.
#
# Sourced by build-deps.sh (deps image) and build.sh (patches+deb image).
# Operates on $PWD/docker-build.sh — the file must already be re-namespaced
# (rename.sh) so the line-anchor patterns match.
#
# Modes (driven by env vars; argv parsed at the top of the patched script):
#  SCALEPLEX_PHASE=deps   — run prepare_extra_*; do NOT run dpkg-buildpackage.
#                            Used by the deps image's `RUN /docker-build.sh`.
#  SCALEPLEX_PHASE=build  — skip prepare_extra_*; assume the deps image
#                            already installed everything under
#                            ${TARGET_DIR}. Just run mk-build-deps +
#                            dpkg-buildpackage. Used by the patches+deb
#                            image at runtime.
#  unset (default)        — original behaviour: both phases inline. Lets us
#                            roll back to the legacy build.sh path without
#                            touching docker-build.sh's logic.
#
# The two seams in upstream docker-build.sh:
#   - `case ${ARCH} in 'amd64') ... prepare_extra_common; prepare_extra_amd64 ... esac`
#       → gated by [[ $SCALEPLEX_PHASE != build ]]
#   - `# Move to source directory ... dpkg-buildpackage ...` (final block)
#       → gated by [[ $SCALEPLEX_PHASE != deps ]]
#
# This is awk/sed surgery rather than a unified patch on purpose: the
# upstream file evolves between jellyfin tags (line numbers shift), but
# the anchor comments and case-on-ARCH structure are stable.

scaleplex_ffmpeg_split_docker_build() {
  local f="$PWD/docker-build.sh"
  [[ -f $f ]] || { echo "split-docker-build: $f not found" >&2; return 1; }

  # 1. Prepend argv parser. Inserts after the initial shebang+set lines.
  #    Reads SCALEPLEX_PHASE from argv ("--deps-only" / "--build-only") or
  #    env. Default = empty (original behaviour).
  python3 - "$f" <<'PYEOF'
import re, sys
p = sys.argv[1]
with open(p) as fh:
    src = fh.read()

inject = '''
# --- scaleplex split-build injection --------------------------------------
# Mode selector. Argv wins over env. Empty = legacy single-shot.
SCALEPLEX_PHASE="${SCALEPLEX_PHASE:-}"
for arg in "$@"; do
    case "$arg" in
        --deps-only)  SCALEPLEX_PHASE=deps ;;
        --build-only) SCALEPLEX_PHASE=build ;;
    esac
done
export SCALEPLEX_PHASE
# --------------------------------------------------------------------------
'''
# Insert after `set -o xtrace` (last set -o line at the top of upstream).
marker = 'set -o xtrace\n'
if marker not in src:
    sys.exit('split-docker-build: marker not found, cannot inject')
src = src.replace(marker, marker + inject, 1)

# 2. Replace the architecture case-block with a two-arm version that
#    splits "heavy deps prep" from "lightweight per-arch option setting".
#    Option vars (CONFIG_SITE / DEP_ARCH_OPT / BUILD_ARCH_OPT and the arm64
#    cross-toolchain symlinks) must always run — dpkg-buildpackage reads
#    them. Deps prep (apt dist-upgrade + prepare_extra_*) is what we want
#    to skip in --build-only mode.
case_open = '# Set the architecture-specific options\ncase ${ARCH} in'
if case_open not in src:
    sys.exit('split-docker-build: deps case-block header not found')

idx = src.index(case_open)
depth = 0
i = idx
while True:
    m = re.search(r'\bcase\b|\besac\b', src[i:])
    if not m:
        sys.exit('split-docker-build: ran off end looking for esac')
    tok = m.group(0)
    pos = i + m.end()
    if tok == 'case':
        depth += 1
    else:
        depth -= 1
        if depth == 0:
            esac_end = pos
            break
    i = pos

replacement = '''# Set the architecture-specific options
# scaleplex split: option vars always run; deps prep skipped in --build-only.
case ${ARCH} in
    'amd64')
        if [[ "${SCALEPLEX_PHASE}" != "build" ]]; then
            apt-get update && apt-get dist-upgrade -y
            prepare_extra_common
            prepare_extra_amd64
        fi
        CONFIG_SITE=""
        DEP_ARCH_OPT=""
        BUILD_ARCH_OPT=""
    ;;
    'arm64')
        if [[ "${SCALEPLEX_PHASE}" != "build" ]]; then
            prepare_crossbuild_env_arm64
            ln -s /usr/bin/aarch64-linux-gnu-gcc-${GCC_VER} /usr/bin/aarch64-linux-gnu-gcc
            ln -s /usr/bin/aarch64-linux-gnu-gcc-ar-${GCC_VER} /usr/bin/aarch64-linux-gnu-gcc-ar
            ln -s /usr/bin/aarch64-linux-gnu-g++-${GCC_VER} /usr/bin/aarch64-linux-gnu-g++
            prepare_extra_common
            prepare_extra_arm
        fi
        CONFIG_SITE="/etc/dpkg-cross/cross-config.${ARCH}"
        DEP_ARCH_OPT="--host-arch arm64"
        BUILD_ARCH_OPT="-aarm64"
    ;;
esac'''

src = src[:idx] + replacement + src[esac_end:]

# 3. Gate the final `mk-build-deps + dpkg-buildpackage` tail. Marker: the
#    comment line right above it.
tail_marker = '# Install dependencies and build the deb\n'
if tail_marker not in src:
    sys.exit('split-docker-build: tail marker not found')
ti = src.index(tail_marker)
# Wrap from `# Move to source directory` through the artifact-move chown.
# Find the section start (the `pushd ${SOURCE_DIR}` and its preceding
# comment) and the end (after `chown -Rc ...`).
sec_start_marker = '# Move to source directory\n'
sec_start = src.rindex(sec_start_marker, 0, ti)
sec_end_marker = '${ARTIFACT_DIR}\n'  # chown -Rc $(stat -c %u:%g ${ARTIFACT_DIR}) ${ARTIFACT_DIR}
sec_end = src.index(sec_end_marker, ti) + len(sec_end_marker)

head = src[:sec_start]
tail_block = src[sec_start:sec_end]
foot = src[sec_end:]

# Default deps message — keeps logs clean when build-deps image runs.
deps_skip = (
    'if [[ "${SCALEPLEX_PHASE}" == "deps" ]]; then\n'
    '    echo "==> SCALEPLEX_PHASE=deps: stopping before dpkg-buildpackage"\n'
    '    exit 0\n'
    'fi\n\n'
)
src = head + deps_skip + tail_block + foot

with open(p, 'w') as fh:
    fh.write(src)
PYEOF
}
