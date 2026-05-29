#!/usr/bin/env python3
"""Harvest real Plex client transcode profiles from vcflogs.

Pulls the full ``X-Plex-Client-Profile-Extra`` request header (the codec/resolution
capability string a Plex client builds client-side) out of PMS verbose ``Request:``
lines shipped to vcflogs, and emits ready-to-paste ``qa_matrix.CLIENT_PROFILES``
entries. See scaleplex #74 (client-profile axis) + #115 (TV/console capture).

Why this works
--------------
Smart-TV / console / mobile Plex apps don't rely on a server-side named profile; they
POST their limits as the ``X-Plex-Client-Profile-Extra`` header on
``/video/:/transcode/universal/decision`` (e.g.
``add-limitation(scope=videoCodec&...)+add-transcode-target(...)``). Under PMS
``LogVerbose=1`` the whole request line — URL query params *and* headers — is logged and
shipped to vcflogs. Since the move to CFAPI log push the events are no longer truncated
(>4 KB lines arrive whole), so the full profile-extra is recoverable directly from logs —
no in-pod grep, no device replay.

Caveats
-------
* Only clients that actually **transcode** emit a profile-extra. A client that
  **direct-plays** (incl. via a Plex Optimize / pre-optimised version) sends none and PMS
  logs ``Unable to find client profile for device``. To capture a specific direct-playing
  device (LG webOS / PS4 / Xbox here), it must be made to transcode (unsupported
  codec/container, or lower the in-app quality), or the pre-optimiser disabled.
* The profile-extra is a *header*, logged only under ``LogVerbose=1``. Search vcflogs by a
  value token (``add-limitation`` / ``add-transcode-target``) — the hyphenated header name
  ``Profile-Extra`` does not match the tokenizer.

Usage
-----
    python3 test/harvest_client_profiles.py [--days 7] [--limit 2000] [--pages 3] \
        [--out ~/scaleplex-corpus/_client-profiles] [--curated test/client_profiles.json]

The ``--curated`` path is what ``qa_matrix.CLIENT_PROFILES`` consumes; re-run the
harvester with ``--curated test/client_profiles.json`` to refresh the matrix's
real-client coverage (e.g. after a new device shows up in vcflogs).

Creds: ``VCFLOGS_PASSWORD`` or ``INFRA_SSH_PASSWORD`` (vcflogs ``admin`` Local user),
matching ~/.config/infrastructure.env. Host override: ``--host`` / ``VCFLOGS_HOST``.
"""
import argparse
import datetime as _dt
import json
import os
import re
import ssl
import sys
import urllib.parse
import urllib.request

HOST = os.environ.get("VCFLOGS_HOST", "skw-vcflogs.boeye.net")
PORT = 9543
_CTX = ssl.create_default_context()
_CTX.check_hostname = False  # vcflogs serves a self-signed cert (internal infra only)
_CTX.verify_mode = ssl.CERT_NONE

# Identity headers worth keeping for a CLIENT_PROFILES entry (canonical case).
IDENTITY = [
    "X-Plex-Product", "X-Plex-Version", "X-Plex-Platform", "X-Plex-Platform-Version",
    "X-Plex-Client-Platform", "X-Plex-Device", "X-Plex-Device-Name", "X-Plex-Device-Vendor",
    "X-Plex-Model", "X-Plex-Device-Screen-Resolution", "X-Plex-Drm", "X-Plex-Features",
    "X-Plex-Language",
    # X-Plex-Client-Profile-Name is the SEED profile (e.g. "Generic") onto which
    # the profile-extra layers — TV/console identities 400 without it (verified
    # 2026-05-29 on PS4, see #115).
    "X-Plex-Client-Profile-Name", "X-Plex-Client-Profile-Extra",
]
PROFILE_EXTRA = "X-Plex-Client-Profile-Extra"

# Non-Plex-client user agents to drop (qa_matrix probes, monitors, CLI tools).
NOISE_UA = re.compile(r"python-urllib|curl/|wget|go-http-client|probebot|okhttp", re.I)


def _pw():
    pw = os.environ.get("VCFLOGS_PASSWORD") or os.environ.get("INFRA_SSH_PASSWORD")
    if not pw:
        sys.exit("set VCFLOGS_PASSWORD or INFRA_SSH_PASSWORD (vcflogs admin Local password)")
    return pw


def _post(path, body):
    req = urllib.request.Request(
        f"https://{HOST}:{PORT}{path}", data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=30, context=_CTX) as r:
        return json.load(r)


def _get(path, token):
    req = urllib.request.Request(f"https://{HOST}:{PORT}{path}",
                                 headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req, timeout=60, context=_CTX) as r:
        return json.load(r)


