// VAAPI driver detection + Plex-argv driver param strip (#124).
//
// PMS emits the VAAPI init_hw_device line with a hardcoded `,driver=iHD` —
// fine on the Intel-only fleet scaleplex grew up on, broken on AMD radeonsi
// (or any other non-Intel VAAPI host) where iHD doesn't load and the device
// init fails. We detect the local GPU vendor once at startup and:
//
//  1. Default `HW_VAAPI_DRIVER` env to the detected driver (overrides the
//     historical iHD default at the rewriter's spawn-env injection site).
//  2. Strip `,driver=<x>` from any Plex-emitted `-init_hw_device vaapi=...`
//     argument so libva falls back to `LIBVA_DRIVER_NAME` (which the rewriter
//     sets to the resolved driver) and loads the right backend.
//
// Operator override stays: `HW_VAAPI_DRIVER=<name>` env wins over the probe.

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// PCI vendor ID → libva driver name. Intel/AMD/(rarely) NVIDIA covered.
var pciVendorVAAPIDriver = map[string]string{
	"0x8086": "iHD",      // Intel
	"0x1002": "radeonsi", // AMD
	"0x10de": "nvidia",   // NVIDIA-via-VAAPI is unusual (nvenc path is the standard) but covered
}

var (
	detectedVAAPIDriverOnce sync.Once
	detectedVAAPIDriverVal  string
)

// detectVAAPIDriver returns the libva driver name for the local GPU,
// probed once from /sys/class/drm/renderD*/device/vendor. Falls back to "iHD"
// (the historical default) if the probe fails or finds no recognised vendor —
// preserving back-compat on the Intel fleet where this code is a no-op.
//
// First recognised render node wins. On a hypothetical hybrid host (Intel iGPU
// + AMD dGPU), the operator should pin via `HW_VAAPI_DRIVER` env to the one
// they actually want; this probe makes no attempt to be smart about that case.
func detectVAAPIDriver() string {
	detectedVAAPIDriverOnce.Do(func() {
		detectedVAAPIDriverVal = probeVAAPIDriver()
	})
	return detectedVAAPIDriverVal
}

func probeVAAPIDriver() string {
	matches, err := filepath.Glob("/sys/class/drm/renderD*/device/vendor")
	if err != nil || len(matches) == 0 {
		return "iHD"
	}
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(strings.ToLower(string(b)))
		if drv, ok := pciVendorVAAPIDriver[v]; ok {
			return drv
		}
	}
	return "iHD"
}

// reVAAPIDriverParam matches `,driver=<token>` inside a VAAPI -init_hw_device
// argument (e.g. `vaapi=vaapi:/dev/dri/renderD128,driver=iHD`). The optional
// `=` part covers both `,driver=iHD` and (defensive) `,driver=` empty forms.
var reVAAPIDriverParam = regexp.MustCompile(`,driver=[A-Za-z0-9_-]*`)

// stripVAAPIDriverParam removes `,driver=<x>` from a VAAPI init_hw_device
// argument string. No-op when the param is absent OR the argument isn't a
// VAAPI one (caller is expected to pre-filter, but the function is defensive).
// Returns the unchanged string when nothing was stripped — caller uses that to
// decide whether to emit a change-tag.
func stripVAAPIDriverParam(arg string) string {
	if !strings.HasPrefix(arg, "vaapi=") {
		return arg
	}
	return reVAAPIDriverParam.ReplaceAllString(arg, "")
}
