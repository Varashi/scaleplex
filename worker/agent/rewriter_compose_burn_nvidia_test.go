package main

import "testing"

// composeBurn under the NVIDIA dialect — same orthogonal axes as the
// VAAPI test, different filter literals. Locks the {scale_cuda,
// tonemap_cuda, hwupload, inlineass} shape captured 2026-05-27 in
// test/corpus/nvenc/persistent/.
//
// NVIDIA's tonemap is always single-stage tonemap_cuda — there's no
// OpenCL-derive path on this backend (resolveTonemapConfig gates
// useOpenCL=true to VAAPI). So `nv` here is the only NVIDIA shape; we
// don't bother re-running every case under a useOpenCL=true variant.
func TestComposeBurn_NVIDIA(t *testing.T) {
	nv := tonemapConfig{algo: "hable", d: nvencDialect{}}

	cases := []struct {
		name      string
		spec      burnSpec
		wantFltr  string
		wantLabel string
	}{
		{
			name:      "sdr/no-sub/cuda-resident",
			spec:      burnSpec{vaResident: true, w: "1920", h: "1080"},
			wantFltr:  "[0:0]scale_cuda=w=1920:h=1080:format=nv12[0]",
			wantLabel: "[0]",
		},
		{
			name:      "sdr/no-sub/sw-upload — matches corpus shape",
			spec:      burnSpec{vaResident: false, w: "1280", h: "720"},
			wantFltr:  "[0:0]hwupload[0];[0]scale_cuda=w=1280:h=720:format=nv12[1]",
			wantLabel: "[1]",
		},
		{
			name:      "hdr/no-sub/cuda-resident/algo-honored",
			spec:      burnSpec{vaResident: true, w: "3840", h: "2160", hdr: true, algo: "hable"},
			wantFltr:  "[0:0]scale_cuda=w=3840:h=2160:format=p010,tonemap_cuda=tonemap=hable:format=nv12[0]",
			wantLabel: "[0]",
		},
		{
			name:      "hdr/no-sub/sw-upload — matches corpus shape exactly",
			spec:      burnSpec{vaResident: false, w: "1280", h: "720", hdr: true, algo: "hable"},
			wantFltr:  "[0:0]hwupload[0];[0]scale_cuda=w=1280:h=720:format=p010,tonemap_cuda=tonemap=hable:format=nv12[1]",
			wantLabel: "[1]",
		},
		{
			// sub-burn inserts hwdownload,format=nv12 before inlineass —
			// vf_inlineass is CPU/libass with no CUDA branch; matches Plex's
			// own nvenc sub chain. h264_nvenc takes the sysmem output directly.
			name:      "sdr/text-sub/sw-upload",
			spec:      burnSpec{vaResident: false, w: "1920", h: "1080", burnSub: true, subParams: "overrides=Outline=2"},
			wantFltr:  "[0:0]hwupload[0];[0]scale_cuda=w=1920:h=1080:format=nv12[1];[1]hwdownload,format=nv12[2];[2]inlineass=overrides=Outline=2:render_height=1080[3]",
			wantLabel: "[3]",
		},
		{
			name:      "hdr/text-sub/sw-upload",
			spec:      burnSpec{vaResident: false, w: "3840", h: "2160", hdr: true, algo: "hable", burnSub: true},
			wantFltr:  "[0:0]hwupload[0];[0]scale_cuda=w=3840:h=2160:format=p010,tonemap_cuda=tonemap=hable:format=nv12[1];[1]hwdownload,format=nv12[2];[2]inlineass=render_height=1080[3]",
			wantLabel: "[3]",
		},
		{
			name:      "text/animated-tier-down",
			spec:      burnSpec{vaResident: true, w: "1920", h: "1080", burnSub: true, animatedTierDown: true},
			wantFltr:  "[0:0]scale_cuda=w=1920:h=1080:format=nv12[0];[0]hwdownload,format=nv12[1];[1]inlineass=render_height=1080:animated_tier_down=1[2]",
			wantLabel: "[2]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotFltr, gotLabel := nv.composeBurn(c.spec)
			if gotFltr != c.wantFltr {
				t.Errorf("filter:\n got  %s\n want %s", gotFltr, c.wantFltr)
			}
			if gotLabel != c.wantLabel {
				t.Errorf("label: got %s want %s", gotLabel, c.wantLabel)
			}
		})
	}
}

// resolveTonemapConfig under the NVIDIA dialect: useOpenCL must stay
// false (NVIDIA has no OpenCL-derive path), regardless of the
// SCALEPLEX_TONEMAP env. SCALEPLEX_TONEMAP_ALGO still honored.
func TestResolveTonemapConfig_NVIDIA_NoOpenCL(t *testing.T) {
	prev := activeDialect
	activeDialect = nvencDialect{}
	defer func() { activeDialect = prev }()

	t.Run("default", func(t *testing.T) {
		t.Setenv("SCALEPLEX_TONEMAP", "")
		t.Setenv("SCALEPLEX_TONEMAP_ALGO", "")
		c := resolveTonemapConfig()
		if c.useOpenCL {
			t.Errorf("NVIDIA: useOpenCL must be false; got true")
		}
		if c.algo != "hable" {
			t.Errorf("default algo: got %q, want hable", c.algo)
		}
		if c.d.backendName() != "nvenc" {
			t.Errorf("dialect: got %q, want nvenc", c.d.backendName())
		}
	})
	t.Run("SCALEPLEX_TONEMAP=opencl is ignored on NVIDIA", func(t *testing.T) {
		t.Setenv("SCALEPLEX_TONEMAP", "opencl")
		c := resolveTonemapConfig()
		if c.useOpenCL {
			t.Errorf("NVIDIA: useOpenCL must stay false even with env=opencl")
		}
	})
	t.Run("algo override honored", func(t *testing.T) {
		t.Setenv("SCALEPLEX_TONEMAP_ALGO", "mobius")
		c := resolveTonemapConfig()
		if c.algo != "mobius" {
			t.Errorf("algo: got %q, want mobius", c.algo)
		}
	})
}
