// Package source resolves authoritative source-media metadata from
// the actual file on disk via ffprobe — NOT from Plex's stored library
// metadata, which goes stale after every Tdarr transcode. The argv
// shape Plex emits is keyed off the real codec/profile/transfer/audio
// layout, so the corpus tag has to come from ffprobe to be useful.
package source

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Profile is the subset of ffprobe output the generator records as
// "what was this file, really" — written into the capture's sidecar
// metadata so the corpus can be sliced by source shape.
type Profile struct {
	Path string `json:"path"`

	// Container: matroska,webm / mov,mp4,m4a,3gp,… / mpegts / etc.
	Format string `json:"format"`

	// First video stream — what Plex's `-codec:0` will reference.
	VideoCodec    string `json:"video_codec"`
	VideoProfile  string `json:"video_profile"`
	VideoLevel    int    `json:"video_level"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	PixFmt        string `json:"pix_fmt"`
	ColorTransfer string `json:"color_transfer"`  // smpte2084=HDR10, arib-std-b67=HLG, bt709=SDR
	ColorPrimaries string `json:"color_primaries"`
	ColorSpace    string `json:"color_space"`
	BitDepth      int    `json:"bit_depth"`
	HDRFormat     string `json:"hdr_format"` // "HDR10" | "HLG" | "DV" | "SDR"

	// Audio streams in source order. The Optimize argv often re-encodes
	// only some of these, so the multi-track shape matters.
	Audio []AudioStream `json:"audio"`

	// Subtitle streams (codec_name + language + disposition forced/default).
	// PMS picks based on the client's burn-in selection but the *presence*
	// of certain sub kinds (PGS, ASS) gates rewriter branches.
	Subs []SubStream `json:"subs"`
}

type AudioStream struct {
	Index       int    `json:"index"`
	Codec       string `json:"codec"`
	Profile     string `json:"profile"`
	Channels    int    `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	SampleRate  int    `json:"sample_rate"`
	Language    string `json:"language"`
}

type SubStream struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language"`
	Forced   bool   `json:"forced"`
	Default  bool   `json:"default"`
}

// Prober runs ffprobe against a path. The default (NewProber("", nil))
// shells out to local `ffprobe`; configure invoker + pathTranslate to
// route through a different binary or remote command (e.g. `kubectl
// exec -n media-toolkit <pod> -- ffprobe`) for hosts that don't have
// the media share mounted locally.
type Prober struct {
	// invoker is the argv prefix that wraps ffprobe — e.g.
	// ["kubectl","exec","-n","media-toolkit","<pod>","--","ffprobe"]. When
	// empty we exec local `ffprobe` directly. The ffprobe args
	// (`-v error -print_format json …`) are appended.
	invoker []string

	// pathTranslate rewrites the source path before it's passed to
	// ffprobe. Each rule is (fromPrefix, toPrefix); paths starting with
	// fromPrefix have it replaced with toPrefix. Useful when ffprobe runs
	// in an environment with a different mount layout than the source
	// path (PMS sees /media/..., toolkit pod sees /mnt/media/media/...).
	pathTranslate [][2]string
}

// NewProber builds a Prober from a CLI-friendly invoker string + path
// rules. invokerCmd is split on whitespace (no shell escaping — keep it
// simple: pod names, namespaces, ffprobe binary name don't contain
// spaces). Translate rules come as "FROM=TO" strings.
func NewProber(invokerCmd string, translate []string) (*Prober, error) {
	p := &Prober{}
	if invokerCmd != "" {
		for _, tok := range strings.Fields(invokerCmd) {
			p.invoker = append(p.invoker, tok)
		}
	}
	for _, r := range translate {
		eq := strings.Index(r, "=")
		if eq < 1 || eq >= len(r)-1 {
			return nil, fmt.Errorf("translate rule %q: expected FROM=TO", r)
		}
		p.pathTranslate = append(p.pathTranslate, [2]string{r[:eq], r[eq+1:]})
	}
	return p, nil
}

// translate applies pathTranslate rules. First matching rule wins;
// unmatched paths pass through unchanged.
func (p *Prober) translate(path string) string {
	for _, r := range p.pathTranslate {
		if strings.HasPrefix(path, r[0]) {
			return r[1] + path[len(r[0]):]
		}
	}
	return path
}

