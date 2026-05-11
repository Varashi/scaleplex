# scaleplex-ffmpeg

A thin patch layer on top of [jellyfin-ffmpeg](https://github.com/jellyfin/jellyfin-ffmpeg)
that backports the small subset of Plex Inc.'s ffmpeg fork that lets us
delete a meaningful chunk of worker rewriter logic.

## Why fork

scaleplex's worker rewriter spends a lot of effort coercing stock ffmpeg
to produce output that PMS expects from its own ffmpeg fork. Several of
those rewrites are workarounds for stock ffmpeg behavior that Plex
already fixed in their tree, e.g.:

- `matroskaenc.c`: stock skips the Duration header field when
  `live=1`. Plex always writes it. Without the patch we fall back to
  `live=0` + per-frame clusters, which bloats the chunk stream and
  still doesn't fix the related playhead-reset on audio-swap.
- `segment.c`: stock segment muxer can't emit unfinished entries (`#`
  prefix) and lacks separate audio/video end times. The rewriter
  strips those PMS-emitted options before spawning ffmpeg.
- `dashenc.c`: stock can't be told "start writing at segment N" — the
  worker compensates via `tfdt` patching + segment_index renumber at
  the relay. Plex's `-skip_to_segment N` makes that a no-op.

## Patch series

Patches live in `patches/` and follow Debian numbering, continuing
jellyfin's series (jellyfin has 1..93 today). Each is a unified diff
applied with `-p1` from the ffmpeg source root.

| # | File | Source | What it does |
|---|---|---|---|
| 0094 | `matroskaenc.c` | Plex 6.1.3 | Always write Duration in matroska header. Kills the slider-grows-from-zero bug on Plex Windows. |
| 0095 | `segment.c` | Plex 6.1.3 | (TODO) `segment_list_unfinished` + `segment_list_separate_stream_times` options; CSV emits `#`-prefixed in-progress entries. |
| 0096 | `dashenc.c` | Plex 6.1.3 | (TODO) `-manifest_name <url>` + `-skip_to_segment N`. Worker no longer rewrites DASH on seek. |

## Build

`build.sh` clones jellyfin-ffmpeg at a known tag, drops our patches
into `debian/patches/` + appends to `debian/patches/series`, then runs
their `build-linux-amd64` script. Output is a `.deb` in `dist/`.

```sh
./build.sh                 # uses default version (see VERSION)
./build.sh v7.1.3-1        # build against a specific jellyfin tag
```

`build.sh` also re-namespaces the deb from `jellyfin-ffmpeg7` to
`scaleplex-ffmpeg7` (package + install path → `/usr/lib/scaleplex-ffmpeg/`)
so the dpkg name and on-disk layout are unambiguously ours and never
collide with an upstream jellyfin-ffmpeg install. The binary inside is
still called `ffmpeg`.

Inside `worker/Dockerfile` we install the resulting deb via
`dpkg -i scaleplex-ffmpeg7_*.deb` instead of `apt install
jellyfin-ffmpeg7`, and the `/usr/bin/ffmpeg` symlink points at
`/usr/lib/scaleplex-ffmpeg/ffmpeg`.

## Rebasing on a new jellyfin release

1. Bump `VERSION` to the new jellyfin tag.
2. Re-run `build.sh`. If patches reject, edit the offsets and re-test.
3. Commit the new `VERSION` + any patch fixups.

Patches are intentionally small and located in three files only —
rebasing should remain a 5-minute exercise per jellyfin bump.