def auth():
    d = _post("/api/v2/sessions",
              {"username": "admin", "password": _pw(), "provider": "Local"})
    sid = d.get("sessionId")
    if not sid:
        sys.exit(f"vcflogs auth failed: {d}")
    return sid


def query(token, term, window_ms, limit, before_ms=None):
    """CONTAINS <term> within the last window_ms (or older than before_ms), newest first."""
    parts = [f"text/CONTAINS%20{urllib.parse.quote(term)}"]
    if before_ms:
        parts.append(f"timestamp/LT%20{before_ms}")
    else:
        parts.append(f"timestamp/LAST%20{window_ms}")
    path = f"/api/v2/events/{'/'.join(parts)}?limit={limit}"
    return _get(path, token).get("events", [])


def parse_event(text):
    """Split a PMS verbose 'Request:' line into url-query params + headers.

    Headers are ' / '-separated 'Name => Value'; header values use '/' without
    surrounding spaces (so ' / ' is a safe separator). First value of a repeated
    header wins."""
    segs = text.split(" / ")
    headers, qs = {}, {}
    mu = re.search(r"\b(?:GET|POST|PUT|DELETE|PATCH)\s+(\S+)", segs[0])
    if mu:
        qs = dict(urllib.parse.parse_qsl(urllib.parse.urlparse(mu.group(1)).query))
    # Header names are logged in mixed case (clients send lowercase, PMS adds
    # canonical) — key by lowercase, first value wins.
    for seg in segs[1:]:
        m = re.match(r"\s*([A-Za-z0-9\-]+)\s*=>\s*(.*)", seg)
        if m:
            headers.setdefault(m.group(1).lower(), m.group(2).strip())
    return qs, headers


def _field(event, name):
    return next((f.get("content") for f in event.get("fields", []) if f.get("name") == name), None)


def _slug(headers):
    prod = (headers.get("x-plex-product") or "").strip()
    plat = (headers.get("x-plex-client-platform") or headers.get("x-plex-platform") or "").strip()
    model = (headers.get("x-plex-model") or "").strip()
    base = "_".join(t for t in (prod, plat, model) if t) or "unknown"
    return re.sub(r"[^A-Za-z0-9]+", "_", base).strip("_").lower()[:48]


def _product_class(product):
    """Bucket the X-Plex-Product into the device class used for case filtering.

    Returns one of: web, desktop_windows, desktop_mac, desktop_linux, mobile_android,
    mobile_ios, tv_android, tv_apple, tv_lg, tv_roku, tv_samsung, console_playstation,
    console_xbox, other.
    """
    p = (product or "").lower()
    if "plex web" in p:                    return "web"
    if "plex for windows" in p:            return "desktop_windows"
    if "plex for mac" in p:                return "desktop_mac"
    if "plex for linux" in p:              return "desktop_linux"
    if "plex for android (tv)" in p:       return "tv_android"
    if "plex for android" in p:            return "mobile_android"
    if "plex for ios" in p:                return "mobile_ios"
    if "plex for apple tv" in p:           return "tv_apple"
    if "plex for lg" in p or "webos" in p: return "tv_lg"
    if "plex for roku" in p:               return "tv_roku"
    if "plex for samsung" in p:            return "tv_samsung"
    if "playstation" in p:                 return "console_playstation"
    if "xbox" in p:                        return "console_xbox"
    return "other"


