package main

import (
	"strings"
	"testing"
)

// PMS-emitted argv shape for an audio-bearing transcode. eac3_eae is
// Plex Transcoder's bundled encoder and not present in stock or
// jellyfin ffmpeg; eae_prefix is its session-token marker.
var swArgsWithEAE = []string{
	"-codec:0", "libdav1d",
	"-codec:1", "eac3_eae",
	"-eae_prefix:1", "bb94kvg7m89u33mvy49s1k2y_",
	"-analyzeduration", "20000000",
	"-probesize", "20000000",
	"-i", "/media/Movies/Balls.mkv",
	"-init_hw_device", "vaapi=vaapi:",
	"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
	"-map", "[1]",
	"-codec:0", "libx264",
	"-crf:0", "16",
	"-preset:0", "veryfast",
	"-codec:1", "eac3_eae", // Plex sometimes repeats codec spec
	"-b:1", "256k",
}

func TestRewriter_EAE_AudioCodecSwap(t *testing.T) {
	out := Rewrite(swArgsWithEAE, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "audio:eac3_eae->eac3") {
		t.Fatalf("missing eac3_eae->eac3 change: %v", out.Changes)
	}
	if !containsString(out.Changes, "drop:-eae_prefix:1") {
		t.Fatalf("missing drop:-eae_prefix change: %v", out.Changes)
	}
	if containsString(out.Args, "eac3_eae") {
		t.Fatal("eac3_eae must not survive the rewrite")
	}
	if containsString(out.Args, "-eae_prefix:1") {
		t.Fatal("-eae_prefix:1 must be stripped")
	}
	// Both -codec:1 occurrences should now be eac3.
	count := 0
	for i, a := range out.Args {
		if a == "-codec:1" && i+1 < len(out.Args) && out.Args[i+1] == "eac3" {
			count++
		}
	}
	if count < 1 {
		t.Fatalf("expected -codec:1 eac3 in args: %v", out.Args)
	}
}

func TestRewriter_StripsPlexEnv(t *testing.T) {
	in := map[string]string{
		"EAE_ROOT":             "/run/plex-temp/.../EasyAudioEncoder",
		"FFMPEG_EXTERNAL_LIBS": "/config/.../Codecs/abc/",
		"X_PLEX_TOKEN":         "secret",
		"TZ":                   "Europe/Brussels",
	}
	out := Rewrite(swArgsWithEAE, in, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	for _, k := range []string{"EAE_ROOT", "FFMPEG_EXTERNAL_LIBS", "X_PLEX_TOKEN"} {
		if _, ok := out.Env[k]; ok {
			t.Errorf("%s should be stripped from env (still present: %q)", k, out.Env[k])
		}
		if !containsString(out.Changes, "env:strip:"+k) {
			t.Errorf("missing env:strip:%s change: %v", k, out.Changes)
		}
	}
	if out.Env["TZ"] != "Europe/Brussels" {
		t.Fatalf("TZ should be preserved, got %q", out.Env["TZ"])
	}
	for _, c := range out.Changes {
		if strings.HasPrefix(c, "env:strip:TZ") {
			t.Fatal("TZ should not be stripped")
		}
	}
}
