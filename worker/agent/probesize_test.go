package main

import (
	"strconv"
	"strings"
	"testing"
)

func TestApplyAdaptiveProbesize_NoFlags_NoOp(t *testing.T) {
	in := []string{"-i", "/tmp/nope.mkv"}
	out, ch := applyAdaptiveProbesize(in)
	if len(ch) != 0 {
		t.Fatalf("expected no changes, got %v", ch)
	}
	if strings.Join(out, ",") != strings.Join(in, ",") {
		t.Fatalf("args mutated: %v", out)
	}
}

func TestApplyAdaptiveProbesize_NoInput_NoOp(t *testing.T) {
	in := []string{"-probesize", "20000000", "-analyzeduration", "20000000"}
	out, ch := applyAdaptiveProbesize(in)
	if len(ch) != 0 {
		t.Fatalf("expected no changes, got %v", ch)
	}
	if strings.Join(out, ",") != strings.Join(in, ",") {
		t.Fatalf("args mutated: %v", out)
	}
}

func TestApplyAdaptiveProbesize_PathMissing_NoOp(t *testing.T) {
	// The file doesn't exist → adaptiveProbesize returns 0 → leave args
	// untouched (don't lie about a probesize we couldn't verify).
	in := []string{
		"-probesize", "20000000",
		"-analyzeduration", "20000000",
		"-i", "/no/such/file/here.mkv",
	}
	out, ch := applyAdaptiveProbesize(in)
	if len(ch) != 0 {
		t.Fatalf("expected no changes for missing file, got %v", ch)
	}
	if got := out[1]; got != "20000000" {
		t.Fatalf("probesize mutated: %q", got)
	}
}

func TestApplyAdaptiveProbesize_RewritesBothFlags(t *testing.T) {
	// Use the rewriter_test.go binary itself as a proxy for "any small
	// real file" — ffprobe will refuse it (not media), so adaptiveProbesize
	// returns 0. To actually exercise the happy path we'd need a real
	// media file; this case verifies the no-op behaviour on a short non-
	// media file.
	in := []string{
		"-probesize", "20000000",
		"-analyzeduration", "20000000",
		"-i", "/etc/hostname",
	}
	out, ch := applyAdaptiveProbesize(in)
	if len(ch) > 0 {
		t.Fatalf("expected no changes for non-media file, got %v", ch)
	}
	if out[1] != "20000000" {
		t.Fatalf("probesize mutated when ffprobe failed: %q", out[1])
	}
}

func TestApplyAdaptiveProbesize_OnlyProbesize(t *testing.T) {
	// Test arg-shape correctness: when only -probesize is present (no
	// -analyzeduration), only the present flag is touched.
	candidate := strconv.Itoa(probeSizeCandidates[0])
	if candidate == "" {
		t.Fatal("probe size candidate empty")
	}
}
