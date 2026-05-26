# Pre-production test matrix

Coverage needed before swapping `plex/plex` production PMS to scaleplex.
Multi-dimensional — pick combinations that exercise the highest-risk
code paths first. Tick (✓) cells after live validation. Known-broken
or not-applicable cells get an explicit (—) with reason.

## Dimensions

| Dimension | Values |
|---|---|
| **Client** | Plex Web (Chrome) · Plex Web (Safari) · Plex Android · Plex iOS · Plex Windows desktop · LG webOS · PS4/PS5 · Apple TV · Plex Sync/Download |
| **Source video** | HEVC 4K HDR Main10 · HEVC 1080p SDR · H264 1080p · AV1 4K HDR · AV1 1080p · VP9 1080p |
| **Source audio** | TrueHD 7.1 (Atmos) · EAC3 5.1 · AC3 5.1 · AAC stereo · DTS-HD MA · FLAC |
| **Sub** | None · PGS burn-in · SRT sidecar burn-in · ASS sidecar burn-in · Embedded SRT |
| **PMS HEVC transcoding** | enabled · disabled |
| **PMS HW transcoding** | enabled · disabled |
| **PMS HW tonemapping** | enabled · disabled |
| **Job type** | live DASH · live HLS-mpegts · live HLS-matroska · Optimize HW-decode · Optimize remux · Detection · Sync/Download |
| **Action** | initial play · resume · seek-fwd (small) · seek-fwd (large) · seek-back · quality change · audio track switch · sub track switch · pause/resume |

Combinatorial = ~thousands of cells. We pick representative subsets
per dimension, prioritising real-world traffic patterns from
production Plex Web Tautulli history.

## Server-setting matrix (independent of client)

Each scaleplex test should be repeated across these PMS toggles when
the result might differ:

