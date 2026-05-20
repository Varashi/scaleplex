# Known issues

Tracked limitations as of v1.2.1. None block playback; each has a
documented cause and, where relevant, a path to a fix.

## SRT sub-burn pre-render bails to wide band on positional cues (planned v1.2.2)

**Severity:** perf — not correctness.

**Status.** v1.2.1 shipped a tight bottom band for plain sidecar SRT (~37 %
smaller canvas at 4K for 1-2 line cues). This issue tracks the remaining
case: SRTs that contain even a single positional override (`\anN` with
N>3, `\pos(x,y)`, `\move(...)`) fall back to the static 40 % band — common
in hearing-impaired (`.en.hi.srt`) flavours that use `{\an8}` for sign
translations and `[SOUND]` cues at the top of frame.

**Cause.** `overlay_vaapi` locks its overlay-input dimensions at filter
graph init, so the pre-render band has to be sized for the worst case in
the session. One top-anchored cue forces the band to cover top + bottom
→ effectively the full canvas → same CPU as v1.2.0's static fallback.

**Path to a fix.** Multi-region pre-render. Bucket each cue by anchor
region (bottom / middle / top, plus quantised `\pos` y), emit one
pre-render per non-empty region with a tight band sized to its own max
line count, and chain `overlay_vaapi` instances per region in the main
filter graph. Total canvas CPU stays well below the current bail-to-full
behaviour even for HI-flavour SRTs. Tracked for v1.2.2.

## No HEVC software-encode path

**Severity:** by design.

Plex's software encoder only ever emits h264 / libx264; HEVC encode is
gated behind Plex's "enable HW encode" server setting. The rewriter's
`libx265 → hevc_vaapi` mapping therefore never fires in practice — no
libx265 sessions appear in the 287-entry argv corpus. Not a bug, just a
path that is unreachable given how PMS builds argv.
