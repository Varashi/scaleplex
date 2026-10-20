package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scaleplex#214: the HEVC Main10 pin must agree with the bit depth of the
// FINAL filter graph. Before the fix, ensureHEVCMain10 ran on Plex's original
// graph and the burn recomposers then rebuilt it 8-bit unless the HDR probe
// fired — a 10-bit SDR source + text burn + hevc_vaapi produced
// `scale_vaapi=…:format=nv12` + `-profile:0 main10` → iHD "No usable encoding
// profile found" (exit 218). Seen in prod 2026-10-20 on a 10-bit SDR AV1
// WEB-DL with SRT burn to Plex for Android (TV).

// main10Pinned reports whether args carry a `-profile[:spec] main10`.
func main10Pinned(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if (args[i] == "-profile" || strings.HasPrefix(args[i], "-profile:")) && args[i+1] == "main10" {
			return true
		}
	}
	return false
}

// assertMain10Invariant: `-profile main10` ⇒ the encoder's graph ends 10-bit.
func assertMain10Invariant(t *testing.T, label string, args []string) {
	t.Helper()
	if main10Pinned(args) && !tenBitOutput(args) {
		t.Errorf("%s: -profile main10 pinned but final graph hands the encoder 8-bit; args=%q", label, args)
	}
}

func videoGraph(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-filter_complex" && reVideoInput0.MatchString(args[i+1]) {
			return args[i+1]
		}
	}
	return ""
}

func writeSRT(t *testing.T) string {
	t.Helper()
	srt := filepath.Join(t.TempDir(), "temp-0.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:04,000\n#214\n\n"), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	return srt
}

// prod214Argv is the PMS HW-decode text-burn shape from the 2026-10-20 prod
// failure: AV1 Main 10-bit SDR, VAAPI decode, Plex graph kept p010 end to end,
// hevc_vaapi encoder. withOutputFormat=false drops `-hwaccel_output_format:0`
// so the rewriter has to splice it in (trigger 3).
func prod214Argv(srt, graph, encoder string, withOutputFormat bool) []string {
	args := []string{"-codec:0", "av1", "-hwaccel:0", "vaapi"}
	if withOutputFormat {
		args = append(args, "-hwaccel_output_format:0", "vaapi")
	}
	args = append(args,
		"-hwaccel_device:0", "vaapi",
		"-i", "/media/Anime/x.mkv",
		"-i", srt,
		"-init_hw_device", "vaapi=vaapi:/dev/dri/renderD128,driver=iHD",
		"-filter_hw_device", "vaapi",
		"-map_inlineass", "1:s:0",
		"-filter_complex", graph,
		"-map", "[4]",
		"-codec:0", encoder, "-maxrate:0", "20121k",
		"-map", "0:1", "-codec:1", "copy",
		"-f", "segment", "/transcode/media-%05d.ts",
		"-map", "1:s:0", "-f", "null", "-codec", "ass", "nullfile",
	)
	return args
}

const (
	graph214TenBit   = "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=p010[1];[1]hwdownload,format=p010[2];[2]inlineass=font_scale=1.0:font_path=/x.otf:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]"
	graph214EightBit = "[0:0]hwupload[0];[0]scale_vaapi=w=1920:h=1080:format=nv12[1];[1]hwdownload,format=nv12[2];[2]inlineass=font_scale=1.0:font_path=/x.otf:outline=2:shadow=1:font_size=54[3];[3]hwupload[4]"
)

func rewrite214(args []string, transfer string, probe bool) RewriteResult {
	opts := &RewriteOpts{
		FSExists:           func(string) bool { return true },
		ProbeSubtitleCodec: func(string, string) string { return "subrip" },
	}
	if probe {
		opts.ProbeVideoColor = func(string) (string, string, string) { return transfer, "", "" }
	}
	return Rewrite(args, nil, opts)
}