def main():
    global HOST
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default=HOST,
                    help=f"vcflogs host (default {HOST}; env: VCFLOGS_HOST)")
    ap.add_argument("--days", type=int, default=7, help="lookback window (default 7)")
    ap.add_argument("--limit", type=int, default=2000, help="events per page (<=2000)")
    ap.add_argument("--pages", type=int, default=3, help="max pages to paginate")
    ap.add_argument("--terms", default="add-transcode-target,add-limitation",
                    help="vcflogs CONTAINS value-tokens that flag a profile-extra")
    ap.add_argument("--out", default=os.path.expanduser("~/scaleplex-corpus/_client-profiles"),
                    help="dir to write the harvest JSON (skip with empty string)")
    ap.add_argument("--curated", default="",
                    help="path to write the curated JSON consumed by qa_matrix "
                         "(e.g. test/client_profiles.json). Each entry = {headers, meta}.")
    args = ap.parse_args()
    HOST = args.host

    window_ms = args.days * 86400000
    token = auth()
    terms = [t.strip() for t in args.terms.split(",") if t.strip()]

    # Collect events carrying a profile-extra, de-duped by vcflogs event identity.
    seen_ids, raw = set(), []
    for term in terms:
        before = None
        for _ in range(args.pages):
            evs = query(token, term, window_ms, args.limit, before)
            if not evs:
                break
            for e in evs:
                key = (e.get("timestamp"), e.get("text", "")[:80])
                if key in seen_ids:
                    continue
                seen_ids.add(key)
                raw.append(e)
            before = min(e.get("timestamp") for e in evs)  # paginate older

    # Aggregate per-profile across all samples: pick the richest headers + collect
    # distinct observed values for protocol / subtitles / directPlay / directStream
    # (these are what build_cases needs to filter cases to real combinations).
    import collections as _co
    profiles = {}  # name -> aggregated entry
    for e in raw:
        text = e.get("text", "")
        if PROFILE_EXTRA.lower() not in text.lower():
            continue
        qs, headers = parse_event(text)  # headers keyed lowercase
        pe = headers.get(PROFILE_EXTRA.lower())
        if not pe:
            continue
        ua = headers.get("user-agent", "")
        # Drop non-Plex-client traffic + entries without a real product/capability.
        if (NOISE_UA.search(ua) or not headers.get("x-plex-product")
                or ("add-transcode-target" not in pe and "add-limitation" not in pe)):
            continue
        name = _slug(headers)
        cur = profiles.get(name)
        entry_headers = {k: headers[k.lower()] for k in IDENTITY if headers.get(k.lower())}
        # First sample for this profile: init aggregator.
        if cur is None:
            cur = {
                "headers": entry_headers,
                "_extra_len": len(pe),
                "meta": {
                    "product_class": _product_class(headers.get("x-plex-product")),
                    "protocols": _co.Counter(),
                    "subtitles": _co.Counter(),
                    "direct_play": _co.Counter(),
                    "direct_stream": _co.Counter(),
                    "sample_count": 0,
                    "user_agent": ua,
                    "first_seen": e.get("timestampString"),
                    "last_seen": e.get("timestampString"),
                },
            }
            profiles[name] = cur
        else:
            # Keep the richest headers (most ID fields, then longest profile-extra).
            if (len(entry_headers), len(pe)) > (len(cur["headers"]), cur["_extra_len"]):
                cur["headers"] = entry_headers
                cur["_extra_len"] = len(pe)
        m = cur["meta"]
        for k, key in (("protocol", "protocols"), ("subtitles", "subtitles"),
                       ("directPlay", "direct_play"), ("directStream", "direct_stream")):
            v = qs.get(k)
            if v:
                m[key][v] += 1
        m["sample_count"] += 1
        ts = e.get("timestampString")
        if ts:
            if not m["first_seen"] or ts < m["first_seen"]: m["first_seen"] = ts
            if not m["last_seen"]  or ts > m["last_seen"]:  m["last_seen"]  = ts

    # Finalize: Counter -> sorted list (most-frequent first), drop scratch fields.
    for p in profiles.values():
        m = p["meta"]
        for k in ("protocols", "subtitles", "direct_play", "direct_stream"):
            m[k] = [v for v, _ in m[k].most_common()]
        p.pop("_extra_len", None)

    print(f"vcflogs {HOST}: {len(raw)} events w/ profile directives, "
          f"{len(profiles)} distinct client profiles (last {args.days}d)\n")
    for name, p in sorted(profiles.items(), key=lambda kv: -kv[1]["meta"]["sample_count"]):
        h = p["headers"]; m = p["meta"]
        pe_len = len(h.get("X-Plex-Client-Profile-Extra", ""))
        print(f"[{m['sample_count']:>3}x] {name}  ({m['product_class']})")
        print(f"        product={h.get('X-Plex-Product','?')} "
              f"platform={h.get('X-Plex-Client-Platform') or h.get('X-Plex-Platform','?')} "
              f"extra={pe_len}c")
        print(f"        protocols={m['protocols']} subtitles={m['subtitles']} "
              f"directPlay={m['direct_play']} directStream={m['direct_stream']}")
        if m["user_agent"]:
            print(f"        UA: {m['user_agent'][:90]}")

    if args.out:
        os.makedirs(args.out, exist_ok=True)
        stamp = _dt.date.today().isoformat()
        path = os.path.join(args.out, f"profile_extras_{stamp}.json")
        with open(path, "w") as f:
            json.dump({"generated": stamp, "host": HOST, "window_days": args.days,
                       "profiles": profiles}, f, indent=2)
        print(f"\nwrote {len(profiles)} profiles -> {path}")

    if args.curated:
        # Slim repo-bound JSON consumed by qa_matrix.CLIENT_PROFILES:
        #   {name: {"headers": {...}, "meta": {...}}}
        curated = {n: {"headers": p["headers"], "meta": p["meta"]}
                   for n, p in profiles.items()}
        os.makedirs(os.path.dirname(os.path.abspath(args.curated)) or ".", exist_ok=True)
        with open(args.curated, "w") as f:
            json.dump(curated, f, indent=2)
        print(f"wrote curated -> {args.curated}")


if __name__ == "__main__":
    main()
