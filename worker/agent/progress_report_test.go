package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two ffmpeg-shaped progress blocks back-to-back — the reporter must
// emit one query-string PUT per `progress=...` line, body empty.
const sampleStream = `frame=15
fps=15.0
bitrate=1024kbits/s
total_size=2048
out_time_us=1000000
out_time_ms=1000
out_time=00:00:01.000000
dup_frames=0
drop_frames=0
speed=1.0x
progress=continue
frame=30
fps=15.0
bitrate=2048kbits/s
total_size=4096
out_time_us=2000000
out_time_ms=2000
out_time=00:00:02.000000
dup_frames=0
drop_frames=0
speed=2.0x
progress=end
`

type capturedReq struct {
	method string
	path   string
	query  url.Values
	bodyN  int
}

func newCaptureServer(t *testing.T) (*httptest.Server, *[]capturedReq, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var reqs []capturedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, capturedReq{method: r.Method, path: r.URL.Path, query: r.URL.Query(), bodyN: len(b)})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return srv, &reqs, &mu
}

// Reporter must produce one query-string PUT per progress block, no
// body — matching Plex Transcoder's wire format.
func TestRunProgressReporter_OnePUTPerBlock_QueryString(t *testing.T) {
	srv, reqs, mu := newCaptureServer(t)
	defer srv.Close()

	pr, pw := io.Pipe()
	go func() { pw.Write([]byte(sampleStream)); pw.Close() }()

	rc := reportContext{
		URL:       srv.URL + "/sess/uuid/progress",
		SessionID: "test",
		DurationS: 100,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runProgressReporter(ctx, pr, rc)

	mu.Lock()
	defer mu.Unlock()
	if len(*reqs) != 2 {
		t.Fatalf("expected 2 PUTs, got %d", len(*reqs))
	}
	for i, r := range *reqs {
		if r.method != http.MethodPut {
			t.Errorf("PUT[%d] method=%s, want PUT", i, r.method)
		}
		if r.bodyN != 0 {
			t.Errorf("PUT[%d] body=%d bytes, want 0 (Plex expects query-only)", i, r.bodyN)
		}
		if r.path != "/sess/uuid/progress" {
			t.Errorf("PUT[%d] path=%s, want /sess/uuid/progress", i, r.path)
		}
		if r.query.Get("progress") == "" {
			t.Errorf("PUT[%d] missing progress=", i)
		}
	}
	// 1s of 100s = 1.0%; speed=1.0; remaining=99
	q0 := (*reqs)[0].query
	if q0.Get("progress") != "1.0" {
		t.Errorf("PUT[0] progress=%q want 1.0", q0.Get("progress"))
	}
	if q0.Get("size") != "2048" {
		t.Errorf("PUT[0] size=%q want 2048", q0.Get("size"))
	}
	if q0.Get("speed") != "1" {
		t.Errorf("PUT[0] speed=%q want 1", q0.Get("speed"))
	}
	if q0.Get("remaining") != "99" {
		t.Errorf("PUT[0] remaining=%q want 99", q0.Get("remaining"))
	}
}

func TestRunProgressReporter_EmptyURL(t *testing.T) {
	pr, pw := io.Pipe()
	go pw.Close()
	done := make(chan struct{})
	go func() {
		runProgressReporter(context.Background(), pr, reportContext{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reporter blocked on empty url")
	}
}

func TestProgressPipeArg(t *testing.T) {
	got := progressPipeArg(0)
	if len(got) != 2 || got[0] != "-progress" || got[1] != "pipe:3" {
		t.Errorf("extraIdx=0 → %v, want [-progress pipe:3]", got)
	}
	got = progressPipeArg(2)
	if got[1] != "pipe:5" {
		t.Errorf("extraIdx=2 → %v, want pipe:5", got)
	}
}

// sendPrelude must fire one duration PUT, one streamDetail per output
// stream, and one dimensions PUT for the first video stream.
func TestSendPrelude_FullSet(t *testing.T) {
	srv, reqs, mu := newCaptureServer(t)
	defer srv.Close()

	rc := reportContext{
		URL:       srv.URL + "/sess/uuid/progress",
		SessionID: "test",
		DurationS: 6461.876,
		Streams: []outputStream{
			{Index: 0, ID: 0, Codec: "h264", Type: "video", Width: 1280, Height: 720, FrameRate: 23.976, Profile: "Main"},
			{Index: 1, ID: 0, Codec: "aac", Type: "audio", Channels: 2, Layout: "stereo", SampleRate: 48000, Language: "eng"},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sendPrelude(ctx, &http.Client{Timeout: 2 * time.Second}, rc)

	mu.Lock()
	defer mu.Unlock()
	// 1 duration + 2× streamDetail + 2× stream + 1 dimensions = 6
	if len(*reqs) != 6 {
		t.Fatalf("want 6 PUTs (duration + 2× streamDetail + 2× stream + dimensions), got %d: %+v", len(*reqs), *reqs)
	}
	// duration first
	if got := (*reqs)[0].query.Get("duration"); got == "" {
		t.Errorf("PUT[0] missing duration: %v", (*reqs)[0])
	}
	// streamDetail PUTs
	var sawVideo, sawAudio, sawDim bool
	for _, r := range *reqs {
		if strings.HasSuffix(r.path, "/streamDetail") {
			switch r.query.Get("type") {
			case "video":
				sawVideo = true
				if r.query.Get("codec") != "h264" {
					t.Errorf("video streamDetail codec=%s want h264", r.query.Get("codec"))
				}
				if r.query.Get("width") != "1280" || r.query.Get("height") != "720" {
					t.Errorf("video streamDetail w/h=%s/%s want 1280/720", r.query.Get("width"), r.query.Get("height"))
				}
			case "audio":
				sawAudio = true
				if r.query.Get("language") != "eng" {
					t.Errorf("audio streamDetail language=%s want eng", r.query.Get("language"))
				}
			}
		}
		if r.query.Get("width") != "" && r.query.Get("height") != "" && !strings.HasSuffix(r.path, "/streamDetail") {
			sawDim = true
		}
		if r.bodyN != 0 {
			t.Errorf("PUT %s carried body=%d bytes; want 0", r.path, r.bodyN)
		}
	}
	if !sawVideo {
		t.Error("missing video streamDetail")
	}
	if !sawAudio {
		t.Error("missing audio streamDetail")
	}
	if !sawDim {
		t.Error("missing dimensions PUT")
	}
}

func TestExtractOutputStreams(t *testing.T) {
	args := []string{
		"-codec:0", "libdav1d", // INPUT codec, must be skipped
		"-i", "/x.mkv",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=544:format=nv12[1];[1]hwupload[2]",
		"-map", "[2]",
		"-codec:0", "h264_vaapi",
		"-codec:1", "aac",
		"-metadata:s:1", "language=eng",
		"-b:1", "256k",
	}
	got := extractOutputStreams(args)
	if len(got) != 2 {
		t.Fatalf("want 2 streams, got %d: %+v", len(got), got)
	}
	if got[0].Index != 0 || got[0].Codec != "h264" || got[0].Type != "video" || got[0].Width != 1280 || got[0].Height != 544 {
		t.Errorf("video stream wrong: %+v", got[0])
	}
	if got[1].Index != 1 || got[1].Codec != "aac" || got[1].Type != "audio" || got[1].Language != "eng" {
		t.Errorf("audio stream wrong: %+v", got[1])
	}
}

// rc.URL ends with `?X-Plex-Token=…` in production. The streamDetail
// PUT must append /streamDetail to the PATH, not after the query
// string, AND the token must survive — otherwise PMS 401s.
func TestSendPrelude_TokenPreserved(t *testing.T) {
	srv, reqs, mu := newCaptureServer(t)
	defer srv.Close()

	rc := reportContext{
		URL:       srv.URL + "/sess/uuid/progress?X-Plex-Token=secret",
		SessionID: "test",
		DurationS: 100,
		Streams:   []outputStream{{Index: 0, Codec: "h264", Type: "video", Width: 1280, Height: 720}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sendPrelude(ctx, &http.Client{Timeout: 2 * time.Second}, rc)

	mu.Lock()
	defer mu.Unlock()
	if len(*reqs) == 0 {
		t.Fatal("no PUTs captured")
	}
	for _, r := range *reqs {
		if r.query.Get("X-Plex-Token") != "secret" {
			t.Errorf("PUT %s lost token; query=%v", r.path, r.query)
		}
	}
	// streamDetail must be at /sess/uuid/progress/streamDetail, not
	// jammed inside the query string.
	sawStreamDetail := false
	for _, r := range *reqs {
		if strings.HasSuffix(r.path, "/progress/streamDetail") {
			sawStreamDetail = true
		}
	}
	if !sawStreamDetail {
		t.Errorf("streamDetail PUT missing from %v", *reqs)
	}
}

func TestExtractInputPath(t *testing.T) {
	args := []string{"-x", "y", "-i", "/m.mkv", "-c", "copy"}
	if got := extractInputPath(args); got != "/m.mkv" {
		t.Errorf("got %q want /m.mkv", got)
	}
	if got := extractInputPath([]string{"-x", "y"}); got != "" {
		t.Errorf("got %q want empty", got)
	}
}
