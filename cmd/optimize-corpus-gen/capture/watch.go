// Package capture watches the worker's argv-corpus dir for new files
// landing during a single Optimize cell, copies them to a local sink
// (when the corpus is on a remote NFS the host can't see), and writes
// a sidecar tag joining cell metadata to the captured argv.
//
// Two execution modes:
//
//   - **Local**: Watcher is constructed with a directory path; it
//     ReadDir's that path directly. Fast, simple, requires the corpus
//     share mounted on the host.
//
//   - **Remote**: Watcher is constructed with an invoker (e.g.
//     `kubectl exec -n plex-test <pod> --`) plus a remote dir path
//     plus a local sink directory. List runs as `<invoker> ls <dir>`,
//     reads run as `<invoker> cat <dir>/<file>` and are written to the
//     sink so sidecars + captures live together locally.
//
// Mode is chosen by which constructor was used; the rest of the
// watcher interface (Snapshot, WaitForNew) is identical.
package capture

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Watcher tracks the set of capture files in a (local or remote) dir
// and reports newly-arrived ones via WaitForNew. Use:
//
//	w, _ := capture.NewLocalWatcher(dir)             // or NewRemoteWatcher(...)
//	w.Snapshot()                                     // baseline
//	// … trigger the Optimize job …
//	newFiles, _ := w.WaitForNew(30 * time.Second)
//	// newFiles == local file paths (copied from remote if applicable)
//
// Captures land as <session-uuid>-<hash>.json in the corpus dir
// (worker's persistArgvCapture writes them). Watcher matches them by
// listing the dir and diffing — simple and robust under NFS where
// inotify is unreliable, and the kubectl-exec equivalent doesn't
// expose inotify either.
type Watcher struct {
	// localDir is where the watcher reads from directly (mode=local) OR
	// where remote captures get copied to (mode=remote).
	localDir string

	// remote mode: invoker is the exec-wrapper that runs commands inside
	// the worker pod, remoteDir is the path inside the pod.
	invoker   []string
	remoteDir string

	// baseline is the set of capture basenames present at Snapshot time.
	baseline map[string]struct{}
}

// NewLocalWatcher returns a Watcher that reads dir directly. Use when
// the corpus share is mounted on the host.
func NewLocalWatcher(dir string) (*Watcher, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("local corpus dir %s: %w", dir, err)
	}
	return &Watcher{localDir: dir}, nil
}

// NewRemoteWatcher returns a Watcher that lists / reads the corpus via
// invokerCmd (split on whitespace, e.g. "kubectl exec -n plex-test <pod> --").
// remoteDir is the corpus path inside the pod; sinkDir is where captures
// are copied to locally (so sidecars + captures live together).
func NewRemoteWatcher(invokerCmd, remoteDir, sinkDir string) (*Watcher, error) {
	if invokerCmd == "" || remoteDir == "" {
		return nil, fmt.Errorf("NewRemoteWatcher: invokerCmd and remoteDir required")
	}
	if sinkDir == "" {
		return nil, fmt.Errorf("NewRemoteWatcher: sinkDir required (where remote captures get copied)")
	}
	if err := os.MkdirAll(sinkDir, 0o755); err != nil {
		return nil, fmt.Errorf("sink dir %s: %w", sinkDir, err)
	}
	var invoker []string
	for _, tok := range strings.Fields(invokerCmd) {
		invoker = append(invoker, tok)
	}
	return &Watcher{
		localDir:  sinkDir,
		invoker:   invoker,
		remoteDir: remoteDir,
	}, nil
}

// Snapshot records the current set of *.json files as the baseline.
// WaitForNew compares against this set.
func (w *Watcher) Snapshot() error {
	files, err := w.list()
	if err != nil {
		return err
	}
	w.baseline = make(map[string]struct{}, len(files))
	for _, f := range files {
		w.baseline[f] = struct{}{}
	}
	return nil
}

// WaitForNew polls the dir until at least one new *.json basename
// appears (relative to the last Snapshot), or timeout elapses. Returns
// LOCAL paths — in remote mode, new captures are copied into localDir
// before being returned.
//
// Poll interval is 1500 ms in remote mode (kubectl exec is slow,
// hammering it is rude), 500 ms in local mode.
func (w *Watcher) WaitForNew(timeout time.Duration) ([]string, error) {
	if w.baseline == nil {
		return nil, fmt.Errorf("WaitForNew called without Snapshot")
	}
	interval := 500 * time.Millisecond
	if w.invoker != nil {
		interval = 1500 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		files, err := w.list()
		if err != nil {
			return nil, err
		}
		var fresh []string
		for _, f := range files {
			if _, seen := w.baseline[f]; !seen {
				fresh = append(fresh, f)
			}
		}
		if len(fresh) > 0 {
			localPaths, err := w.materialize(fresh)
			if err != nil {
				return nil, err
			}
			// Sort by mtime ascending so the caller can tag captures in
			// the order PMS spawned them (multi-output cells produce
			// several captures; first one is the main video transcode).
			sort.Slice(localPaths, func(i, j int) bool {
				si, _ := os.Stat(localPaths[i])
				sj, _ := os.Stat(localPaths[j])
				return si.ModTime().Before(sj.ModTime())
			})
			return localPaths, nil
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		<-tick.C
	}
}

// list returns basenames (not full paths) of every *.json file in the
// corpus dir at this moment. Sidecars (.optimize-cell.json) are
// filtered out so a previous run's tags don't pollute the baseline.
func (w *Watcher) list() ([]string, error) {
	if w.invoker == nil {
		return w.listLocal()
	}
	return w.listRemote()
}

func (w *Watcher) listLocal() ([]string, error) {
	entries, err := os.ReadDir(w.localDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.HasSuffix(name, ".optimize-cell.json") {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

func (w *Watcher) listRemote() ([]string, error) {
	args := append(append([]string{}, w.invoker[1:]...), "ls", "-1", w.remoteDir)
	cmd := exec.Command(w.invoker[0], args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return nil, fmt.Errorf("remote ls %s: %w — stderr=%s", w.remoteDir, err, stderr)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".optimize-cell.json") {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// materialize ensures each basename has a local file at localDir/<name>.
// In local mode that's a no-op (already local). In remote mode it cats
// the file out of the pod and writes it to the sink.
func (w *Watcher) materialize(basenames []string) ([]string, error) {
	out := make([]string, 0, len(basenames))
	for _, name := range basenames {
		localPath := filepath.Join(w.localDir, name)
		if w.invoker == nil {
			out = append(out, localPath)
			continue
		}
		// Skip if we've already materialized this file (e.g. on a
		// previous tick of the same WaitForNew call — current logic
		// returns on first hit, so this is a defensive idempotency).
		if _, err := os.Stat(localPath); err == nil {
			out = append(out, localPath)
			continue
		}
		body, err := w.catRemote(name)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(localPath, body, 0o644); err != nil {
			return nil, fmt.Errorf("write sink %s: %w", localPath, err)
		}
		out = append(out, localPath)
	}
	return out, nil
}

// catRemote reads one remote capture file via the invoker.
func (w *Watcher) catRemote(basename string) ([]byte, error) {
	remotePath := w.remoteDir
	if !strings.HasSuffix(remotePath, "/") {
		remotePath += "/"
	}
	remotePath += basename
	args := append(append([]string{}, w.invoker[1:]...), "cat", remotePath)
	cmd := exec.Command(w.invoker[0], args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return nil, fmt.Errorf("remote cat %s: %w — stderr=%s", remotePath, err, stderr)
	}
	return out, nil
}
