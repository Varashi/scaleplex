#!/usr/bin/env python3
"""scaleplex QA matrix — Tier-2 API-driven transcoder-error harness.

Drives real Plex transcode sessions on a test PMS across the full server-pref
matrix (HW decode/encode, HEVC mode, tonemapping) × scaleplex FORCE_HW ×
representative content, then auto-verifies each on the worker side (spawned
ffmpeg + first segment + no -38/218/234/bail). This catches transcoder errors
and branch-shape regressions WITHOUT a human; quality / smoothness / visual
correctness stay a Tier-3 human pass (see test/README-qa.md).

Why API-driven: the server prefs change the argv *Plex* generates, so we must
let Plex generate them. The request template was captured from a real client
(directStream=0 forces a full transcode; recognized Plex-Web headers load a
base profile). See reference_scaleplex_tonemap_regression_test for the live
findings this guards against.

Run:  PLEX_TOKEN=... python3 test/qa_matrix.py [--quick] [--force-hw 0,1]
Env:  PLEX_URL (default the plex-test LB), NS, WORKER_DS, SECTION
Requires: kubectl context on the cluster; PMS reachable at PLEX_URL.
"""
import argparse
import itertools
import os
import re
import subprocess
import sys
import time
import urllib.parse
import urllib.request
import uuid

PLEX_URL = os.environ.get("PLEX_URL", "http://172.16.4.106:32400")
TOKEN = os.environ.get("PLEX_TOKEN", "")
NS = os.environ.get("NS", "plex-test")
WORKER_DS = os.environ.get("WORKER_DS", "plex-test-worker")
WORKER_CONTAINER = os.environ.get("WORKER_CONTAINER", "app")
MOVIE_SECTION = os.environ.get("SECTION", "1")

# Recognized Plex-Web client identity → Plex loads a base profile (an
# unrecognized platform 400s with "unable to find a matching profile").
CLIENT_HEADERS = {
    "X-Plex-Product": "Plex Web",
    "X-Plex-Version": "4.140.0",
    "X-Plex-Platform": "Chrome",
    "X-Plex-Platform-Version": "120.0",
    "X-Plex-Device": "Linux",
    "X-Plex-Device-Name": "scaleplex-QA",
    "X-Plex-Model": "hosted",
}

# Server-pref axes (the "every combination" backbone). Keys are PMS prefs.
SERVER_AXES = {
    "HardwareAcceleratedCodecs": [1, 0],     # HW decode
    "HardwareAcceleratedEncoders": [1, 0],   # HW encode
    "TranscoderHEVCEncodingMode": ["hevc-sources", "never"],  # HEVC out
    "TranscoderToneMapping": [1, 0],         # HDR tonemap
}

# ---------------------------------------------------------------------------


