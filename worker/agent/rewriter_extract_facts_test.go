package main

import "testing"

// extractGraphFacts pulls the orthogonal axes (w/h, hdr+algo, sub kind/params/
// spec) from every Plex graph shape the reFilter* regexes match — the input
// side of the orthogonal rewriter. One case per shape, plus the safety guard.
func TestExtractGraphFacts(t *testing.T) {
	cases := []struct {
		name  string
		graph string
		sub   *subtitleSource
		want  graphFacts
	}{
		{
			name:  "reFilterAss: SW text SDR",
			graph: "[0:0]scale=w=1920:h=1080[0];[0]format=pix_fmts=nv12[1];[1]inlineass=overrides=Outline=2[2]",
			want:  graphFacts{w: "1920", h: "1080", subKind: "text", subParams: "overrides=Outline=2", ok: true},
		},
		{
			name:  "reFilterPlain: SW no-sub SDR",
			graph: "[0:0]scale=w=1280:h=720[0];[0]format=pix_fmts=nv12[1]",
			want:  graphFacts{w: "1280", h: "720", ok: true},
		},
		{
			name:  "reFilterHWAss: HW text SDR",
			graph: "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1];[1]hwdownload,format=nv12[2];[2]inlineass=font_size=54[3];[3]hwupload[4]",
			want:  graphFacts{w: "1920", h: "1080", subKind: "text", subParams: "font_size=54", ok: true},
		},
		{
			name:  "reFilterHDR: SW no-sub HDR (zscale+tonemap)",
			graph: "[0:0]scale=w=1920:h=1080[0];[0]zscale=t=linear[1];[1]tonemap=hable[2];[2]zscale=p=bt709[3];[3]format=pix_fmts=nv12[4]",
			want:  graphFacts{w: "1920", h: "1080", hdr: true, algo: "hable", ok: true},
		},
		{
			name:  "reFilterHDRAss: SW text HDR",
			graph: "[0:0]scale=w=1920:h=1080[0];[0]format=p010,tonemap=mobius[1];[1]format=pix_fmts=nv12[2];[2]inlineass=font_size=54[3]",
			want:  graphFacts{w: "1920", h: "1080", hdr: true, algo: "mobius", subKind: "text", subParams: "font_size=54", ok: true},
		},
		{
			name:  "reFilterHWOpenCLAss: HW text HDR (opencl tonemap)",
			graph: "[0:0]hwupload[0];[0]scale_vaapi=w=3840:h=2160:format=p010[1];[1]hwmap=derive_device=opencl[2];[2]tonemap_opencl=tonemap=hable:format=nv12[3];[3]hwdownload,format=nv12[4];[4]inlineass=font_size=54[5];[5]hwupload[6]",
			want:  graphFacts{w: "3840", h: "2160", hdr: true, algo: "hable", subKind: "text", subParams: "font_size=54", ok: true},
		},
		{
			name:  "bitmap overlay HDR (the captured PGS shape)",
			graph: "[0:5]scale=3840:2160,hwupload[0];[0:0]hwupload[1];[1]scale_vaapi=w=3840:h=2160:format=p010[2];[2]hwmap=derive_device=opencl,tonemap_opencl=tonemap=mobius:format=nv12,hwmap=derive_device=vaapi:reverse=1[5];[5][0]overlay_vaapi,scale_vaapi=format=nv12[6];[6]hwupload[7]",
			want:  graphFacts{w: "3840", h: "2160", hdr: true, algo: "mobius", subKind: "bitmap", subSpec: "0:5", ok: true},
		},
		{
			name:  "bitmap overlay SDR",
			graph: "[0:3]scale=1920:1080,hwupload[0];[0:0]hwupload[1];[1]scale_vaapi=w=1920:h=1080:format=p010[2];[2][0]overlay_vaapi,scale_vaapi=format=p010[3];[3]hwupload[4]",
			want:  graphFacts{w: "1920", h: "1080", subKind: "bitmap", subSpec: "0:3", ok: true},
		},
		{
			name:  "safety guard: unmodeled node (crop) bails",
			graph: "[0:0]scale=w=1920:h=1080[0];[0]crop=100:100[1];[1]format=pix_fmts=nv12[2]",
			want:  graphFacts{w: "1920", h: "1080"}, // ok=false
		},
		// NVIDIA shapes — directly from test/corpus/nvenc/persistent/ 2026-05-27.
		// Sub-burn path on NVIDIA downloads to system memory for inlineass
		// (libass is CPU-only); the rewriter's text-sub branch picks it up the
		// same way as VAAPI since subKind comes from subSrc or `inlineass=`.
		{
			name:  "NVIDIA HW no-sub SDR (corpus shape)",
			graph: "[0:0]hwupload[0];[0]scale_cuda=w=1280:h=720:format=nv12[1]",
			want:  graphFacts{w: "1280", h: "720", ok: true},
		},
		{
			name:  "NVIDIA HW text SDR (corpus shape)",
			graph: "[0:0]hwupload[0];[0]scale_cuda=w=1280:h=720:format=nv12[1];[1]hwdownload,format=nv12[2];[2]inlineass=font_size=54[3]",
			want:  graphFacts{w: "1280", h: "720", subKind: "text", subParams: "font_size=54", ok: true},
		},
		{
			name:  "NVIDIA HW no-sub HDR (corpus shape — tonemap_cuda)",
			graph: "[0:0]hwupload[0];[0]scale_cuda=w=1280:h=720:format=p010[1];[1]tonemap_cuda=hable:nv12[2]",
			want:  graphFacts{w: "1280", h: "720", hdr: true, algo: "hable", ok: true},
		},
		{
			name:  "NVIDIA HW text HDR (corpus shape)",
			graph: "[0:0]hwupload[0];[0]scale_cuda=w=1280:h=720:format=p010[1];[1]tonemap_cuda=hable:nv12[2];[2]hwdownload,format=nv12[3];[3]inlineass=font_size=54[4]",
			want:  graphFacts{w: "1280", h: "720", hdr: true, algo: "hable", subKind: "text", subParams: "font_size=54", ok: true},
		},
		{
			name:  "no scale → bail",
			graph: "[0:0]format=pix_fmts=nv12[0]",
			want:  graphFacts{}, // ok=false
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractGraphFacts(c.graph, c.sub)
			if got != c.want {
				t.Errorf("extractGraphFacts mismatch:\n got  %+v\n want %+v", got, c.want)
			}
		})
	}
}
