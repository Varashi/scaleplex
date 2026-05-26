package synth

import (
	"fmt"
	"os/exec"
	"strings"
)

// EnsureAll synthesizes every Spec into outDir. Idempotent: a Spec
// whose filename is already present is skipped (no re-encode). Returns
// the list of paths (in the ffmpeg-side environment).
//
// One-shot bring-up of a 20-clip Phase-2 matrix is ~2-3 minutes — the
// black-frame inputs encode in seconds at ultrafast. Re-runs after
// the initial setup are sub-second (all skipped).
func (b *Builder) EnsureAll(specs []Spec, log func(string)) ([]string, error) {
	if log == nil {
		log = func(string) {}
	}

	existing, err := b.listOutDir()
	if err != nil {
		return nil, fmt.Errorf("list outdir: %w", err)
	}

	var out []string
	for _, s := range specs {
		name := s.Filename()
		path := b.outDir + "/" + name
		if existing[name] {
			log(fmt.Sprintf("skip (exists): %s", name))
			out = append(out, path)
			continue
		}
		if s.SubKind != "" {
			if err := b.WriteSidecar(s); err != nil {
				return nil, err
			}
		}
		log(fmt.Sprintf("build: %s", name))
		p, err := b.Build(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// listOutDir lists the existing .mkv files in outDir. Works for both
// local and remote (invoker-wrapped) modes.
func (b *Builder) listOutDir() (map[string]bool, error) {
	shCmd := fmt.Sprintf("mkdir -p %q && ls -1 %q || true", b.outDir, b.outDir)
	var cmd *exec.Cmd
	if len(b.invoker) > 0 {
		shInvoker := append([]string{}, b.invoker...)
		shInvoker[len(shInvoker)-1] = "sh"
		full := append(append([]string{}, shInvoker[1:]...), "-c", shCmd)
		cmd = exec.Command(shInvoker[0], full...)
	} else {
		cmd = exec.Command("sh", "-c", shCmd)
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listOutDir: %w", err)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if strings.HasSuffix(name, ".mkv") {
			set[name] = true
		}
	}
	return set, nil
}
