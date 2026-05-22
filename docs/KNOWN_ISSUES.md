# Known issues

Tracked limitations as of v1.2.1. None block playback; each has a
documented cause and, where relevant, a path to a fix.

## SRT sub-burn pre-render bails to wide band on positional cues

**Severity:** perf — not correctness.

**Status.** v1.2.1 shipped a tight bottom band for plain sidecar SRT; v1.2.2
extended it to **embedded SRT too** (band resolved agent-side after extraction
— sidecar and embedded now have parity). This issue tracks the remaining
case: SRTs with even a single positional override (`\anN` with N>3,
`\pos(x,y)`, `\move(...)`) fall back to the static 40 % band — common in
hearing-impaired (`.en.hi.srt`) flavours that use `{\an8}` for sign
translations and `[SOUND]` cues at the top of frame.

**Cause.** `overlay_vaapi` locks its overlay-input dimensions at filter
graph init, so the pre-render band has to be sized for the worst case in
the session. One top-anchored cue forces the band to cover top + bottom
→ effectively the full canvas → same CPU as v1.2.0's static fallback.

**Path to a fix — multi-region pre-render: ABANDONED.** Bucketing cues by
anchor region (one tight pre-render + chained `overlay_vaapi` per region)
was built and canaried (v1.2.2-dev) but **refuted by benchmark**: real
`.hi.srt` flavours strip `\an` so the hit-rate was ~0, and where it did fire
at 4K it cost +69 % CPU/video-sec vs the plain v1.2.1 band. Not worth it.
Positional-cue SRTs stay on the wide-band fallback. The render-resolution
knob (`SCALEPLEX_SUB_RENDER_HEIGHT`, default 1080) blunts the cost: even the
full-canvas fallback renders at the capped resolution.

## Static ASS/SSA sidecars render full-frame (no tight band)

**Severity:** perf — not correctness.

**Status.** Static (non-animated) ASS/SSA burn-ins render the full frame
rather than a tight bottom band — they pay full-canvas libass + qtrle +
overlay area, so a static-ASS session sits below the ~5-stream SRT/PGS
ceiling. SRT (sidecar+embedded) and PGS get tight/optimised bands; ASS does
not yet.

**Cause.** ASS cues can be positioned anywhere, so the rewriter can't assume
a bottom band without parsing the script. There is no ASS/SSA cue parser
feeding the agent-side band resolver yet (SRT has one).

**Path to a fix.** Port an ASS/SSA cue parser (alignment/`\an`, `\pos`,
`\move`, margins, PlayResX/Y) into the agent-side resolve path so static ASS
gets a tight band like SRT, bailing to full-frame on any positional/animated
cue. Animated ASS keeps the per-frame `inlineass` path regardless.

## No HEVC software-encode path

**Severity:** by design.

Plex's software encoder only ever emits h264 / libx264; HEVC encode is
gated behind Plex's "enable HW encode" server setting. The rewriter's
`libx265 → hevc_vaapi` mapping therefore never fires in practice — no
libx265 sessions appear in the 287-entry argv corpus. Not a bug, just a
path that is unreachable given how PMS builds argv.
