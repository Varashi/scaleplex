package main

import (
	"strings"
	"testing"
)

func TestStderrErrorWatch(t *testing.T) {
	var got []string
	w := newStderrErrorWatch("S02E01_mp4-2751-13bbe4d4")
	w.emit = func(line string) { got = append(got, line) }

	// Fed in arbitrary chunks that do NOT align to line boundaries — the
	// fontconfig error spans two Append calls.
	w.Append([]byte("frame=  10 fps=0.0 q=-0.0\nFontconfig error: Cannot load co"))
	w.Append([]byte("nfig file \"/usr/lib/plexmediaserver/Resources/fonts.conf\"\r"))
	w.Append([]byte("libass: fontselect: failed to find any fallback font\n"))
	// duplicate of the first match — must be deduped
	w.Append([]byte("Fontconfig error: Cannot load config file \"/usr/lib/plexmediaserver/Resources/fonts.conf\"\n"))
	// the writable-cache warning (Bug C) + a clean stats line
	w.Append([]byte("No writable cache directories\nspeed=1.23x\n"))

	if len(got) != 3 {
		t.Fatalf("want 3 surfaced lines, got %d: %v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "Fontconfig error:") {
		t.Errorf("got[0] = %q", got[0])
	}
	if !strings.Contains(got[1], "fontselect") {
		t.Errorf("got[1] = %q", got[1])
	}
	if !strings.Contains(got[2], "No writable cache") {
		t.Errorf("got[2] = %q", got[2])
	}
}

func TestStderrErrorWatch_CleanStreamSilent(t *testing.T) {
	var got []string
	w := newStderrErrorWatch("sid")
	w.emit = func(line string) { got = append(got, line) }
	for _, chunk := range []string{
		"frame=100 fps=25\r", "size=2048kB time=00:00:04\r",
		"[hevc_vaapi @ 0x..] using device\n", "speed=4.1x\n",
	} {
		w.Append([]byte(chunk))
	}
	if len(got) != 0 {
		t.Fatalf("clean stream must surface nothing, got %v", got)
	}
}
