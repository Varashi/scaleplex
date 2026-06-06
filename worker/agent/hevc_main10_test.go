package main

import (
	"strings"
	"testing"
)

// scaleplex#189: 10-bit hevc_vaapi must carry -profile main10, else VAAPI
// defaults to Rext (unspecified pixfmt) which Apple VideoToolbox can't decode.
func TestEnsureHEVCMain10(t *testing.T) {
	// decoder -codec:0 is before -i; encoder -codec:0 is after -i.
	base10 := []string{
		"-codec:0", "av1", "-hwaccel:0", "vaapi", "-i", "in.mkv",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1]",
		"-codec:0", "hevc_vaapi", "-qp:0", "15", "-maxrate:0", "8000k",
	}
	cases := []struct {
		name      string
		args      []string
		wantTag   bool
		wantAfter string // token expected immediately after the encoder, if injected
	}{
		{"10bit-hevc-injects", base10, true, "main10"},
		{"8bit-hevc-untouched", []string{
			"-codec:0", "av1", "-i", "in.mkv",
			"-filter_complex", "scale_vaapi=w=1920:h=1080:format=nv12",
			"-codec:0", "hevc_vaapi", "-qp:0", "15",
		}, false, ""},
		{"10bit-h264-untouched", []string{
			"-codec:0", "av1", "-i", "in.mkv",
			"-filter_complex", "scale_vaapi=w=3840:h=2160:format=p010",
			"-codec:0", "h264_vaapi", "-qp:0", "15",
		}, false, ""},
		{"existing-profile-not-overridden", []string{
			"-codec:0", "av1", "-i", "in.mkv",
			"-filter_complex", "scale_vaapi=format=p010",
			"-codec:0", "hevc_vaapi", "-profile:0", "main", "-qp:0", "15",
		}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changes := ensureHEVCMain10(append([]string(nil), c.args...), nil)
			tagged := false
			for _, ch := range changes {
				if ch == TagEncodeHEVCMain10 {
					tagged = true
				}
			}
			if tagged != c.wantTag {
				t.Fatalf("tag=%v want %v (changes=%v)", tagged, c.wantTag, changes)
			}
			if c.wantTag {
				// -profile:0 main10 must sit right after the encoder token
				enc := -1
				for i, a := range got {
					if a == "hevc_vaapi" {
						enc = i
					}
				}
				if enc < 0 || enc+2 >= len(got) || got[enc+1] != "-profile:0" || got[enc+2] != c.wantAfter {
					t.Fatalf("profile not injected after encoder: %v", got[enc:])
				}
			} else if c.wantTag == false && strings.Contains(strings.Join(got, " "), "main10") && c.name != "existing-profile-not-overridden" {
				t.Fatalf("unexpected main10 injected: %v", got)
			}
		})
	}
}

