package main

import (
	"errors"
	"strings"
	"testing"
)

// stubPass swaps passCheck + clears the cache for one test.
func stubPass(t *testing.T, fn func(base, tok string) (bool, error)) {
	t.Helper()
	prev := passCheck
	passCheck = fn
	passCacheMu.Lock()
	passCache = map[string]passCacheEntry{}
	passCacheMu.Unlock()
	t.Cleanup(func() {
		passCheck = prev
		passCacheMu.Lock()
		passCache = map[string]passCacheEntry{}
		passCacheMu.Unlock()
	})
}

func wiredEnv() map[string]string {
	return map[string]string{
		"SCALEPLEX_PMS_BASE_URL": "http://pms.lan:32499",
		"X_PLEX_TOKEN":           "tok123",
	}
}

func TestHWAccelAllowed(t *testing.T) {
	t.Run("wired + active Pass → allow", func(t *testing.T) {
		stubPass(t, func(_, _ string) (bool, error) { return true, nil })
		if !hwAccelAllowed(wiredEnv()) {
			t.Error("active Pass should allow")
		}
	})
	t.Run("wired + no Pass → deny", func(t *testing.T) {
		stubPass(t, func(_, _ string) (bool, error) { return false, nil })
		if hwAccelAllowed(wiredEnv()) {
			t.Error("no Pass should deny")
		}
	})
	t.Run("wired + query error → fail-closed (deny)", func(t *testing.T) {
		stubPass(t, func(_, _ string) (bool, error) { return false, errors.New("timeout") })
		if hwAccelAllowed(wiredEnv()) {
			t.Error("query error should fail closed")
		}
	})
	t.Run("not wired (no env) → allow (gate inert)", func(t *testing.T) {
		stubPass(t, func(_, _ string) (bool, error) { t.Fatal("should not query when unwired"); return false, nil })
		if !hwAccelAllowed(map[string]string{}) {
			t.Error("unwired gate should allow")
		}
	})
	t.Run("missing token → allow (gate inert)", func(t *testing.T) {
		stubPass(t, func(_, _ string) (bool, error) { t.Fatal("should not query"); return false, nil })
		if !hwAccelAllowed(map[string]string{"SCALEPLEX_PMS_BASE_URL": "http://pms:32499"}) {
			t.Error("missing token → inert → allow")
		}
	})

	// L2 (#78): the shim-supplied SCALEPLEX_PASS_ACTIVE is authoritative and
	// skips the HTTP probe entirely.
	t.Run("L2 PASS_ACTIVE=1 → allow without probe", func(t *testing.T) {
		stubPass(t, func(_, _ string) (bool, error) {
			t.Fatal("should not HTTP-probe when PASS_ACTIVE set")
			return false, nil
		})
		env := wiredEnv()
		env["SCALEPLEX_PASS_ACTIVE"] = "1"
		if !hwAccelAllowed(env) {
			t.Error("PASS_ACTIVE=1 should allow")
		}
	})
	t.Run("L2 PASS_ACTIVE=0 → deny without probe", func(t *testing.T) {
		stubPass(t, func(_, _ string) (bool, error) {
			t.Fatal("should not HTTP-probe when PASS_ACTIVE set")
			return false, nil
		})
		env := wiredEnv()
		env["SCALEPLEX_PASS_ACTIVE"] = "0"
		if hwAccelAllowed(env) {
			t.Error("PASS_ACTIVE=0 should deny")
		}
	})
	t.Run("L2 absent → falls back to HTTP probe", func(t *testing.T) {
		probed := false
		stubPass(t, func(_, _ string) (bool, error) { probed = true; return true, nil })
		if !hwAccelAllowed(wiredEnv()) || !probed {
			t.Errorf("no PASS_ACTIVE should fall back to probe (probed=%v)", probed)
		}
	})
}

func TestHWAccelAllowed_Caches(t *testing.T) {
	calls := 0
	stubPass(t, func(_, _ string) (bool, error) { calls++; return true, nil })
	for i := 0; i < 3; i++ {
		hwAccelAllowed(wiredEnv())
	}
	if calls != 1 {
		t.Errorf("expected 1 query (cached), got %d", calls)
	}
}

