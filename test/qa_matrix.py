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
import json
import os
import re
import shlex
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

# Worker control mode. Default "auto" combines:
#   k8s sources — the in-cluster Arc worker DaemonSet (set env + rollout,
#       kubectl logs). Active when worker_pods() returns pods.
#   docker sources — an external compose/docker-run worker over SSH (e.g. the
#       skw-d-frank NVIDIA worker, or a remote AMD test bench). Active when
#       WORKER_SSH + WORKER_DOCKER_NAME envs are set; reads `docker logs`.
# Both can be active simultaneously (hybrid fleet — k8s DS + external push
# worker). worker_logs() combines logs from all active sources, so a matrix
# session dispatched to an external push worker no longer NODISPATCH-misses
# the spawn signal just because the harness was looking only at kubectl logs.
# Explicit "k8s" or "docker" pins one channel (legacy behaviour; back-compat).
# #125 item 1.
WORKER_MODE = os.environ.get("WORKER_MODE", "auto")
if WORKER_MODE not in ("auto", "k8s", "docker"):
    sys.exit(f"WORKER_MODE={WORKER_MODE!r} invalid; expected 'auto', 'k8s', or 'docker'")
WORKER_SSH = os.environ.get("WORKER_SSH", "")               # e.g. root@skw-d-frank.boeye.net
WORKER_COMPOSE_DIR = os.environ.get("WORKER_COMPOSE_DIR", "/root/scaleplex-deploy")
WORKER_DOCKER_NAME = os.environ.get("WORKER_DOCKER_NAME", "scaleplex-deploy-worker-1")
# Seconds to wait after a docker-worker recreate for the agent to boot +
# PUSH-re-register with the orchestrator before driving sessions.
WORKER_DOCKER_SETTLE = float(os.environ.get("WORKER_DOCKER_SETTLE", "18"))

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

# Plex-for-Windows identity → Plex emits the segmented-matroska output muxer
# (-f segment -segment_format matroska), a distinct worker path vs Web's DASH /
# HLS-mpegts. Used by the windows-mkv muxer case.
WINDOWS_HEADERS = {
    "X-Plex-Product": "Plex for Windows",
    "X-Plex-Version": "1.112.0",
    "X-Plex-Platform": "Windows",
    "X-Plex-Platform-Version": "10",
    "X-Plex-Device": "Windows",
    "X-Plex-Device-Name": "scaleplex-QA-win",
    "X-Plex-Model": "standalone",
}

# Real client profiles harvested from prod via test/harvest_client_profiles.py
# (LogVerbose X-Plex-* capture → vcflogs → distilled to the headers PMS needs to
# profile that device). Each entry carries the full X-Plex-Client-Profile-Extra
# capability string + the X-Plex-Client-Profile-Name seed, so PMS produces a real
# transcode decision when --client-profiles selects it. Smart-TV/console identities
# (PS4, LG webOS) need BOTH the seed and the extra; bare entries 400.
# Refresh: re-run the harvester and overwrite test/client_profiles.json. See
# scaleplex #74 / #115.
_PROFILES_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "client_profiles.json")
try:
    with open(_PROFILES_PATH) as _f:
        CLIENT_PROFILES = json.load(_f)
except FileNotFoundError:
    CLIENT_PROFILES = {}

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


def _ssh(cmd, timeout=200, check=False):
    r = subprocess.run(["ssh", "-o", "StrictHostKeyChecking=no", WORKER_SSH, cmd],
                       capture_output=True, text=True, timeout=timeout)
    if check and r.returncode != 0:
        raise subprocess.CalledProcessError(r.returncode, cmd, r.stdout, r.stderr)
    return r


def worker_pods():
    r = kubectl("get", "pods", "-l", "app.kubernetes.io/controller=worker",
                "-o", "name")
    return [p.split("/", 1)[1] for p in r.stdout.split() if p.strip()]


# Cache the per-mode availability (single check per matrix run).
_k8s_available = None
_docker_available = None


