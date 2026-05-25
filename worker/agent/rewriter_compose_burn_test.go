package main

import "testing"

// composeBurn is the orthogonal-stage composer: one builder emits every
// {SDR,HDR}×{none,text,bitmap}×{any res}×{VA-resident,SW} shape. These tests
// pin each axis independently so a change to one stage can't silently alter
// another.
func TestComposeBurn_Axes(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "1080")
	ocl := tonemapConfig{useOpenCL: true, algo: "hable"}
	va := tonemapConfig{useOpenCL: false, algo: "hable"}
	const tmOCL = "hwmap=derive_device=opencl,tonemap_opencl=tonemap=mobius:transfer=bt709:matrix=bt709:primaries=bt709:format=nv12,hwmap=derive_device=vaapi:reverse=1"

	cases := []struct {
		name      string
		tm        tonemapConfig
		spec      burnSpec
		wantFltr  string
		wantLabel string
	}{
		{
			name:      "sdr/no-sub/va-resident",
			tm:        ocl,
			spec:      burnSpec{vaResident: true, w: "1920", h: "1080"},
			wantFltr:  "[0:0]scale_vaapi=w=1920:h=1080:format=nv12[0]",
			wantLabel: "[0]",
		},
		{
			name:      "sdr/no-sub/sw-upload",
			tm:        ocl,
			spec:      burnSpec{vaResident: false, w: "1280", h: "720"},
			wantFltr:  "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1]",
			wantLabel: "[1]",
		},
		{
			name:      "hdr/no-sub/va-resident/honors-algo",
			tm:        ocl,
			spec:      burnSpec{vaResident: true, w: "3840", h: "2160", hdr: true, algo: "mobius"},
			wantFltr:  "[0:0]scale_vaapi=w=3840:h=2160:format=p010," + tmOCL + "[0]",
			wantLabel: "[0]",
		},
		{
			name:      "hdr/bitmap/va-resident",
			tm:        ocl,
			spec:      burnSpec{vaResident: true, w: "3840", h: "2160", hdr: true, algo: "mobius", burnSub: true},
			wantFltr:  "[0:0]scale_vaapi=w=3840:h=2160:format=p010," + tmOCL + "[0];[0]inlineass=render_height=1080[1]",
			wantLabel: "[1]",
		},
		{
			name:      "sdr/bitmap/sw-upload",
			tm:        ocl,
			spec:      burnSpec{vaResident: false, w: "3840", h: "2160", burnSub: true},
			wantFltr:  "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=nv12[1];[1]inlineass=render_height=1080[2]",
			wantLabel: "[2]",
		},
		{
			name:      "text/keeps-plex-params/then-render_height",
			tm:        ocl,
			spec:      burnSpec{vaResident: true, w: "1920", h: "1080", burnSub: true, subParams: "overrides=Outline=2"},
			wantFltr:  "[0:0]scale_vaapi=w=1920:h=1080:format=nv12[0];[0]inlineass=overrides=Outline=2:render_height=1080[1]",
			wantLabel: "[1]",
		},
		{
			name:      "hdr/vaapi-fixed-curve-backend",
			tm:        va,
			spec:      burnSpec{vaResident: true, w: "3840", h: "2160", hdr: true, algo: "mobius"},
			wantFltr:  "[0:0]scale_vaapi=w=3840:h=2160:format=p010,tonemap_vaapi=transfer=bt709:format=nv12[0]",
			wantLabel: "[0]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotFltr, gotLabel := c.tm.composeBurn(c.spec)
			if gotFltr != c.wantFltr {
				t.Errorf("filter:\n got  %s\n want %s", gotFltr, c.wantFltr)
			}
			if gotLabel != c.wantLabel {
				t.Errorf("label: got %s want %s", gotLabel, c.wantLabel)
			}
		})
	}
}

func TestDetectBitmapOverlayBurn(t *testing.T) {
	// Real captured 4K HDR PGS session graph (after substituteOpenCLTonemap
	// normalized the tonemap to comma form): the variant that escaped the
	// optimizer pre-fix.
	hdrGraph := "[0:5]scale=3840:2160,hwupload[0];[0:0]hwupload[1];" +
		"[1]scale_vaapi=w=3840:h=2160:format=p010[2];" +
		"[2]hwmap=derive_device=opencl,tonemap_opencl=tonemap=mobius:transfer=bt709:matrix=bt709:primaries=bt709:format=nv12,hwmap=derive_device=vaapi:reverse=1[5];" +
		"[5][0]overlay_vaapi,scale_vaapi=format=nv12[6];[6]hwupload[7]"
	// SDR bitmap overlay (no tonemap) — the shape reFilterHWBitmapOverlay
	// already matched.
	sdrGraph := "[0:3]scale=1920:1080,hwupload[0];[0:0]hwupload[1];" +
		"[1]scale_vaapi=w=1920:h=1080:format=p010[2];" +
		"[2][0]overlay_vaapi,scale_vaapi=format=p010[3];[3]hwupload[4]"
	// Text inlineass graph — must NOT match (no overlay_vaapi).
	textGraph := "[0:0]hwupload[10];[10]scale_vaapi=w=1920:h=1080:format=nv12[11];" +
		"[11]inlineass=render_height=1080[15]"

	t.Run("hdr-bitmap", func(t *testing.T) {
		spec, w, h, algo, hdr, ok := detectBitmapOverlayBurn(hdrGraph)
		if !ok || spec != "0:5" || w != "3840" || h != "2160" || algo != "mobius" || !hdr {
			t.Fatalf("got spec=%q w=%q h=%q algo=%q hdr=%v ok=%v", spec, w, h, algo, hdr, ok)
		}
	})
	t.Run("sdr-bitmap", func(t *testing.T) {
		spec, w, h, algo, hdr, ok := detectBitmapOverlayBurn(sdrGraph)
		if !ok || spec != "0:3" || w != "1920" || h != "1080" || algo != "" || hdr {
			t.Fatalf("got spec=%q w=%q h=%q algo=%q hdr=%v ok=%v", spec, w, h, algo, hdr, ok)
		}
	})
	t.Run("text-not-bitmap", func(t *testing.T) {
		if _, _, _, _, _, ok := detectBitmapOverlayBurn(textGraph); ok {
			t.Fatal("text inlineass graph wrongly detected as bitmap overlay")
		}
	})
}
