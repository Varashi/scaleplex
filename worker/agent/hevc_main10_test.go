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
		{"10bit-hevc-vaapi-injects", base10, true, "main10"},
		{"10bit-hevc-nvenc-injects", []string{
			"-codec:0", "av1", "-hwaccel:0", "cuda", "-i", "in.mkv",
			"-filter_complex", "[0:0]scale_cuda=w=3840:h=2160:format=p010[1]",
			"-codec:0", "hevc_nvenc", "-qp:0", "15",
		}, true, "main10"},
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
		// scaleplex#200: Plex's 4K HDR tonemap shape declares p010 upstream
		// of tonemap_opencl, then converges to nv12 via the opencl tonemap
		// and the hwdownload before re-uploading to the encoder. The
		// encoder receives 8-bit; main10 would fail with "No usable
		// encoding profile found" on iHD hevc_vaapi.
		{"hdr-tonemap-opencl-to-nv12-not-tagged", []string{
			"-codec:0", "av1", "-hwaccel:0", "vaapi", "-i", "in.mkv",
			"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=3072:h=1280:format=p010[1];[1]hwmap=derive_device=opencl[2];[2]tonemap_opencl=tonemap=mobius:format=nv12:m=bt709:p=bt709:r=tv[3];[3]hwdownload,format=nv12[4];[4]inlineass=font_path=/x.otf[5];[5]hwupload[6]",
			"-map", "[6]",
			"-codec:0", "hevc_vaapi", "-qp:0", "15", "-maxrate:0", "20000k",
			"-filter_complex", "[0:1] aresample=async=1[7]", // audio chain, no pixfmt
			"-map", "[7]", "-codec:1", "aac",
		}, false, ""},
		// Same shape but on NVENC (cuda tonemap path).
		{"hdr-tonemap-cuda-to-nv12-not-tagged", []string{
			"-codec:0", "av1", "-hwaccel:0", "cuda", "-i", "in.mkv",
			"-filter_complex", "[0:0]scale_cuda=w=3072:h=1280:format=p010[1];[1]tonemap_cuda=tonemap=mobius:format=nv12[2]",
			"-codec:0", "hevc_nvenc", "-qp:0", "15",
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
				// -profile:0 main10 must sit right after the HW hevc encoder
				enc := -1
				for i, a := range got {
					if a == "hevc_vaapi" || a == "hevc_nvenc" {
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

// scaleplex#200: tenBitOutput must track the LAST format= token in the video
// filter_complex, not match a naked p010 substring anywhere in argv.
func TestTenBitOutput(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"p010-only", []string{"-filter_complex", "scale_vaapi=format=p010"}, true},
		{"nv12-only", []string{"-filter_complex", "scale_vaapi=format=nv12"}, false},
		{"p010-then-nv12", []string{
			"-filter_complex",
			"scale_vaapi=format=p010,tonemap_opencl=format=nv12,hwdownload,format=nv12",
		}, false},
		{"nv12-then-p010", []string{
			"-filter_complex",
			"scale_vaapi=format=nv12,scale_vaapi=format=p010",
		}, true},
		{"no-filter-complex", []string{"-codec:0", "hevc_vaapi"}, false},
		{"audio-chain-only", []string{
			"-filter_complex", "[0:0]aresample=async=1",
		}, false},
		{"video-then-audio-fc", []string{
			"-filter_complex", "scale_vaapi=format=p010,tonemap_opencl=format=nv12",
			"-filter_complex", "[0:1]aresample=async=1",
		}, false},
		{"no-partial-match-yuv420p10", []string{
			"-filter_complex", "scale=format=yuv420p10",
		}, false}, // unknown 10-bit pixfmt — conservative false (we only recognize p010/nv12)
		{"no-bare-p010-in-non-fc-arg", []string{
			"-codec:0", "av1", "-some_opt", "p010-suffix",
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tenBitOutput(c.args); got != c.want {
				t.Fatalf("tenBitOutput=%v want %v args=%v", got, c.want, c.args)
			}
		})
	}
}

func TestLastFormatToken(t *testing.T) {
	cases := []struct {
		graph, pixfmt string
		want          int
	}{
		{"format=p010", "p010", 0},
		{"format=nv12", "nv12", 0},
		{"format=nv12,format=p010", "p010", 12},
		{"format=p010,format=nv12", "p010", 0},
		{"format=p010,format=nv12,format=p010", "p010", 24},
		// Partial-match guard: "format=nv12" should not match the "nv12" inside
		// a longer identifier.
		{"format=nv12_extra", "nv12", -1},
		{"format=p010le", "p010", -1}, // p010le is a different identifier
		{"no format here", "p010", -1},
	}
	for _, c := range cases {
		if got := lastFormatToken(c.graph, c.pixfmt); got != c.want {
			t.Errorf("lastFormatToken(%q, %q)=%d want %d", c.graph, c.pixfmt, got, c.want)
		}
	}
}

