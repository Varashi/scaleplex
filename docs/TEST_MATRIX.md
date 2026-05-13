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

## Status (2026-05-12)

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

### NOT yet validated on `phase4-audit2`

These need a play-through before we can promote:

- Plex Windows desktop · live HLS-matroska · cold start + seek
  **PARTIAL 2026-05-13:** direct-play works (sha-03b2cd0 era),
  but TRANSCODE at 720p/1080p **fails — mpv aborts demux**.
  Tracked in `project_scaleplex_plex_windows_720p_gap.md` (memory).
  Need prod vs scaleplex EBML diff to find the matroska byte-stream
  delta.
- Plex iOS · any path
- Apple TV · any path
- LG webOS · any path (the original bug-class hardware)
- PS4/PS5 · live HLS · any
- Plex Sync / Download · any
- Quality change mid-playback (audio + video bitrate change)
- Audio track switch mid-playback
- Subtitle track switch mid-playback
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

scaleplex graduates from `clusterplex-pms` (test) to `plex/plex`
(production) when:

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
