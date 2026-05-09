// argv-extract reads scaleplex argv data from one or more sources and
// emits one JSON per session into $CORPUS_DIR (default
// ~/scaleplex-corpus/). Idempotent: skips sessions whose JSON already
// exists.
//
// Corpus deliberately lives outside the repo working tree because
// captured argv contains real source paths from the user's media
// library (privacy: title strings, household viewing habits).
// Override CORPUS_DIR if you want it elsewhere.
//
// Three input formats supported:
//
//  1. Worker log lines on stdin (Go %q-printed argv slices). Includes
//     rewriter_applied / rewriter_changes + outcome (segments_created,
//     exit_reason, encode_speed).
//
//     kubectl -n clusterplex logs -l app.kubernetes.io/controller=worker \
//       --tail=99999 --since=24h --prefix=true \
//       | argv-extract
//
//  2. Worker NFS JSON files (one per session, written by worker's
//     persistArgvCapture). Includes argv + env. Pass with -sweep.
//
//  3. Plex production wrapper NUL-args files (timestamp on first line,
//     then NUL-separated argv). Pristine PMS argv, no rewriter
//     metadata. Pass with -sweep.
//
//     argv-extract \
//       -sweep /tmp/clusterplex-corpus \
//       -sweep /tmp/plex-corpus
//
// Format auto-detected per file by reading the first byte: `{` → JSON,
// otherwise → NUL-args. Both modes can run together; -sweep can be
// repeated.
//
// Each emitted JSON has structural metadata (output_format,
// output_codec, output_resolution, source_path, has_input_seek,
// seek_offset_sec, has_map_inlineass, input_count, segment_time,
// segment_start_number) extracted from the argv plus a capture_source
// field (worker_log / worker_nfs_json / plex_wrapper_nfs) for filtering.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	rePodPrefix      = regexp.MustCompile(`^\[pod/(\S+)/\S+\]\s+`)
	reTime           = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)
	reArgv           = regexp.MustCompile(`session (\S+): argv=\[(.*)\]$`)
	reRewriter       = regexp.MustCompile(`session (\S+): rewriter applied: (.*)$`)
	reRewriterSkip   = regexp.MustCompile(`session (\S+): rewriter NOT applied \(([^)]*)\)`)
	reSpawn          = regexp.MustCompile(`session (\S+): spawned ffmpeg pid=(\d+)`)
	reExit           = regexp.MustCompile(`session (\S+): ffmpeg exit: ([^=]*) stderr_tail=(.*)$`)
	reChunkCreate    = regexp.MustCompile(`session (\S+): chunk-renumber raw event op=CREATE name=(\S+)`)
	reSpeed          = regexp.MustCompile(`speed=\s*(\S+)x`)
	reFilterScale    = regexp.MustCompile(`scale(?:_vaapi)?=w=(\d+):h=(\d+)`)
	reSeekOffsetTag  = regexp.MustCompile(`seek-offset:captured=([\d.]+)s`)
	reMediaSegmentTS = regexp.MustCompile(`^media-\d+\.(?:ts|m4s)$`)
)

// Capture sources.
const (
	sourceWorkerLog     = "worker_log"
	sourceWorkerNFSJSON = "worker_nfs_json"
	sourcePlexWrapper   = "plex_wrapper_nfs"
)

