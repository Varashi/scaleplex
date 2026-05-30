package main

import "testing"

// TestStripVAAPIDriverParam covers the #124 fix: PMS's `,driver=iHD` in a
// VAAPI init_hw_device argument must be stripped so libva falls back to
// LIBVA_DRIVER_NAME (vendor-aware default).
func TestStripVAAPIDriverParam(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Plex's literal — iHD stripped",
			in:   "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
			want: "vaapi=vaapi:/dev/dri/renderD128",
		},
		{
			name: "other driver token also stripped (forward compat)",
			in:   "vaapi=vaapi:/dev/dri/renderD128,driver=radeonsi",
			want: "vaapi=vaapi:/dev/dri/renderD128",
		},
		{
			name: "no driver param — unchanged",
			in:   "vaapi=vaapi:/dev/dri/renderD128",
			want: "vaapi=vaapi:/dev/dri/renderD128",
		},
		{
			name: "scaleplex dialect's injected form (no device, no driver) — unchanged",
			in:   "vaapi=vaapi:",
			want: "vaapi=vaapi:",
		},
		{
			name: "empty driver= (defensive) — also stripped",
			in:   "vaapi=vaapi:/dev/dri/renderD128,driver=",
			want: "vaapi=vaapi:/dev/dri/renderD128",
		},
		{
			name: "non-VAAPI arg (e.g. cuda) — untouched",
			in:   "cuda=cu:0",
			want: "cuda=cu:0",
		},
		{
			name: "non-VAAPI arg with bogus driver= — untouched (caller pre-filters; defensive)",
			in:   "opencl=ocl,driver=intel",
			want: "opencl=ocl,driver=intel",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripVAAPIDriverParam(tc.in)
			if got != tc.want {
				t.Errorf("stripVAAPIDriverParam(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDetectVAAPIDriver doesn't assert a specific value (depends on host) —
// it just confirms a non-empty default is returned and the cache works
// (multiple calls return the same value).
func TestDetectVAAPIDriver_NonEmptyAndCached(t *testing.T) {
	a := detectVAAPIDriver()
	if a == "" {
		t.Fatal("detectVAAPIDriver() returned empty string; expected a libva driver name (default 'iHD')")
	}
	b := detectVAAPIDriver()
	if a != b {
		t.Errorf("detectVAAPIDriver() not cached: first=%q second=%q", a, b)
	}
}