def _use_k8s():
    """True when k8s worker pods exist (or mode forces it)."""
    global _k8s_available
    if WORKER_MODE == "docker":
        return False
    if WORKER_MODE == "k8s":
        return True
    if _k8s_available is None:
        try:
            _k8s_available = bool(worker_pods())
        except Exception:
            _k8s_available = False
    return _k8s_available


def _use_docker():
    """True when an external docker worker is wired via env (or mode forces it)."""
    global _docker_available
    if WORKER_MODE == "k8s":
        return False
    if WORKER_MODE == "docker":
        return True
    if _docker_available is None:
        _docker_available = bool(WORKER_SSH and WORKER_DOCKER_NAME)
    return _docker_available


def set_force_hw(val):
    """Apply FORCE_HW to all available worker channels. k8s = DS env+rollout,
    docker = compose recreate (no-op if no compose dir wired). When neither
    applies, log + skip (operator must have set it on the worker externally).
    #125 item 2."""
    did = []
    if _use_k8s():
        kubectl("set", "env", f"ds/{WORKER_DS}", f"SCALEPLEX_FORCE_HW={val}")
        kubectl("rollout", "status", f"ds/{WORKER_DS}", "--timeout=180s")
        did.append("k8s-DS-rollout")
    if _use_docker() and WORKER_COMPOSE_DIR:
        # Rewrite the compose FORCE_HW env + recreate; agent re-PUSH-registers
        # within a few seconds. check=True — a failed recreate invalidates the
        # cells that follow.
        _ssh("cd {d} && "
             "sed -i 's/SCALEPLEX_FORCE_HW: .*/SCALEPLEX_FORCE_HW: \"{v}\"/' compose.yaml && "
             "docker compose up -d".format(
                 d=shlex.quote(WORKER_COMPOSE_DIR), v=int(val)),
             check=True)
        time.sleep(WORKER_DOCKER_SETTLE)
        did.append("docker-compose-recreate")
    if not did:
        print(f"  WARN: set_force_hw({val}) — no k8s DS, no compose-wired docker worker; "
              f"skipping (worker FORCE_HW must be set externally on plain docker run)")


def worker_logs(since):
    """Combined worker log text for the given lookback window, from ALL active
    channels (k8s pods + ssh+docker for external worker, when applicable).
    Hybrid fleets see both. #125 item 1."""
    out = []
    if _use_k8s():
        for pod in worker_pods():
            out.append(kubectl("logs", pod, "-c", WORKER_CONTAINER, f"--since={since}").stdout)
    if _use_docker():
        # Tolerant: transient ssh/docker hiccup shouldn't abort the matrix.
        out.append(_ssh("docker logs {n} --since={s} 2>&1".format(
            n=shlex.quote(WORKER_DOCKER_NAME), s=shlex.quote(since))).stdout)
    return "\n".join(out)


PMS_POD_PREFIX = os.environ.get("PMS_POD_PREFIX", f"{NS}-pms")
ORCH_POD_PREFIX = os.environ.get("ORCH_POD_PREFIX", f"{NS}-orchestrator")


def _pod_by_prefix(prefix):
    r = kubectl("get", "pods", "-o", "name")
    for p in r.stdout.split():
        n = p.split("/", 1)[1] if "/" in p else p
        if n.startswith(prefix):
            return n
    return ""


def pms_logs(since):
    """plex-test PMS pod logs (the 'plex' container) — the Plex-side ground
    truth for SKIP/NODISPATCH cross-checks (decision + transcode-session spawn).
    Returns kubectl output when reachable; empty otherwise (tolerant — PMS is
    in k8s even in hybrid fleets with external workers, so don't skip on
    WORKER_MODE alone)."""
    if WORKER_MODE == "docker" and not _use_k8s():
        return ""
    try:
        pod = _pod_by_prefix(PMS_POD_PREFIX)
        return kubectl("logs", pod, "-c", "plex", f"--since={since}").stdout if pod else ""
    except Exception:
        return ""