// The prod case plus trigger 2 (HDR/color probe missing or empty): Plex's graph
// alone says 10-bit, so the recomposed burn chain must stay p010 and the
// Main10 pin must follow it.
func TestRewriter214_TenBitSDR_TextBurn_HEVC_StaysTenBit(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0")
	withDialect(t, vaapiDialect{})
	for _, tc := range []struct {
		name     string
		transfer string
		probe    bool
	}{
		{"sdr-bt709-probed", "bt709", true},
		{"probe-returns-empty", "", true},
		{"no-probe", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := rewrite214(prod214Argv(writeSRT(t), graph214TenBit, "hevc_vaapi", true), tc.transfer, tc.probe)
			if !out.Applied {
				t.Fatalf("expected rewrite; changes=%v", out.Changes)
			}
			fc := videoGraph(out.Args)
			if !strings.Contains(fc, "scale_vaapi=w=1920:h=1080:format=p010") {
				t.Errorf("10-bit source chain must stay p010; graph=%q", fc)
			}
			if strings.Contains(fc, "format=nv12") {
				t.Errorf("10-bit source chain must not drop to nv12; graph=%q", fc)
			}
			if !containsString(out.Changes, TagHWDecodeFilterInlineassVA) {
				t.Errorf("expected %s; changes=%v", TagHWDecodeFilterInlineassVA, out.Changes)
			}
			if !main10Pinned(out.Args) || !containsString(out.Changes, TagEncodeHEVCMain10) {
				t.Errorf("expected Main10 pin on 10-bit HEVC; args=%q changes=%v", out.Args, out.Changes)
			}
			assertMain10Invariant(t, tc.name, out.Args)
		})
	}
}

// Trigger 3: HDR source + text burn, Plex sent no -hwaccel_output_format, so
// the rewriter splices it in. The encoder lookup used to read a stale index
// after that splice and silently decide "not HEVC" → 8-bit chain + Main10 pin.
func TestRewriter214_SplicedOutputFormat_StillTenBit(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0")
	withDialect(t, vaapiDialect{})
	for _, graph := range []string{graph214TenBit, graph214EightBit} {
		out := rewrite214(prod214Argv(writeSRT(t), graph, "hevc_vaapi", false), "smpte2084", true)
		if !out.Applied {
			t.Fatalf("expected rewrite; changes=%v", out.Changes)
		}
		if !argvHasSeq(out.Args, "-hwaccel_output_format:0", "vaapi") {
			t.Errorf("expected spliced -hwaccel_output_format:0 vaapi; args=%q", out.Args)
		}
		fc := videoGraph(out.Args)
		if !strings.Contains(fc, "format=p010") || strings.Contains(fc, "format=nv12") {
			t.Errorf("HDR-passthrough HEVC burn must stay p010 after the splice; graph=%q", fc)
		}
		if !main10Pinned(out.Args) {
			t.Errorf("expected Main10 pin; args=%q", out.Args)
		}
		assertMain10Invariant(t, "spliced", out.Args)
	}
}

// Controls: an 8-bit SDR source stays 8-bit with no pin, and a 10-bit source
// to H.264 never gets a Main10 pin (H.264 has no Main10).
func TestRewriter214_Controls(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0")
	withDialect(t, vaapiDialect{})

	out := rewrite214(prod214Argv(writeSRT(t), graph214EightBit, "hevc_vaapi", true), "bt709", true)
	if !out.Applied {
		t.Fatalf("8-bit: expected rewrite; changes=%v", out.Changes)
	}
	if fc := videoGraph(out.Args); strings.Contains(fc, "p010") {
		t.Errorf("8-bit SDR source must stay nv12; graph=%q", fc)
	}
	if main10Pinned(out.Args) {
		t.Errorf("8-bit SDR source must not get Main10; args=%q", out.Args)
	}

	out = rewrite214(prod214Argv(writeSRT(t), graph214TenBit, "h264_vaapi", true), "bt709", true)
	if !out.Applied {
		t.Fatalf("h264: expected rewrite; changes=%v", out.Changes)
	}
	if main10Pinned(out.Args) {
		t.Errorf("h264 must never get Main10; args=%q", out.Args)
	}
	if fc := videoGraph(out.Args); strings.Contains(fc, "p010") {
		t.Errorf("h264 output must keep the 8-bit default chain; graph=%q", fc)
	}
}

