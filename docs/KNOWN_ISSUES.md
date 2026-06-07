# Known issues

Tracked limitations as of v1.7.0. None block playback; each has a
documented cause and, where relevant, a path to a fix.

See also: [`TEST_MATRIX.md`](TEST_MATRIX.md) for cells flagged
`[KNOWN: <slug>]` — those cross-reference back here so a known-broken
shape isn't re-validated each release.

## Plex for Apple TV 8.45 · transcoded HEVC crashed (HEVC Rext) — RESOLVED in worker v1.13.0

**Resolved (worker v1.13.0, #189):** the worker was emitting HEVC **Rext** on 10-bit HW encodes (no `-profile` → VAAPI default); `ensureHEVCMain10` now forces `-profile main10` (all HW backends), which Apple VideoToolbox decodes. The history below is the diagnosis.

Pre-v1.13.0, ATV 8.45 (tvOS 26.5/26.6) could not play a **transcoded HEVC** stream. Its
"Enhanced Player" forces HLS transcodes into `container=mkv` (via an
`add-transcode-target(... replace=true)` in `X-Plex-Client-Profile-Extra`),
and the tvOS demuxer rejects a *re-encoded* mkv-in-HLS (copy plays fine — see
the original #122 finding). But the bug is deeper than the container.

Full matrix, real devices, av1 sources (SDR *and* DoVi — not HDR-specific):

| output | result |
|---|---|
| hevc 4K — mkv / mp4 / mpegts | ❌ fail |
| hevc 4K — fMP4 via a clean custom `tvOS.xml` (profile-extra fully stripped) | ❌ **crashes the app** (0 segments fetched, crash at decoder/init) |
| h264 1080p — mpegts (stock `tvOS.xml`) | ✅ plays |

**Root cause (client mpv logs, #189) — it's our encode, not a client limit.**
Plex-for-AppleTV uses an mpv/ffmpeg player (hwdec=videotoolbox). On every 4K
HEVC attempt:

```
[ffmpeg/demuxer] error: mov,mp4: invalid size 0 in stsd / error reading header
[ffmpeg/demuxer] warn:  hls: ... (Video: hevc (Rext), none, 3840x2160): unspecified pixel format
→ app crash ~4s later
```

The worker emits **HEVC Range-Extensions (Rext) with unspecified pixel format**
plus a **malformed fMP4 `hvcC`/`stsd` (size 0)**. A 4:2:0 10-bit source must be
**Main10**; Apple VideoToolbox decodes only Main/Main10, so the Rext stream is
undecodable → mpv hwdec crashes. ATV 8.45 **can** play HEVC Main10. **Fixed in
worker v1.13.0 (#189):** `ensureHEVCMain10` forces `-profile main10` on 10-bit
HW HEVC encodes (hevc_vaapi Intel+AMD, hevc_nvenc) — validated plex-test
(ffprobe `Main 10`) + qa_matrix 60/60 + live Android 4K HEVC. qa_matrix now
guards it (#193/#195).

**Interim workaround (now superseded by v1.13.0):** [`clientfix`](CLIENTFIX.md)
`CLIENTFIX_DECISION_MODE=strip` forced **h264 1080p** to dodge the crash. With
v1.13.0 shipped, clientfix is **bypassed** in prod (ATV gets native HEVC Main10);
pending a real-ATV confirm → full teardown. (Worker SW→HW av1 decode, #185, is a
separate fleet-load item.)

## Burned PGS cue stayed on screen until next cue — RESOLVED in v1.6.1

Was: `vf_inlineass::refresh_bitmap` recomputed `have_bmp` every frame from the
last bitmap's `[bmp_start_ms, bmp_end_ms)` window. PGS cues carry
`end_display_time=0` → `bmp_end_ms=-1`, so the empty-PCS clear (which dropped
`have_bmp` and returned early) was undone the very next frame by the recompute
— the cue resurrected one frame after the clear and hung until the next cue.
Latent in patch `0115` since v1.3.0; only spotted when watched closely.

**Fixed (v1.6.1, patch `0121`):** on clear, set `bmp_end_ms = time_ms` to close
the window so the recompute yields `have_bmp=false` until a new bitmap's
`do_upload` resets it. Live-validated on plex-test (PGS-SDH burn filmstrip
shows the cue clearing at its end).

## 4K HDR + burned PGS sub-realtime (full-frame overlay) — RESOLVED in v1.6.1

Was: in the full-HW passthrough path, scaleplex trusted Plex's own
`sub2video → scale-to-4K → overlay_vaapi` graph (with a leading `[0:0]hwupload`
that re-uploaded the already-VA decoded surface — a decode→sysmem→re-upload
round-trip). At 4K HDR the full-frame overlay + the round-trip ran ~0.37×
realtime → buffering. The existing reroute only matched the SDR shape; the HDR
variant (a `tonemap_opencl` chain spliced between the scaled video and the
overlay) escaped it.

**Fixed (v1.6.1, #37):** `detectBitmapOverlayBurn` extracts the orthogonal
facts regardless of an intervening tonemap, and `composeBurn` re-emits the
canonical VA-resident `scale_vaapi(p010) → [tonemap] → inlineass(render_height)`
graph. Measured **0.37× → 4.6× realtime** end-to-end live.

## Plex for Windows · live HLS-matroska transcode — mpv aborts demux — RESOLVED in v1.7.0

Was: when Plex for Windows desktop negotiated HLS-matroska transcode at
720p or 1080p, mpv (the client's built-in player) aborted demuxing the
matroska stream shortly after the first segment. Source-codec independent
— happened on HEVC, AV1, and h264 sources. Direct streaming the same
content to Plex for Windows played cleanly; the issue was the
scaleplex-produced mkv byte stream. Open since sha-03b2cd0 (2026-05-13).
Affected ~3.9% of prod transcoded traffic (top-5 client per Tautulli).

**Fixed in v1.7.0 (live-validated 2026-05-26):** From Dusk Till Dawn 4K
HEVC HDR → 720p forced transcode plays correctly in Plex Windows, with a
sustained 10-minute run at ~4.9× realtime confirming stability beyond
first-segment. Specific patch attribution requires bisection across the
v1.5.0→v1.7.0 fork-patch stack (0115-0122); top candidates: 0120 paced
self-decode (changes how matroska demux feeds the decoder), 0107
matroskaenc Duration backport, 0122 sched-sinkless-per-output. The
cumulative effect closed the bug; no targeted fix was authored against
the symptom.

## Plex `framedrop` audio bitstream filter — `exit status 8` (FIXED post-v1.7.0)

`[FIXED: FramedropBSF]`

**Severity (pre-fix):** recoverable transient. Affected a subset of
Plex Web DASH sessions with AAC audio + seek (PMS emits `-bsf:1
framedrop=count=N` to drop initial AAC frames for A/V alignment
post-seek). The fork's ffmpeg has no `framedrop` bitstream filter
(Plex-Transcoder-only), ffmpeg failed to open it, exited 8. Plex
re-spawned the session with different argv and playback resumed —
user-visible effect was a brief stutter or silent retry at session
start, not a stuck failure.

Spotted live during the v1.7.0 release-gate sweep (2026-05-26, Plex
Web Chrome + Firefox seek bursts).

**Fix:** rewriter now strips `-bsf:N framedrop=*` from output args
in both the bail path (safety net) and the main path via the new
`dropFramedropBSF` helper — parallel to the existing `*_eae` codec
strip and `-eae_prefix:N` drop in `swapEAEAudioDecoders` /
`dropEAEPrefixFlags`. Emits change tag `drop:-bsf:N(framedrop)` (or
`drop:-bsf:N(framedrop)(bail)` from the bail-path scrub). Preserves
other `-bsf:N` flags (e.g. `dovi_rpu=strip=1`). Tracked in memory
`project_scaleplex_framedrop_bsf_strip.md`.

## Duplicate `video:hdr-source(...)` tag (FIXED post-v1.7.0)

`[FIXED: DupHDRTag]`

**Severity (pre-fix):** cosmetic — log noise only, no behavioral
impact.

The HDR-source detection in `rewriter.go` emitted the same
`video:hdr-source(<transfer>)` tag from up to three branch-local
sites: SW-reshape (line ~2151), HW-decode passthrough (line ~2252),
and the HW-decode-sub-burn text branch (line ~2309). On sessions
that hit both the HW-decode passthrough block AND the
HW-decode-text-sub-burn sub-branch (HEVC/AV1 HDR + text SRT/ASS
burn, ~10% of HDR transcodes), the tag appeared twice in
`res.Changes`. Bitmap-burn path emitted once. argv output and
rewriter behavior were unaffected; only the change-list logging
was noisy.

Spotted during the v1.7.0 release-gate sweep, Android TV + Dusk
Till Dawn with embedded SRT burn.

**Fix:** HDR-source detection hoisted to a single session-level
site immediately after `mediaPath` resolution, before the branch
split. All three downstream branches now read `sourceIsHDR` from
the same fact and do not re-emit the tag. The
HW-decode-sub-text-burn branch also keeps the `facts.hdr`
graph-derived signal as an OR (so a probe that returns "" still
triggers the right tonemap downstream). Regression-guard tests:
`TestRewriter_HDRSource_EmittedExactlyOnce_HWDecodeTextBurnHDR`
and `_SWReshape` in `rewriter_test.go`.

## HW-decode + SW-encode hybrid — 4K HDR corruption

`[KNOWN: HWDecSWEnc4KHDR]`

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
