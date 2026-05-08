// argv-extract reads scaleplex worker logs on stdin and emits one
// JSON per session into $CORPUS_DIR (default test/argv-corpus/).
// Idempotent: skips sessions whose JSON already exists.
//
// Usage:
//
//	kubectl -n clusterplex logs -l app.kubernetes.io/controller=worker \
//	  --tail=20000 --since=4h --prefix=true \
//	  | CORPUS_DIR=test/argv-corpus go run ./cmd/argv-extract
//
// Each emitted file captures:
//   - session_id, captured_at, worker_pod
//   - full argv (parsed back from Go's %q-printed slice)
//   - rewriter_changes (split from "rewriter applied: ..." line)
//   - structural metadata (output_format, output_codec, output_resolution,
//     source_path, has_input_seek, seek_offset_sec, has_map_inlineass,
//     has_second_input, segment_time, segment_start_number)
//   - outcome (segments_created, exit_reason, encode_speed)
//
// The structural metadata is the input to pattern-recognition — query
// the corpus by these fields to find argv classes that share traits.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
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

type Capture struct {
	SessionID        string   `json:"session_id"`
	CapturedAt       string   `json:"captured_at,omitempty"`
	WorkerPod        string   `json:"worker_pod,omitempty"`
	Argv             []string `json:"argv"`
	RewriterApplied  *bool    `json:"rewriter_applied,omitempty"`
	RewriterChanges  []string `json:"rewriter_changes,omitempty"`
	RewriterSkipWhy  string   `json:"rewriter_skip_why,omitempty"`
	OutputFormat     string   `json:"output_format,omitempty"`
	OutputCodec      string   `json:"output_codec,omitempty"`
	OutputResolution string   `json:"output_resolution,omitempty"`
	SourcePath       string   `json:"source_path,omitempty"`
	HasInputSeek     bool     `json:"has_input_seek"`
	SeekOffsetSec    float64  `json:"seek_offset_sec,omitempty"`
	HasMapInlineass  bool     `json:"has_map_inlineass"`
	InputCount       int      `json:"input_count"`
	SegmentTime      string   `json:"segment_time,omitempty"`
	SegmentStartNum  string   `json:"segment_start_number,omitempty"`
	SegmentsCreated  int      `json:"segments_created"`
	EncodeSpeed      string   `json:"encode_speed,omitempty"`
	ExitReason       string   `json:"exit_reason,omitempty"`
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

func main() {
	corpusDir := os.Getenv("CORPUS_DIR")
	if corpusDir == "" {
		corpusDir = "test/argv-corpus"
	}
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", corpusDir, err)
		os.Exit(1)
	}

	captures := make(map[string]*Capture)
	get := func(sid string) *Capture {
		if c := captures[sid]; c != nil {
			return c
		}
		c := &Capture{SessionID: sid}
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
		fmt.Fprintf(os.Stderr, "scan: %v\n", err)
	}

	keys := make([]string, 0, len(captures))
	for k := range captures {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	written, skipped := 0, 0
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
	}
	fmt.Fprintf(os.Stderr, "wrote %d, skipped %d (already present)\n", written, skipped)
}