def orch_logs(since):
    """Orchestrator pod logs — did the PMS-side shim's /task reach the
    orchestrator + get dispatched? Used to localize a NODISPATCH."""
    if WORKER_MODE == "docker" and not _use_k8s():
        return ""
    try:
        pod = _pod_by_prefix(ORCH_POD_PREFIX)
        return kubectl("logs", pod, f"--since={since}").stdout if pod else ""
    except Exception:
        return ""


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


SUB_TEXT = {"srt", "subrip", "ass", "ssa", "webvtt", "mov_text"}
SUB_BITMAP = {"pgs", "pgssub", "vobsub", "dvd_subtitle", "dvdsub"}


def find_sub_stream(rk):
    """Return {'text': id, 'bitmap': id} of the first matching subtitle stream
    IDs on this item (for forcing sub-burn). Missing kinds are absent."""
    code, body = plex(f"/library/metadata/{rk}")
    out = {}
    if code != 200:
        return out
    for sid, codec in re.findall(r"<Stream id=\"(\d+)\"[^>]*?streamType=\"3\"[^>]*?codec=\"([a-z0-9_]+)\"", body):
        c = codec.lower()
        if c in SUB_TEXT and "text" not in out:
            out["text"] = sid
        elif c in SUB_BITMAP and "bitmap" not in out:
            out["bitmap"] = sid
    return out


def _filter_cases_for_profile(cases, meta):
    """Drop case variants the profile's harvested behaviour says never happen.

    Skips `windows-segmkv` unless the device is Plex-for-Windows; drops sub-burn
    cases unless 'burn' was observed in the harvest's `subtitles=` query param
    (empty observation = keep, treat as unknown); drops `audio-mkv(...)` unless
    the profile direct-streams audio (directStream=1 observed). #116."""
    if not meta:
        return cases
    pclass = meta.get("product_class", "")
    subs = set(meta.get("subtitles") or [])
    dstream = set(meta.get("direct_stream") or [])
    out = []
    for c in cases:
        lbl = c["label"]
        if lbl == "windows-segmkv" and pclass != "desktop_windows":
            continue
        if lbl in ("text-burn", "bitmap-burn") and subs and "burn" not in subs:
            continue
        if lbl.startswith("audio-mkv(") and dstream and "1" not in dstream:
            continue
        out.append(c)
    return out


def _profile_proto(meta, default_protos):
    """Most-common observed protocol for the profile, mapped to hls/dash. '*' (Plex
    wildcard) and 'http' (direct-play) fall back to default."""
    if not meta:
        return default_protos
    for p in meta.get("protocols") or []:
        if p in ("hls", "dash"):
            return [p]
    return default_protos


# Smart-mode prod-baseline server combo (the de-facto prod prefs). When
# --client-profiles is active and smart-mode on, this single combo replaces the
# 16-combo cartesian — profile axis is orthogonal to server-pref axis. #116.
SMART_BASELINE_COMBO = {
    "HardwareAcceleratedCodecs": 1,
    "HardwareAcceleratedEncoders": 1,
    "TranscoderHEVCEncodingMode": "hevc-sources",
    "TranscoderToneMapping": 1,
}


