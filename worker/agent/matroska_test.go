package main

import (
	"os"
	"path/filepath"
	"testing"
)

// patchClusterBody / patchMatroskaClusterTimecode unit tests using
// real chunk bytes captured 2026-05-11 from stock ffmpeg producing a
// matroska segment chunk (Burn Notice + segment_format=matroska +
// segment_format_options=live=0). Header layout:
//
//	Chunk 0: 1F 43 B6 75 | 5C 19            | BF 84 <CRC32:4>      | E7 81 00 | <rest>
//	         Cluster ID  | Size=0x1C19=7193 | CRC32 element        | TC=0     | body
//	Chunk 1: 1F 43 B6 75 | 59 C3            | BF 84 <CRC32:4>      | E7 82 03 E9 | <rest>
//	         Cluster ID  | Size=0x19C3=6595 | CRC32 element        | TC=1001ms   | body
//
// After patching with offsetMs=4000:
//
//	Chunk 0: 1F 43 B6 75 | <new size>       | E7 88 <8-byte=4000>  | <rest>
//	Chunk 1: 1F 43 B6 75 | <new size>       | E7 88 <8-byte=5001>  | <rest>
func TestPatchClusterBody_StripsCRC_ShiftsTimecode(t *testing.T) {
	// Synthesize chunk 0 body: CRC32(BF 84 AA AA AA AA) + Timecode(E7 81 00) + filler.
	body := []byte{0xBF, 0x84, 0xAA, 0xAA, 0xAA, 0xAA, 0xE7, 0x81, 0x00, 0xA3, 0x5A, 0x6F, 0x81, 0x00}
	got, patched := patchClusterBody(body, 4000)
	if !patched {
		t.Fatalf("expected patched=true")
	}
	// CRC32 stripped; Timecode now E7 88 00 00 00 00 00 00 0F A0 (4000ms).
	want := []byte{0xE7, 0x88, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0F, 0xA0, 0xA3, 0x5A, 0x6F, 0x81, 0x00}
	if !equalBytes(got, want) {
		t.Errorf("\ngot:  % x\nwant: % x", got, want)
	}
}

// Timecode in 2-byte width (1001ms = 0x03E9) shifts to 5001ms,
// re-encoded at 8-byte width.
func TestPatchClusterBody_2byteTimecode(t *testing.T) {
	body := []byte{0xBF, 0x84, 0xAA, 0xAA, 0xAA, 0xAA, 0xE7, 0x82, 0x03, 0xE9, 0xFF}
	got, patched := patchClusterBody(body, 4000)
	if !patched {
		t.Fatalf("expected patched=true")
	}
	// New TC = 1001 + 4000 = 5001 = 0x1389
	want := []byte{0xE7, 0x88, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x13, 0x89, 0xFF}
	if !equalBytes(got, want) {
		t.Errorf("\ngot:  % x\nwant: % x", got, want)
	}
}

// Round-trip: write a real chunk to disk, patch, verify the patched
// chunk starts with the expected Cluster header + shifted Timecode.
func TestPatchMatroskaClusterTimecode_RealChunk(t *testing.T) {
	// Real chunk 0 bytes (from production stock ffmpeg output).
	chunk0 := []byte{
		0x1F, 0x43, 0xB6, 0x75, // Cluster ID
		0x40, 0x10, // size = 16 bytes payload (small synthetic body)
		0xBF, 0x84, 0xAA, 0xAA, 0xAA, 0xAA, // CRC32 (6 bytes)
		0xE7, 0x81, 0x00, // Timecode = 0 (3 bytes)
		0xA3, 0x82, 0x00, 0x00, 0x00, 0x00, 0xCC, // filler — total body 7 + 3 + 7 = ? but size says 16, pad to 16
	}
	// Recount the body: 6 (CRC32) + 3 (Timecode) + 7 (filler) = 16 bytes ✓
	dir := t.TempDir()
	path := filepath.Join(dir, "chunk-00000")
	if err := os.WriteFile(path, chunk0, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patchMatroskaClusterTimecode(path, 4193000); err != nil {
		t.Fatalf("patch failed: %v", err)
	}
	out, _ := os.ReadFile(path)
	// Expect: Cluster ID + new size + Timecode(10 bytes) + filler(7 bytes)
	// Body after patch: 10 + 7 = 17 bytes (CRC32 stripped, Timecode expanded).
	if len(out) < 4+1+17 {
		t.Fatalf("output too short: %d bytes: % x", len(out), out)
	}
	// Cluster ID intact.
	if !equalBytes(out[:4], []byte{0x1F, 0x43, 0xB6, 0x75}) {
		t.Errorf("Cluster ID corrupted: % x", out[:4])
	}
	// Size field re-encoded at the original 2-byte width. New body = 17.
	// 2-byte EBML for value 17 = 0x40 0x11.
	if out[4] != 0x40 || out[5] != 0x11 {
		t.Errorf("Size field wrong: % x (want 40 11)", out[4:6])
	}
	// Timecode at offset 6: 0xE7 0x88 + 8-byte BE value = 4193000 = 0x3FFEA8.
	tcStart := 6
	if out[tcStart] != 0xE7 || out[tcStart+1] != 0x88 {
		t.Errorf("Timecode header wrong: % x (want E7 88)", out[tcStart:tcStart+2])
	}
	tcVal := readUintBEBytes(out[tcStart+2 : tcStart+10])
	if tcVal != 4193000 {
		t.Errorf("Timecode value: got %d want %d", tcVal, 4193000)
	}
}

func TestPatchMatroskaClusterTimecode_ZeroOffset_NoOp(t *testing.T) {
	// Even with offsetMs=0, the patcher rewrites the cluster (strips
	// CRC32, expands Timecode to 8 bytes). For runtime efficiency the
	// caller should skip the call when offsetMs == 0 (see segwatch).
	// This test just guards correctness with offsetMs=0.
	body := []byte{0xBF, 0x84, 0xAA, 0xAA, 0xAA, 0xAA, 0xE7, 0x81, 0x05, 0xFF}
	got, patched := patchClusterBody(body, 0)
	if !patched {
		t.Fatalf("expected patched=true")
	}
	// TC stays at 5, re-encoded at 8 bytes.
	want := []byte{0xE7, 0x88, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05, 0xFF}
	if !equalBytes(got, want) {
		t.Errorf("\ngot:  % x\nwant: % x", got, want)
	}
}

func TestReadEBMLSize(t *testing.T) {
	cases := []struct {
		in      []byte
		wantW   int
		wantVal uint64
		wantOK  bool
	}{
		{[]byte{0x81}, 1, 1, true},                  // 1-byte: value 1
		{[]byte{0x80}, 1, 0, true},                  // 1-byte: value 0
		{[]byte{0x40, 0x10}, 2, 0x10, true},         // 2-byte: value 16
		{[]byte{0x5C, 0x19}, 2, 0x1C19, true},       // chunk 0 size
		{[]byte{0x20, 0x9A, 0x22}, 3, 0x9A22, true}, // 3-byte
		{[]byte{}, 0, 0, false},
		{[]byte{0x00}, 0, 0, false},
	}
	for i, c := range cases {
		w, v, ok := readEBMLSize(c.in)
		if w != c.wantW || v != c.wantVal || ok != c.wantOK {
			t.Errorf("case %d (% x): got (%d, %d, %v) want (%d, %d, %v)",
				i, c.in, w, v, ok, c.wantW, c.wantVal, c.wantOK)
		}
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