// Case B: a VAAPI-shaped 10-bit text burn dispatched to an NVIDIA worker. The
// cross-backend reshape used to rebuild it as scale_cuda format=nv12, dropping
// the 10-bit chain (and Main10) the source argv asked for.
func TestRewriter214_CrossBackend_VAAPI_to_NVENC_KeepsTenBit(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0")
	withDialect(t, nvencDialect{})
	out := rewrite214(prod214Argv(writeSRT(t), graph214TenBit, "hevc_vaapi", true), "bt709", true)
	if !out.Applied {
		t.Fatalf("expected rewrite; changes=%v", out.Changes)
	}
	if !containsString(out.Changes, "cross-backend:vaapi->nvenc") {
		t.Fatalf("expected cross-backend reshape; changes=%v", out.Changes)
	}
	fc := videoGraph(out.Args)
	if !strings.Contains(fc, "scale_cuda=") || !strings.Contains(fc, "format=p010") || strings.Contains(fc, "format=nv12") {
		t.Errorf("cross-backend 10-bit burn must stay p010 on scale_cuda; graph=%q", fc)
	}
	if !containsString(out.Args, "hevc_nvenc") || !main10Pinned(out.Args) {
		t.Errorf("expected hevc_nvenc + Main10 pin; args=%q", out.Args)
	}
	assertMain10Invariant(t, "cross-backend", out.Args)
}

// Invariant sweep: every committed replay-corpus argv × every HW dialect ×
// every color-probe outcome. Whatever the rewriter emits, a Main10 pin must
// never sit on an 8-bit encoder hand-off (scaleplex#214 test invariant; the
// broader corpus × dialect matrix is #218).
func TestRewriter214_CorpusMain10Invariant(t *testing.T) {
	t.Setenv("SCALEPLEX_SUB_RENDER_HEIGHT", "0")
	files, err := filepath.Glob(filepath.Join("testdata", "replay-corpus", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no replay corpus fixtures found (err=%v)", err)
	}
	dialects := map[string]dialect{
		"vaapi-intel": vaapiDialect{},
		"vaapi-amd":   vaapiDialect{vendor: "amd"},
		"nvenc":       nvencDialect{},
	}
	checked, pinned := 0, 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var fx struct {
			SessionID string            `json:"session_id"`
			Argv      []string          `json:"argv"`
			Env       map[string]string `json:"env"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil || len(fx.Argv) == 0 {
			continue
		}
		for dname, d := range dialects {
			for _, transfer := range []string{"bt709", "smpte2084", ""} {
				prev := activeDialect
				activeDialect = d
				args := append([]string(nil), fx.Argv...)
				tr := transfer
				res := Rewrite(args, fx.Env, &RewriteOpts{
					SessionDir:      filepath.Join("/tmp/replay", fx.SessionID),
					ProbeVideoColor: func(string) (string, string, string) { return tr, "", "" },
				})
				activeDialect = prev
				if !res.Applied {
					continue
				}
				checked++
				if main10Pinned(res.Args) {
					pinned++
				}
				assertMain10Invariant(t, filepath.Base(f)+"/"+dname+"/"+transfer, res.Args)
			}
		}
	}
	if checked == 0 || pinned == 0 {
		t.Fatalf("sweep exercised nothing useful: checked=%d pinned=%d", checked, pinned)
	}
	t.Logf("checked %d rewrites, %d carried a Main10 pin", checked, pinned)
}
