# optimize-corpus-gen

Drives a Plex Media Server through Optimize jobs across a matrix of
`{source × target × prefs}`, capturing the ffmpeg argv each cell
produces via the worker's existing `WORKER_DUMP_ARGV=1` instrumentation.
Output: a representative corpus of Plex Optimize argvs the rewriter's
parity harness (`worker/agent/replay_test.go +tags=replay`) can consume.

## Why

Plex Optimize jobs are under-represented in the organic argv corpus
because they fire only when a user explicitly requests an Optimize —
unlike live playback, which dominates corpus capture. This makes it
hard to evaluate refactors that touch the Optimize fast-path
(`tryOptimizeRemux` in `worker/agent/rewriter.go`) with the same
confidence the orthogonal-detector refactor had over the
~1583-entry playback corpus.

This generator builds a deliberate Optimize corpus by automating the
API surface PMS exposes — same mechanism `python-plexapi`'s
`media.optimize()` and Tautulli's `plex_optimize.py` use.

## Subcommands

| Subcommand | Purpose |
|---|---|
| `smoke`   | One-cell end-to-end test against an existing library item. Validates Plex REST + ffprobe + watcher + cancel plumbing. |
| `synth`   | Build the synthetic source-clip matrix into a directory (idempotent — skips clips already present). |
| `library` | Create+scan the Plex library section that holds the synth clips. Idempotent — refreshes existing sections. |
| `sweep`   | Run the full {sources × targets × prefs} matrix sweep. Resumable via `manifest.json`. |
| `clean`   | Cancel all `scaleplex-corpus-gen-*` Optimize jobs + stop stuck static transcode sessions. |
| `analyze` | Cluster the captured corpus by argv-shape fingerprint. Reports distinct shapes + their representative cells. |

## Full workflow (homelab)

```bash
source ~/.config/plex.env   # PLEX_TOKEN, PLEX_TEST_URL, PLEX_URL

POD_FFMPEG="kubectl exec -n media-toolkit $(k get pod -n media-toolkit -o name | head -1 | cut -d/ -f2) -- ffmpeg"
POD_FFPROBE="kubectl exec -n media-toolkit $(k get pod -n media-toolkit -o name | head -1 | cut -d/ -f2) -- ffprobe"
POD_WORKER="kubectl exec -n plex-test $(k get pod -n plex-test -l app.kubernetes.io/name=scaleplex-worker -o name | head -1 | cut -d/ -f2) --"

# 1. Synthesize the 19-clip source matrix into the NFS-shared dir.
#    ~3 min first run; sub-second on re-runs (all skipped).
optimize-corpus-gen synth \
    -out-dir /mnt/media/media/scaleplex-test-clips \
    -ffmpeg-exec "$POD_FFMPEG"

# 2. Create + scan the Plex library section (idempotent).
optimize-corpus-gen library \
    -plex "$PLEX_TEST_URL" -token "$PLEX_TOKEN" \
    -name "scaleplex-test-corpus" \
    -location /media/scaleplex-test-clips \
    -expected-items 19 -wait-timeout 90s

# 3. Sweep the matrix (resumable; ~1.5s/cell, ~45 min for the full 1824).
optimize-corpus-gen sweep \
    -plex "$PLEX_TEST_URL" -token "$PLEX_TOKEN" \
    -source-section "scaleplex-test-corpus" \
    -ffprobe-exec "$POD_FFPROBE" -path-translate "/media=/mnt/media/media" \
    -corpus-exec "$POD_WORKER" \
    -corpus-dir ~/scaleplex-corpus/optimize-sweep

# 4. Analyze: cluster captures by argv-shape fingerprint.
optimize-corpus-gen analyze -corpus-dir ~/scaleplex-corpus/optimize-sweep

# Recovery: kill stuck PMS state if a previous run was interrupted.
optimize-corpus-gen clean -plex "$PLEX_TEST_URL" -token "$PLEX_TOKEN"
```

## Matrix dimensions

**Sources** (19 synthetic clips, codec × profile × bit-depth × transfer × audio × sub):
- 3 h264 cells (1080p stereo, 1080p 5.1, 720p stereo)
- 8 hevc cells (Main 8-bit, Main10 10-bit × {1080p, 4K} × {SDR, HDR10, HLG})
- 4 av1 cells (Main + Main10, +HDR variants, +SRT variant)
- 2 7.1-channel cells (h264, hevc HDR)
- 4 sub-burn variants (SRT + ASS on h264/hevc)

**Targets** (3 built-in):
- tagID=1 "Optimized for Mobile" (720p, 4 Mbps)
- tagID=2 "Optimized for TV" (1080p, 8 Mbps)
- tagID=3 "Original Quality" (no transcode — pure remux)

