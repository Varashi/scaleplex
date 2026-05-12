# scaleplex-ffmpeg

A thin patch layer on top of [jellyfin-ffmpeg](https://github.com/jellyfin/jellyfin-ffmpeg)
that backports the small subset of Plex Inc.'s ffmpeg fork that lets us
delete a meaningful chunk of worker rewriter / agent scaffolding.

## Why fork

scaleplex's worker rewriter spends a lot of effort coercing stock ffmpeg
to produce output that PMS expects from its own ffmpeg fork. Several of
those rewrites are workarounds for stock ffmpeg behavior that Plex
already fixed in their tree, and a few worker-side helpers (manifest
publisher, chunk renumber + tfdt patcher, external SIGSTOP/SIGCONT
throttle) exist purely to approximate Plex-fork features that don't
ship in upstream FFmpeg.

This subdir builds a re-namespaced deb (`scaleplex-ffmpeg7`) with each
of those Plex-fork features ported as a clean Debian-format patch on
top of jellyfin-ffmpeg's existing 93-patch stack.

## Patch series

Patches live in `patches/` and follow Debian numbering, continuing
jellyfin's series. Each is a unified diff applied with `-p1` from the
ffmpeg source root.

| #    | Target file                | Phase | Origin                | What it does |
|------|----------------------------|-------|-----------------------|---|
| 0094 | `libavformat/matroskaenc.c` | 1   | Plex 6.1.3            | Always write `Duration` in matroska header (drops the `is_live` guard). Plex Windows mpv slider works from the first segment. |
| 0095 | `libavformat/dashenc.c`     | 3   | Plex 6.1.3            | `-manifest_name <url>` PUTs the .mpd to a URL; `-skip_to_segment N` starts dash muxer at N; `-break_non_keyframes`; `-delete_removed`. |
| 0096 | `libavformat/segment.c`     | 2a  | Plex 6.1.3 (stub)     | Accepts `-segment_list_unfinished` and `-segment_list_separate_stream_times` as no-op AVOptions so the rewriter can stop stripping them. Functional emit logic comes in Phase 2b. |
| 0097 | `fftools/ffmpeg.c` + `ffmpeg_demux.c` + `ffmpeg_opt.c` + `ffmpeg.h` | 4 | Plex 1.12.3 mirror (Diagonactic) | New option `-canthrottleurl <url>`. One-shot PUT after each progress report, parses response body for `canThrottle`, applies per-input-packet `av_usleep(100ms)`. Replaces worker's external SIGSTOP/SIGCONT throttle. |

### Patch authoring playbook

When you need a new patch (Plex adds a feature, jellyfin updates, etc.):

1. **Find the canonical Plex source.** The official GPL tarball at
   `https://downloads.plex.tv/ffmpeg-source/` only ships
   `libavformat/` + `libavcodec/` (the libs Plex modified). For
   anything under `fftools/`, `cmdutils.c`, or other parts of FFmpeg,
   use the **Diagonactic/plex-new-transcoder** auto-mirror
   (https://github.com/Diagonactic/plex-new-transcoder) — its
   `plex-ffmpeg-source/NewPlexTranscoder/` tree contains the full
   Plex Transcoder source synced nightly from Plex's GPL drops.
   See memory `reference_plex_ffmpeg_full_source.md`.

2. **Identify the change.** `diff -u jellyfin-ffmpeg/<file>
   plex-source/<file>` and grep for `PLEX` comment markers Plex
   leaves around their additions. Filter out incidental FFmpeg
   version-drift (e.g. `.unit = "..."` AVOption struct changes
   between 6.1 and 7.1 — DO NOT regress those).

3. **Forward-port to jellyfin's tag.** Hunk line numbers will differ;
   recompute against `jellyfin-ffmpeg/<file>` at the pinned tag.
   `patch --dry-run -p1 < your.patch` from the jellyfin checkout
   validates clean apply.

4. **Document the patch.** Every `.patch` file in `patches/` carries
   a `Description:`, `Origin:`, and `Forwarded:` header at the top.
   `Origin:` must include the upstream commit / file path / line
   numbers (so the next person rebasing knows where to look). When
   the Plex source isn't in the official tarball, cite Diagonactic.

5. **Append to series.** `build.sh` auto-appends every
   `patches/*.patch` to jellyfin's `debian/patches/series` in
   filename order, so numeric prefixes (`0094-`, `0095-`...) drive
   apply order.

## Build pipeline

The build is split into two images for fast iteration:

1. **`scaleplex-ffmpeg-deps:<jellyfin-tag>`** — slow-changing base.
   Re-namespaced jellyfin checkout + every bundled dep (iconv,
   freetype, fribidi, harfbuzz, libass, x264, x265, dav1d, SVT-AV1,
   libdrm, libva, libplacebo, intel media-driver, oneVPL, NVENC
   headers, …) pre-compiled into `/usr/lib/scaleplex-ffmpeg/`. Built
   by `./build-deps.sh`, only re-built when `VERSION` (jellyfin tag)
   bumps. Pushed to GHCR.

2. **`scaleplex-ffmpeg-builder:<tag>`** — fast-changing patches-on-top
   layer. `./build.sh` clones jellyfin at `VERSION`, applies our
   patches, runs `dpkg-buildpackage` against the prebaked deps base.
   ~5-10 min per patch iteration vs ~60 min unified.

```sh
./build-deps.sh                  # build + push deps base (slow, rare)
./build.sh                       # build the deb against the latest patches (fast)
./build.sh v7.1.3-1              # build against a specific jellyfin tag
PUSH=1 ./build-deps.sh           # CI mode: also push deps base to GHCR
AUTO_BUILD_DEPS=1 ./build.sh     # build deps locally if not on GHCR yet
```

Output deb: `scaleplex-ffmpeg/dist/scaleplex-ffmpeg7_<version>_<arch>.deb`.

### Re-namespacing

`build.sh` and `build-deps.sh` source `lib/rename.sh` to mass-sed
`jellyfin-ffmpeg` → `scaleplex-ffmpeg` across `debian/`, `Dockerfile.in`,
`docker-build.sh`, and `builder/`. URL references to jellyfin's GitHub
are preserved (origin tracking). Result: deb installs to
`/usr/lib/scaleplex-ffmpeg/` and never collides with an upstream
jellyfin-ffmpeg install. Binary name inside stays `ffmpeg`.

### docker-build.sh phase split

The deps build needs to stop *before* `dpkg-buildpackage` so the layer
can be cached. `lib/split-docker-build.sh` rewrites jellyfin's
`docker-build.sh` to honor `SCALEPLEX_PHASE=deps|build`:

- `SCALEPLEX_PHASE=deps`: do every `prepare_extra_*` (bundled-deps
  compile + install into `${TARGET_DIR}`), then exit before
  `mk-build-deps`/`dpkg-buildpackage`. This is the inner `RUN` step
  of the deps-image Dockerfile.
- `SCALEPLEX_PHASE=build`: skip the prepare-extra phase, jump
  straight to `mk-build-deps` + `dpkg-buildpackage`. This is what
  the patches-on-top layer runs.
- (unset): legacy single-shot behaviour.

## Rebasing on a new jellyfin release

1. Bump `VERSION` to the new jellyfin tag.
2. `./build-deps.sh` to rebuild the deps base for that tag. (`PUSH=1`
   to publish.) This is the slow step (~60 min) and only runs once
   per jellyfin bump.
3. `./build.sh` to rebuild the deb against the new deps. If patches
   reject, edit hunk offsets (`patch --dry-run` against a fresh
   checkout shows the new line numbers) and re-test.
4. `./build.sh` should run in ~5-10 min once the deps base is cached
   locally / on GHCR.
5. Commit the new `VERSION` + any patch fixups. Rebuild the worker
   image (`build-worker` GHA picks up the new ffmpeg deb artifact
   via cross-workflow download).

Patches are intentionally small (Phase 1 = 1 LOC, Phase 3 = ~80 LOC,
Phase 4 = ~50 LOC, etc.) and located in a handful of files only —
rebasing should remain a 15-minute exercise per jellyfin bump.

## Rebasing on a new Plex Transcoder release

Plex doesn't tag releases on their GPL drops, but Diagonactic's mirror
notes the version in `latest.version` in its repo root.

1. Pull a fresh clone of `Diagonactic/plex-new-transcoder` to `/tmp`.
2. For each of our patches, diff our backported logic against the new
   Plex source. Watch for refactors — Plex's fork sometimes moves
   code blocks around between versions (e.g. `segment_write_list`
   refactor in segment.c between 1.10 and 1.12).
3. If Plex's logic changed materially, regenerate the patch. If it's
   just a line-number drift, just refresh the hunk offsets.
4. Bump the `Origin:` line in the patch header to cite the new Plex
   version.

This is rarer than a jellyfin bump (Plex doesn't tear up their
transcoder often) but worth doing once a year to catch new features
we'd want to backport (e.g. canThrottle was added in Plex's 1.4 era,
the dashenc seek-resume in 1.8).
