package main

import "testing"

// #145: the rewriter resolves PMS stream specifiers at match time instead of
// rewriting `#0xNN` → ordinal upfront. These pin the matcher across all three
// forms PMS emits, plus the audio-not-video guard.
func TestStreamSpecIndex(t *testing.T) {
	// Decoder hint (#0x01=video, #0x02=audio), HW flags, encoder ordinal.
	args := []string{
		"-codec:#0x01", "hevc",
		"-hwaccel:#0x01", "vaapi",
		"-codec:#0x02", "aac",
		"-i", "src.mkv",
		"-codec:0", "hevc_vaapi",
	}
	if i := streamSpecIndex(args, "-hwaccel", 0, 0); i != 2 {
		t.Errorf("hwaccel #0x01 → video ordinal 0: got idx %d, want 2", i)
	}
	// Decoder -codec for video resolves the #0x01 hint at idx 0…
	if i := streamSpecIndex(args, "-codec", 0, 0); i != 0 {
		t.Errorf("decoder -codec video: got idx %d, want 0", i)
	}
	// …and the post-`-i` scan finds the ordinal encoder, not the hex decoder.
	in := indexOfArg(args, "-i", 0)
	if i := streamSpecIndex(args, "-codec", 0, in+1); i != 8 {
		t.Errorf("encoder -codec:0 after -i: got idx %d, want 8", i)
	}
	// Audio (#0x02 → ordinal 1) resolves to its own slot…
	if i := streamSpecIndex(args, "-codec", 1, 0); i != 4 {
		t.Errorf("audio #0x02 → ordinal 1: got idx %d, want 4", i)
	}
	// …and a video-ordinal-0 query is the #0x01 decoder at idx 0, never audio.
	if i := streamSpecIndex(args, "-codec", 0, 0); i != 0 {
		t.Errorf("video ordinal 0 must be the #0x01 decoder at idx 0; got %d", i)
	}
	// flagBase must not bleed across the `:` boundary into a sibling flag.
	hov := []string{"-hwaccel_output_format:#0x01", "vaapi", "-hwaccel:#0x01", "vaapi"}
	if i := streamSpecIndex(hov, "-hwaccel", 0, 0); i != 2 {
		t.Errorf("-hwaccel must not match -hwaccel_output_format:; got idx %d, want 2", i)
	}
}

func TestStreamSpecSelectsOrdinal(t *testing.T) {
	idMap := map[string]int{"0x01": 0, "0x02": 1}
	cases := []struct {
		spec string
		ord  int
		want bool
	}{
		{"0", 0, true},
		{"1", 0, false},
		{"#0x01", 0, true},  // first-seen id → ordinal 0
		{"#0x02", 0, false}, // audio
		{"#0x02", 1, true},
		{"#0x99", 0, false}, // unknown id
		{"v:0", 0, true},    // type+index video
		{"V:0", 0, true},
		{"a:0", 0, false}, // audio type never matches a video ordinal
		{"s:0", 0, false},
	}
	for _, c := range cases {
		if got := streamSpecSelectsOrdinal(c.spec, c.ord, idMap); got != c.want {
			t.Errorf("streamSpecSelectsOrdinal(%q, %d) = %v, want %v", c.spec, c.ord, got, c.want)
		}
	}
}

func TestStreamIDOrdinalMap(t *testing.T) {
	// First-occurrence order across flags + filtergraph refs.
	args := []string{
		"-codec:#0x01", "hevc", "-codec:#0x02", "aac",
		"-filter_complex", "[0:#0x01]hwupload[0]",
	}
	m := streamIDOrdinalMap(args)
	if m["0x01"] != 0 || m["0x02"] != 1 {
		t.Errorf("id→ordinal map = %v, want 0x01:0 0x02:1", m)
	}
	// Ordinal-form argv allocates no map.
	if m := streamIDOrdinalMap([]string{"-codec:0", "hevc", "-i", "x"}); m != nil {
		t.Errorf("ordinal-form argv built a map: %v", m)
	}
}

// The graph-reshape entry points accept a `[0:#0xNN]` video input label, so a
// (hypothetical, not-in-corpus) seeked-or-plain HW-decode text sub-burn graph
// in PMS's pristine hex form still reshapes instead of bailing to SW-inlineass.
func TestHexFilterGraphReshapeEntry(t *testing.T) {
	if !reVideoInput0.MatchString("[0:#0x11]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1]") {
		t.Error("hex input label [0:#0x11] not recognized at reshape entry")
	}
	if !reVideoInput0.MatchString("[0:0]scale=w=1920:h=1080[0]") {
		t.Error("ordinal input label [0:0] regressed")
	}
	// A non-zero ordinal stream must NOT read as video input 0.
	if reVideoInput0.MatchString("[0:5]scale=w=1:h=1") {
		t.Error("[0:5] wrongly matched video input 0")
	}
	if !graphLeadsWithVideoInput0("[0:#0x11]scale_vaapi=w=1:h=1[0]", "scale_vaapi=") {
		t.Error("suffix match failed on hex input label")
	}
}
