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

// Cross-backend reshape is also gated: a foreign VAAPI argv on a NVENC worker
// with no Pass must NOT be reshaped (stays foreign, not re-accelerated).
func TestRewriter_PassGate_DeniesCrossBackend(t *testing.T) {
	withDialect(t, nvencDialect{})
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
	if !containsString(out.Changes, TagPassGateDenied) {
		t.Fatalf("expected pass-gate denial: %v", out.Changes)
	}
	if containsString(out.Changes, "cross-backend:vaapi->nvenc") {
		t.Errorf("no-Pass cross-backend session must NOT be reshaped: %v", out.Changes)
	}
	if !strings.Contains(strings.Join(out.Args, " "), "scale_vaapi") {
		t.Errorf("foreign graph should be left untouched (not reshaped) without Pass: %v", out.Args)
	}
}
