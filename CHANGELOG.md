# Changelog

## v1.0.0 — 2026-05-15

First tagged release. scaleplex replaces the `Plex Transcoder` binary on
distributed-transcode workers with stock ffmpeg (`scaleplex-ffmpeg7` —
jellyfin-ffmpeg + a small Plex-backport patch layer), a thin Go
orchestrator, and a PMS-side shim. See [`README.md`](README.md) for the
why and the architecture.

### Validated

End-to-end on the scaleplex PMS deployment: Plex Web DASH (Chrome /
Firefox), Plex Android HLS (mpegts + matroska), Plex Windows desktop
(segmented matroska), LG webOS HLS, and Plex Optimize jobs — initial
play, seek, quality change, and subtitle burn-in as applicable. Source
matrix covers AV1 / HEVC / H264, SDR / HDR10, embedded and sidecar
SRT / ASS text subs, and embedded PGS bitmap subs.

### scaleplex-ffmpeg7 patch series

14 patches (`0094`–`0107`) layered on jellyfin-ffmpeg `v7.1.3-1`,
continuing jellyfin's Debian patch numbering:

- **0094** matroskaenc — always write `Duration` (Plex Windows slider).
- **0095** dashenc — `-manifest_name` URL PUT, `-skip_to_segment`,
  `-break_non_keyframes`, `-delete_removed`.
- **0096** segment — functional `-segment_list_separate_stream_times`
  CSV emit.
- **0097** fftools — `-canthrottleurl` Plex canThrottle handler.
- **0098** fftools + segment — Plex CLI compat (`-loglevel_plex` sink,
  `ssegment` global-header flag).
- **0099–0101** libavfilter + fftools — `inlineass` filter binding and
  pre-graph subtitle buffering.
- **0102** segment — stage-rename chunks (NFS race), skips text-sub
  outputs.
- **0103** segment — drop jellyfin's `reference_stream_first_pts`
  adjust so `-ss + -copyts` splits correctly.
- **0104** dashenc — default CMAF-strict `movflags` on URL output.
- **0105** vaapi_encode — auto-select QVBR when `-qp` + `-maxrate` set.
- **0106** segment — URL-handler segment lists buffer full history.
- **0107** fftools — `-strict_ts` no-op stub.

### Resilience

PMS `canThrottle` pass-through, multi-engine GPU load reporting, and
transparent mid-stream worker recovery across DaemonSet rolls.

### Notes

- The worker rewriter carries no remaining "bandaid" translations — every
  Plex-private argv quirk is either handled by a fork patch or is an
  intentional integration (relay routing, auth token injection).
- Known limitations are tracked in [`docs/KNOWN_ISSUES.md`](docs/KNOWN_ISSUES.md);
  none block playback.
