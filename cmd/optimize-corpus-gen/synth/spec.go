// Package synth generates the synthetic source-clip matrix that the
// Optimize corpus runs against. Each clip is 5-10s of black video at a
// specific codec/profile/transfer/audio/sub shape — Plex Optimize's
// argv depends entirely on probed source metadata, not pixel content,
// so the clip can be tiny.
//
// Filenames encode the matrix axes ("av1__main10__2160p__hdr10__eac3-7.1__nosub.mkv")
// — Plex's library item title comes from the filename and survives
// across runs/scans, so the cell ID derived from the spec stays stable
// for diffing across PMS upgrades and rewriter changes.
//
// What synth can NOT produce from scratch (and is therefore not in the
// matrix yet): Dolby Vision RPU metadata, PGS bitmap subs, TrueHD
// Atmos object beds. Those need to be borrowed from real source files
// via `-ss 0 -t 5 -c copy` snippets — a future BorrowFrom mode left
// to a follow-up.
package synth

import (
	"fmt"
	"strings"
)

// Spec describes one synthetic source clip — one cell of the source
// axis of the corpus matrix.
type Spec struct {
	// Video shape.
	VideoCodec    string // "h264" | "hevc" | "av1"
	VideoProfile  string // "high" | "main" | "main10"
	BitDepth      int    // 8 | 10
	Transfer      string // "bt709" (SDR) | "smpte2084" (HDR10) | "arib-std-b67" (HLG)
	Width, Height int    // 1280x720 | 1920x1080 | 3840x2160
	FrameRate     int    // 24 | 25 | 30 (kept low so encode is fast)
	DurationSec   int    // 5..10

	// Audio shape (one stream per clip — multi-track is a Phase-3 axis).
	AudioCodec    string // "aac" | "ac3" | "eac3"
	AudioChannels int    // 2 | 6 | 8
	AudioLayout   string // "stereo" | "5.1" | "7.1"

	// Subtitle shape ("" = none, "srt" or "ass" mux an embedded text stream).
	SubKind string // "" | "srt" | "ass"
}

// Filename builds the canonical filename for this spec. The chosen
// separator (`__`) is one Plex won't mangle (no whitespace, no chars
// it strips from titles) and is greppable.
func (s Spec) Filename() string {
	parts := []string{
		s.VideoCodec,
		s.VideoProfile,
		s.resolutionTag(),
		fmt.Sprintf("%dbit", s.BitDepth),
		s.hdrTag(),
		fmt.Sprintf("%s-%s", s.AudioCodec, s.AudioLayout),
		s.subTag(),
	}
	return strings.Join(parts, "__") + ".mkv"
}

// Title is the Plex display title — same as Filename without .mkv.
// Used to look up the synthesized item via Plex search.
func (s Spec) Title() string {
	return strings.TrimSuffix(s.Filename(), ".mkv")
}

func (s Spec) resolutionTag() string {
	switch s.Height {
	case 480, 540:
		return "sd"
	case 720:
		return "720p"
	case 1080:
		return "1080p"
	case 1440:
		return "1440p"
	case 2160:
		return "2160p"
	default:
		return fmt.Sprintf("%dp", s.Height)
	}
}

func (s Spec) hdrTag() string {
	switch s.Transfer {
	case "smpte2084":
		return "hdr10"
	case "arib-std-b67":
		return "hlg"
	default:
		return "sdr"
	}
}

func (s Spec) subTag() string {
	switch s.SubKind {
	case "srt":
		return "sub-srt"
	case "ass":
		return "sub-ass"
	default:
		return "nosub"
	}
}

