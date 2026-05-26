package synth

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Builder synthesizes a Spec into an .mkv file at <outDir>/<filename>.
// outDir is the path *in the environment ffmpeg runs* (local OR
// inside the kubectl-exec'd container).
//
// Like source.Prober, the Builder supports a wrapper invoker so it can
// shell out to a containerized ffmpeg that has the NFS share mounted —
// the homelab workstation doesn't.
type Builder struct {
	// invoker prefix that wraps the ffmpeg invocation. Empty = run
	// local `ffmpeg`. Example for the homelab:
	//   ["kubectl","exec","-n","media-toolkit","<pod>","--","ffmpeg"]
	invoker []string

	// outDir is the directory ffmpeg writes to (in its own environment).
	outDir string
}

// NewBuilder constructs a Builder. invokerCmd is split on whitespace
// (same convention as source.Prober). outDir must already exist in
// the ffmpeg-side filesystem.
func NewBuilder(invokerCmd, outDir string) *Builder {
	b := &Builder{outDir: strings.TrimRight(outDir, "/")}
	if invokerCmd != "" {
		b.invoker = strings.Fields(invokerCmd)
	}
	return b
}

// Build synthesizes one Spec. Returns the absolute output path (in
// ffmpeg's environment). Returns an error if ffmpeg fails. Caller is
// responsible for idempotency checks (skip when the file already
// exists and matches the spec).
func (b *Builder) Build(s Spec) (string, error) {
	outPath := b.outDir + "/" + s.Filename()
	args := b.ffmpegArgs(s, outPath)
	var cmd *exec.Cmd
	if len(b.invoker) > 0 {
		full := append(append([]string{}, b.invoker[1:]...), args...)
		cmd = exec.Command(b.invoker[0], full...)
	} else {
		cmd = exec.Command("ffmpeg", args...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ffmpeg %s: %w — output:\n%s", s.Filename(), err, tailLines(string(out), 30))
	}
	return outPath, nil
}

// ffmpegArgs builds the encoder command for a Spec. The construction
// is deliberately verbose — one branch per codec — because each
// encoder needs different flag families (libx265 wants -x265-params,
// libsvtav1 wants -svtav1-params, etc.) and trying to share a single
// builder ends up branchier than the per-codec version.
//
// Common pattern:
//   -y                              # overwrite
//   -f lavfi -i color=black:s=WxH:r=R:d=D
//   -f lavfi -i aevalsrc=0:s=48000:channel_layout=L:d=D
//   [-f srt|ass -i <sidecar.path>]   # for sub-bearing clips
//   <video encode flags>
//   <audio encode flags>
//   [-c:s copy]                      # for sub-bearing clips
//   <outPath>
func (b *Builder) ffmpegArgs(s Spec, outPath string) []string {
	var args []string
	args = append(args, "-y", "-hide_banner", "-loglevel", "error")

	// Inputs.
	args = append(args,
		"-f", "lavfi",
		"-i", fmt.Sprintf("color=black:s=%dx%d:r=%d:d=%d", s.Width, s.Height, s.FrameRate, s.DurationSec),
	)
	args = append(args,
		"-f", "lavfi",
		"-i", fmt.Sprintf("aevalsrc=0:s=48000:channel_layout=%s:d=%d", s.AudioLayout, s.DurationSec),
	)
	if s.SubKind != "" {
		// Mux a 1-cue sidecar so ffmpeg has a sub stream to copy. The
		// sidecar text is written by writeSubSidecar before Build runs;
		// the path is provided alongside the spec via subSidecarPath.
		args = append(args,
			"-f", s.SubKind,
			"-i", filepath.Join(b.outDir, "_sidecars", s.Title()+"."+s.SubKind),
		)
	}

	// Video encoder.
	args = append(args, b.videoArgs(s)...)

	// Audio encoder.
	args = append(args, b.audioArgs(s)...)

	// Subtitle stream copy (when present).
	if s.SubKind != "" {
		args = append(args, "-map", "2:0", "-c:s", "copy")
	}

	args = append(args, outPath)
	return args
}

func (b *Builder) videoArgs(s Spec) []string {
	switch s.VideoCodec {
	case "h264":
		return []string{
			"-map", "0:0",
			"-c:v", "libx264",
			"-profile:v", "high",
			"-pix_fmt", "yuv420p",
			"-preset", "ultrafast",
			"-tune", "stillimage",
			"-x264-params", "keyint=24:min-keyint=24",
		}
	case "hevc":
		args := []string{
			"-map", "0:0",
			"-c:v", "libx265",
			"-pix_fmt", b.pixFmt(s),
			"-preset", "ultrafast",
		}
		params := []string{"keyint=24:min-keyint=24"}
		if s.VideoProfile == "main10" {
			args = append(args, "-profile:v", "main10")
		} else {
			args = append(args, "-profile:v", "main")
		}
		// Color tagging — x265 won't bake bt2020/SMPTE 2084 into the
		// stream metadata unless you tell it explicitly.
		args = append(args, "-color_primaries", b.colorPrimaries(s),
			"-color_trc", s.Transfer,
			"-colorspace", b.colorMatrix(s))
		switch s.Transfer {
		case "smpte2084":
			// Minimal HDR10 master-display + max-CLL so ffprobe sees
			// "hdr_format=HDR10"-equivalent metadata downstream.
			params = append(params, "master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)", "max-cll=1000,400")
		}
		args = append(args, "-x265-params", strings.Join(params, ":"))
		return args
	case "av1":
		// libsvtav1 is fast + reliable on the matrix shapes we care
		// about. SVT options come via -svtav1-params (colon-separated).
		args := []string{
			"-map", "0:0",
			"-c:v", "libsvtav1",
			"-pix_fmt", b.pixFmt(s),
			"-preset", "10", // 0 (slowest) .. 13 (fastest); 10 is fast w/ good metadata fidelity
		}
		params := []string{
			"color-primaries=" + b.colorPrimariesNumeric(s),
			"transfer-characteristics=" + b.transferNumeric(s),
			"matrix-coefficients=" + b.matrixNumeric(s),
		}
		if s.Transfer == "smpte2084" {
			params = append(params, "mastering-display=G(0.265,0.690)B(0.150,0.060)R(0.680,0.320)WP(0.3127,0.3290)L(1000.0,0.0001)")
			params = append(params, "content-light=1000,400")
		}
		args = append(args, "-svtav1-params", strings.Join(params, ":"))
		return args
	}
	return nil
}

func (b *Builder) audioArgs(s Spec) []string {
	// Channel-layout flag goes BEFORE -c:a so the encoder picks up the
	// correct downmix/upmix matrix. ffmpeg infers from input layout but
	// being explicit avoids encoder-default surprises.
	return []string{
		"-map", "1:0",
		"-c:a", s.AudioCodec,
		"-ac", fmt.Sprintf("%d", s.AudioChannels),
		"-b:a", b.audioBitrate(s),
	}
}

func (b *Builder) audioBitrate(s Spec) string {
	switch {
	case s.AudioCodec == "aac" && s.AudioChannels <= 2:
		return "192k"
	case s.AudioCodec == "ac3":
		return "640k"
	case s.AudioCodec == "eac3" && s.AudioChannels <= 6:
		return "640k"
	case s.AudioCodec == "eac3":
		return "768k"
	default:
		return "192k"
	}
}

func (b *Builder) pixFmt(s Spec) string {
	if s.BitDepth == 10 {
		return "yuv420p10le"
	}
	return "yuv420p"
}

func (b *Builder) colorPrimaries(s Spec) string {
	if s.Transfer == "smpte2084" || s.Transfer == "arib-std-b67" {
		return "bt2020"
	}
	return "bt709"
}

func (b *Builder) colorMatrix(s Spec) string {
	if s.Transfer == "smpte2084" || s.Transfer == "arib-std-b67" {
		return "bt2020nc"
	}
	return "bt709"
}

// AV1 numeric codes for color metadata (ITU-T H.273 enum). Mirrors
// ffmpeg's libavutil/pixfmt.h AVColor*.
func (b *Builder) colorPrimariesNumeric(s Spec) string {
	if s.Transfer == "smpte2084" || s.Transfer == "arib-std-b67" {
		return "9" // BT.2020
	}
	return "1" // BT.709
}

func (b *Builder) transferNumeric(s Spec) string {
	switch s.Transfer {
	case "smpte2084":
		return "16" // SMPTE ST 2084 (PQ)
	case "arib-std-b67":
		return "18" // ARIB STD-B67 (HLG)
	}
	return "1" // BT.709
}

func (b *Builder) matrixNumeric(s Spec) string {
	if s.Transfer == "smpte2084" || s.Transfer == "arib-std-b67" {
		return "9" // BT.2020 non-constant
	}
	return "1" // BT.709
}

func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
