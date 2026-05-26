package synth

import (
	"fmt"
	"os/exec"
	"strings"
)

// WriteSidecar writes a tiny one-cue SRT or ASS file for one Spec into
// <outDir>/_sidecars/<title>.<srt|ass>. Used by Build so ffmpeg has a
// real subtitle stream to copy into the mkv. Single-cue is enough —
// Plex Optimize's argv shape branches on the *presence* of an embedded
// sub stream + its codec, not on the cue count.
//
// Like Builder, supports a wrapper invoker for the kubectl-exec path
// (writes happen via `<invoker> sh -c 'mkdir -p .../_sidecars && cat
// > .../<file>'`).
func (b *Builder) WriteSidecar(s Spec) error {
	if s.SubKind == "" {
		return nil
	}
	body := srtBody()
	if s.SubKind == "ass" {
		body = assBody()
	}
	sidecarPath := b.outDir + "/_sidecars/" + s.Title() + "." + s.SubKind

	// Use sh -c "mkdir -p ... && cat > ..." piped via stdin. Works
	// identically for local + remote modes — kubectl exec passes stdin
	// to the wrapped sh.
	shCmd := fmt.Sprintf("mkdir -p %q && cat > %q", b.outDir+"/_sidecars", sidecarPath)
	var cmd *exec.Cmd
	if len(b.invoker) > 0 {
		// invoker for ffmpeg ended with "ffmpeg"; swap the final token for "sh".
		shInvoker := append([]string{}, b.invoker...)
		shInvoker[len(shInvoker)-1] = "sh"
		full := append(append([]string{}, shInvoker[1:]...), "-c", shCmd)
		// kubectl exec needs -i (stdin) when piping. If the invoker is
		// kubectl exec, inject -i. Idempotent: skip if already there.
		full = ensureKubectlStdin(shInvoker, full)
		cmd = exec.Command(shInvoker[0], full...)
	} else {
		cmd = exec.Command("sh", "-c", shCmd)
	}
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("write sidecar %s: %w — out:\n%s", sidecarPath, err, string(out))
	}
	return nil
}

// ensureKubectlStdin injects "-i" right after "exec" if the wrapper is
// `kubectl exec ...` and -i isn't already present. Required because
// `kubectl exec` without -i drops stdin, breaking the cat-pipe.
func ensureKubectlStdin(invoker, args []string) []string {
	if len(invoker) == 0 || invoker[0] != "kubectl" {
		return args
	}
	// Look for "exec" then check if any -i follows before the "--".
	for i, tok := range args {
		if tok == "exec" {
			// Check tokens after "exec" up to "--".
			for j := i + 1; j < len(args); j++ {
				if args[j] == "--" {
					break
				}
				if args[j] == "-i" || args[j] == "--stdin" {
					return args
				}
			}
			// Insert -i right after exec.
			out := append([]string{}, args[:i+1]...)
			out = append(out, "-i")
			out = append(out, args[i+1:]...)
			return out
		}
	}
	return args
}

func srtBody() string {
	return "1\n00:00:01,000 --> 00:00:04,000\nscaleplex test sub\n\n"
}

func assBody() string {
	return `[Script Info]
ScriptType: v4.00+
PlayResX: 1920
PlayResY: 1080

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,54,&H00FFFFFF,&H000000FF,&H00000000,&H80000000,0,0,0,0,100,100,0,0,1,2,1,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,scaleplex test sub
`
}
