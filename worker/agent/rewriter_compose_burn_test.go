package main

import (
	"strings"
	"testing"
)

// TestSeekSelectModeling covers #154: a seeked HW-decode sub-burn graph carries
// a standalone `select=gte(t\,SEEK)` frame gate after inlineass. Before the fix
// `select` was an unmodeled node → extractGraphFacts bailed (ok=false) → the
// rewriter fell back to Plex's SW-inlineass round-trip instead of the
// GPU-resident reshape. Now select is modeled, its expr lifted (escaped comma
// preserved), and the composers re-emit it at the chain tail.
func TestSeekSelectModeling(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "1080")
	// The real Shes_Out_of_My_League 320x180 dash rendition shape (inlineass
	// params trimmed; the select node + escaped comma are the point).
	graph := `[0:0]hwupload[0];[0]scale_vaapi=w=320:h=180:format=nv12[1];` +
		`[1]hwdownload,format=nv12[2];[2]inlineass=font_size=54:language=en[3];` +
		`[3]select=gte(t\,203.995661)[4];[4]hwupload[5]`

	t.Run("extractGraphFacts-lifts-select", func(t *testing.T) {
		f := extractGraphFacts(graph, &subtitleSource{Kind: "text"})
		if !f.ok {
			t.Fatal("ok=false: select node still treated as unmodeled (the #154 bail)")
		}
		if f.subKind != "text" || f.w != "320" || f.h != "180" {
			t.Fatalf("facts: subKind=%q w=%q h=%q", f.subKind, f.w, f.h)
		}
		if f.selectExpr != `gte(t\,203.995661)` {
			t.Fatalf("selectExpr=%q — want the escaped-comma expr verbatim", f.selectExpr)
		}
	})

	t.Run("reshape-re-emits-select-at-tail", func(t *testing.T) {
		f := extractGraphFacts(graph, &subtitleSource{Kind: "text"})
		va := tonemapConfig{}
		fltr, label := va.composeBurn(burnSpec{
			vaResident: true, w: f.w, h: f.h, burnSub: true, subParams: f.subParams,
		})
		fltr, label = appendSelectStage(fltr, label, f.selectExpr)
		if !strings.HasSuffix(fltr, `select=gte(t\,203.995661)[vsel]`) {
			t.Fatalf("select not appended at tail:\n%s", fltr)
		}
		if label != "[vsel]" {
			t.Fatalf("label=%q want [vsel] (the -map target must follow select)", label)
		}
		// The GPU-resident reshape must NOT carry Plex's hwdownload/hwupload
		// round-trip — that's the whole point of modeling it.
		if strings.Contains(fltr, "hwdownload") {
			t.Fatalf("reshape still round-trips through sysmem:\n%s", fltr)
		}
	})

	t.Run("unparseable-select-bails-safe", func(t *testing.T) {
		// A select node with no output label (unobserved shape): extraction must
		// fail closed (ok=false) rather than drop the seek gate and reset to t=0.
		bad := `[2]inlineass=x[3];[3]select=gte(t\,5)`
		if f := extractGraphFacts(bad, &subtitleSource{Kind: "text"}); f.ok {
			t.Fatal("ok=true on an unextractable select — would silently drop the seek")
		}
	})

	t.Run("appendSelectStage-noop-when-empty", func(t *testing.T) {
		fltr, label := appendSelectStage("[0:0]scale_vaapi=w=320:h=180:format=nv12[0]", "[0]", "")
		if fltr != "[0:0]scale_vaapi=w=320:h=180:format=nv12[0]" || label != "[0]" {
			t.Fatalf("non-noop on empty expr: %s %s", fltr, label)
		}
	})
}

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