type Capture struct {
	SessionID        string            `json:"session_id"`
	CaptureSource    string            `json:"capture_source,omitempty"`
	CapturedAt       string            `json:"captured_at,omitempty"`
	WorkerPod        string            `json:"worker_pod,omitempty"`
	WorkerHost       string            `json:"worker_host,omitempty"`
	SessionCwd       string            `json:"session_cwd,omitempty"`
	Argv             []string          `json:"argv"`
	Env              map[string]string `json:"env,omitempty"`
	RewriterApplied  *bool             `json:"rewriter_applied,omitempty"`
	RewriterChanges  []string          `json:"rewriter_changes,omitempty"`
	RewriterSkipWhy  string            `json:"rewriter_skip_why,omitempty"`
	OutputFormat     string            `json:"output_format,omitempty"`
	OutputCodec      string            `json:"output_codec,omitempty"`
	OutputResolution string            `json:"output_resolution,omitempty"`
	SourcePath       string            `json:"source_path,omitempty"`
	HasInputSeek     bool              `json:"has_input_seek"`
	SeekOffsetSec    float64           `json:"seek_offset_sec,omitempty"`
	HasMapInlineass  bool              `json:"has_map_inlineass"`
	InputCount       int               `json:"input_count"`
	SegmentTime      string            `json:"segment_time,omitempty"`
	SegmentStartNum  string            `json:"segment_start_number,omitempty"`
	SegmentsCreated  int               `json:"segments_created"`
	EncodeSpeed      string            `json:"encode_speed,omitempty"`
	ExitReason       string            `json:"exit_reason,omitempty"`
}

func parseQuotedSlice(body string) []string {
	var out []string
	i := 0
	for i < len(body) {
		for i < len(body) && body[i] == ' ' {
			i++
		}
		if i >= len(body) {
			break
		}
		if body[i] != '"' {
			break
		}
		j := i + 1
		for j < len(body) {
			if body[j] == '\\' && j+1 < len(body) {
				j += 2
				continue
			}
			if body[j] == '"' {
				break
			}
			j++
		}
		if j >= len(body) {
			break
		}
		s, err := strconv.Unquote(body[i : j+1])
		if err == nil {
			out = append(out, s)
		}
		i = j + 1
	}
	return out
}

func indexOfArg(args []string, flag string, from int) int {
	for i := from; i < len(args); i++ {
		if args[i] == flag {
			return i
		}
	}
	return -1
}

func extractStructuralMeta(c *Capture) {
	for i := 0; i+1 < len(c.Argv); i++ {
		switch c.Argv[i] {
		case "-ss":
			c.HasInputSeek = true
			if v, err := strconv.ParseFloat(c.Argv[i+1], 64); err == nil && c.SeekOffsetSec == 0 {
				c.SeekOffsetSec = v
			}
		case "-map_inlineass":
			c.HasMapInlineass = true
		case "-f":
			if c.OutputFormat == "" {
				c.OutputFormat = c.Argv[i+1]
			}
		case "-segment_time":
			c.SegmentTime = c.Argv[i+1]
		case "-segment_start_number":
			c.SegmentStartNum = c.Argv[i+1]
		}
	}
	for _, a := range c.Argv {
		if a == "-i" {
			c.InputCount++
		}
	}
	firstI := indexOfArg(c.Argv, "-i", 0)
	if firstI >= 0 && firstI+1 < len(c.Argv) {
		c.SourcePath = c.Argv[firstI+1]
		if encIdx := indexOfArg(c.Argv, "-codec:0", firstI+1); encIdx >= 0 && encIdx+1 < len(c.Argv) {
			c.OutputCodec = c.Argv[encIdx+1]
		}
	}
	for i := 0; i+1 < len(c.Argv); i++ {
		if c.Argv[i] == "-filter_complex" && strings.HasPrefix(c.Argv[i+1], "[0:0]") {
			if m := reFilterScale.FindStringSubmatch(c.Argv[i+1]); m != nil {
				c.OutputResolution = m[1] + "x" + m[2]
			}
			break
		}
	}
}

// stringSliceFlag accepts repeated -sweep <dir> on the command line.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

