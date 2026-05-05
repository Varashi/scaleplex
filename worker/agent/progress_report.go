package main

// progress_report — drives Plex's /progress endpoint from the worker.
//
// Plex Transcoder PUTs one discrete payload per progress tick. Stock
// ffmpeg's `-progress <url>` does HTTP differently: a single chunked
// PUT for the whole transcode, with key=value blocks streamed inside
// the body. Plex's PUT handler reads the body to EOF before parsing,
// so it never sees a "first" report and stalls /header for ~120s
// (until the internal timeout). Workaround: ffmpeg writes its progress
// stream to a pipe we own, this reporter reissues each completed block
// as its own PUT. Block boundary = the `progress=continue|end` line
// ffmpeg always emits at the end of every tick (default tick = 1s).

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var metricProgressPUT = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "scaleplex_worker_progress_put_total",
	Help: "Per-block PUTs to Plex's /progress endpoint, labelled by HTTP class or err.",
}, []string{"result"}) // 2xx | 3xx | 4xx | 5xx | err

// runProgressReporter consumes ffmpeg -progress output from r and PUTs
// each completed block to url. Returns when r reaches EOF or ctx done.
func runProgressReporter(ctx context.Context, r io.Reader, url, sessionID string) {
	if url == "" {
		return
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 1<<20)

	httpClient := &http.Client{Timeout: 4 * time.Second}
	var blk bytes.Buffer
	for sc.Scan() {
		line := sc.Bytes()
		blk.Write(line)
		blk.WriteByte('\n')
		if bytes.HasPrefix(line, []byte("progress=")) {
			putProgress(ctx, httpClient, url, blk.Bytes(), sessionID)
			blk.Reset()
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		log.Printf("session %s: progress reporter scan: %v", sessionID, err)
	}
}

func putProgress(ctx context.Context, c *http.Client, url string, body []byte, sessionID string) {
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		metricProgressPUT.WithLabelValues("err").Inc()
		return
	}
	req.Header.Set("Content-Type", "text/plain")
	req.ContentLength = int64(len(body))

	resp, err := c.Do(req)
	if err != nil {
		metricProgressPUT.WithLabelValues("err").Inc()
		if ctx.Err() == nil {
			log.Printf("session %s: progress PUT: %v", sessionID, err)
		}
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	metricProgressPUT.WithLabelValues(httpClass(resp.StatusCode)).Inc()
	if resp.StatusCode >= 400 {
		log.Printf("session %s: progress PUT status=%d", sessionID, resp.StatusCode)
	}
}

func httpClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	}
	return "1xx"
}

// progressPipeArg returns the argv pair to pass `-progress pipe:N` to
// ffmpeg, where N is the fd the child will see for the writer placed at
// extraIdx in cmd.ExtraFiles (0-based).
func progressPipeArg(extraIdx int) []string {
	return []string{"-progress", "pipe:" + strconv.Itoa(3+extraIdx)}
}
