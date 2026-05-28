package main

import (
	"strings"
	"testing"
)

// composeBurnSW must emit Plex's stock software filtergraph shapes byte-for-byte
// (captured from plex-test HardwareAcceleratedCodecs=0 sessions) — the drop-in
// goal: the fork sees stock-Plex SW args, no SW-specific patch needed.
func TestComposeBurnSW_MatchesPlexShapes(t *testing.T) {
	tm := tonemapConfig{d: swDialect{}, algo: "hable"}
	cases := []struct {
		name string
		spec burnSpec
		want string
	}{
		{"sdr-nosub", burnSpec{w: "1280", h: "720"},
			"[0:0]scale=w=1280:h=720:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1]"},
		{"hdr-nosub", burnSpec{w: "1280", h: "720", hdr: true, algo: "mobius"},
			"[0:0]scale=w=1280:h=720:force_divisible_by=4[0];[0]format=p010,tonemap=mobius[1];[1]format=pix_fmts=yuv420p|nv12[2]"},
		{"sdr-text", burnSpec{w: "1280", h: "720", burnSub: true, subKind: "text", subParams: "font_scale=1.000000:language=en"},
			"[0:0]scale=w=1280:h=720:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1];[1]inlineass=font_scale=1.000000:language=en[2]"},
		{"hdr-bitmap", burnSpec{w: "1280", h: "720", hdr: true, algo: "mobius", burnSub: true, subKind: "bitmap", subSpec: "0:3"},
			"[0:3]scale=1280:720[0];[0:0]scale=w=1280:h=720:force_divisible_by=4[1];[1]format=p010,tonemap=mobius[2];[2]format=pix_fmts=yuv420p|nv12[3];[3][0]overlay[4]"},
		{"sdr-bitmap", burnSpec{w: "1920", h: "1080", burnSub: true, subKind: "bitmap", subSpec: "0:5"},
			"[0:5]scale=1920:1080[0];[0:0]scale=w=1920:h=1080:force_divisible_by=4[1];[1]format=pix_fmts=yuv420p|nv12[2];[2][0]overlay[3]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := tm.composeBurnSW(tc.spec)
			if got != tc.want {
				t.Errorf("composeBurnSW mismatch:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// A no-GPU worker downgrades a foreign VAAPI HW argv to a pure-CPU pipeline: no
// hwaccel, no HW device, SW scale + libx264, no VAAPI/CUDA literals.
func TestReshapeToSW_VAAPI_HWdec_HWenc(t *testing.T) {
	withDialect(t, swDialect{})
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/x.mkv",
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD", "-filter_hw_device", "vaapi",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1]",
		"-map", "[1]", "-codec:0", "h264_vaapi", "-qp:0", "22",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	j := strings.Join(out.Args, " ")
	for _, banned := range []string{"vaapi", "-hwaccel", "-init_hw_device", "hwupload", "scale_vaapi", "h264_vaapi", "renderD128"} {
		if strings.Contains(j, banned) {
			t.Errorf("HW literal %q leaked into SW output: %s", banned, j)
		}
	}
	if !containsString(out.Args, "libx264") {
		t.Errorf("encoder not downgraded to libx264: %v", out.Args)
	}
	if !strings.Contains(j, "[0:0]scale=w=1280:h=720:force_divisible_by=4") {
		t.Errorf("SW scale not emitted: %s", j)
	}
}

// HW-decode + SW-encode hybrid on a no-GPU worker: strip the HW decode, keep SW.
func TestReshapeToSW_Hybrid_StripsHWDecode(t *testing.T) {
	withDialect(t, swDialect{})
	args := []string{
		"-codec:0", "av1",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/x.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]scale=w=1280:h=720:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1]",
		"-map", "[1]", "-codec:0", "libx264",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	j := strings.Join(out.Args, " ")
	for _, banned := range []string{"-hwaccel", "-init_hw_device", "vaapi"} {
		if strings.Contains(j, banned) {
			t.Errorf("HW decode not stripped (%q): %s", banned, j)
		}
	}
	if !containsString(out.Changes, "to-sw:strip-hwdecode") {
		t.Errorf("missing strip-hwdecode tag: %v", out.Changes)
	}
}

// HEVC cross-backend: a GPU PMS emits hevc_vaapi; a CPU worker downgrades to
// libx265 (the "GPU PMS + CPU worker + HEVC" case).
func TestReshapeToSW_HEVC_to_libx265(t *testing.T) {
	withDialect(t, swDialect{})
	args := []string{
		"-codec:0", "hevc", "-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
		"-i", "/media/x.mkv", "-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1]",
		"-map", "[1]", "-codec:0", "hevc_vaapi",
	}
	out := Rewrite(args, nil, nil)
	if !containsString(out.Args, "libx265") || containsString(out.Args, "hevc_vaapi") {
		t.Errorf("hevc_vaapi not downgraded to libx265: %v", out.Args)
	}
}

// An already-software argv (SW PMS aligned with SW worker) is honored as-is —
// no filter rewrite, minimal divergence.
func TestReshapeToSW_AlreadySW_Honored(t *testing.T) {
	withDialect(t, swDialect{})
	fc := "[0:0]scale=w=1280:h=720:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1]"
	args := []string{
		"-codec:0", "libdav1d", "-i", "/media/x.mkv",
		"-filter_complex", fc, "-map", "[1]", "-codec:0", "libx264",
	}
	out := Rewrite(args, nil, nil)
	if !out.Applied {
		t.Fatalf("not applied: %v", out.Changes)
	}
	if containsString(out.Changes, "to-sw:strip-hwdecode") {
		t.Errorf("already-SW argv should not be reshaped: %v", out.Changes)
	}
	var got string
	for i, a := range out.Args {
		if a == "-filter_complex" && i+1 < len(out.Args) {
			got = out.Args[i+1]
		}
	}
	if got != fc {
		t.Errorf("already-SW filter altered:\n got %s\nwant %s", got, fc)
	}
}