// readStdinLogs parses worker log lines on stdin into the captures map.
func readStdinLogs(captures map[string]*Capture) {
	get := func(sid string) *Capture {
		if c := captures[sid]; c != nil {
			return c
		}
		c := &Capture{SessionID: sid, CaptureSource: sourceWorkerLog}
		captures[sid] = c
		return c
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		pod := ""
		if m := rePodPrefix.FindStringSubmatch(line); m != nil {
			pod = m[1]
			line = line[len(m[0]):]
		}
		ts := ""
		if m := reTime.FindStringSubmatch(line); m != nil {
			ts = m[1]
		}

		if m := reArgv.FindStringSubmatch(line); m != nil {
			c := get(m[1])
			argv := parseQuotedSlice(m[2])
			if len(argv) == 0 {
				continue
			}
			c.Argv = argv
			if c.CapturedAt == "" {
				c.CapturedAt = ts
			}
			if c.WorkerPod == "" {
				c.WorkerPod = pod
			}
			extractStructuralMeta(c)
			continue
		}
		if m := reRewriter.FindStringSubmatch(line); m != nil {
			c := get(m[1])
			t := true
			c.RewriterApplied = &t
			parts := strings.Split(m[2], ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			c.RewriterChanges = parts
			if mt := reSeekOffsetTag.FindStringSubmatch(m[2]); mt != nil {
				if v, err := strconv.ParseFloat(mt[1], 64); err == nil {
					c.SeekOffsetSec = v
				}
			}
			continue
		}
		if m := reRewriterSkip.FindStringSubmatch(line); m != nil {
			c := get(m[1])
			f := false
			c.RewriterApplied = &f
			c.RewriterSkipWhy = m[2]
			continue
		}
		if m := reExit.FindStringSubmatch(line); m != nil {
			c := get(m[1])
			c.ExitReason = strings.TrimSpace(m[2])
			if sm := reSpeed.FindStringSubmatch(m[3]); sm != nil {
				c.EncodeSpeed = sm[1] + "x"
			}
			continue
		}
		if m := reChunkCreate.FindStringSubmatch(line); m != nil {
			c := get(m[1])
			if reMediaSegmentTS.MatchString(m[2]) {
				c.SegmentsCreated++
			}
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "scan stdin: %v\n", err)
	}
}

// readSweepDir walks dir, auto-detects each file's format, and merges
// captures into the map keyed by session_id. JSON files (worker NFS)
// are unmarshalled directly; everything else is treated as the plex
// wrapper format (timestamp on first line, then NUL-separated args).
func readSweepDir(captures map[string]*Capture, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	skipped := 0
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		// Skip the leftover ".argv" file from the broken-bash-quoting deploy
		// (no SID prefix, content unparseable).
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		c, err := readSweepFile(path, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sweep %s: %v\n", path, err)
			skipped++
			continue
		}
		if c == nil {
			skipped++
			continue
		}
		// If we already have a more-detailed capture for this session
		// (e.g. from worker logs which have rewriter_changes), keep
		// the richer one and skip the sweep-derived one. Otherwise
		// take the new capture.
		if existing, ok := captures[c.SessionID]; ok {
			if richer(existing, c) {
				continue
			}
		}
		captures[c.SessionID] = c
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "sweep %s: skipped %d unparseable\n", dir, skipped)
	}
	return nil
}

// richer reports whether `a` is a richer capture than `b` (more
// metadata that we don't want to lose by overwriting).
func richer(a, b *Capture) bool {
	score := func(c *Capture) int {
		s := 0
		if c.RewriterApplied != nil {
			s += 4
		}
		if len(c.RewriterChanges) > 0 {
			s += 2
		}
		if c.ExitReason != "" {
			s++
		}
		if c.SegmentsCreated > 0 {
			s++
		}
		if len(c.Env) > 0 {
			s++
		}
		return s
	}
	return score(a) > score(b)
}

func readSweepFile(path, basename string) (*Capture, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var head [1]byte
	n, _ := f.Read(head[:])
	if n == 0 {
		return nil, errors.New("empty file")
	}
	// Rewind for full read.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	if head[0] == '{' {
		return parseWorkerNFSJSON(body)
	}
	return parsePlexWrapper(body, basename)
}

