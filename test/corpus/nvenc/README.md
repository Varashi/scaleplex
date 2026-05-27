# NVENC argv corpus

Plex Media Server NVENC/NVDEC argv captures from `skw-d-linuxtest`
(RTX 3080 / WSL2 Ubuntu 24.04 / nvidia-container-toolkit 1.19), used as
the deterministic golden input for `worker/agent/rewriter_nvidia_test.go`.

Captured 2026-05-27 via `WORKER_DUMP_ARGV=1` on the dev box (see
[reference_skw_d_linuxtest](https://github.com/Varashi/scaleplex/blob/main/docs/HW_PROFILE.md)).
No tokens, no credentials — only argv + PID + TS_NS + session UUIDs.

## Layout

- `persistent/` (63 files, ~457 KB raw → 296 KB after dedup): full
  argv-shape matrix sweep against `/mnt/media/scaleplex-test-clips/`
  (h264/hevc/av1, 1080p/2160p, 8/10-bit, sdr/hdr10, eac3/aac, no-sub /
  embedded-srt / pgs / ass).
- `phone/` (7 files, ~81 KB): Android Plex client captures.
  hevc_nvenc playback shape — distinct subset (mobile profile bitrates
  + audio-only re-encodes).

## Format

One `.argv` file per PMS invocation. Header:

```
ARGC: 91
PID: 1637
TS_NS: 1779912336497226217
SID: 3196a5c5-5174-4ee6-a24b-536efe361601
---
<argv[0]>
<argv[1]>
...
```

Lines after `---` are one argv token per line, in order. The header
fields are advisory; the replay test only consumes the post-`---`
section.

## Why these are checked in

Backend-divergence regressions (PR #2+) need a deterministic gate
that doesn't depend on the dev box being reachable from CI. ~296 KB
is acceptable repo weight for what they protect; rotate (not append)
on the next major argv-shape change.
