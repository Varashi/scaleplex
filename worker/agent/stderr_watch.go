package main

import (
	"log"
	"regexp"
	"strings"
)

// subBurnStderrRe matches the libass / fontconfig stderr lines that signal a
// subtitle-burn environment problem: an unwritable/missing font cache, a
// missing font, or a bad fontconfig path. These are frequently NON-fatal — the
// session keeps running — so they never reach the exit-time stderr_tail and
// were invisible in `kubectl logs` under PMS's -loglevel quiet (that's how Bug
// C, the writable-cache warning, hid). Surfacing them live lets qa_matrix's
// sub-burn cleanliness assertion (#149) catch image-level libass/fontconfig
// regressions, and gives prod diag a breadcrumb.
var subBurnStderrRe = regexp.MustCompile(
	`(?i)fontconfig error|libass: fontselect|cannot find font|no writable cache director`)

// stderrErrorWatch line-buffers streamed ffmpeg stderr and emits any line
// matching subBurnStderrRe (deduped per session). Append has the same
// func([]byte) shape as the stderr peek callback, so it composes with the
// existing ring-buffer tap without a second pipe.
type stderrErrorWatch struct {
	sid  string
	buf  []byte
	seen map[string]bool
	emit func(line string) // overridable in tests; defaults to the agent log
}

func newStderrErrorWatch(sid string) *stderrErrorWatch {
	w := &stderrErrorWatch{sid: sid, seen: map[string]bool{}}
	w.emit = func(line string) {
		log.Printf("session %s: subtitle-stderr: %s", sid, line)
	}
	return w
}

func (s *stderrErrorWatch) Append(p []byte) {
	if s == nil {
		return
	}
	s.buf = append(s.buf, p...)
	// Belt-and-braces: a stderr stream without a newline can't grow the line
	// buffer unbounded (ffmpeg stderr is line-oriented, so this rarely trips).
	if len(s.buf) > 64*1024 {
		s.buf = s.buf[len(s.buf)-64*1024:]
	}
	for {
		idx := -1
		for i, c := range s.buf {
			if c == '\n' || c == '\r' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(s.buf[:idx]), "\r\n ")
		s.buf = s.buf[idx+1:]
		if line == "" || !subBurnStderrRe.MatchString(line) || s.seen[line] {
			continue
		}
		s.seen[line] = true
		s.emit(line)
	}
}