func TestHWAccelAllowed_ErrorNotCached(t *testing.T) {
	calls := 0
	stubPass(t, func(_, _ string) (bool, error) { calls++; return false, errors.New("x") })
	hwAccelAllowed(wiredEnv())
	hwAccelAllowed(wiredEnv())
	if calls != 2 {
		t.Errorf("query failures must not cache; expected 2 calls, got %d", calls)
	}
}

// End-to-end: FORCE_HW=1 session, wired PMS, NO Pass → the rewriter denies
// re-accel and honors Plex's SW pipeline (no h264_vaapi swap), tagging the gate.
func TestRewriter_PassGate_DeniesForceHW(t *testing.T) {
	stubPass(t, func(_, _ string) (bool, error) { return false, nil })
	args := []string{
		"-codec:0", "libdav1d", "-i", "/media/x.mkv",
		"-filter_complex", "[0:0]scale=w=1280:h=720[0];[0]format=pix_fmts=nv12[1]",
		"-map", "[1]", "-codec:0", "libx264", "-preset:0", "veryfast",
	}
	out := Rewrite(args, wiredEnv(), nil)
	if !containsString(out.Changes, TagPassGateDenied) {
		t.Fatalf("expected pass-gate denial tag: %v", out.Changes)
	}
	if containsString(out.Args, "h264_vaapi") || containsString(out.Args, "scale_vaapi") {
		t.Errorf("no-Pass session must NOT be HW-re-accelerated: %v", out.Args)
	}
	if !containsString(out.Changes, "honor:plex-sw") {
		t.Errorf("expected honor:plex-sw fallback: %v", out.Changes)
	}
}

// Cross-backend reshape is NOT Pass-gated (#99): a foreign VAAPI argv only
// exists because Plex emitted HW, which Plex does only for an active Pass — so
// it's proof of entitlement. Even with the L3 Pass probe failing (no Pass
// confirmable — e.g. an external NVENC worker that can't reach the in-cluster
// PMS), the worker MUST reshape VAAPI→NVENC and run it, never leave the
// un-runnable foreign argv. Without this the session 234s on a non-VAAPI box.
func TestRewriter_PassGate_CrossBackendNotGated(t *testing.T) {
	withDialect(t, nvencDialect{})
	t.Setenv("SCALEPLEX_FORCE_HW", "1") // the #99 scenario: FORCE_HW on + foreign HW must still bypass the gate (explicit, not relying on TestMain's package default)
	stubPass(t, func(_, _ string) (bool, error) { return false, nil })
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/x.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1]",
		"-map", "[1]", "-codec:0", "h264_vaapi",
	}
	out := Rewrite(args, wiredEnv(), nil)
	if containsString(out.Changes, TagPassGateDenied) {
		t.Fatalf("cross-backend must NOT consult the Pass gate: %v", out.Changes)
	}
	if !containsString(out.Changes, "cross-backend:vaapi->nvenc") {
		t.Errorf("foreign-HW source must be reshaped to the worker's backend: %v", out.Changes)
	}
	joined := strings.Join(out.Args, " ")
	if !containsString(out.Args, "h264_nvenc") || strings.Contains(joined, "scale_vaapi") {
		t.Errorf("expected VAAPI→NVENC reshape (h264_nvenc, no scale_vaapi): %v", out.Args)
	}
}

// Same-backend HW passthrough under FORCE_HW=1 is also NOT gated: a VAAPI argv
// on a VAAPI worker is proof of Pass just like a foreign one, so it must not
// probe PMS (a probe flake would needlessly fail-close a licensed session).
func TestRewriter_PassGate_SameBackendHWNotGated(t *testing.T) {
	withDialect(t, vaapiDialect{})
	t.Setenv("SCALEPLEX_FORCE_HW", "1")
	stubPass(t, func(_, _ string) (bool, error) { return false, nil })
	args := []string{
		"-codec:0", "hevc",
		"-hwaccel:0", "vaapi", "-hwaccel_output_format:0", "vaapi", "-hwaccel_device:0", "vaapi",
		"-i", "/media/x.mkv",
		"-init_hw_device", "vaapi=vaapi:",
		"-filter_complex", "[0:0]hwupload[0];[0]scale_vaapi=w=1280:h=720:format=nv12[1]",
		"-map", "[1]", "-codec:0", "h264_vaapi",
	}
	out := Rewrite(args, wiredEnv(), nil)
	if containsString(out.Changes, TagPassGateDenied) {
		t.Fatalf("same-backend HW passthrough must NOT consult the Pass gate: %v", out.Changes)
	}
}
