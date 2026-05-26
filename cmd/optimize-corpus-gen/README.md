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

## Status

**Phase 1 — end-to-end one-cell smoke test (this commit).** Validates:
- Plex REST plumbing (identity, sections, items, prefs, optimize)
- ffprobe via local OR remote (kubectl exec into media-toolkit pod)
- Pref snapshot + restore on exit (incl. SIGINT/SIGTERM)
- Optimize trigger via background-processing playlist (id 1066 on our
  PMS, discovered at runtime)
- Worker capture watcher (local dir OR remote via kubectl exec)
- Capture tagging (sidecar `.optimize-cell.json` with source probe +
  target + prefs)
- Sweep-cancel pending corpus jobs + stuck static sessions

**Phase 2 — matrix sweep (next).** TODO:
- Synthetic source clip generator (codec × profile × bit-depth ×
  transfer × audio × sub matrix, ~30-50 short ffmpeg-built clips into a
  dedicated test library section)
- Pref combination enumerator (32 cells: `HardwareAcceleratedCodecs ×
  HardwareAcceleratedEncoders × TranscoderToneMapping ×
  TranscoderHEVCEncodingMode × TranscoderHEVCOptimize`)
- Cell-loop with pacing (PMS Optimize is one-at-a-time:
  `OptimizerTranscodeCountLimit=1`)
- Resume-from-manifest on interrupt
- Optimize-version disk cleanup (PMS keeps the optimized file even
  after job delete; need to `DELETE /library/metadata/<id>` on the
  optimized children to truly free the source for re-Optimize)

## Run (Phase 1)

```bash
source ~/.config/plex.env   # PLEX_TOKEN, PLEX_TEST_URL, PLEX_URL

# Remote ffprobe + remote corpus (homelab setup — no NFS on workstation):
go run . \
    -plex "$PLEX_TEST_URL" \
    -token "$PLEX_TOKEN" \
    -source-section "Movies Kids NL" \
    -source-rating-key 313359 \
    -ffprobe-exec "kubectl exec -n media-toolkit media-toolkit-<pod> -- ffprobe" \
    -path-translate "/media=/mnt/media/media" \
    -corpus-exec "kubectl exec -n plex-test plex-test-worker-<pod> --" \
    -corpus-remote-dir /transcode/_argv-corpus \
    -corpus-dir ~/scaleplex-corpus/optimize-gen \
    -target-tag 2 \
    -capture-timeout 60s

# Local-only (NFS mounted on workstation):
go run . \
    -plex "$PLEX_TEST_URL" \
    -token "$PLEX_TOKEN" \
    -source-section Movies \
    -corpus-dir ~/scaleplex-corpus \
    -dry-run    # validate plumbing without touching PMS state
```

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
