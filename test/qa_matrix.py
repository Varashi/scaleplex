#!/usr/bin/env python3
"""scaleplex QA matrix — Tier-2 API-driven transcoder-error harness.

Drives real Plex transcode sessions on a test PMS across the full server-pref
matrix (HW decode/encode, HEVC mode, tonemapping) × scaleplex FORCE_HW ×
representative content, then auto-verifies each on the worker side (spawned
ffmpeg + first segment + a post-segment SOAK with no fatal/non-zero exit).
This catches transcoder errors and branch-shape regressions WITHOUT a human;
quality / smoothness / visual correctness stay a Tier-3 human pass (see
test/README-qa.md).

Verifier hardening (#141/#142):
  - Fatal-exit set widened beyond -38/218/234: now also exit 145 (libass /
    fontconfig init family — the bug that escaped for ~3 weeks) and exit 8
    (unknown decoder/encoder/BSF/filter), plus ANY non-zero `ffmpeg exit:`.
  - Soak: 'first segment ready' is necessary but NOT sufficient — for DASH/HLS
    the init segment lands before the first frame, so a first-frame fatal fires
    ~1s later. After the segment we keep watching (--soak-seconds) and FAIL on a
    late exit. A run is GREEN only if every cell survives the soak.
  - Liveness / process-check (#141b): at soak end the session must still be a
    real, running transcode — it must have encoded >=1 frame ('first progress
    block'), its out_time_us must have ADVANCED across the soak (not frozen
    mid-stream after one frame), and it must NOT have terminated (ANY exit code,
    incl. a clean 0 — a premature exit-0 after the init segment is exactly what
    the non-zero-only soak missed). out_time_us rides the throttled 'progress
    heartbeat' line (#166), the signal that made mid-stream advancement
    log-observable; --min-progress N demands >=N heartbeats for a strict bar.
  - NODISPATCH is a hard FAIL (not silent green): cells that never reached the
    worker went unvalidated. Reclassified by observed state (PMS_NO_TRANSCODE /
    ORCH_NOT_NOTIFIED / WORKER_NEVER_SPAWNED). Waive with --allow-nodispatch.
    Hardened against phantom NODISPATCH: a pref-combo is pre-warmed (read back
    until applied) before its first cell, and a no-spawn re-polls out to 2x
    settle (slow av1->hevc inits) before being classified. A startup content
    audit lists sub-burn shapes the library lacks, so a gap isn't a silent skip.
  - Sub-burn stderr cleanliness (#149): the worker surfaces libass/fontconfig
    stderr lines live as `subtitle-stderr: ...`; their presence fails the cell
    (catches image-level font/cache regressions that are often non-fatal).

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
# Section to mine for an embedded-ASS sub-burn cell (#149). Movies rarely carry
# ASS; anime/Series do (styled + animated "Dialogue"/"Signs" tracks). Default
# the Anime show library; "" disables the ASS cell.
ASS_SECTION = os.environ.get("ASS_SECTION", "3")

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
# Empty default so a plain `docker run` worker (no compose) doesn't accidentally
# take the compose-recreate path in set_force_hw() and die in _ssh(check=True).
# Set this when the external worker IS compose-managed.
WORKER_COMPOSE_DIR = os.environ.get("WORKER_COMPOSE_DIR", "")
WORKER_DOCKER_NAME = os.environ.get("WORKER_DOCKER_NAME", "scaleplex-deploy-worker-1")
if WORKER_MODE == "docker" and not WORKER_SSH:
    sys.exit("WORKER_MODE='docker' requires WORKER_SSH (e.g. root@host)")
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
#
# TranscoderToneMapping is deliberately NOT an axis (#160): it's an
# advanced="1" pref PMS pins at value="1" — an explicit
# `?TranscoderToneMapping=0` is silently ignored, so iterating [1, 0] ran
# both as 1 and every ToneMapping=0 combo just duplicated its
# ToneMapping=1 sibling (2 of 4 --quick combos wasted). Dropping it halves
# the cartesian (2^3=8) with zero coverage loss — the HDR-tonemap path is
# still exercised by the HDR content cells, which run under the pinned
# ToneMapping=1. SMART_BASELINE_COMBO + the prod-restore still pin it to 1.
SERVER_AXES = {
    "HardwareAcceleratedCodecs": [1, 0],     # HW decode
    "HardwareAcceleratedEncoders": [1, 0],   # HW encode
    "TranscoderHEVCEncodingMode": ["hevc-sources", "never"],  # HEVC out
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


def prefs_applied(prefs, tries=6):
    """Pre-warm (#142): set_prefs PUTs the combo, but the transcoder re-reads
    prefs lazily — the first cell of a combo could race PMS's re-eval and lose a
    spawn. Poll /:/prefs and confirm the values landed before driving cells (so
    we don't pay it as a phantom NODISPATCH). Returns False on timeout; caller
    proceeds anyway (best-effort warm, not a gate).

    Only the HardwareAccelerated* bools are checked — they read back verbatim
    and gate the spawn-relevant HW path. Skipped: the text enum
    TranscoderHEVCEncodingMode (PMS normalizes "hevc-sources" → "always"),
    which would otherwise never confirm and spam warnings. (TranscoderToneMapping
    is no longer iterated — #160 dropped it as a degenerate axis.)"""
    checkable = {k: v for k, v in prefs.items()
                 if k.startswith("HardwareAccelerated") and str(v) in ("0", "1")}
    for _ in range(tries):
        code, body = plex("/:/prefs")
        if code == 200 and all(
                re.search(rf'id="{re.escape(k)}"[^>]*\bvalue="{v}"', body)
                for k, v in checkable.items()):
            return True
        time.sleep(1)
    return False


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
        except Exception as e:  # noqa: BLE001
            # Don't memoize a probe failure (e.g. transient kubectl timeout) —
            # next caller retries. Memoizing False would disable the k8s
            # channel for the rest of the matrix on one flaky call.
            print(f"  WARN: k8s worker probe failed: {e}; retrying on next use")
            return False
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
        # Tolerant: transient ssh/docker hiccup shouldn't abort the matrix, but
        # surface the failure — a silent empty stdout collapses into bogus
        # NODISPATCH for sessions that DID land on the external worker.
        r = _ssh(
            f"docker logs {shlex.quote(WORKER_DOCKER_NAME)} --since={shlex.quote(since)} 2>&1"
        )
        if r.returncode == 0:
            out.append(r.stdout)
        else:
            print(f"  WARN: docker log collection failed (rc={r.returncode}): "
                  f"{r.stderr.strip() or r.stdout.strip()}")
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


SUB_TEXT = {"srt", "subrip", "webvtt", "mov_text"}
SUB_ASS = {"ass", "ssa"}  # styled/animated — stresses libass far more than srt
SUB_BITMAP = {"pgs", "pgssub", "vobsub", "dvd_subtitle", "dvdsub"}


def find_sub_stream(rk):
    """Return subtitle stream IDs bucketed by '<kind>-<source>' — kind is
    text|bitmap, source is embedded|external. External (sidecar, e.g. a bazarr
    .srt) streams carry key="/library/streams/.."; embedded (in-container) ones
    don't. The two drive different PMS argv shapes — embedded `-map 0:s:N`
    vs sidecar `-i temp.srt` + `-map_inlineass 1:s:0` — so the rewriter exercises
    different paths (#141a). First match per bucket wins; missing buckets are
    absent."""
    code, body = plex(f"/library/metadata/{rk}")
    out = {}
    if code != 200:
        return out
    for tag in re.findall(r"<Stream\b[^>]*streamType=\"3\"[^>]*>", body):
        mid = re.search(r'\bid="(\d+)"', tag)
        mcodec = re.search(r'\bcodec="([a-z0-9_]+)"', tag)
        if not (mid and mcodec):
            continue
        c = mcodec.group(1).lower()
        source = "external" if 'key="/library/streams/' in tag else "embedded"
        if c in SUB_ASS:
            out.setdefault(f"ass-{source}", mid.group(1))
        elif c in SUB_TEXT:
            out.setdefault(f"text-{source}", mid.group(1))
        elif c in SUB_BITMAP:
            out.setdefault(f"bitmap-{source}", mid.group(1))
    return out


def _alnum(s):
    """Lowercase, strip everything non-alphanumeric. Used to compare a worker
    log line against a cell's correlation key separator-agnostically."""
    return re.sub(r"[^a-z0-9]+", "", s.lower())


def source_corr_key(rk):
    """Correlation key for matching worker logs to this cell. PMS derives the
    transcode SessionID from the source FILENAME, not the Plex title, and the
    two can diverge (a localized title vs the release filename — e.g. title
    "40-45 De Musical" vs file "40-45, the Musical (2025)"). Slugging the title
    then misses every worker log line → false NODISPATCH (#142).

    Use an alphanumeric PREFIX of the file basename: alnum-only survives PMS's
    separator normalization (hyphens kept, spaces/punct → _), and the short
    prefix survives PMS truncating the SessionID. Empty on lookup failure (the
    caller falls back to the title slug)."""
    code, body = plex(f"/library/metadata/{rk}")
    if code != 200:
        return ""
    m = re.search(r'<Part [^>]*\bfile="([^"]*)"', body)
    if not m:
        return ""
    base = m.group(1).rsplit("/", 1)[-1]
    base = re.sub(r"\.[A-Za-z0-9]+$", "", base)  # drop extension
    return _alnum(base)[:16]


def discover_ass_item(section, limit=60):
    """Find one item in `section` carrying an embedded ASS subtitle. Returns
    (rk, title, ass_stream_id) or None. Used to source the #149 ass-burn cell
    from the anime/Series library (type=4 = episodes) where styled/animated ASS
    actually lives."""
    if not section:
        return None
    code, body = plex(f"/library/sections/{section}/all", {"type": 4, "limit": limit})
    if code != 200:
        return None
    for m in re.finditer(r'<Video\b[^>]*\bratingKey="(\d+)"[^>]*?>', body):
        rk = m.group(1)
        tm = re.search(r'\b(?:grandparentTitle|title)="([^"]{1,40})"', m.group(0))
        title = tm.group(1) if tm else f"rk{rk}"
        subs = find_sub_stream(rk)
        if "ass-embedded" in subs:
            return rk, title, subs["ass-embedded"]
    return None


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
        if lbl.startswith(("text-burn", "bitmap-burn", "ass-burn")) and subs and "burn" not in subs:
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
# 8-combo cartesian — profile axis is orthogonal to server-pref axis. #116.
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
    # sub-burn: scan content for items carrying text/bitmap subs, embedded AND
    # external (sidecar). The embedded vs external rewriter paths differ —
    # in-container `-map 0:s:N` + extractGraphFacts, vs sidecar `-i temp.srt` +
    # `-map_inlineass 1:s:0` — so cover whichever buckets the library provides
    # (#141a). One representative cell per bucket.
    SUB_BUCKETS = ["text-embedded", "text-external", "bitmap-embedded", "bitmap-external"]
    done = set()
    for rk, title, *_ in content:
        if len(done) == len(SUB_BUCKETS):
            break
        subs = find_sub_stream(rk)
        for bucket in SUB_BUCKETS:
            if bucket in done or bucket not in subs:
                continue
            kind, source = bucket.split("-")
            cases.append(case(rk, title, f"{kind}-burn-{source}",
                              {"subtitleStreamID": subs[bucket], "subtitles": "burn"}))
            done.add(bucket)

    # ASS sub-burn (#149): styled/animated ASS exercises libass far harder than
    # SRT (positioning, \pos/\move/\t, font substitution). Movies rarely carry
    # it, so source one embedded-ASS item from the anime/Series library.
    ass = discover_ass_item(ASS_SECTION)
    if ass:
        arc, atitle, asid = ass
        cases.append(case(arc, atitle, "ass-burn-embedded",
                          {"subtitleStreamID": asid, "subtitles": "burn"}))

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
    r"exit status 234|exit status 145|exit status 8|invalid source device name|"
    r"Conversion failed|Error reinitializing|skip:[a-z-]+", re.I)
# ANY non-zero ffmpeg exit is a fatal — the specific-code list above is for
# pattern context, this is the catch-all (#141: exit 145 escaped the old set).
EXIT_RE = re.compile(r"ffmpeg exit:\s*exit status ([1-9]\d*)", re.I)
TAG_RE = re.compile(r"rewriter applied: ([^\"]+)")
# Liveness signals (#141b). ANY ffmpeg exit (incl. a clean status-0) — EXIT_RE
# above is non-zero only, so a premature clean exit slips its net.
EXIT_ANY_RE = re.compile(r"ffmpeg exit:\s*(.+)")
# out_time_us (microseconds of encoded media) rides on BOTH the 'first progress
# block' line and the throttled 'progress heartbeat' line (#166). Watching it
# climb is how a watcher tells an advancing encode from one that wrote a frame
# then froze mid-stream — the stall class #166's heartbeat was added to expose.
OUT_TIME_RE = re.compile(r"out_time_us=(\d+)")


def _scan_logs(slug, since):
    spawned = first_seg = False
    tags = ""
    errors = []
    for line in worker_logs(since).splitlines():
        if slug not in _alnum(line):
            continue
        if "spawned ffmpeg" in line:
            spawned = True
        if "first segment ready" in line:
            first_seg = True
        mt = TAG_RE.search(line)
        if mt:
            tags = mt.group(1)
        # Sub-burn cleanliness (#149): the agent surfaces libass/fontconfig
        # stderr lines live as `subtitle-stderr: <line>`. They only appear when
        # a font cache is unwritable / a font is missing / fontconfig path is
        # bad — image-level regressions that are often non-fatal (so they'd
        # never reach stderr_tail). Their mere presence fails the cell.
        if "subtitle-stderr:" in line:
            errors.append("subtitle-stderr:" + line.split("subtitle-stderr:", 1)[1].strip()[:60])
            continue
        head = line.split("stderr_tail=", 1)[0]  # skip the source stream dump
        m = ERR_RE.search(head)
        if m:
            errors.append(m.group(0))
        me = EXIT_RE.search(head)
        if me:
            errors.append(f"ffmpeg-exit-{me.group(1)}")
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


def _scan_liveness(slug, since):
    """Post-first-segment liveness from worker logs (#141b). 'first segment
    ready' is necessary but not sufficient: a session can write the init
    segment and then stall (no frame ever encoded), encode exactly one frame
    then hang mid-stream, or exit cleanly after one segment — all three pass the
    non-zero-exit soak. Returns (progressed, advanced, beats, exited):

      progressed — saw >=1 out_time_us sample ('first progress block' or a
                   'progress heartbeat', #166): ffmpeg encoded past the init
                   moov. Distinguishes a real transcode from an init-only stall.
      advanced   — True  if >=2 out_time_us samples AND the last exceeds the
                            first (the encode is moving, not frozen);
                   False if >=2 samples that did NOT climb — a mid-stream stall,
                            the class #166's heartbeat was added to expose;
                   None  if <2 samples (a short soak / heavy throttle can't
                            prove a stall, so the caller must not FAIL on it).
      beats      — count of 'progress heartbeat' lines (#166), for --min-progress.
      exited     — the 'ffmpeg exit: <detail>' text if the session terminated
                   (ANY code, incl. a clean 0), else None. For 4K streaming
                   content a termination inside the settle+soak window is
                   premature regardless of code.

    (out_time_us is log-observable on every progress PUT via #166's throttled
    heartbeat — superseding the earlier note that progress signals never reached
    the worker log, which is why a 'segments produced' stall bar was once dropped.)
    """
    samples = []
    beats = 0
    exited = None
    for line in worker_logs(since).splitlines():
        if slug not in _alnum(line):
            continue
        if "progress heartbeat" in line:
            beats += 1
        if "first progress block" in line or "progress heartbeat" in line:
            mo = OUT_TIME_RE.search(line)
            if mo:
                samples.append(int(mo.group(1)))
        m = EXIT_ANY_RE.search(line)
        if m:
            exited = m.group(1).split("stderr_tail=", 1)[0].strip()[:80]
    progressed = bool(samples)
    advanced = (samples[-1] > samples[0]) if len(samples) >= 2 else None
    return progressed, advanced, beats, exited


def drive_cell(case, proto, settle, soak, min_progress=0):
    """Drive one cell to an AUTHORITATIVE verdict:
      SKIP       — PMS chose not to transcode (directplay/copy) → not a worker test
      NODISPATCH — PMS decided transcode but no ffmpeg ever spawned. Reclassified
                   by observed state (PMS_NO_TRANSCODE / ORCH_NOT_NOTIFIED /
                   WORKER_NEVER_SPAWNED) and counted as a hard FAIL by default (the
                   cell went unvalidated) unless --allow-nodispatch (#142).
      PASS       — worker spawned, produced a first segment, survived the soak
                   window with no fatal / non-zero ffmpeg exit (#141), AND passed
                   the liveness gate (#141b): encoded >=1 frame ('first progress
                   block'), out_time_us advanced across the soak (not frozen
                   mid-stream, #166), was still running at soak end (no premature
                   exit of ANY code), and emitted >= --min-progress heartbeats if set.
      FAIL       — worker spawned but errored (incl. a late soak-window exit), or
                   produced no segment, or stalled / exited prematurely after the
                   init segment (liveness).
    Returns (status, info, sid)."""
    sid = str(uuid.uuid4())
    params = build_params(case["rk"], sid, proto, case["extra"])
    hdrs = {**(case["client"] or CLIENT_HEADERS), "X-Plex-Client-Identifier": f"qa-{sid[:8]}"}
    # Correlate worker logs by the source FILENAME (alnum prefix), not the Plex
    # title — PMS's SessionID is filename-derived and the two can diverge (#142).
    # Fall back to the title slug if the file lookup fails.
    slug = source_corr_key(case["rk"]) or _alnum(case["title"])[:16]

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
    eff_settle = settle
    spawned, _, _, errs = _poll_logs(slug, started, eff_settle, want_seg=False)

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
        # Adaptive settle (#142): a slow av1->hevc HW init (12-15s, #118), or a
        # cold first-cell after a worker roll, can cross the settle window.
        # Before declaring a NODISPATCH, poll once more out to 2x settle — same
        # session, NO new trigger (a 2nd trigger races a 2nd PMS session, the
        # #118 footgun). Cheap when the cell really did dispatch slowly. Carry
        # the widened window into the first-segment poll below (else a rescued
        # slow-spawn would instantly FAIL "no first-segment" — CodeRabbit #159).
        eff_settle = settle * 2
        spawned, _, _, errs = _poll_logs(slug, started, eff_settle, want_seg=False)

    if not spawned and not errs:
        # NODISPATCH should never happen for a real transcode — capture PMS +
        # orchestrator evidence to localize where the dispatch dropped, and
        # reclassify by observed state so the headline isn't one opaque bucket
        # (#142). All three classes count as hard FAILs by default.
        pms = pms_logs("120s")
        orch = orch_logs("120s")
        pms_tx = bool(re.search(r"(?i)transcod", pms))
        orch_task = ("/task" in orch or sid[:8] in orch)
        if not pms and not orch:
            # Both log sources empty — e.g. docker-only worker mode with no k8s
            # access to plex-test-pms / -orchestrator. Don't assert
            # PMS_NO_TRANSCODE off absent evidence; flag it as unattributable
            # so the operator wires up log access. (CodeRabbit, #155.)
            klass = "LOGS_UNAVAILABLE"
        elif not pms_tx:
            klass = "PMS_NO_TRANSCODE"      # prefs flip didn't take / PMS chose copy
        elif not orch_task:
            klass = "ORCH_NOT_NOTIFIED"     # shim never POSTed /task to the orchestrator
        else:
            klass = "WORKER_NEVER_SPAWNED"  # orch got the task, PMS transcoding, no worker spawn
        return "NODISPATCH", {"class": klass,
                              "reason": f"decided-transcode, no worker spawn ({settle:.0f}s)",
                              "pms_started_transcode": pms_tx,
                              "orch_got_task": orch_task}, sid

    _, seg, tags, errors = _poll_logs(slug, started, eff_settle, want_seg=True)
    if errors:
        return "FAIL", {"errors": sorted(set(errors)), "tags": tags}, sid
    if not seg:
        return "FAIL", {"reason": "spawned, no first-segment", "tags": tags}, sid

    # Soak: 'first segment ready' is necessary but NOT sufficient. For DASH/HLS
    # the init segment (moov) is written before the video pipeline processes a
    # frame, so a first-frame fatal (libass/fontconfig exit 145, #141) fires
    # ~1s AFTER the segment — the old verifier declared PASS and exited just
    # before it. Keep watching for `soak` secs and FAIL on any fatal / non-zero
    # ffmpeg exit in that window.
    soak_deadline = time.time() + soak
    while time.time() < soak_deadline:
        time.sleep(2)
        elapsed = int(time.time() - started) + 4
        _, _, t2, soak_errs = _scan_logs(slug, f"{elapsed}s")
        if t2:
            tags = t2
        if soak_errs:
            return "FAIL", {"errors": sorted(set(soak_errs)), "tags": tags,
                            "phase": "post-segment-soak"}, sid

    # Liveness / process-check (#141b). Only when soaking — the soak window is
    # what gives the progress signals time to appear; --soak-seconds 0 keeps the
    # old first-segment-only bar (escape hatch). Catches two classes the
    # non-zero-exit soak misses: (1) a clean exit-0 after the init segment
    # (premature termination), and (2) an init-segment-only stall that never
    # encodes a frame.
    if soak > 0:
        window = f"{int(time.time() - started) + 4}s"
        progressed, advanced, beats, exited = _scan_liveness(slug, window)
        if exited is not None:
            return "FAIL", {"reason": f"ffmpeg exited during soak (premature): {exited}",
                            "tags": tags, "phase": "liveness"}, sid
        if not progressed:
            return "FAIL", {"reason": "init segment only — no 'first progress block' "
                            "(ffmpeg never encoded a frame / stalled)",
                            "tags": tags, "phase": "liveness"}, sid
        if advanced is False:
            # >=2 out_time_us samples that did not climb: encoded a frame then
            # froze mid-stream (#166). advanced is None on <2 samples — a short
            # soak / heavy throttle can't prove a stall, so don't FAIL there.
            return "FAIL", {"reason": "mid-stream stall — out_time_us frozen across the "
                            "soak (encoded a frame then hung; #166 heartbeat did not advance)",
                            "tags": tags, "phase": "liveness"}, sid
        if min_progress and beats < min_progress:
            return "FAIL", {"reason": f"liveness below bar — {beats} progress heartbeat(s) "
                            f"< --min-progress {min_progress}", "tags": tags,
                            "phase": "liveness"}, sid
    return "PASS", {"tags": tags}, sid


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
    ap.add_argument("--soak-seconds", dest="soak", type=float, default=8,
                    help="after 'first segment ready', keep watching this long for a LATE "
                         "fatal/non-zero ffmpeg exit (libass/fontconfig exit 145 fires ~1s "
                         "after the init segment, #141) AND run the liveness gate (#141b: "
                         "encoded a frame + still alive at soak end). 0 disables both.")
    ap.add_argument("--min-progress", type=int, default=0,
                    help="strict liveness bar (#141b): require >= N 'progress heartbeat' "
                         "lines (#166, ~one per 5s of advancing encode) within the soak "
                         "window or FAIL. 0 = off (default): still asserts out_time_us "
                         "advanced, but tolerates a single sample on a short soak. Raise "
                         "with a longer --soak-seconds to demand sustained progress.")
    ap.add_argument("--allow-nodispatch", action="store_true",
                    help="don't fail the run on NODISPATCH cells (cells that never reached the "
                         "worker, so went unvalidated). Default: NODISPATCH is a hard fail "
                         "so a partial run can't look green (#142).")
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
    # Pre-flight content audit (#142): report sub-burn shapes the library has NO
    # content for, so a missing shape reads as a content gap — not a silent
    # omission, and not a later NODISPATCH on a cell that was never built.
    covered = {c["label"] for c in cases}
    # Only audit shapes the harness actually PROBES for content: text/bitmap ×
    # embedded/external (scanned across movie content) + ass-embedded (the anime
    # scan). ass-external isn't probed (discover_ass_item is embedded-only), so
    # don't claim "no content" for it. (CodeRabbit #159.)
    probed_shapes = ["text-burn-embedded", "text-burn-external",
                     "bitmap-burn-embedded", "bitmap-burn-external", "ass-burn-embedded"]
    missing = [s for s in probed_shapes if s not in covered]
    if missing:
        print(f"content audit: no library content for {len(missing)} probed sub-burn "
              f"shape(s) (skipped, not failed): {', '.join(missing)}")
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
            if not prefs_applied(combo):
                print(f"  WARN prefs not confirmed applied (proceeding): {combo}")
            plabel = ",".join(f"{k.replace('HardwareAccelerated','HW').replace('Transcoder','')[:10]}={v}"
                              for k, v in combo.items())
            for pname, phdrs, pmeta in client_axis:
                for c in cases_for(pmeta):
                    # When a real profile is selected it IS the client under test,
                    # so it overrides the case's synthetic client (e.g. WINDOWS_HEADERS).
                    cc = c if phdrs is None else {**c, "client": phdrs}
                    for proto in protos_for(cc, pmeta):
                        label = (f"{pname+':' if pname else ''}{cc['label']}/{proto} | {plabel}")
                        status, info, sid = drive_cell(cc, proto, args.settle, args.soak,
                                                       min_progress=args.min_progress)
                        stop_session(sid)
                        results.append((fhw, label, status, info))
                        print(f"  [{status:10s}] FORCE_HW={fhw} {label} :: {info}")
                        time.sleep(8)  # let worker cleanup + logs age out before next cell (#118)

    from collections import Counter
    tally = Counter(s for *_, s, _ in results)
    worker_cells = tally["PASS"] + tally["FAIL"]
    nd = tally["NODISPATCH"]
    print(f"\n=== worker: {tally['PASS']}/{worker_cells} PASS "
          f"| SKIP(no-transcode)={tally['SKIP']} | NODISPATCH={nd} "
          f"| total cells={len(results)} ===")
    if tally["FAIL"]:
        print("FAILURES (worker errored / no segment / late soak-window exit):")
        for fhw, label, status, info in results:
            if status == "FAIL":
                print(f"  FAIL FORCE_HW={fhw} {label} :: {info}")
    if nd:
        # Count by reclassified subtype so the headline isn't one opaque bucket
        # and each root cause is visible (#142).
        nd_classes = Counter(info.get("class", "UNKNOWN")
                             for *_, status, info in results if status == "NODISPATCH")
        verdict = "WAIVED" if args.allow_nodispatch else "FAIL — unvalidated cells"
        print(f"NODISPATCH [{verdict}] (cells that never reached the worker):")
        for klass, n in nd_classes.most_common():
            print(f"  {klass}: {n}")
        for fhw, label, status, info in results:
            if status == "NODISPATCH":
                print(f"  NODISPATCH[{info.get('class','?')}] FORCE_HW={fhw} {label} :: {info}")
    # Ironclad gate: any real worker FAIL is a hard fail. NODISPATCH cells went
    # unvalidated, so they ALSO fail the run by default — a partial sweep must
    # not read green (#142). --allow-nodispatch waives only the NODISPATCH gate.
    if nd and not args.allow_nodispatch:
        print(f"\n!! {nd} NODISPATCH cell(s) went unvalidated — failing the run "
              f"(pass --allow-nodispatch to waive).")
    sys.exit(1 if tally["FAIL"] or (nd and not args.allow_nodispatch) else 0)


if __name__ == "__main__":
    main()