**Prefs** (32 cells = 2⁵):
- `HardwareAcceleratedCodecs`     {true, false}    HW decode
- `HardwareAcceleratedEncoders`   {true, false}    HW encode
- `TranscoderToneMapping`         {true, false}    HW HDR tonemap
- `TranscoderHEVCEncodingMode`    {always, never}  HEVC for standard transcodes
- `TranscoderHEVCOptimize`        {true, false}    HEVC for Optimize jobs (independent!)

Cartesian: **19 × 3 × 32 = 1824 cells**. Many collapse to identical argv
shapes in PMS (HW-encode-on with HW-decode-off is a no-op variant, etc.);
the `analyze` subcommand counts the actual distinct shapes.

## Output

For each capture the worker writes (e.g.
`/transcode/_argv-corpus/<basename>.json`), the generator writes a
sidecar in `-corpus-dir`:

```
~/scaleplex-corpus/optimize-gen/
├── 101_Dalmatians_II_-63158-77f2aaa4.json                  # worker's captured argv
└── 101_Dalmatians_II_-63158-77f2aaa4.optimize-cell.json    # generator's cell tag
```

The cell tag holds:
```json
{
  "cell_id": "<rfc3339ts>-<ratingKey>-<targetTagID>",
  "source": {
    "rating_key": "313359",
    "title": "101 Dalmatiërs",
    "probe": { "video_codec": "av1", "width": 1920, "hdr_format": "SDR", ... }
  },
  "target": { "tag_id": 2, "title": "Optimized for TV" },
  "prefs": { "HardwareAcceleratedCodecs": "true", ... }
}
```

`source.probe` is from **ffprobe on the actual file**, NOT from Plex's
stored library metadata — which goes stale after every Tdarr transcode
(per [user-observed gotcha](../../../docs/REWRITER.md)). The cell is
keyed off ground-truth source shape.

## Endpoints used (Plex 1.43+)

| Action | Method | Path |
|---|---|---|
| Ping + version | GET | `/identity` |
| All prefs | GET | `/:/prefs` |
| Set pref | PUT | `/:/prefs?<ID>=<value>[&<ID2>=<value2>...]` |
| Library sections | GET | `/library/sections` |
| Section items | GET | `/library/sections/<key>/all` |
| Series episodes | GET | `/library/metadata/<ratingKey>/allLeaves` |
| Item metadata | GET | `/library/metadata/<ratingKey>` |
| Optimize targets | GET | `/library/tags?type=42` |
| Background-processing playlist | GET | `/playlists?type=42` |
| Trigger Optimize | PUT | `<bgkey>` ← e.g. `/playlists/1066/items` |
| List Optimize jobs | GET | `<bgkey>` |
| Cancel Optimize | DELETE | `<bgkey>/<id>` |
| Active transcode sessions | GET | `/transcode/sessions` |
| Stop transcode session | DELETE | `/transcode/sessions/<key>` |

Token auth via `?X-Plex-Token=…` query param (header form works too).

## Quirks discovered

- **Optimize endpoint shape** is `PUT <background-processing-playlist>` ,
  NOT `POST /playlists/all` or `PUT /library/optimize` (both 404). The
  background-processing playlist's ratingKey is per-install (1066 on our
  PMS; Tautulli hardcoded 1066 because theirs was too).
- **`Item[Location][uri]` format** is
  `library://<section.uuid>/item/<url-encoded /library/metadata/N>` —
  uri-encoded path component. `library:///library/metadata/N` (the
  obvious shape) returns 400.
- **`Optimized.remove()` endpoint** is `DELETE <bgkey>/<id>`, NOT
  `DELETE /playlists/<id>` — Optimize jobs are Item children of the
  background-processing playlist, not top-level playlists.
- **PMS state after a cell** can carry over: a stuck static transcode
  session blocks `OptimizerTranscodeCountLimit=1` further dispatches;
  the "media version already exists" 400 (code 1006) lingers across
  deletes because PMS preserves the on-disk Optimize output file. The
  sweep-cancel in `main.go` handles the session; Phase 2 needs the
  source-version disk cleanup.
- **Plex stores titles untruncated** in `Item[title]`, but the cell-
  loop should still match by prefix (`scaleplex-corpus-gen-`) rather
  than exact title to survive timing races where the optimize was
  already dispatched + dequeued before our list call.
- **HEVC pref nuance**: `TranscoderHEVCEncodingMode` gates standard
  transcoding's HEVC encoder choice; **Optimize jobs ignore it and
  consult `TranscoderHEVCOptimize` separately**. A pref matrix that
  toggles only the standard pref misses half the Optimize cells.
