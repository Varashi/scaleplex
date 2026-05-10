package main

import (
	"testing"
)

// Plex Transcoder ships with the dovi_rpu BSF (libavcodec/bsf/dovi_rpu.c)
// and PMS emits `-bsf:N dovi_rpu=strip=1` to drop the DV enhancement
// layer when serving a non-DV-capable client. The rewriter must pass
// the flag+value pair through unchanged — we don't recognise the bsf,
// but the worker uses Plex Transcoder which has it compiled in.
//
// Corpus shape 2026-05-10 (6 entries) is HEVC stream-copy (no
// transcode), but dovi_rpu can also appear on a real HW-decode +
// HW-encode chain (DV → HDR10 transcode for an HDR-only client).
// The fixture uses the HW transcode shape so we exercise the
// rewriter's main path; pass-through must still hold.
//
// Defensive — current behaviour is correct, this guards against a
// future rewriter pass deciding it knows better.
func TestRewriter_DoviRPU_BSFPassesThrough(t *testing.T) {
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi",
		"-hwaccel_output_format:0", "vaapi",
		"-hwaccel_device:0", "vaapi",
		"-analyzeduration", "20000000",
		"-probesize", "20000000",
		"-i", "/media/Movies/DVTitle.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_hw_device", "vaapi",
		"-filter_complex", "[0:0]scale_vaapi=w=1920:h=1080:format=nv12[1]",
		"-map", "[1]",
		"-codec:0", "hevc_vaapi",
		"-qp:0", "20",
		"-bufsize:0", "16000k",
		"-r:0", "23.976",
		"-sei:0", "-a53_cc",
		"-bsf:0", "dovi_rpu=strip=1",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	flagIdx := indexOfArg(out.Args, "-bsf:0", 0)
	if flagIdx < 0 {
		t.Fatalf("-bsf:0 dropped from output: %v", out.Args)
	}
	if got := out.Args[flagIdx+1]; got != "dovi_rpu=strip=1" {
		t.Fatalf("bsf value mutated: got %q want %q", got, "dovi_rpu=strip=1")
	}
}