def plex(path, params=None, headers=None, timeout=30):
    p = dict(params or {})
    p["X-Plex-Token"] = TOKEN
    url = f"{PLEX_URL}{path}?{urllib.parse.urlencode(p)}"
    req = urllib.request.Request(url, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001
        return 0, str(e)


def set_prefs(prefs):
    code, _ = plex("/:/prefs", {k: v for k, v in prefs.items()})
    return code == 200


def kubectl(*args, timeout=200):
    return subprocess.run(["kubectl", "-n", NS, *args], capture_output=True,
                          text=True, timeout=timeout)


def set_force_hw(val):
    kubectl("set", "env", f"ds/{WORKER_DS}", f"SCALEPLEX_FORCE_HW={val}")
    kubectl("rollout", "status", f"ds/{WORKER_DS}", "--timeout=180s")


def worker_pods():
    r = kubectl("get", "pods", "-l", "app.kubernetes.io/controller=worker",
                "-o", "name")
    return [p.split("/", 1)[1] for p in r.stdout.split() if p.strip()]


# ---------------------------------------------------------------------------


def discover_content(max_items=300):
    """Classify movie-section items into representative buckets by
    videoCodec / resolution / HDR(+DoVi) / audio / subtitle, one per shape."""
    code, body = plex(f"/library/sections/{MOVIE_SECTION}/all",
                      {"limit": max_items})
    if code != 200:
        return []
    vids = re.findall(r"<Video ratingKey=\"(\d+)\"[^>]*?title=\"([^\"]{1,40})\""
                      r"[^>]*?>(.*?)</Video>", body, re.S)
    buckets = {}
    for rk, title, sub in vids:
        m = re.search(r"videoCodec=\"([a-z0-9]+)\".*?videoResolution=\"([a-z0-9]+)\"",
                      sub)
        if not m:
            continue
        vcodec, res = m.group(1), m.group(2)
        hdr = bool(re.search(r"(smpte2084|colorTrc=\"smpte2084\"|DOVI)", sub))
        dovi = "DOVIPresent=\"1\"" in sub or "dolby vision" in sub.lower()
        key = (vcodec, res, "dovi" if dovi else ("hdr" if hdr else "sdr"))
        buckets.setdefault(key, (rk, title, vcodec, res, key[2]))
    return list(buckets.values())


def is_hdr(rk):
    """HDR needs the metadata call — the section listing omits colorTrc."""
    code, body = plex(f"/library/metadata/{rk}")
    return code == 200 and bool(re.search(r"colorTrc=\"(smpte2084|arib-std-b67)\""
                                          r"|DOVIPresent=\"1\"", body))


def content_streams(rk):
    """Per-item audio/subtitle codecs (needs the metadata call)."""
    code, body = plex(f"/library/metadata/{rk}")
    if code != 200:
        return set(), set()
    aud = set(re.findall(r"streamType=\"2\"[^>]*?codec=\"([a-z0-9]+)\"", body))
    sub = set(re.findall(r"streamType=\"3\"[^>]*?codec=\"([a-z0-9]+)\"", body))
    return aud, sub


# ---------------------------------------------------------------------------


def start_session(rating_key, extra_params=None):
    sid = str(uuid.uuid4())
    params = {
        "hasMDE": 1, "path": f"/library/metadata/{rating_key}",
        "mediaIndex": 0, "partIndex": 0, "protocol": "hls", "fastSeek": 1,
        "directPlay": 0, "directStream": 0, "directStreamAudio": 0,
        "subtitleSize": 100, "audioBoost": 100, "location": "lan",
        "mediaBufferSize": 50000, "videoQuality": 100,
        "session": sid, "X-Plex-Session-Identifier": sid,
    }
    params.update(extra_params or {})
    # X-Plex-Client-Identifier must be a HEADER, consistent across decision +
    # start, so Plex binds the session + spawns the transcode (as a param it
    # 200s but never reaches the worker).
    hdrs = {**CLIENT_HEADERS, "X-Plex-Client-Identifier": f"qa-{sid[:8]}"}
    # hasMDE=1 requires a transcode DECISION before start.m3u8 will serve
    # ("Denying access due to session lacking decision" → 400 otherwise).
    plex("/video/:/transcode/universal/decision", params, headers=hdrs, timeout=30)
    code, body = plex("/video/:/transcode/universal/start.m3u8", params,
                      headers=hdrs, timeout=30)
    # Transcode is lazy: start.m3u8 returns a master playlist with a RELATIVE
    # index URL (session/<guid>/base/index.m3u8). Fetching the resolved index
    # is what actually spawns ffmpeg + produces the first segment.
    if code == 200:
        for rel in [ln.strip() for ln in body.splitlines()
                    if ln.strip() and not ln.startswith("#")][:1]:
            plex(f"/video/:/transcode/universal/{rel}",
                 {"X-Plex-Session-Identifier": sid}, headers=hdrs, timeout=20)
    return sid, code


def stop_session(sid):
    plex("/video/:/transcode/universal/stop", {"session": sid}, timeout=10)


ERR_RE = re.compile(
    r"not supported by the device type|error code: -38|exit status 218|"
    r"exit status 234|invalid source device name|Conversion failed|"
    r"Error reinitializing|skip:[a-z-]+", re.I)
TAG_RE = re.compile(r"rewriter applied: ([^\"]+)")


def verify(title, since="40s"):
    """Grep worker logs for this title's session; classify pass/fail."""
    slug = re.sub(r"[^A-Za-z0-9]+", "_", title).strip("_")[:18]
    spawned = first_seg = False
    tags = ""
    errors = []
    for pod in worker_pods():
        r = kubectl("logs", pod, "-c", WORKER_CONTAINER, f"--since={since}")
        for line in r.stdout.splitlines():
            if slug not in line:
                continue
            if "spawned ffmpeg" in line:
                spawned = True
            if "first segment ready" in line:
                first_seg = True
            mt = TAG_RE.search(line)
            if mt:
                tags = mt.group(1)
            # error scan only on lines that aren't the source's stream dump
            head = line.split("stderr_tail=", 1)[0]
            if ERR_RE.search(head):
                errors.append(ERR_RE.search(head).group(0))
    ok = first_seg and not errors
    return {"ok": ok, "spawned": spawned, "first_seg": first_seg,
            "tags": tags, "errors": sorted(set(errors))}


# ---------------------------------------------------------------------------


def server_combos(quick):
    keys = list(SERVER_AXES)
    vals = [SERVER_AXES[k] for k in keys]
    combos = [dict(zip(keys, c)) for c in itertools.product(*vals)]
    if quick:  # prod-representative + a couple variations
        combos = [c for c in combos if c["HardwareAcceleratedCodecs"] == 1][:4]
    return combos


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--quick", action="store_true", help="small slice, FORCE_HW=1 only")
    ap.add_argument("--force-hw", default="1,0", help="comma list, e.g. 1,0")
    ap.add_argument("--settle", type=float, default=12, help="secs to wait per session")
    args = ap.parse_args()
    if not TOKEN:
        sys.exit("set PLEX_TOKEN")

    content = discover_content()
    if not content:
        sys.exit("no content discovered")
    # backbone uses the richest item: BACKBONE_RK env override (a known HDR 4k),
    # else a real HDR 4k confirmed via metadata (section HDR flag is unreliable),
    # else first item.
    backbone_rk = os.environ.get("BACKBONE_RK")
    rich = None
    if backbone_rk:
        rich = next((c for c in content if c[0] == backbone_rk), None)
        if rich is None:
            _, mb = plex(f"/library/metadata/{backbone_rk}")
            mt = re.search(r"<Video [^>]*?title=\"([^\"]{1,40})\"", mb)
            rich = (backbone_rk, mt.group(1) if mt else f"rk{backbone_rk}",
                    "?", "?", "hdr")
    if rich is None:
        for c in sorted(content, key=lambda x: (x[2] != "av1", x[3] != "4k")):
            if c[4] in ("hdr", "dovi") or is_hdr(c[0]):
                rich = c
                break
    rich = rich or content[0]
    print(f"backbone content: {rich[0]} {rich[1]} ({rich[2]}/{rich[3]}/{rich[4]})")
    print(f"discovered {len(content)} content shapes\n")

    fhw_vals = [1] if args.quick else [int(x) for x in args.force_hw.split(",")]
    combos = server_combos(args.quick)
    results = []
    for fhw in fhw_vals:
        print(f"### FORCE_HW={fhw} — rolling workers ###")
        set_force_hw(fhw)
        for combo in combos:
            if not set_prefs(combo):
                print(f"  PREFS-FAIL {combo}")
                continue
            time.sleep(2)
            label = ",".join(f"{k.replace('HardwareAccelerated','HW').replace('Transcoder','')[:10]}={v}"
                             for k, v in combo.items())
            sid, code = start_session(rich[0])
            time.sleep(args.settle)
            res = verify(rich[1]) if code == 200 else {"ok": False, "errors": [f"start={code}"], "tags": "", "spawned": False, "first_seg": False}
            stop_session(sid)
            status = "PASS" if res["ok"] else "FAIL"
            results.append((fhw, label, status, res))
            print(f"  [{status}] FORCE_HW={fhw} {label} "
                  f"seg={res['first_seg']} err={res['errors']}")
            time.sleep(2)

    npass = sum(1 for *_, r in results if r["ok"])
    print(f"\n=== {npass}/{len(results)} PASS ===")
    for fhw, label, status, res in results:
        if not res["ok"]:
            print(f"  FAIL FORCE_HW={fhw} {label} :: {res['errors']} tags={res['tags'][:80]}")


if __name__ == "__main__":
    main()
