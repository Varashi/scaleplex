package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestRelabelCrossBackendTags_NVENC covers #113: after a cross-backend reshape
// onto NVENC, VAAPI-canonical filter-tag substrings (inlineass-vaapi,
// opencl-tonemap->vaapi, bitmap-inlineass-vaapi, tonemap_opencl-normalized)
// must be rewritten to the CUDA equivalents so the logged tags match the
// executed graph.
func TestRelabelCrossBackendTags_NVENC(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "text-burn HDR — full real prod-shape tag list",
			in: []string{
				"cross-backend:vaapi->nvenc",
				"decode:hw-passthrough:av1",
				"video:hdr-source(smpte2084)",
				"encode:hw-passthrough:h264_nvenc",
				"hw-decode-sub:tonemap-preserved(mobius)",
				"hw-decode:filter:opencl-tonemap->vaapi:inlineass-vaapi",
				"audio:eac3_eae->eac3",
			},
			want: []string{
				"cross-backend:vaapi->nvenc",
				"decode:hw-passthrough:av1",
				"video:hdr-source(smpte2084)",
				"encode:hw-passthrough:h264_nvenc",
				"hw-decode-sub:tonemap-preserved(mobius)",
				"hw-decode:filter:tonemap_cuda:inlineass-cuda",
				"audio:eac3_eae->eac3",
			},
		},
		{
			name: "bitmap HDR-tonemap prefix tag",
			in: []string{
				"cross-backend:vaapi->nvenc",
				"hw-decode:filter:bitmap-inlineass-vaapi:hdr-tonemap(mobius)",
			},
			want: []string{
				"cross-backend:vaapi->nvenc",
				"hw-decode:filter:bitmap-inlineass-cuda:hdr-tonemap(mobius)",
			},
		},
		{
			name: "SW-tonemap normalized tag",
			in: []string{
				"cross-backend:vaapi->nvenc",
				"filter:tonemap_opencl-normalized",
			},
			want: []string{
				"cross-backend:vaapi->nvenc",
				"filter:tonemap_cuda-normalized",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := append([]string(nil), tc.in...)
			relabelCrossBackendTags(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("relabel mismatch:\n  got:  %s\n  want: %s",
					strings.Join(got, ","), strings.Join(tc.want, ","))
			}
		})
	}
}

// TestRelabelCrossBackendTags_NoOp confirms the relabel is a true no-op when
// there is no cross-backend marker, or when the target IS the canonical vaapi
// backend, or when the target is an unknown backend (don't touch tags we
// don't have a mapping for).
func TestRelabelCrossBackendTags_NoOp(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{
			name: "no cross-backend marker — same-backend rewrite",
			in: []string{
				"decode:hw-passthrough:av1",
				"encode:hw-passthrough:h264_vaapi",
				"hw-decode:filter:inlineass-vaapi",
			},
		},
		{
			name: "cross-backend to vaapi — canonical target, leave alone",
			in: []string{
				"cross-backend:nvenc->vaapi",
				"hw-decode:filter:inlineass-vaapi",
			},
		},
		{
			name: "cross-backend to unknown backend — no mapping, leave alone",
			in: []string{
				"cross-backend:sw->amf",
				"hw-decode:filter:inlineass-vaapi",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := append([]string(nil), tc.in...)
			relabelCrossBackendTags(got)
			if !reflect.DeepEqual(got, tc.in) {
				t.Errorf("relabel should have been a no-op:\n  got: %s\n  want: %s",
					strings.Join(got, ","), strings.Join(tc.in, ","))
			}
		})
	}
}