def build_cases(content):
    """Curate a content/subtitle case list that exercises every worker SHAPE
    axis: resolution + HDR (via content) and subtitle kind none/text/bitmap
    (via forced burn). Decode/encode/HDR-tonemap come from the server-pref
    axis; protocol (hls/dash) is layered in the main loop. One representative
    item per axis-value, discovered from the library."""
    def first(pred):
        return next((c for c in content if pred(c)), None)

    def case(rk, title, label, extra=None, client=None, protocols=None):
        return {"rk": rk, "title": title, "label": label,
                "extra": extra or {}, "client": client, "protocols": protocols}

    cases = []
    sdr1080 = first(lambda c: c[3] in ("1080", "720") and c[4] == "sdr")
    # The section listing omits colorTrc, so HDR items mislabel as "sdr" — confirm
    # via the metadata call (is_hdr). Prefer a 4K HDR item; fall back to any HDR.
    hdr4k = first(lambda c: c[3] == "4k" and is_hdr(c[0])) or first(lambda c: is_hdr(c[0]))
    if sdr1080:
        cases.append(case(sdr1080[0], sdr1080[1], "sdr-nosub"))
    if hdr4k:
        cases.append(case(hdr4k[0], hdr4k[1], "hdr-nosub"))
    # sub-burn: scan content for items carrying a text / bitmap sub stream.
    text_done = bitmap_done = False
    for rk, title, *_ in content:
        if text_done and bitmap_done:
            break
        subs = find_sub_stream(rk)
        if not text_done and "text" in subs:
            cases.append(case(rk, title, "text-burn",
                              {"subtitleStreamID": subs["text"], "subtitles": "burn"}))
            text_done = True
        if not bitmap_done and "bitmap" in subs:
            cases.append(case(rk, title, "bitmap-burn",
                              {"subtitleStreamID": subs["bitmap"], "subtitles": "burn"}))
            bitmap_done = True

    # Muxer coverage beyond the protocol param (the HLS segment container +
    # Plex-Windows shapes the corpus showed are real):
    #  - ssegment+matroska: force mpegts-INCOMPATIBLE audio kept as-is
    #    (directStreamAudio=1) on an EAC3/TrueHD/DTS title → Plex picks mkv
    #    segments over .ts. hls only.
    #  - segment+matroska: a Plex-for-Windows client → its native seg-mkv muxer.
    AUDIO_MKV = {"eac3", "truehd", "dca", "dts", "mlp"}
    audio_item = None
    for rk, title, *_ in content:
        aud, _ = content_streams(rk)
        if aud & AUDIO_MKV:
            audio_item = (rk, title, sorted(aud & AUDIO_MKV)[0])
            break
    if audio_item:
        cases.append(case(audio_item[0], audio_item[1], f"audio-mkv({audio_item[2]})",
                          {"directStreamAudio": 1}, protocols=["hls"]))
    win_item = sdr1080 or hdr4k or (content[0] if content else None)
    if win_item:
        cases.append(case(win_item[0], win_item[1], "windows-segmkv",
                          client=WINDOWS_HEADERS, protocols=["hls"]))
    return cases


# ---------------------------------------------------------------------------


def build_params(rating_key, sid, proto, extra):
    p = {
        "hasMDE": 1, "path": f"/library/metadata/{rating_key}",
        "mediaIndex": 0, "partIndex": 0, "protocol": proto, "fastSeek": 1,
        "directPlay": 0, "directStream": 0, "directStreamAudio": 0,
        "subtitleSize": 100, "audioBoost": 100, "location": "lan",
        "mediaBufferSize": 50000, "videoQuality": 100,
        "session": sid, "X-Plex-Session-Identifier": sid,
    }
    p.update(extra or {})
    return p


def transcode_decision(params, hdrs):
    """Return (video_decision, code). The MDE decision body marks each Stream
    with decision="transcode|copy|directplay"; we read the VIDEO stream
    (streamType="1"). Only 'transcode' is a worker test — 'copy' (remux) /
    'directplay' mean PMS won't feed the worker an encode (→ SKIP). Attr order
    varies, so match within each <Stream> tag."""
    code, body = plex("/video/:/transcode/universal/decision", params, headers=hdrs, timeout=30)
    for stream in re.findall(r'<Stream\b[^>]*>', body):
        if 'streamType="1"' in stream:
            d = re.search(r'decision="([a-z]+)"', stream)
            if d:
                return d.group(1), code
    return "", code


def trigger_spawn(params, hdrs):
    """Force the lazy ffmpeg spawn: fetch the manifest then follow its first
    resolved sub-URLs (HLS index → segment). Idempotent — safe to re-call as a
    retry when the worker hasn't spawned yet."""
    code, body = plex("/video/:/transcode/universal/start.m3u8", params, headers=hdrs, timeout=30)
    if code == 200:
        for rel in [s for s in (ln.strip() for ln in body.splitlines())
                    if s and not s.startswith("#")][:2]:
            plex(f"/video/:/transcode/universal/{rel}",
                 {"X-Plex-Session-Identifier": params["X-Plex-Session-Identifier"]},
                 headers=hdrs, timeout=20)
    return code


