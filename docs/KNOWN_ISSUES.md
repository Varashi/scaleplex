# Known issues

Tracked limitations as of v1.2. None block playback; each has a
documented cause and, where relevant, a path to a fix.

## SRT sub-burn pre-render renders full canvas (planned v1.2.1)

**Severity:** perf — not correctness. SRT burn-in works; it just costs
more CPU than PGS at the same source resolution.

**Symptom.** 4K + SRT burn-in pre-render runs at ~1.5 cores total
(pre-render ~50% + main ~100%) on plex-test, vs ~0.6 cores total for
PGS at the same resolution. Per-worker concurrency cap for 4K SRT-burn
sits around 2 streams.

**Cause.** SRT cues can be positioned anywhere on screen (top, mid,
sign translations), so the libass-driven pre-render rasterises the
full 4K canvas (`s=3840x2160`) before crop. PGS pre-render bitmaps are
already a band; the rewriter crops to `3840x864` at `y=1296` and
overlays the band — much smaller rasterise + composite surface.

**Path to a fix.** Two angles:
- Parse the .srt ahead of pre-render. If all cues lack explicit
  position tags AND fit a bottom-band threshold, render at band-size
  and overlay at `y=H-BandH`. Fall back to full canvas otherwise.
- Or post-rasterise per-cue bbox tracking — render full canvas to a
  tiny in-memory ring, detect cue bbox, crop before encode. Heavier
  to wire but covers positioned cues correctly.

Tracked for v1.2.1.

## Plex Windows desktop — playhead resets to 0 on seek

**Severity:** cosmetic. Playback and seeking work correctly; only the
scrubber thumb position is wrong immediately after a seek.

**Symptom.** On Plex for Windows (segmented-matroska output), seeking
mid-playback leaves the on-screen playhead at 0:00 instead of the seek
target. The video plays from the correct position; the slider catches
up on the next position report.

**Cause.** Plex Windows plays a growing matroska byte stream. Each
chunk's `Cluster.Timecode` should carry the absolute source PTS so mpv
anchors its timeline. scaleplex strips `-copyts` is *not* the issue here
(patch 0103 keeps it on matroska); the residual gap is that the very
first chunk after a seek can reach the client before the timeline
re-anchors, and mpv momentarily shows 0.

**Why it isn't fixed.** The clean fix is upstream of scaleplex — it is a
client-side timeline-anchoring quirk in Plex's mpv build. Server-side
workarounds (per-chunk EBML `Timecode` rewriting) were prototyped and
rejected: they add a parse-patch-rewrite pass to every chunk for a
purely cosmetic gain.

**Workaround for users.** None needed — re-clicking the scrubber or
waiting one position-report cycle corrects the display.

## Plex Windows desktop — external sidecar SRT swap is a no-op

**Severity:** cosmetic / client-side, NOT scaleplex.

**Symptom.** On Plex for Windows direct-play / direct-stream, embedded
SRT and embedded PGS render fine. Switching the subtitle dropdown from
an embedded track to an **external sidecar** (`.nl.srt`) is a no-op —
the previously-rendered track keeps showing. The web "Burn Subtitles:
Always" preference doesn't affect this client either; that pref is
Plex-Web-only.

**Cause.** Sub track switching is decided 100% on the client for
direct play. PMS metadata correctly marks the external sub as
`selected="1"` and the sidecar file is genuinely Dutch — the Windows
client just doesn't load it on a hot swap.

**Workaround for users.** Stop playback fully + restart it; the
external sub then loads correctly.

**Verification:** the PMS log carries no subtitle-decision lines for
direct play. The actual selection lives in
`%LOCALAPPDATA%\Plex\Logs\Plex.log` on the desktop. Grep for the
sidecar's PMS stream id to confirm what the client loaded.

## SRT sidecar on 4K HEVC HDR — PMS downscales video to SD

**Severity:** expected PMS behaviour, not a scaleplex defect.

**Symptom.** Selecting an external SRT subtitle on a 4K HEVC HDR title
in Plex Web makes PMS pick a 720x404 ~1.6 Mbps video target.

**Cause.** PMS's own quality-selection logic downscales when an SRT
sidecar is delivered as a side-channel on a 4K HDR source. scaleplex
faithfully transcodes whatever target PMS requests — the downscale
decision happens before the worker ever sees the argv.

**Why it isn't fixed.** It is not scaleplex's decision to make. The
transcode is correct for the target PMS asked for.

## No HEVC software-encode path

**Severity:** by design.

Plex's software encoder only ever emits h264 / libx264; HEVC encode is
gated behind Plex's "enable HW encode" server setting. The rewriter's
`libx265 → hevc_vaapi` mapping therefore never fires in practice — no
libx265 sessions appear in the 287-entry argv corpus. Not a bug, just a
path that is unreachable given how PMS builds argv.