// DefaultMatrix returns the curated Phase-2 source list — 20
// representative cells covering the codec/profile/bit-depth × transfer
// × audio × sub combinations Plex Optimize's decision tree actually
// branches on. Not a full cartesian (which would be ~120 clips); the
// extra cells we'd add are uniform Optimize behavior already covered
// by the listed ones, so they'd contribute zero argv variety to the
// corpus.
//
// Notable omissions documented in the package doc:
//   - Dolby Vision (needs real RPU)
//   - PGS bitmap subs (needs real .sup)
//   - TrueHD / DTS-HD MA (ffmpeg encoders flagged experimental / sub-RT)
//
// Frame rate stays 24 across the board — encode time is dominated by
// initialization, not the 5s payload.
func DefaultMatrix() []Spec {
	common := func(width, height, fr, dur int) Spec {
		return Spec{Width: width, Height: height, FrameRate: fr, DurationSec: dur}
	}

	mk := func(s Spec, vc, vp string, bd int, tr, ac, al string, ch int, sub string) Spec {
		s.VideoCodec, s.VideoProfile, s.BitDepth, s.Transfer = vc, vp, bd, tr
		s.AudioCodec, s.AudioLayout, s.AudioChannels = ac, al, ch
		s.SubKind = sub
		return s
	}

	hd := func() Spec { return common(1920, 1080, 24, 5) }
	hd720 := func() Spec { return common(1280, 720, 24, 5) }
	uhd := func() Spec { return common(3840, 2160, 24, 5) }

	return []Spec{
		// 1-3: h264 — most common live-playback shape, baseline against
		// hevc/av1 alternatives.
		mk(hd(), "h264", "high", 8, "bt709", "aac", "stereo", 2, ""),
		mk(hd(), "h264", "high", 8, "bt709", "ac3", "5.1", 6, ""),
		mk(hd720(), "h264", "high", 8, "bt709", "aac", "stereo", 2, ""),

		// 4-9: hevc SDR/HDR/HLG sweep. Optimize HEVC pref keys on these.
		mk(hd(), "hevc", "main", 8, "bt709", "eac3", "5.1", 6, ""),
		mk(hd(), "hevc", "main10", 10, "bt709", "eac3", "5.1", 6, ""),
		mk(uhd(), "hevc", "main10", 10, "bt709", "eac3", "5.1", 6, ""),
		mk(uhd(), "hevc", "main10", 10, "smpte2084", "eac3", "5.1", 6, ""),  // HDR10 4K
		mk(hd(), "hevc", "main10", 10, "smpte2084", "eac3", "5.1", 6, ""),   // HDR10 1080p
		mk(uhd(), "hevc", "main10", 10, "arib-std-b67", "eac3", "5.1", 6, ""), // HLG 4K

		// 10-12: av1 — increasingly common via Tdarr re-encode.
		mk(hd(), "av1", "main", 8, "bt709", "eac3", "5.1", 6, ""),
		mk(hd(), "av1", "main10", 10, "bt709", "eac3", "5.1", 6, ""),
		mk(uhd(), "av1", "main10", 10, "smpte2084", "eac3", "5.1", 6, ""),

		// 13-16: embedded subtitle variants. Sub-burn is a major Optimize
		// branch on the rewriter side.
		mk(hd(), "h264", "high", 8, "bt709", "aac", "stereo", 2, "srt"),
		mk(hd(), "h264", "high", 8, "bt709", "aac", "stereo", 2, "ass"),
		mk(uhd(), "hevc", "main10", 10, "smpte2084", "eac3", "5.1", 6, "srt"),
		mk(hd(), "hevc", "main10", 10, "bt709", "eac3", "5.1", 6, "ass"),

		// 17-18: 7.1 channel mix-down — relevant for Mobile target
		// downmix-to-stereo decisions. ffmpeg's eac3 + ac3 encoders cap
		// at 5.1, and the truehd encoder is flagged experimental + sub-
		// realtime, so 7.1 cells use aac (which natively supports 7.1).
		// Apple/iOS-sourced 7.1 content really is aac-7.1, so the shape
		// is representative.
		mk(hd(), "h264", "high", 8, "bt709", "aac", "7.1", 8, ""),
		mk(uhd(), "hevc", "main10", 10, "smpte2084", "aac", "7.1", 8, ""),

		// 19: av1 + embedded SRT — av1 sources are increasingly present
		// in Tdarr-rebuilt libraries and the SRT+HDR combo exercises
		// the rewriter's HW-decode-text path on a non-hevc codec.
		mk(uhd(), "av1", "main10", 10, "smpte2084", "eac3", "5.1", 6, "srt"),
	}
}