def stop_session(sid):
    plex("/video/:/transcode/universal/stop", {"session": sid}, timeout=10)


ERR_RE = re.compile(
    r"not supported by the device type|error code: -38|exit status 218|"
    r"exit status 234|invalid source device name|Conversion failed|"
    r"Error reinitializing|skip:[a-z-]+", re.I)
TAG_RE = re.compile(r"rewriter applied: ([^\"]+)")


def _scan_logs(slug, since):
    spawned = first_seg = False
    tags = ""
    errors = []
    for line in worker_logs(since).splitlines():
        if slug not in line:
            continue
        if "spawned ffmpeg" in line:
            spawned = True
        if "first segment ready" in line:
            first_seg = True
        mt = TAG_RE.search(line)
        if mt:
            tags = mt.group(1)
        head = line.split("stderr_tail=", 1)[0]  # skip the source stream dump
        m = ERR_RE.search(head)
        if m:
            errors.append(m.group(0))
    return spawned, first_seg, tags, errors


def _poll_logs(slug, started_at, max_wait, want_seg):
    """Poll worker logs in a window that grows only from started_at (so it can't
    reach a prior same-slug case — see drive_cell). Stops once the wanted signal
    (spawn, or first-segment when want_seg) or an error appears, or on timeout."""
    spawned = first_seg = False
    tags = ""
    errors = []
    while True:
        elapsed = int(time.time() - started_at) + 4
        spawned, first_seg, tags, errors = _scan_logs(slug, f"{elapsed}s")
        hit = errors or (first_seg if want_seg else spawned)
        if hit or time.time() - started_at >= max_wait:
            break
        time.sleep(2)
    return spawned, first_seg, tags, errors


