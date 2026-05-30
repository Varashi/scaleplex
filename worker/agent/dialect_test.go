package main

import "testing"

func TestSelectDialect(t *testing.T) {
	// Explicit pins are deterministic regardless of the runner's hardware.
	cases := []struct {
		name        string
		envBackend  string
		wantBackend string
	}{
		{"vaapi pin", "vaapi", "vaapi"},
		{"VAAPI uppercase", "VAAPI", "vaapi"},
		{"vaapi padded", "  vaapi  ", "vaapi"},
		{"nvenc pin", "nvenc", "nvenc"},
		{"NVENC uppercase", "NVENC", "nvenc"},
		{"nvidia alias → nvenc", "nvidia", "nvenc"},
		{"NVIDIA alias uppercase", "NVIDIA", "nvenc"},
		{"sw pin", "sw", "sw"},
		{"cpu alias → sw", "cpu", "sw"},
		{"software alias → sw", "software", "sw"},
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
	// "auto"/unset probes for an NVIDIA device first (/dev/nvidia0 on
	// bare-metal, /dev/dxg on WSL2), then a DRM render node for VAAPI,
	// else CPU. Override the device probes so the matrix is deterministic
	// regardless of the runner's hardware.
	origDev, origRender := deviceExists, hasRenderNode
	t.Cleanup(func() { deviceExists, hasRenderNode = origDev, origRender })
	autoCases := []struct {
		name        string
		present     map[string]bool
		renderNode  bool
		wantBackend string
	}{
		{"bare-metal /dev/nvidia0 → nvenc", map[string]bool{"/dev/nvidia0": true}, false, "nvenc"},
		{"WSL2 /dev/dxg only → nvenc", map[string]bool{"/dev/dxg": true}, true, "nvenc"},
		{"Intel render node → vaapi", nil, true, "vaapi"},
		{"no GPU → sw", nil, false, "sw"},
	}
	for _, tc := range autoCases {
		for _, be := range []string{"", "auto"} {
			t.Run(tc.name+" (WORKER_BACKEND="+be+")", func(t *testing.T) {
				deviceExists = func(p string) bool { return tc.present[p] }
				hasRenderNode = func() bool { return tc.renderNode }
				t.Setenv("WORKER_BACKEND", be)
				if got := selectDialect().backendName(); got != tc.wantBackend {
					t.Fatalf("auto %s (WORKER_BACKEND=%q): got %q, want %q", tc.name, be, got, tc.wantBackend)
				}
			})
		}
	}
}

func TestNvidiaDialect_EncoderMap(t *testing.T) {
	d := nvencDialect{}
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

func TestDialect_BackendIdentity(t *testing.T) {
	cases := []struct {
		name             string
		d                dialect
		wantBackend      string
		wantHwaccel      string
		wantHwaccelFmt   string
		wantFilterHWName string
	}{
		{"vaapi", vaapiDialect{}, "vaapi", "vaapi", "vaapi", "vaapi"},
		{"nvenc", nvencDialect{}, "nvenc", "nvdec", "cuda", "cuda"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.backendName(); got != tc.wantBackend {
				t.Errorf("backendName: got %q, want %q", got, tc.wantBackend)
			}
			if got := tc.d.hwaccelName(); got != tc.wantHwaccel {
				t.Errorf("hwaccelName: got %q, want %q", got, tc.wantHwaccel)
			}
			if got := tc.d.hwaccelOutputFormat(); got != tc.wantHwaccelFmt {
				t.Errorf("hwaccelOutputFormat: got %q, want %q", got, tc.wantHwaccelFmt)
			}
			if got := tc.d.filterHWDeviceName(); got != tc.wantFilterHWName {
				t.Errorf("filterHWDeviceName: got %q, want %q", got, tc.wantFilterHWName)
			}
		})
	}
}

func TestDialect_InitHWDeviceArg(t *testing.T) {
	cases := []struct {
		name   string
		d      dialect
		devIdx int
		want   string
	}{
		{"vaapi devIdx ignored", vaapiDialect{}, 0, "vaapi=vaapi:"},
		{"vaapi devIdx still ignored", vaapiDialect{}, 7, "vaapi=vaapi:"},
		{"nvidia 0", nvencDialect{}, 0, "cuda=cuda:0"},
		{"nvidia 3", nvencDialect{}, 3, "cuda=cuda:3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.initHWDeviceArg(tc.devIdx); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDialect_ScaleFilter(t *testing.T) {
	cases := []struct {
		name string
		d    dialect
		w, h string
		pix  string
		want string
	}{
		{"vaapi nv12", vaapiDialect{}, "1280", "720", "nv12", "scale_vaapi=w=1280:h=720:format=nv12"},
		{"vaapi p010 hdr stage", vaapiDialect{}, "3840", "2160", "p010", "scale_vaapi=w=3840:h=2160:format=p010"},
		{"nvidia nv12", nvencDialect{}, "1280", "720", "nv12", "scale_cuda=w=1280:h=720:format=nv12"},
		{"nvidia p010 hdr stage", nvencDialect{}, "3840", "2160", "p010", "scale_cuda=w=3840:h=2160:format=p010"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.scaleFilter(tc.w, tc.h, tc.pix); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDialect_TonemapFilter(t *testing.T) {
	cases := []struct {
		name string
		d    dialect
		algo string
		pix  string
		want string
	}{
		{"vaapi ignores algo", vaapiDialect{}, "hable", "nv12", "tonemap_vaapi=transfer=bt709:format=nv12"},
		{"vaapi empty algo same", vaapiDialect{}, "", "nv12", "tonemap_vaapi=transfer=bt709:format=nv12"},
		{"nvidia hable nv12 — corpus shape", nvencDialect{}, "hable", "nv12", "tonemap_cuda=tonemap=hable:format=nv12"},
		{"nvidia bt2390 nv12", nvencDialect{}, "bt2390", "nv12", "tonemap_cuda=tonemap=bt2390:format=nv12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.tonemapFilter(tc.algo, tc.pix); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDialect_HWUploadDownload(t *testing.T) {
	for _, d := range []dialect{vaapiDialect{}, nvencDialect{}} {
		t.Run(d.backendName(), func(t *testing.T) {
			if got := d.hwUploadFilter(); got != "hwupload" {
				t.Errorf("hwUploadFilter: got %q, want hwupload", got)
			}
			if got := d.hwDownloadFilter(); got != "hwdownload" {
				t.Errorf("hwDownloadFilter: got %q, want hwdownload", got)
			}
		})
	}
}

func TestNvidiaDialect_SharedMaps(t *testing.T) {
	// decoderMap + hwDecodeShortCodecs are intentionally shared with
	// VAAPI — Plex's SW decoder names + HW-decode short codecs don't
	// depend on the worker's HW backend.
	v := vaapiDialect{}
	n := nvencDialect{}
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

// PickVaapiDialect routes to AMD vendor when the libva probe says radeonsi,
// else Intel. Vendor-agnostic VAAPI methods (encode/decode/scale/...) stay
// shared. #123.
func TestPickVaapiDialect_VendorRouting(t *testing.T) {
	orig := probeVAAPIDriverForDialect
	t.Cleanup(func() { probeVAAPIDriverForDialect = orig })

	cases := []struct {
		probe      string
		wantVendor string
	}{
		{"radeonsi", "amd"},
		{"iHD", "intel"},
		{"nvidia", "intel"}, // VAAPI-over-NVIDIA is unusual; default to Intel-shape
		{"", "intel"},       // probe failure → default Intel (matches detectVAAPIDriver's iHD fallback)
		{"some-future-driver", "intel"},
	}
	for _, tc := range cases {
		t.Run(tc.probe, func(t *testing.T) {
			probeVAAPIDriverForDialect = func() string { return tc.probe }
			d := pickVaapiDialect()
			vd, ok := d.(vaapiDialect)
			if !ok {
				t.Fatalf("pickVaapiDialect returned non-vaapiDialect: %T", d)
			}
			if vd.vendor != tc.wantVendor {
				t.Errorf("probe=%q: vendor=%q, want %q", tc.probe, vd.vendor, tc.wantVendor)
			}
		})
	}
}

// selectDialect's "auto" branch + a render node + radeonsi probe → AMD vendor
// end-to-end from the operator-facing knob.
func TestSelectDialect_AMDAutoRoute(t *testing.T) {
	origRender, origProbe, origDev := hasRenderNode, probeVAAPIDriverForDialect, deviceExists
	t.Cleanup(func() {
		hasRenderNode, probeVAAPIDriverForDialect, deviceExists = origRender, origProbe, origDev
	})

	hasRenderNode = func() bool { return true }
	probeVAAPIDriverForDialect = func() string { return "radeonsi" }
	deviceExists = func(string) bool { return false } // no NVIDIA device

	t.Setenv("WORKER_BACKEND", "auto")
	d := selectDialect()
	if d.backendName() != "vaapi" {
		t.Fatalf("backendName: got %q, want vaapi", d.backendName())
	}
	vd, ok := d.(vaapiDialect)
	if !ok {
		t.Fatalf("got %T, want vaapiDialect", d)
	}
	if !vd.isAMD() {
		t.Errorf("isAMD: got false, want true (vendor=%q)", vd.vendor)
	}
}