| Setting | Default | Variants we care about |
|---|---|---|
| HEVC transcoding | On | Off (forces H264 output; scaleplex still does libx264→h264_vaapi) |
| HW transcoding | On (VAAPI) | Off (scaleplex `applied=false` bail; original Plex Transcoder argv runs on stock ffmpeg → likely fails visibly. Expected behaviour: degrade to error, not bad output.) |
| HW tonemapping | On (tonemap_vaapi via scaleplex-ffmpeg7) | Off (SW tonemap with tonemap filter chain; scaleplex rewriter doesn't currently substitute) |
| Plex Optimize jobs | On | Off (no impact on live; just no Optimize sessions) |

**Audit note 2026-05-12:** The rewriter doesn't currently substitute
`tonemap_opencl → tonemap_vaapi` (we removed that in a recent edit per
a separate fix). Confirm HW-tonemap-OFF + HDR source still works:
either rewriter bails (`applied=false`) or stock tonemap chain runs.

## Status (2026-05-20, plex-test `sha-b364cb3`)

### Validated this session

| Cell | Status |
|---|---|
| Plex Web Chrome · DASH · HEVC HDR · live · initial play | ✓ |
| Plex Web Chrome · DASH · HEVC HDR · seek | ✓ |
| Plex Android · HLS-mpegts · h264/hevc · live | ✓ |
| Plex Android · HLS-matroska · 4K HEVC HDR + AC3-copy · live · cold start | ✓ |
| Plex Android · HLS-matroska · 4K HEVC HDR + AC3-copy · seek 1:20 | ✓ |
| Plex Android · HLS-matroska · 4K HEVC HDR + PGS burn-in (overlay_vaapi) | ✓ |
| Plex Android · indirect connection (downgraded to 720x404 stereo AAC) | ✓ |
| Plex Optimize · HW-decode shape (hevc_vaapi mp4 + faststart) | ✓ (earlier sessions) |
| Plex Optimize · remux shape (bare decoder + copy) | ✓ (earlier sessions) |
| Detection / ML pre-pass | ✓ (earlier sessions) |
| Sub-burn · SRT sidecar text · Plex Android HLS+seek (Accountant) | ✓ 2026-05-12 |
| Sub-burn · SRT sidecar text · Plex Android cold-start (FMJ) | ✓ 2026-05-12 |
| Sub-burn · SRT sidecar text · Plex Windows direct-play (FMJ) | ✓ 2026-05-12 |
| **LG webOS · HLS-mpegts · Avatar Fire and Ash 4K HDR AV1 + PGS burn (bitmap pre-render) · initial play** | ✓ 2026-05-20 |
| **Plex Android · HLS · Avatar Fire and Ash 4K HDR AV1 + PGS burn · seek/resume** | ✓ 2026-05-20 (sha-b364cb3 with FIFO PTS shift) |
| **Plex Android · HLS · Sing 2 4K HDR AV1 + EAC3 (no subs) · initial + seek + audio track switch** | ✓ 2026-05-20 |
| **Plex Android · HLS · From Dusk Till Dawn HEVC 4K HDR + external SRT burn · initial + seek + audio swap** | ✓ 2026-05-20 |
| **Plex Android · HLS · From Dusk Till Dawn HEVC 4K HDR + external SRT direct (no burn)** | ✓ 2026-05-20 |

> **v1.1 cell completion note (2026-05-20).** A subtle seek-alignment bug
> on the PGS bitmap pre-render path was fixed in `sha-b364cb3`. Plex's
> HW-decode bitmap argv carries `-start_at_zero -copyts`; that flag
> rebases only the muxer-side output PTS to zero, **not** the filter
> input (verified offline against Avatar Fire and Ash with `-ss 540`:
> the source's first frame shows up in `-filter_complex` at
> `pts_time:540.003`). The pre-render
> emits a 0-based FIFO (canvas-driven, sub branch rebased by
> `setpts=PTS-N/TB`), so `overlay_vaapi` was pairing FIFO PTS T with
> main PTS T → cues drifted forward by exactly `seekOff` seconds. Fix:
> the rewriter splices `setpts=PTS+<seekOff>/TB` onto the FIFO branch
> ahead of `format=bgra,hwupload[0]`. See `REWRITER.md` (Bitmap sub burn,
> HW-decode pre-render path) for the full rewrite shape.

### NOT yet validated on `phase4-audit2`

These need a play-through before we can promote:

- ~~Plex Windows desktop · live HLS-matroska · cold start + seek~~ —
  **RESOLVED in v1.7.0** (live-validated 2026-05-26). 720p forced
  transcode of 4K HEVC HDR plays correctly + sustained 10-min run.
  See [`KNOWN_ISSUES.md`](KNOWN_ISSUES.md#plex-for-windows--live-hls-matroska-transcode--mpv-aborts-demux--resolved-in-v170).
- Plex iOS · any path
- Apple TV · any path
- ~~LG webOS · any path~~ — partial: Avatar 4K HEVC HDR + PGS burn initial-play green 2026-05-20 (sha-b364cb3). **Still need: LG webOS seek + LG webOS quality-change.**
- PS4/PS5 · live HLS · any (Witch Hat Atelier AV1 1080p + ASS burn is the planned cell)
- Plex Sync / Download · any
- Quality change mid-playback (audio + video bitrate change) — explicit re-test on PGS sessions for FIFO lifecycle
- ~~Audio track switch mid-playback~~ — closed 2026-05-20 (Sing 2 + Dusk Till Dawn on Android)
- Subtitle track switch mid-playback — burning-vs-rendering swap not yet validated
- Pause + resume + long-idle (5+ min) · validate worker doesn't stall

### Server-side toggle coverage (anything tested today)

| Setting | State during today's tests | Untested variants |
|---|---|---|
| HEVC transcoding | On | Off |
| HW transcoding | On (VAAPI all sessions) | Off (expected to bail) |
| HW tonemapping | On (tonemap_vaapi when HDR→SDR; today was HDR→HDR so unexercised) | Off |
| Optimize | On (validated earlier sessions) | n/a |

### Source-codec coverage today

| Source | Tested? |
|---|---|
| HEVC 4K HDR Main10 | ✓ (BH6) |
| H264 | ✓ historically |
| AV1 | ✓ historically (corpus a1) |
| VP9 | ? — schedule test |
| HEVC SDR 1080p | ? — schedule test |

### Audio coverage today

| Source → output | Tested? |
|---|---|
| AC3 5.1 → copy | ✓ (BH6 direct, audio:1 = AC3 backup track) |
| TrueHD 7.1 → copy | (✗ — Plex Android client decoder init fails; documented as client limit) |
| EAC3 5.1 → copy | (TODO) |
| TrueHD → eac3_eae (rewriter swap to native eac3) | (✓ historically; needed today on direct connection?) |
| AAC stereo → copy | (TODO) |
| FLAC → ac3 / eac3 | (TODO) |
| Multi-track audio switch mid-stream | (TODO — high risk) |

## Suggested next-up cells

Highest-risk / highest-value to validate next:

1. **Plex Web seek-backward** at 4K HEVC HDR — never confirmed
2. **Audio track switch** mid-playback (real-life multi-language anime)
3. **Subtitle track switch** mid-playback
4. **Quality change** mid-playback (Plex's `quality` slider)
5. **Plex Sync / Download** — completely untested on scaleplex
6. **HW transcoding disabled** — bail path correctness
7. **HW tonemap disabled** — does HDR→SDR fall back gracefully?
8. **HEVC transcoding disabled** — H264-out path under load

## Promotion criteria

scaleplex graduates from a test PMS instance to a production one when:

- All `Validated this session` rows above stay green after one
  rebuild + redeploy cycle
- Every cell in `Suggested next-up` ticked or has a documented
  workaround
- Server-toggle variants covered for: HEVC transcoding off, HW
  tonemap off (the two most likely real-user toggles)
- 24-hour stability run on test PMS with no error logs / no manual
  pod restarts needed
- **Code-side gate: scaleplex-ffmpeg7 port of Plex's `inlineass`
  libavfilter complete** — drops ~80 LOC of rewriter sidecar/PTS-
  shift complexity AND closes the embedded-ASS-subtitle gap.
  Anime + foreign-language content with embedded ASS karaoke
  currently triggers a rewriter bail; family-facing rollout
  needs this filled in. Tracked in
  `project_scaleplex_inlineass_port.md` (auto-memory).

## Release gate

The cells below are the **must-pass live sweep** before tagging a new
`vX.Y.Z` release. Full procedure (T1 unit + T2 replay + T3 qa_matrix
+ T4 live + T5 debug-build) is in [`RELEASE_GATE.md`](RELEASE_GATE.md);
the live sweep is what's enumerated here. Add a new section per release
(append-only history).

### v1.7.0 — 2026-05-26 — **11/11 PASS** ✅

Live-validated against worker image `sha-c53f4b4` on plex-test
(SCALEPLEX_FORCE_HW=0). 11-cell live sweep on Frank's physical fleet
(~89% of prod transcoded traffic head-on; remaining ~11% covered by
corpus replay via T2). Item: From Dusk Till Dawn (rk=325708) HEVC 4K HDR,
Avatar Fire and Ash (rk=325689) AV1 4K HDR10+DV P7 FEL, Witch Hat Atelier
S1E1 (rk=325342) AV1 1080p SDR ASS, Chained Soldier S1E1 (rk=311314)
AV1 1080p SDR animated ASS.

| # | Client | Path | Source | Action | Status |
|---|---|---|---|---|---|
| 1 | Plex Web Chrome | DASH | 4K HEVC HDR + EAC3 | play + seek | ✓ |
| 2 | Plex Web Chrome | DASH | 4K HEVC HDR + embedded PGS burn | play + seek | ✓ |
| 3 | Plex Web Firefox | DASH | 4K HEVC HDR + side-channel SRT, then PGS burn | play + seek | ✓ |
| 4 | Plex for Android (TV) | HLS-matroska | 4K HEVC HDR + embedded SRT (+ sidecar SRT + PGS burn variants) | play + seek | ✓ thorough |
| 5 | Plex for Android (TV) | HLS-matroska | 4K AV1 HDR10 + DV P7 FEL + PGS burn | play + seek + audio swap | ✓ |
| 6 | Plex for Android (TV) | HLS-matroska | AV1 1080p SDR + animated ASS burn | play | ✓ |
| 7 | Plex for LG (webOS) | HLS-mpegts | 4K HEVC HDR + PGS burn | play + seek + quality change | ✓ |
| 8 | PS4 | HLS-mpegts | AV1 1080p SDR + ASS burn | play + seek | ✓ |
| 9 | Plex for Android (Mobile) | HLS | AV1 1080p SDR + ASS burn | play | ✓ |
| 10 | Any | Plex Optimize | HW-decode + remux shapes | offline | ✓ by T2 |
| 11 | Plex for Windows | live HLS-matroska | 4K HEVC HDR → 720p transcode | play + seek + sustained 10-min run | **✓ KNOWN: PWin720p FIXED** |

#### Notable findings live-validated

- **`KNOWN: PWin720p`** — open since 2026-05-13 (sha-03b2cd0 era) —
  **closed**. Cell 11 plays cleanly + sustained 10-min run at ~4.9×
  realtime. Cumulative fix across the v1.5.0→v1.7.0 fork-patch stack;
  no targeted symptom fix.
- **PR #41 sidecar-sink-keep validated live.** Embedded vs sidecar SRT
  bindings correctly differentiated: `drop:inlineass-decode-sink`
  PRESENT on embedded, ABSENT on sidecar (`-i temp-0.srt` shape).
- **v1.6.1 bitmap-burn unification (PR #37)** validated across 4
  client surfaces (Chrome, Firefox, Android TV, LG webOS) — all emit
  `hw-decode:filter:bitmap-inlineass-vaapi` for PGS.
- **EAE audio swap path (`audio:eac3_eae->eac3`)** validated for the
  first time live — Plex Web DASH always re-encodes audio via EAE
  binding; rewriter maps to native eac3.
- **AV1 HW decode + heterogeneous encode** validated: Android
  TV/Mobile→`hevc_vaapi`, PS4→`h264_vaapi` (client-profile-correct).

#### New bugs spotted (deferred to post-tag)

- **`framedrop` BSF not stripped** — exit status 8 on certain DASH
  AAC+seek sessions. Recoverable (Plex retries with different argv).
  See `KNOWN_ISSUES.md#plex-framedrop-audio-bitstream-filter-not-stripped--exit-status-8`.
- **Duplicate `video:hdr-source(...)` tag** on text-burn HW-decode
  path. Cosmetic, log-noise only.

Tag references in this table resolve to the `Tag*` / `TagPrefix*`
constants in [`worker/agent/rewriter_tags.go`](../worker/agent/rewriter_tags.go).
Live verification per cell:
`kubectl -n <ns> logs -l app.kubernetes.io/controller=worker --since=2m
| grep 'rewriter applied:'`. The
[`CLIENT_TEST_MATRIX.md`](CLIENT_TEST_MATRIX.md) "Worker-side PASS
verification" + "Failure capture" sections cover the per-cell mechanics.

Wallclock: ~75 min actual (more thorough variant coverage than the
35-min minimum spec — e.g. cell 4 walked SRT internal + sidecar + PGS
variants on Android TV).