def drive_cell(case, proto, settle):
    """Drive one cell to an AUTHORITATIVE verdict:
      SKIP       — PMS chose not to transcode (directplay/copy) → not a worker test
      NODISPATCH — PMS decided transcode but no ffmpeg ever spawned (harness/
                   orchestrator dispatch miss; retried) → not a worker correctness fail
      PASS       — worker spawned + produced a first segment, no errors
      FAIL       — worker spawned but errored, or produced no segment
    Returns (status, info, sid)."""
    sid = str(uuid.uuid4())
    params = build_params(case["rk"], sid, proto, case["extra"])
    hdrs = {**(case["client"] or CLIENT_HEADERS), "X-Plex-Client-Identifier": f"qa-{sid[:8]}"}
    slug = re.sub(r"[^A-Za-z0-9]+", "_", case["title"]).strip("_")[:18]

    vdec, dcode = transcode_decision(params, hdrs)
    if dcode == 400:
        # PMS could not match a client profile (TV/console identities need an
        # X-Plex-Client-Profile-Extra we don't send) — not a worker fault.
        return "SKIP", {"reason": "client-profile-unmatched(400)"}, sid
    if dcode != 200:
        return "FAIL", {"reason": f"decision={dcode}"}, sid

    # Trigger + confirm a worker spawn. One trigger, then poll up to `settle` for
    # the spawn signal. The older 4x trigger_spawn retry loop turned out to be
    # counterproductive (#118): each retry started a NEW PMS transcode session +
    # orchestrator dispatch, racing the original worker — av1->hevc inits that
    # crossed the per-retry 8s window were misdiagnosed as NODISPATCH while the
    # worker had actually spawned. A single trigger with a single longer poll
    # gives slow inits room to land in the kubectl-log stream.
    started = time.time()
    trigger_spawn(params, hdrs)
    spawned, _, _, errs = _poll_logs(slug, started, settle, want_seg=False)

    if vdec != "transcode":
        if not (spawned or errs):
            # Genuine SKIP — Plex chose copy/directplay AND no worker ran.
            # Double-check against the PMS log (Frank: confirm it really was a
            # direct-stream decision).
            pms = pms_logs("90s")
            return "SKIP", {"video_decision": vdec or "none",
                            "pms_directplay": bool(re.search(r"(?i)direct ?(play|stream)", pms))
                            if pms else None}, sid
        # else: decision read non-transcode but a worker DID run → not a real
        # skip; fall through and verify it like a transcode.

    if not spawned and not errs:
        # NODISPATCH should never happen for a real transcode — capture PMS +
        # orchestrator evidence to localize where the dispatch dropped.
        pms = pms_logs("120s")
        orch = orch_logs("120s")
        return "NODISPATCH", {"reason": f"decided-transcode, no worker spawn ({settle:.0f}s)",
                              "pms_started_transcode": bool(re.search(r"(?i)transcod", pms)),
                              "orch_got_task": ("/task" in orch or sid[:8] in orch)}, sid

    _, seg, tags, errors = _poll_logs(slug, started, settle, want_seg=True)
    if errors:
        return "FAIL", {"errors": sorted(set(errors)), "tags": tags}, sid
    if seg:
        return "PASS", {"tags": tags}, sid
    return "FAIL", {"reason": "spawned, no first-segment", "tags": tags}, sid


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
    ap.add_argument("--force-hw", default=None,
                    help="comma list of SCALEPLEX_FORCE_HW values (e.g. '1,0'). "
                         "Defaults to '1,0' in synthetic mode, '0' in smart-profile mode.")
    ap.add_argument("--settle", type=float, default=20,
                    help="secs to wait per session for spawn / first segment (default 20; "
                         "av1->hevc HW transcodes need ~12-15s for first segment, #118)")
    ap.add_argument("--protocols", default="hls,dash", help="comma list, e.g. hls,dash")
    ap.add_argument("--client-profiles", default="",
                    help="iterate real captured client profiles as the client (outer axis), "
                         f"instead of the synthetic Plex-Web default. 'all' or a comma list of: "
                         f"{','.join(CLIENT_PROFILES)}. Smart-mode is enabled by default when "
                         "this is set (see --no-smart).")
    ap.add_argument("--no-smart", action="store_true",
                    help="With --client-profiles, restore the full cartesian (all server-combos "
                         "x force_hw x cases x protocols x profiles). Default = smart mode: "
                         "single prod-baseline server combo, per-profile case filter + proto "
                         "pinning, FORCE_HW=[0]. #116.")
    args = ap.parse_args()
    protocols = [p.strip() for p in args.protocols.split(",") if p.strip()]
    if not TOKEN:
        sys.exit("set PLEX_TOKEN")

    # Client axis: synthetic default, or one entry per selected real profile (with meta).
    if args.client_profiles.strip():
        sel = (list(CLIENT_PROFILES) if args.client_profiles.strip() == "all"
               else [s.strip() for s in args.client_profiles.split(",") if s.strip()])
        bad = [s for s in sel if s not in CLIENT_PROFILES]
        if bad:
            sys.exit(f"unknown client profile(s): {bad}; known: {list(CLIENT_PROFILES)}")
        client_axis = [(name, CLIENT_PROFILES[name].get("headers"),
                        CLIENT_PROFILES[name].get("meta") or {}) for name in sel]
        smart = not args.no_smart
    else:
        client_axis = [("", None, None)]  # synthetic per-case client (unchanged default)
        smart = False

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
    cases = build_cases(content)
    if not cases:  # fall back to the single rich backbone if discovery is thin
        cases = [{"rk": rich[0], "title": rich[1], "label": "backbone",
                  "extra": {}, "client": None, "protocols": None}]
    print(f"backbone content: {rich[0]} {rich[1]} ({rich[2]}/{rich[3]}/{rich[4]})")
    print(f"discovered {len(content)} content shapes; cases:")
    for c in cases:
        ps = c["protocols"] or protocols
        print(f"  - {c['label']}: {c['title']} (rk={c['rk']}) protos={ps}"
              f"{' [windows]' if c['client'] else ''}")
    print(f"default protocols: {protocols}\n")

    # Resolve fhw + server_combos honouring smart-mode defaults.
    if args.quick:
        fhw_vals = [1]
    elif args.force_hw is None:
        fhw_vals = [0] if smart else [1, 0]
    else:
        fhw_vals = [int(x) for x in args.force_hw.split(",")]
    combos = [SMART_BASELINE_COMBO] if smart else server_combos(args.quick)

    # Precompute per-profile filtered cases (so the projected-count is honest).
    def cases_for(meta):
        return _filter_cases_for_profile(cases, meta) if smart else cases
    def protos_for(case, meta):
        if case["protocols"]:
            return case["protocols"]
        # Smart-mode: pin to the profile's observed proto (hls/dash); fall back to
        # ['hls'] when the harvest only saw '*' or 'http' (the transcode proto
        # Plex uses for TV/console/mobile clients by default).
        if smart:
            return _profile_proto(meta, ["hls"])
        return protocols

    projected = 0
    for _, _, meta in client_axis:
        pc = cases_for(meta)
        projected += sum(len(protos_for(c, meta)) for c in pc)
    projected *= len(fhw_vals) * len(combos)

    if client_axis[0][0]:
        mode = "smart" if smart else "cartesian (no-smart)"
        print(f"client profiles ({mode}): {', '.join(n for n, _, _ in client_axis)}")
    print(f"projected cells: {projected} "
          f"({len(fhw_vals)} force-hw x {len(combos)} server-combo x "
          f"{len(client_axis)} client x filtered cases-protos)\n")
    results = []
    for fhw in fhw_vals:
        print(f"### FORCE_HW={fhw} — rolling workers ###")
        set_force_hw(fhw)
        for combo in combos:
            if not set_prefs(combo):
                print(f"  PREFS-FAIL {combo}")
                continue
            time.sleep(2)
            plabel = ",".join(f"{k.replace('HardwareAccelerated','HW').replace('Transcoder','')[:10]}={v}"
                              for k, v in combo.items())
            for pname, phdrs, pmeta in client_axis:
                for c in cases_for(pmeta):
                    # When a real profile is selected it IS the client under test,
                    # so it overrides the case's synthetic client (e.g. WINDOWS_HEADERS).
                    cc = c if phdrs is None else {**c, "client": phdrs}
                    for proto in protos_for(cc, pmeta):
                        label = (f"{pname+':' if pname else ''}{cc['label']}/{proto} | {plabel}")
                        status, info, sid = drive_cell(cc, proto, args.settle)
                        stop_session(sid)
                        results.append((fhw, label, status, info))
                        print(f"  [{status:10s}] FORCE_HW={fhw} {label} :: {info}")
                        time.sleep(8)  # let worker cleanup + logs age out before next cell (#118)

    from collections import Counter
    tally = Counter(s for *_, s, _ in results)
    worker_cells = tally["PASS"] + tally["FAIL"]
    print(f"\n=== worker: {tally['PASS']}/{worker_cells} PASS "
          f"| SKIP(no-transcode)={tally['SKIP']} | NODISPATCH={tally['NODISPATCH']} "
          f"| total cells={len(results)} ===")
    if tally["FAIL"]:
        print("FAILURES (worker errored / no segment):")
        for fhw, label, status, info in results:
            if status == "FAIL":
                print(f"  FAIL FORCE_HW={fhw} {label} :: {info}")
    if tally["NODISPATCH"]:
        print("NODISPATCH (decided-transcode but never reached the worker — investigate dispatch):")
        for fhw, label, status, info in results:
            if status == "NODISPATCH":
                print(f"  NODISPATCH FORCE_HW={fhw} {label} :: {info}")
    # Ironclad gate: any real worker FAIL is a hard fail.
    sys.exit(1 if tally["FAIL"] else 0)


if __name__ == "__main__":
    main()
