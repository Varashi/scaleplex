package main

import "testing"

func TestDetectSourceBackend(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want sourceBackend
	}{
		{
			name: "VAAPI HW-decode (hwaccel + scale_vaapi + h264_vaapi)",
			args: []string{
				"-codec:0", "hevc", "-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi",
				"-i", "/media/x.mkv",
				"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1]",
				"-map", "[1]", "-codec:0", "h264_vaapi",
			},
			want: srcVAAPI,
		},
		{
			name: "VAAPI HDR via tonemap_opencl",
			args: []string{
				"-codec:0", "hevc", "-hwaccel:0", "vaapi",
				"-i", "/media/x.mkv",
				"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=p010,hwmap=derive_device=opencl,tonemap_opencl=tonemap=hable:format=nv12,hwmap=derive_device=vaapi:reverse=1[1]",
				"-map", "[1]", "-codec:0", "h264_vaapi",
			},
			want: srcVAAPI,
		},
		{
			name: "NVENC HW-decode (nvdec + scale_cuda + h264_nvenc)",
			args: []string{
				"-codec:0", "hevc", "-hwaccel:0", "nvdec", "-hwaccel_output_format:0", "cuda",
				"-i", "/media/x.mkv",
				"-filter_complex", "[0:0]hwupload[0];[0]scale_cuda=w=1280:h=720:format=nv12[1]",
				"-map", "[1]", "-codec:0", "h264_nvenc",
			},
			want: srcNVENC,
		},
		{
			name: "NVENC HDR via tonemap_cuda",
			args: []string{
				"-codec:0", "hevc", "-hwaccel:0", "cuda",
				"-i", "/media/x.mkv",
				"-filter_complex", "[0:0]scale_cuda=w=1280:h=720:format=p010[1];[1]tonemap_cuda=tonemap=hable:format=nv12[2]",
				"-map", "[2]", "-codec:0", "hevc_nvenc",
			},
			want: srcNVENC,
		},
		{
			name: "pure SW (libx264, scale=, no hwaccel)",
			args: []string{
				"-codec:0", "libdav1d", "-i", "/media/x.mkv",
				"-filter_complex", "[0:0]scale=w=1280:h=720[0];[0]format=pix_fmts=nv12[1]",
				"-map", "[1]", "-codec:0", "libx264", "-preset:0", "veryfast",
			},
			want: srcSW,
		},
		{
			name: "encoder decides when no hwaccel/filter signal (bare h264_nvenc)",
			args: []string{
				"-codec:0", "hevc", "-i", "/media/x.mkv",
				"-map", "0:0", "-codec:0", "h264_nvenc",
			},
			want: srcNVENC,
		},
		{
			name: "audio-only / detection pass — no video encoder",
			args: []string{
				"-codec:1", "eac3_eae", "-i", "/media/x.mkv",
				"-map", "0:1", "-codec:1", "aac", "-f", "null", "-",
			},
			want: srcNone,
		},
		{
			name: "video -codec:0 copy is not a transcode → none",
			args: []string{
				"-i", "/media/x.mkv", "-map", "0:0", "-codec:0", "copy",
			},
			want: srcNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectSourceBackend(tc.args); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHWEncoderCodec(t *testing.T) {
	cases := map[string]string{
		"h264_vaapi": "h264", "hevc_vaapi": "hevc", "av1_vaapi": "av1",
		"h264_nvenc": "h264", "hevc_nvenc": "hevc", "av1_nvenc": "av1",
	}
	for enc, want := range cases {
		if got := hwEncoderCodec[enc]; got != want {
			t.Errorf("hwEncoderCodec[%q]: got %q, want %q", enc, got, want)
		}
	}
	// A SW encoder is not a HW encoder — must be absent.
	if _, ok := hwEncoderCodec["libx264"]; ok {
		t.Error("hwEncoderCodec should not contain libx264")
	}
}

// Foreign HW encoder → codec → canonical SW name → worker dialect's HW encoder.
// This is the reverse-then-forward path PR 2 uses for cross-backend encoder swap.
func TestForeignEncoderRenormalizes(t *testing.T) {
	cases := []struct {
		foreign string
		dialect dialect
		want    string // worker's HW encoder
	}{
		{"h264_nvenc", vaapiDialect{}, "h264_vaapi"},
		{"hevc_nvenc", vaapiDialect{}, "hevc_vaapi"},
		{"h264_vaapi", nvencDialect{}, "h264_nvenc"},
		{"hevc_vaapi", nvencDialect{}, "hevc_nvenc"},
	}
	for _, tc := range cases {
		t.Run(tc.foreign+"->"+tc.dialect.backendName(), func(t *testing.T) {
			codec := hwEncoderCodec[tc.foreign]
			libname := codecCanonicalSWEncoder[codec]
			got := tc.dialect.encoderMap()[libname]
			if got != tc.want {
				t.Errorf("%s → codec=%s → %s → %s; got %q want %q",
					tc.foreign, codec, libname, tc.dialect.backendName(), got, tc.want)
			}
		})
	}
}
