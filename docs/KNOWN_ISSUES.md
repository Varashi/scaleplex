# Known issues

Tracked limitations as of v1.4.0. None block playback; each has a
documented cause and, where relevant, a path to a fix.

## HW-decode + SW-encode hybrid — 4K HDR corruption

**Severity:** affects only `SCALEPLEX_FORCE_HW=0` (honor mode); prod runs
`FORCE_HW=1` so the honor paths are dormant.

When Plex is configured "HW acceleration on, HW encoding off" it emits a
HW-decode + SW-encode argv. v1.4.0's per-axis honor (`honor:plex-hwdec-swenc`)
runs that pipeline verbatim — HW AV1 decode → Plex's SW chain
(`tonemap=mobius` + libass) → libx264. On 4K HDR AV1 sources this shows
graphical corruption (and buffering at high output res). Root cause not yet
isolated (candidates: HW-AV1-HDR decode under throttle/starvation, the auto
hwdownload of p010 VAAPI surfaces into the SW chain, or a fork-vs-Plex ffmpeg
difference). **Do not rely on the hybrid until root-caused** — keep
`FORCE_HW=1` (re-accelerate). Follow-up: retest with extra worker vCPUs to
separate a buffering/starvation cause from a graph bug.

## Sub-burn startup I/O burst (decode-sink read-ahead) — RESOLVED in v1.5.0

Was: `-map_inlineass` triggered subtitle decoding via an unbounded
`-map <spec> -f null -codec ass` decode-sink. As an independent, unrate-limited
consumer it pulled the demuxer through the whole file to collect the sparse,
interleaved sub packets → a one-time startup I/O burst that competed with the
buffer-fill read and caused brief playback skips during the flat-out fill
(embedded-sub burn-in only; sidecar-sub sessions never burst).

**Fixed (v1.5.0):** decode-sink-free paced self-decode. The sub decoder is
created from the `-map_inlineass` binding as a **sink-less decoder** (no output
stream/encoder/muxer) and paced by the single demux thread's video-read
backpressure; the rewriter drops the null sink. No independent consumer, no
read-ahead. Fork patch `0120`; see `docs/PACED_SELF_DECODE.md` and
`docs/LATENCY.md`. Live-validated on plex-test (4K HDR, play + seek).

## Sub-burn band-sizing issues — RESOLVED in v1.3.0

**Status.** The two prior band-sizing limitations — *SRT pre-render bails to a
wide band on positional cues* and *static ASS/SSA renders full-frame* — are
**gone with the subtitle-burn unification (v1.3.0)**. There is no pre-render
band any more: the merged single-input `inlineass` VAAPI filter renders each
cue with libass at its true position (positional `\anN`/`\pos`/`\move` cues
included) and VPP-blends a cached surface onto the video. The
`SCALEPLEX_SUB_RENDER_HEIGHT` cap became the filter's `render_height` option
(default 1080) and applies uniformly to SRT, ASS, and animated ASS — the
per-cue area, not a session-wide worst-case band, drives cost. `overlay_vaapi`
(and its overlay-input dimension lock) is no longer in the sub path.

## No HEVC software-encode path

**Severity:** by design.

Plex's software encoder only ever emits h264 / libx264; HEVC encode is
gated behind Plex's "enable HW encode" server setting. The rewriter's
`libx265 → hevc_vaapi` mapping therefore never fires in practice — no
libx265 sessions appear in the 287-entry argv corpus. Not a bug, just a
path that is unreachable given how PMS builds argv.
