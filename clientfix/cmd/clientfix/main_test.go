package main

import (
	"net/http"
	"strings"
	"testing"
)

// Real ATV-8.45 decision shapes (trimmed) captured from prod vcflogs.
const copyDecision = `<MediaContainer size="1">
  <Video>
    <Media id="474527" selected="1" container="mkv">
      <Part id="528632" decision="transcode" selected="1" container="mkv">
        <Stream streamType="1" decision="copy" codec="hevc"/>
        <Stream streamType="2" decision="copy" codec="ac3"/>
      </Part>
    </Media>
  </Video>
</MediaContainer>`

const transcodeDecision = `<MediaContainer size="1">
  <Video>
    <Media id="442478" selected="1" container="mkv">
      <Part id="496583" decision="transcode" selected="1" container="mkv">
        <Stream streamType="1" decision="transcode" encoder="hevc_vaapi"/>
        <Stream streamType="2" decision="copy" codec="eac3"/>
      </Part>
    </Media>
  </Video>
</MediaContainer>`

// Two media versions present; the SELECTED one is the copy.
const multiVersionCopy = `<MediaContainer size="1">
  <Video>
    <Media id="442478" container="mkv">
      <Part decision="transcode"><Stream streamType="1" decision="transcode"/></Part>
    </Media>
    <Media id="474527" selected="1" container="mkv">
      <Part selected="1" decision="transcode"><Stream streamType="1" decision="copy"/></Part>
    </Media>
  </Video>
</MediaContainer>`

func TestVideoIsTranscode(t *testing.T) {
	cases := []struct {
		name string
		xml  string
		want bool
	}{
		{"copy/direct-stream", copyDecision, false},
		{"re-encode", transcodeDecision, true},
		{"multi-version selected=copy", multiVersionCopy, false},
		{"garbage fails open to false", "<not xml", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := videoIsTranscode([]byte(c.xml)); got != c.want {
				t.Fatalf("videoIsTranscode = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRewriteContainer(t *testing.T) {
	h := http.Header{}
	h.Set("X-Plex-Client-Profile-Extra",
		"add-transcode-target(type=videoProfile&context=streaming&protocol=hls&container=mkv&videoCodec=h264,hevc&replace=true)+add-transcode-target-settings(CopyMatroskaAttachments=true)")
	out := rewriteContainer(h, "mp4")
	got := out.Get("X-Plex-Client-Profile-Extra")
	if !strings.Contains(got, "container=mp4") || strings.Contains(got, "container=mkv") {
		t.Fatalf("container not flipped: %s", got)
	}
	// Must NOT touch CopyMatroskaAttachments (no "container=mkv" token there).
	if !strings.Contains(got, "CopyMatroskaAttachments=true") {
		t.Fatalf("unexpectedly altered settings: %s", got)
	}
	// Original header must be unchanged (Clone, not mutate).
	if strings.Contains(h.Get("X-Plex-Client-Profile-Extra"), "container=mp4") {
		t.Fatal("rewriteContainer mutated the original header")
	}
}

func TestAppleTV845(t *testing.T) {
	mk := func(prod, ver string) http.Header {
		h := http.Header{}
		h.Set("X-Plex-Product", prod)
		h.Set("X-Plex-Version", ver)
		return h
	}
	cases := []struct {
		prod, ver string
		want      bool
	}{
		{"Plex for Apple TV", "8.45", true},
		{"Plex for Apple TV", "8.45.9684", true},
		{"Plex for Apple TV", "8.46", false},
		{"Plex for Android", "8.45", false},
		{"Plex Web", "4.0", false},
	}
	for _, c := range cases {
		if got := appleTV845(mk(c.prod, c.ver)); got != c.want {
			t.Errorf("appleTV845(%q,%q) = %v, want %v", c.prod, c.ver, got, c.want)
		}
	}
}
