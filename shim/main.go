// scaleplex-shim — drop-in replacement for `Plex Transcoder` (which is
// just Plex-bundled ffmpeg). Forwards the invocation to scaleplex's
// orchestrator over HTTP, streams the response back as if we were
// ffmpeg ourselves, and exits with the right code for PMS to interpret.
//
// Designed to be installed at /usr/lib/plexmediaserver/Plex Transcoder
// (replacing the real binary) by a DOCKER_MOD on the lsio Plex image.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultOrchestratorURL = "http://scaleplex-orchestrator.scaleplex.svc.cluster.local:3500"

	// Worker prefixes its streams so we know what to send where.
	stdoutPrefix = "[stdout] "
	stderrPrefix = "[stderr] "
	eventPrefix  = "[scaleplex] "
)

type taskRequest struct {
	SessionID string            `json:"session_id"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Rewrite   bool              `json:"rewrite"`
}

func main() {
	// argv[0] is "Plex Transcoder"; argv[1:] is the ffmpeg argv.
	args := os.Args[1:]

	orchestratorURL := envOr("SCALEPLEX_ORCHESTRATOR_URL", defaultOrchestratorURL)
	rewrite := envBoolDefault("SCALEPLEX_REWRITE", true)
	timeout := envDurationOr("SCALEPLEX_HTTP_TIMEOUT", 0) // 0 = no overall timeout (transcodes can run hours)

	cwd, _ := os.Getwd()
	sessionID := deriveSessionID(args)

	if envBool("SCALEPLEX_DEBUG") {
		fmt.Fprintf(os.Stderr, "[scaleplex-shim] orchestrator=%s session=%s cwd=%s args=%d\n",
			orchestratorURL, sessionID, cwd, len(args))
	}

	body, err := json.Marshal(taskRequest{
		SessionID: sessionID,
		Args:      args,
		Env:       collectEnv(),
		Cwd:       cwd,
		Rewrite:   rewrite,
	})
	if err != nil {
		die("encode task: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Forward SIGTERM/SIGINT to the orchestrator as a kill request, then
	// cancel the local context so the response read loop unblocks.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		sig := <-signals
		fmt.Fprintf(os.Stderr, "[scaleplex-shim] caught %s, killing remote session\n", sig)
		killRemote(orchestratorURL, sessionID)
		cancel()
	}()

	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, orchestratorURL+"/task", bytes.NewReader(body))
	if err != nil {
		die("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		die("dial orchestrator %s: %v", orchestratorURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain body to stderr for visibility, then exit non-zero.
		fmt.Fprintf(os.Stderr, "[scaleplex-shim] orchestrator returned HTTP %d\n", resp.StatusCode)
		_, _ = io.Copy(os.Stderr, resp.Body)
		os.Exit(int(statusToExit(resp.StatusCode)))
	}

	exitCode := streamAndDemultiplex(resp.Body)
	os.Exit(exitCode)
}

// streamAndDemultiplex reads the worker's chunked response and routes
// each line to the right stream:
//
//	[stdout] foo  → real stdout (without prefix)
//	[stderr] bar  → real stderr (without prefix)
//	[scaleplex] … → diagnostic, written to stderr verbatim. The line
//	                "[scaleplex] ffmpeg exit: <reason>" carries the
//	                final exit code.
//
// Returns the exit code to propagate to PMS.
func streamAndDemultiplex(r io.Reader) int {
	exitCode := int32(1) // assume failure unless we see explicit success
	flushers := newLineFlushers()
	defer flushers.close()

	buf := make([]byte, 8192)
	var carry []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := append(carry, buf[:n]...)
			lines := splitLinesKeepDelim(data)
			carry = lines.tail
			for _, line := range lines.complete {
				routeLine(line, flushers, &exitCode)
			}
		}
		if err != nil {
			break
		}
	}
	if len(carry) > 0 {
		routeLine(carry, flushers, &exitCode)
	}
	return int(atomic.LoadInt32(&exitCode))
}

// routeLine inspects a single line and writes it to stdout/stderr with
// the prefix stripped. Updates exitCode on the terminal "ffmpeg exit"
// event line.
func routeLine(line []byte, fl *lineFlushers, exitCode *int32) {
	switch {
	case bytes.HasPrefix(line, []byte(stdoutPrefix)):
		fl.stdout.Write(line[len(stdoutPrefix):])
	case bytes.HasPrefix(line, []byte(stderrPrefix)):
		fl.stderr.Write(line[len(stderrPrefix):])
	case bytes.HasPrefix(line, []byte(eventPrefix)):
		// Echo to stderr for visibility + parse exit code if present.
		fl.stderr.Write(line)
		if bytes.Contains(line, []byte("ffmpeg exit: success")) {
			atomic.StoreInt32(exitCode, 0)
		} else if bytes.Contains(line, []byte("ffmpeg exit:")) {
			atomic.StoreInt32(exitCode, 1)
		}
	default:
		// Untagged bytes → stderr (prefer visibility). ffmpeg progress
		// lines come over CR-separated and may not have a clean prefix
		// every chunk; keeping them on stderr matches PMS expectations.
		fl.stderr.Write(line)
	}
}

type lineFlushers struct {
	mu     sync.Mutex
	stdout *bufWriter
	stderr *bufWriter
}

func newLineFlushers() *lineFlushers {
	return &lineFlushers{
		stdout: &bufWriter{w: os.Stdout},
		stderr: &bufWriter{w: os.Stderr},
	}
}

func (f *lineFlushers) close() {
	f.stdout.flush()
	f.stderr.flush()
}

// bufWriter is a tiny line-buffered writer. ffmpeg progress is CR-only;
// we don't try to be clever — flush every Write.
type bufWriter struct {
	mu sync.Mutex
	w  *os.File
}

func (b *bufWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.w.Write(p)
}
func (b *bufWriter) flush() { _ = b.w.Sync() }

type splitResult struct {
	complete [][]byte
	tail     []byte
}

// splitLinesKeepDelim splits on '\n' OR '\r' so ffmpeg's CR-only
// progress chunks each become their own line. Trailing partial line
// (no terminator) is returned in tail.
func splitLinesKeepDelim(data []byte) splitResult {
	var out [][]byte
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' || data[i] == '\r' {
			out = append(out, data[start:i+1])
			start = i + 1
		}
	}
	return splitResult{complete: out, tail: data[start:]}
}

// killRemote best-effort POSTs /task/<id>/kill so the worker tears
// down ffmpeg cleanly. 1s timeout — we don't want to block PMS shutdown.
func killRemote(orchestratorURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	url := orchestratorURL + "/task/" + sessionID + "/kill"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// deriveSessionID returns a stable-ish identifier for this transcode
// invocation. Plex doesn't pass us its session token directly, so we
// derive one from the input path + a random suffix. Using only the
// path basename keeps repeats of the same source produce different IDs
// (each ffmpeg spawn is a distinct session in Plex's view too).
func deriveSessionID(args []string) string {
	var inputPath string
	for i, a := range args {
		if a == "-i" && i+1 < len(args) {
			inputPath = args[i+1]
			break
		}
	}
	base := "session"
	if inputPath != "" {
		base = sanitize(filepath.Base(inputPath))
		if len(base) > 32 {
			base = base[:32]
		}
	}
	var nonce [4]byte
	_, _ = rand.Read(nonce[:])
	return fmt.Sprintf("%s-%d-%s", base, os.Getpid(), hex.EncodeToString(nonce[:]))
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// collectEnv copies the calling process's environment into the task
// envelope. The worker uses these to spawn ffmpeg with PMS-equivalent
// state (LIBVA_*, FONTCONFIG_*, plex-specific paths, etc).
//
// Adds SCALEPLEX_PMS_BASE_URL — a worker-reachable address for this
// PMS pod — so the rewriter can substitute Plex's hardcoded
// 127.0.0.1:32400 in -progressurl. Without this, PMS holds segment
// HTTP requests open for ~120s waiting on progress reports.
func collectEnv() map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		out[kv[:eq]] = kv[eq+1:]
	}
	// PMS_SERVICE is set by the lsio plex helmrelease (clusterplex
	// already sets it to clusterplex-pms.clusterplex.svc); fall back
	// to localhost which is correct for non-cluster setups.
	if _, has := out["SCALEPLEX_PMS_BASE_URL"]; !has {
		host := out["PMS_SERVICE"]
		if host == "" {
			host = "127.0.0.1"
		}
		port := out["PMS_PORT"]
		if port == "" {
			port = "32400"
		}
		out["SCALEPLEX_PMS_BASE_URL"] = "http://" + host + ":" + port
	}
	return out
}

func envOr(k, dflt string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return dflt
}

func envBool(k string) bool { return os.Getenv(k) == "true" || os.Getenv(k) == "1" }

func envBoolDefault(k string, dflt bool) bool {
	v, ok := os.LookupEnv(k)
	if !ok {
		return dflt
	}
	return v == "true" || v == "1"
}

func envDurationOr(k string, dflt time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return dflt
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return dflt
	}
	return d
}

func statusToExit(code int) int32 {
	switch code {
	case http.StatusServiceUnavailable:
		return 75 // EX_TEMPFAIL
	case http.StatusBadRequest:
		return 64 // EX_USAGE
	case http.StatusBadGateway:
		return 74 // EX_IOERR
	}
	return 1
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[scaleplex-shim] "+format+"\n", a...)
	os.Exit(1)
}