// Probe runs ffprobe against the file at `path` (after pathTranslate)
// and returns the parsed Profile. Returns an error if ffprobe fails or
// the file has no video stream (Plex Optimize is video-targeted).
func (p *Prober) Probe(path string) (*Profile, error) {
	probePath := p.translate(path)
	probeArgs := []string{
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		probePath,
	}
	var cmd *exec.Cmd
	if len(p.invoker) > 0 {
		args := append(append([]string{}, p.invoker[1:]...), probeArgs...)
		cmd = exec.Command(p.invoker[0], args...)
	} else {
		cmd = exec.Command("ffprobe", probeArgs...)
	}
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return nil, fmt.Errorf("ffprobe %s (translated=%s): %w — stderr=%s", path, probePath, err, stderr)
	}

	var raw struct {
		Format struct {
			FormatName string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			Index          int    `json:"index"`
			CodecType      string `json:"codec_type"`
			CodecName      string `json:"codec_name"`
			Profile        string `json:"profile"`
			Level          int    `json:"level"`
			Width          int    `json:"width"`
			Height         int    `json:"height"`
			PixFmt         string `json:"pix_fmt"`
			ColorTransfer  string `json:"color_transfer"`
			ColorPrimaries string `json:"color_primaries"`
			ColorSpace     string `json:"color_space"`
			BitsPerRawSample string `json:"bits_per_raw_sample"`
			Channels       int    `json:"channels"`
			ChannelLayout  string `json:"channel_layout"`
			SampleRate     string `json:"sample_rate"`
			Tags           struct {
				Language string `json:"language"`
			} `json:"tags"`
			Disposition struct {
				Default int `json:"default"`
				Forced  int `json:"forced"`
			} `json:"disposition"`
			SideDataList []struct {
				SideDataType string `json:"side_data_type"`
			} `json:"side_data_list"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decode ffprobe %s: %w", path, err)
	}

	prof := &Profile{Path: path, Format: raw.Format.FormatName}
	videoSeen := false
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if videoSeen {
				continue // Plex Optimize keys off the first video stream
			}
			videoSeen = true
			prof.VideoCodec = s.CodecName
			prof.VideoProfile = s.Profile
			prof.VideoLevel = s.Level
			prof.Width = s.Width
			prof.Height = s.Height
			prof.PixFmt = s.PixFmt
			prof.ColorTransfer = s.ColorTransfer
			prof.ColorPrimaries = s.ColorPrimaries
			prof.ColorSpace = s.ColorSpace
			prof.BitDepth = bitDepth(s.PixFmt, s.BitsPerRawSample)
			prof.HDRFormat = hdrFormat(s.ColorTransfer, s.SideDataList)
		case "audio":
			sr := 0
			fmt.Sscanf(s.SampleRate, "%d", &sr)
			prof.Audio = append(prof.Audio, AudioStream{
				Index: s.Index, Codec: s.CodecName, Profile: s.Profile,
				Channels: s.Channels, ChannelLayout: s.ChannelLayout,
				SampleRate: sr, Language: s.Tags.Language,
			})
		case "subtitle":
			prof.Subs = append(prof.Subs, SubStream{
				Index: s.Index, Codec: s.CodecName, Language: s.Tags.Language,
				Forced: s.Disposition.Forced == 1, Default: s.Disposition.Default == 1,
			})
		}
	}
	if !videoSeen {
		return nil, fmt.Errorf("ffprobe %s: no video stream", path)
	}
	return prof, nil
}

// bitDepth derives the source's bit depth from its pix_fmt — ffprobe's
// bits_per_raw_sample field is unreliable on some codecs. 10-bit
// pix_fmts contain "10" (yuv420p10le, p010le, …).
func bitDepth(pixFmt, bitsPerRawSample string) int {
	switch {
	case pixFmt == "":
		return 0
	case contains(pixFmt, "12"):
		return 12
	case contains(pixFmt, "10"):
		return 10
	default:
		return 8
	}
}

// hdrFormat classifies the source's HDR variant from color_transfer +
// side-data list. DV is detected via DOVI side-data; HDR10 via
// smpte2084 (PQ); HLG via arib-std-b67. Anything else is SDR.
func hdrFormat(transfer string, sideData []struct{ SideDataType string `json:"side_data_type"` }) string {
	for _, sd := range sideData {
		if sd.SideDataType == "DOVI configuration record" {
			return "DV"
		}
	}
	switch transfer {
	case "smpte2084":
		return "HDR10"
	case "arib-std-b67":
		return "HLG"
	}
	return "SDR"
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
