//go:build replay
// +build replay

// Unit coverage for the #147 bail classifier. The committed fixture
// corpus (testdata/replay-corpus) is all synthetic Optimize cells and
// carries no inlineass+HW-encoder shape, so it never exercises the
// must-reshape assertion. These table tests lock the classifier +
// allowlist directly — in particular that the PR #144 class
// (inlineass= + hevc_vaapi bailing skip:no-decoder) flips to FAIL.
package main

import "testing"

func TestClassifyShape(t *testing.T) {
	activeDialect = selectDialect() // isOptimizeRemux needs a non-nil dialect

	cases := []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "inlineass + hevc_vaapi -> hw-subburn (the #144 shape)",
			argv: []string{"-i", "in.mkv", "-filter_complex", "[0:0]inlineass=font_size=54[v]", "-codec:0", "hevc_vaapi", "out.ts"},
			want: shapeHWSubBurn,
		},
		{
			name: "inlineass + h264_nvenc -> hw-subburn (cross-backend)",
			argv: []string{"-i", "in.mkv", "-vf", "inlineass=language=en", "-c:v", "h264_nvenc", "out.ts"},
			want: shapeHWSubBurn,
		},
		{
			name: "HW encoder but no inlineass -> other",
			argv: []string{"-i", "in.mkv", "-codec:0", "hevc_vaapi", "out.ts"},
			want: shapeOther,
		},
		{
			name: "inlineass but SW encoder (libx264) -> other",
			argv: []string{"-i", "in.mkv", "-vf", "inlineass=x=1", "-c:v", "libx264", "out.ts"},
			want: shapeOther,
		},
		{
			name: "audio-only / detection -> other",
			argv: []string{"-codec:1", "aac", "-i", "in.mkv", "-codec:1", "aac", "out.aac"},
			want: shapeOther,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyShape(tc.argv); got != tc.want {
				t.Errorf("classifyShape = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBailAllowed(t *testing.T) {
	cases := []struct {
		shape, reason string
		want          bool
	}{
		// The regression #147 exists to catch: a sub-burn HW transcode that
		// bails skip:no-decoder must FAIL, not silently PASS.
		{shapeHWSubBurn, "no-decoder", false},
		{shapeHWSubBurn, "no-encoder", false},
		{shapeHWSubBurn, "no-input", false},
		// The unmodeled-graph bail is NO LONGER an accepted exception (#154):
		// the seeked select=gte+inlineass graph that seeded it is now modeled,
		// so an unmodeled bail for this shape is a regression like any other.
		{shapeHWSubBurn, TagPrefixBailHWDecodeSubUnmodeled + "[0:0]hwupload[0];...", false},
		{shapeHWSubBurn, "hw-decode-sub:unmodeled-graph:", false},
		// Permissive shapes keep today's behavior.
		{shapeOther, "no-decoder", true},
		{shapeOptimizeRemux, "no-encoder", true},
		// Unknown shape never fails.
		{"unmapped-shape", "whatever", true},
	}
	for _, tc := range cases {
		if got := bailAllowed(tc.shape, tc.reason); got != tc.want {
			t.Errorf("bailAllowed(%q, %q) = %v, want %v", tc.shape, tc.reason, got, tc.want)
		}
	}
}

func TestBailReasonOf(t *testing.T) {
	if got := bailReasonOf([]string{"scrub:x", TagPrefixSkip + "no-decoder", "other"}); got != "no-decoder" {
		t.Errorf("bailReasonOf = %q, want no-decoder", got)
	}
	if got := bailReasonOf([]string{"scrub:x", "hint:y"}); got != "" {
		t.Errorf("bailReasonOf (no bail) = %q, want empty", got)
	}
}
