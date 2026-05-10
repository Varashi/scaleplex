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

// MHA-style argv: client switched to track 2 (Japanese audio), PMS
// emitted -codec:2 eac3_eae. Pre-fix the audioCodecFlag whitelist
// only matched :0/:1, leaving eac3_eae intact and ffmpeg bailed
// "Unknown decoder 'eac3_eae'" exit 8 (live repro 2026-05-10).
func TestRewriter_EAE_MultiStreamIndex(t *testing.T) {
	args := []string{
		"-codec:0", "libdav1d",
		"-codec:2", "eac3_eae",
		"-eae_prefix:2", "anytoken_",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/media/Anime/MHA.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]",
		"-codec:0", "libx264",
		"-crf:0", "16",
		"-preset:0", "veryfast",
		"-codec:2", "eac3_eae",
		"-b:2", "256k",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "audio:eac3_eae->eac3") {
		t.Fatalf("missing audio:eac3_eae->eac3: %v", out.Changes)
	}
	if !containsString(out.Changes, "drop:-eae_prefix:2") {
		t.Fatalf("missing drop:-eae_prefix:2: %v", out.Changes)
	}
	if containsString(out.Args, "eac3_eae") {
		t.Fatal("eac3_eae must not survive on stream :2")
	}
}

// Atmos remux: PMS emits -codec:1 truehd_eae for TrueHD passthrough.
// Stock truehd encoder is experimental and very slow; rewriter falls
// back to eac3 so the session at least produces audio.
func TestRewriter_EAE_TrueHDFallback(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi",
		"-codec:1", "truehd_eae",
		"-eae_prefix:1", "anytoken_",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/media/Movies/Atmos.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1]",
		"-map", "[1]",
		"-codec:0", "hevc_vaapi",
		"-codec:1", "truehd_eae",
		"-b:1", "768k",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "audio:truehd_eae->eac3") {
		t.Fatalf("missing audio:truehd_eae->eac3: %v", out.Changes)
	}
	if containsString(out.Args, "truehd_eae") {
		t.Fatal("truehd_eae must not survive the rewrite")
	}
}

// TrueHD source where the audio is decoded but never re-encoded
// (`-codec:N copy` on output, or audio mapped through filter that
// doesn't transcode). Pre-2026-05-10-PM: rewriter mapped the input
// `-codec:1 truehd_eae` to `eac3`, asking ffmpeg to decode TrueHD
// bytes with the eac3 decoder → bitstream parse fail → exit 8 silent.
// Post-fix: input position maps to `truehd` (real stock decoder).
func TestRewriter_EAE_TrueHDInputDecodeUsesTrueHD(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi",
		"-codec:1", "truehd_eae",
		"-eae_prefix:1", "tok_",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/media/Movies/TrueHDRemux.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1]",
		"-map", "[1]",
		"-codec:0", "hevc_vaapi",
		// Audio output: copy passthrough — no encoder swap needed.
		"-map", "0:1",
		"-codec:1", "copy",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "audio:truehd_eae->truehd") {
		t.Fatalf("input-side truehd_eae must map to truehd: %v", out.Changes)
	}
	// The first -codec:1 (BEFORE -i) is an INPUT decoder hint; verify
	// it landed at "truehd", not "eac3".
	iIdx := indexOfArg(out.Args, "-i", 0)
	for i := 0; i < iIdx-1; i++ {
		if out.Args[i] == "-codec:1" {
			if out.Args[i+1] != "truehd" {
				t.Fatalf("input -codec:1 = %q, want truehd: %v", out.Args[i+1], out.Args)
			}
			break
		}
	}
}

// Both directions present in one argv: input -codec:1 truehd_eae +
// output -codec:1 truehd_eae. Direction-aware swap MUST land them
// differently — input becomes `truehd` (decoder works), output
// becomes `eac3` (no stock truehd encoder, lossy fallback).
func TestRewriter_EAE_TrueHDBothDirections(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi",
		"-codec:1", "truehd_eae", // INPUT decoder
		"-eae_prefix:1", "tok_",
		"-i", "/media/Movies/Atmos.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_complex", "[0:0]hwupload[0]",
		"-map", "[0]",
		"-codec:0", "hevc_vaapi",
		"-map", "0:1",
		"-codec:1", "truehd_eae", // OUTPUT encoder
		"-b:1", "768k",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if !containsString(out.Changes, "audio:truehd_eae->truehd") {
		t.Errorf("missing input-side decode tag: %v", out.Changes)
	}
	if !containsString(out.Changes, "audio:truehd_eae->eac3") {
		t.Errorf("missing output-side encode-fallback tag: %v", out.Changes)
	}
	// truehd_eae must not survive anywhere.
	if containsString(out.Args, "truehd_eae") {
		t.Fatalf("truehd_eae must not survive: %v", out.Args)
	}
	// Verify positional mapping landed correctly.
	iIdx := indexOfArg(out.Args, "-i", 0)
	for i := 0; i < iIdx-1; i++ {
		if out.Args[i] == "-codec:1" && out.Args[i+1] != "truehd" {
			t.Fatalf("input -codec:1 = %q, want truehd", out.Args[i+1])
		}
	}
	for i := iIdx + 1; i < len(out.Args)-1; i++ {
		if out.Args[i] == "-codec:1" && out.Args[i+1] != "eac3" && out.Args[i+1] != "copy" {
			t.Fatalf("output -codec:1 = %q, want eac3", out.Args[i+1])
		}
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
	for _, k := range []string{"EAE_ROOT", "FFMPEG_EXTERNAL_LIBS"} {
		if _, ok := out.Env[k]; ok {
			t.Errorf("%s should be stripped from env (still present: %q)", k, out.Env[k])
		}
		if !containsString(out.Changes, "env:strip:"+k) {
			t.Errorf("missing env:strip:%s change: %v", k, out.Changes)
		}
	}
	// X_PLEX_TOKEN is intentionally KEPT — the worker-side progress
	// reporter appends it as ?X-Plex-Token=... so PMS authorises the
	// per-session PUT to /video/:/transcode/session/<token>/<uuid>/progress.
	if out.Env["X_PLEX_TOKEN"] != "secret" {
		t.Fatalf("X_PLEX_TOKEN should be preserved, got %q", out.Env["X_PLEX_TOKEN"])
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
