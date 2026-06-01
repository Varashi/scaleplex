package main

import (
	"fmt"
	"strings"
)

// Shape is one point in the synthesis axis matrix. Each field is an
// independent axis; matrix.go takes the Cartesian product.
//
// Every Shape is a force-burn HW-decode + GPU-encode sub-burn graph
// (inlineass= filter paired with a *_vaapi video encoder) — the
// `hw-subburn-transcode` class the rewriter MUST reshape and #147's
// bail-allowlist guards. That class is *absent* from the organic corpus
// (Frank rarely force-burns; the `:#0xNN` + dash combo never got
// captured — see #150), so it has no CI fixture without synthesis.
type Shape struct {
	StreamSpec  string // "ordinal" (-codec:0) | "hex" (-codec:#0x01, the high-PID m2ts form #144 hit)
	DecodeCodec string // "av1" | "hevc"  — input video codec PMS decodes
	EncodeCodec string // "hevc_vaapi" | "h264_vaapi" — output video encoder PMS targets
}

func (s Shape) sig() string {
	return fmt.Sprintf("%s__%s__%s__dash-extsrt", s.DecodeCodec, s.EncodeCodec, s.StreamSpec)
}

// hexVideoSpecPrefixes are the decode-side (input) option names whose
// per-stream selector PMS renders as `:#0xNN` instead of the ordinal
// `:N` when the source stream carries a high program-ID (m2ts, Plex
// Versions / Optimized-for-TV). Only the *video* decode specs flip;
// audio (`-codec:1 eac3_eae`) stays ordinal, matching the captured
// `:#0x01` shape that triggered #144.
var hexVideoSpecPrefixes = []string{
	"-codec:",
	"-hwaccel:",
	"-hwaccel_output_format:",
	"-hwaccel_device:",
}

// build applies the Shape's axis transforms to the embedded base argv
// (av1 / hevc_vaapi / ordinal / external-srt / dash) and returns the
// synthesized Cell. Transforms are surgical token rewrites — the base
// is a real sanitized capture, so the result stays faithful to what PMS
// actually emits for this class. Returns an error if the base doesn't
// contain a token a transform expects (guards against silent drift if
// the template is regenerated).
func build(base Cell, s Shape) (Cell, error) {
	argv := append([]string(nil), base.Argv...)

	iIdx := indexOf(argv, "-i")
	if iIdx < 0 {
		return Cell{}, fmt.Errorf("base argv has no -i")
	}

	// --- decode codec (first -codec value, before -i) ---
	if s.DecodeCodec != "av1" {
		dIdx := firstCodecValueIdx(argv, iIdx)
		if dIdx < 0 {
			return Cell{}, fmt.Errorf("base argv: no decode -codec before -i")
		}
		if argv[dIdx] != "av1" {
			return Cell{}, fmt.Errorf("base decode codec is %q, expected av1 (template drift)", argv[dIdx])
		}
		argv[dIdx] = s.DecodeCodec
	}

	// --- encode codec (output *_vaapi encoder, after -i) ---
	if s.EncodeCodec != "hevc_vaapi" {
		eIdx := tokenIdxAfter(argv, iIdx, "hevc_vaapi")
		if eIdx < 0 {
			return Cell{}, fmt.Errorf("base argv: no hevc_vaapi encoder after -i")
		}
		argv[eIdx] = s.EncodeCodec
	}

	// --- stream-spec ordinal -> hex (decode-side video specs only) ---
	if s.StreamSpec == "hex" {
		flipped := 0
		for i := 0; i < iIdx; i++ {
			for _, p := range hexVideoSpecPrefixes {
				if argv[i] == p+"0" {
					argv[i] = p + "#0x01"
					flipped++
					break
				}
			}
		}
		if flipped == 0 {
			return Cell{}, fmt.Errorf("base argv: no decode-side :0 specs to hex-flip (template drift)")
		}
	}

	sig := s.sig()
	return Cell{
		SessionID:       "synth__" + sig,
		CaptureSource:   "synthesized",
		Argv:            argv,
		Env:             map[string]string{},
		SourcePath:      base.SourcePath,
		HasMapInlineass: true,
		Synthesized:     true,
	}, nil
}

func indexOf(argv []string, tok string) int {
	for i, a := range argv {
		if a == tok {
			return i
		}
	}
	return -1
}

// firstCodecValueIdx returns the index of the value of the first
// `-codec:<spec>` option appearing before limit (the -i index). The
// spec can be ordinal (`-codec:0`) or hex (`-codec:#0x01`).
func firstCodecValueIdx(argv []string, limit int) int {
	for i := 0; i < limit-1; i++ {
		if strings.HasPrefix(argv[i], "-codec:") {
			return i + 1
		}
	}
	return -1
}

func tokenIdxAfter(argv []string, after int, tok string) int {
	for i := after + 1; i < len(argv); i++ {
		if argv[i] == tok {
			return i
		}
	}
	return -1
}