// parseWorkerNFSJSON parses a JSON file written by the worker's
// persistArgvCapture into a Capture. Field names match the worker's
// struct directly (we use the same JSON tags downstream).
func parseWorkerNFSJSON(body []byte) (*Capture, error) {
	var src struct {
		SessionID  string            `json:"session_id"`
		CapturedAt string            `json:"captured_at"`
		WorkerPod  string            `json:"worker_pod"`
		WorkerHost string            `json:"worker_host"`
		Cwd        string            `json:"session_cwd"`
		Argv       []string          `json:"argv"`
		Env        map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	if src.SessionID == "" || len(src.Argv) == 0 {
		return nil, errors.New("missing session_id or argv")
	}
	c := &Capture{
		SessionID:     src.SessionID,
		CaptureSource: sourceWorkerNFSJSON,
		CapturedAt:    src.CapturedAt,
		WorkerPod:     src.WorkerPod,
		WorkerHost:    src.WorkerHost,
		SessionCwd:    src.Cwd,
		Argv:          src.Argv,
		Env:           src.Env,
	}
	extractStructuralMeta(c)
	return c, nil
}

// parsePlexWrapper parses a file written by the Plex production
// wrapper. Format: ISO8601 timestamp newline-terminated, then
// NUL-separated argv. session_id derived from filename (basename
// without `.argv` extension).
func parsePlexWrapper(body []byte, basename string) (*Capture, error) {
	nl := bytes.IndexByte(body, '\n')
	var ts string
	var rest []byte
	if nl < 0 {
		rest = body
	} else {
		ts = string(bytes.TrimSpace(body[:nl]))
		rest = body[nl+1:]
	}
	parts := bytes.Split(rest, []byte{0})
	argv := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		argv = append(argv, string(p))
	}
	if len(argv) == 0 {
		return nil, errors.New("no argv")
	}
	sid := strings.TrimSuffix(basename, ".argv")
	if sid == "" {
		return nil, errors.New("missing session_id from filename")
	}
	c := &Capture{
		SessionID:     sid,
		CaptureSource: sourcePlexWrapper,
		CapturedAt:    ts,
		Argv:          argv,
	}
	extractStructuralMeta(c)
	return c, nil
}

func main() {
	corpusDir := os.Getenv("CORPUS_DIR")
	if corpusDir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = "."
		}
		corpusDir = filepath.Join(home, "scaleplex-corpus")
	}
	var sweepDirs stringSliceFlag
	flag.Var(&sweepDirs, "sweep", "sweep directory of capture files (worker NFS JSON or plex wrapper NUL-args). Repeatable.")
	flag.StringVar(&corpusDir, "corpus", corpusDir, "output corpus directory")
	flag.Parse()

	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", corpusDir, err)
		os.Exit(1)
	}

	captures := make(map[string]*Capture)

	// Sweep dirs first so log-derived data (richer) wins on conflicts.
	for _, d := range sweepDirs {
		if err := readSweepDir(captures, d); err != nil {
			fmt.Fprintf(os.Stderr, "sweep %s: %v\n", d, err)
		}
	}

	// Stdin only if it's not a tty.
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		readStdinLogs(captures)
	}

	keys := make([]string, 0, len(captures))
	for k := range captures {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	written, skipped := 0, 0
	bySource := map[string]int{}
	for _, sid := range keys {
		c := captures[sid]
		if len(c.Argv) == 0 {
			continue
		}
		path := filepath.Join(corpusDir, sid+".json")
		if _, err := os.Stat(path); err == nil {
			skipped++
			continue
		}
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", path, err)
			continue
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(c); err != nil {
			fmt.Fprintf(os.Stderr, "encode %s: %v\n", path, err)
		}
		f.Close()
		written++
		bySource[c.CaptureSource]++
	}
	fmt.Fprintf(os.Stderr, "wrote %d, skipped %d (already present)\n", written, skipped)
	if written > 0 {
		var keys []string
		for k := range bySource {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(os.Stderr, "  %s: %d\n", k, bySource[k])
		}
	}
}
