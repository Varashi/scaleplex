package main

import (
	"testing"
)

func TestSelectDialect(t *testing.T) {
	cases := []struct {
		name        string
		envBackend  string
		wantBackend string
	}{
		{"unset defaults to vaapi", "", "vaapi"},
		{"vaapi", "vaapi", "vaapi"},
		{"VAAPI uppercase", "VAAPI", "vaapi"},
		{"vaapi padded", "  vaapi  ", "vaapi"},
		{"nvidia", "nvidia", "nvidia"},
		{"NVIDIA uppercase", "NVIDIA", "nvidia"},
		{"unknown falls back to vaapi", "intel-qsv", "vaapi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WORKER_BACKEND", tc.envBackend)
			got := selectDialect().backendName()
			if got != tc.wantBackend {
				t.Fatalf("WORKER_BACKEND=%q: got backend %q, want %q", tc.envBackend, got, tc.wantBackend)
			}
		})
	}
	// "auto" is tested only for the negative path here — positive
	// (/dev/nvidia0 present → nvidiaDialect) requires the dev box
	// or a CDI-mocked fs and lives in integration tests.
	t.Run("auto without /dev/nvidia0 falls back to vaapi", func(t *testing.T) {
		t.Setenv("WORKER_BACKEND", "auto")
		if got := selectDialect().backendName(); got != "vaapi" {
			t.Fatalf("auto without /dev/nvidia0: got %q, want vaapi", got)
		}
	})
}

func TestNvidiaDialect_EncoderMap(t *testing.T) {
	d := nvidiaDialect{}
	want := map[string]string{
		"libx264": "h264_nvenc",
		"libx265": "hevc_nvenc",
	}
	got := d.encoderMap()
	if len(got) != len(want) {
		t.Fatalf("encoderMap size: got %d, want %d (%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("encoderMap[%q]: got %q, want %q", k, got[k], v)
		}
	}
}

func TestNvidiaDialect_SharedMaps(t *testing.T) {
	// decoderMap + hwDecodeShortCodecs are intentionally shared with
	// VAAPI — Plex's SW decoder names + HW-decode short codecs don't
	// depend on the worker's HW backend.
	v := vaapiDialect{}
	n := nvidiaDialect{}
	if len(v.decoderMap()) != len(n.decoderMap()) {
		t.Errorf("decoderMap diverges: vaapi=%d nvidia=%d", len(v.decoderMap()), len(n.decoderMap()))
	}
	for k, vv := range v.decoderMap() {
		if n.decoderMap()[k] != vv {
			t.Errorf("decoderMap[%q]: vaapi=%q nvidia=%q", k, vv, n.decoderMap()[k])
		}
	}
	if len(v.hwDecodeShortCodecs()) != len(n.hwDecodeShortCodecs()) {
		t.Errorf("hwDecodeShortCodecs diverges: vaapi=%d nvidia=%d", len(v.hwDecodeShortCodecs()), len(n.hwDecodeShortCodecs()))
	}
	for k := range v.hwDecodeShortCodecs() {
		if _, ok := n.hwDecodeShortCodecs()[k]; !ok {
			t.Errorf("hwDecodeShortCodecs[%q]: present on vaapi, missing on nvidia", k)
		}
	}
}
