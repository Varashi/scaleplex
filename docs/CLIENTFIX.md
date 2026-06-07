# clientfix — per-client decision proxy

`clientfix/` is a small stdlib-only Go reverse proxy that fronts PMS to fix
bugs that live in a *client's* HTTP capability negotiation
(`X-Plex-Client-Profile-Extra`) rather than in the transcode itself. It is a
second quirk layer, distinct from the worker's argv rewriter: the rewriter
reshapes the ffmpeg command a worker runs; clientfix reshapes the **decision
request** PMS answers, so PMS builds a different `.m3u8`/transcode in the first
place. The worker can't do this — PMS synthesizes the served playlist from its
own container decision before any worker is involved.

## Where it sits

clientfix must front **every** ingress path a client can reach PMS by, because
which one a given client picks is not under our control:

- **LoadBalancer** (`plex-plex-lb` → clientfix → PMS): catches `plex.direct`
  remote clients (WAN port-forward → LB) and LAN clients. This path carries
  **raw TLS** (plex.direct is HTTPS, as is Plex's reachability probe) — see
  [TLS-SNI front](#tls-sni-front-the-lb-path) for how clientfix handles it.
- **Gateway** (`HTTPRoute … → clientfix`): catches clients on the custom access
  URL (`plex.boeye.net`). Here **Envoy terminates TLS** and forwards plaintext
  HTTP to clientfix, so the SNI front below is not involved on this path.

A gateway `RequestHeaderModifier` is **not** sufficient on its own: Plex sends
the profile-extra in the URL **query** as well as the header, and a gateway
filter can only touch headers. clientfix edits both.

It streams everything transparently (`httputil.ReverseProxy`, `FlushInterval
-1`) and only special-cases the matched decision request (a small buffered
two-pass). copy / Direct-Stream decisions always pass through untouched.

## Behavior

For a request matching `isDecision(path) && appleTV845(headers)`:

1. Forward unchanged to PMS, read the decision.
2. If the selected video stream is **copy** → return verbatim.
3. If it is **transcode** → re-issue per `CLIENTFIX_DECISION_MODE`:
   - `strip` (default) — remove `X-Plex-Client-Profile-Extra` (header **and**
     query). PMS falls back to its base device profile (e.g. stock
     `tvOS.xml`).
   - `<container>` (`mp4`, `mpegts`, …) — keep the client's hevc/4K/HDR caps,
     swap only `container=mkv` → `container=<target>` in the profile-extra.
4. Fail-open: any parse/re-issue error returns PMS's original answer.

PMS caches the (re-issued) decision under the session GUID; the later
`start.m3u8` replays it.

### Env

| var | default | meaning |
|---|---|---|
| `CLIENTFIX_LISTEN_ADDR` | `:8080` | listen address |
| `CLIENTFIX_PMS_UPSTREAM` | — (required) | `http://plex.plex.svc:32400` |
| `CLIENTFIX_DECISION_MODE` | `strip` | `strip` \| container name (`mp4`,`mpegts`) |
| `CLIENTFIX_UPSTREAM_TIMEOUT` | `30s` | two-pass client timeout |
| `CLIENTFIX_TLS_CERT` / `CLIENTFIX_TLS_KEY` | — (off) | enable the SNI front; PEM cert+key for our custom domain (e.g. cert-manager `*.boeye.net`) |
| `CLIENTFIX_TLS_SNI_SUFFIX` | `.boeye.net` | SNI suffix to terminate; any other TLS SNI is passed through |
| `CLIENTFIX_PMS_PASSTHROUGH_ADDR` | `CLIENTFIX_PMS_UPSTREAM` host | TCP target PMS serves TLS on (passthrough dial) |
| `CLIENTFIX_TLS_RELOAD` | `1h` | cert hot-reload interval (picks up cert-manager renewals) |

## TLS-SNI front (the LB path)

clientfix is a plaintext-HTTP server, but on the LB / port-forward it sits on
the path that carries **raw TLS**: `plex.direct` remote clients connect over
HTTPS, and Plex's **remote-access reachability probe** is HTTPS too. A plain
HTTP listener can't answer that TLS, so the probe fails and PMS flaps
"remote access down/up" — even while playback works (clients fall back to HTTP
under `secureConnections=Preferred`).

We can't terminate `plex.direct`'s own cert (it's Plex's, rotating, on the
RWO `/config`). Instead, when `CLIENTFIX_TLS_CERT`/`KEY` are set, clientfix
peeks the TLS `ClientHello` and routes by **SNI**:

| SNI | action |
|---|---|
| ends with `CLIENTFIX_TLS_SNI_SUFFIX` (our custom domain, e.g. `plex-svc.boeye.net`) | **terminate** with our own cert → run the HTTP handler (decision rewrite over HTTPS) |
| `*.plex.direct` / no SNI / anything else | **raw TCP passthrough** to PMS (`CLIENTFIX_PMS_PASSTHROUGH_ADDR`); PMS answers with its own real `plex.direct` cert |
| not TLS (plaintext) | HTTP handler, as before (e.g. behind the gateway) |

So the **reachability probe + plex.direct clients ride PMS's real cert** (no
flapping, no Plex-cert extraction, Remote Access stays on), while clients that
reach us on **our** custom domain get the decision rewrite over HTTPS. The cert
is hot-reloaded (`CLIENTFIX_TLS_RELOAD`) so cert-manager renewals need no
restart. Terminated TLS forces HTTP/1.1 (ALPN `http/1.1`); passthrough is
transparent so plex.direct h2 is unaffected. Unset `CLIENTFIX_TLS_CERT` →
original plain-HTTP listener (the gateway/Envoy path, where Envoy already
terminates TLS).

> The matched-client decision rewrite only applies on the **terminated** path
> (our domain) and the plaintext/gateway path. A client that connects via
> `plex.direct` is passed straight through to PMS (native decision) — the rewrite
> targets such clients via the custom URL. For ATV-8.45 that's acceptable: it's
> a hard client limit regardless (see the case study).

## Case study: Plex for Apple TV 8.45 (the only rule today)

The 8.45 "Enhanced Player" sends an `add-transcode-target(... protocol=hls
container=mkv ... replace=true)` — `replace=true` wipes PMS's own HLS targets
and installs a single **mkv** one, plus mkv baggage
(`CopyMatroskaAttachments`, `subtitleCodec=srt`). The tvOS player demuxes a
**copy** of mkv-in-HLS fine but **rejects a re-encode** of it (byte-identical
Matroska framing; only the payload differs — client demuxer bug, see #122).

Container swaps alone do **not** fix it, and the failure is **not** the
container or the extra. Full matrix on real devices (tvOS 26.5/26.6, Plex
8.45), av1 sources:

| output | result |
|---|---|
| hevc 4K — mkv / mp4 / mpegts (extra kept, container swapped) | ❌ fail |
| hevc 4K — fMP4 via a clean custom `tvOS.xml` (extra fully stripped) | ❌ crashes the app |
| h264 1080p — mpegts (`strip` → stock `tvOS.xml`) | ✅ plays |

**Two layers (final, 2026-06-07):** (1) the worker emitted HEVC **Rext** on
10-bit HW encodes — a real encoder bug, FIXED in worker v1.13.0 (#189,
`ensureHEVCMain10` → `-profile main10`, all backends; correct + helps non-ATV
hevc clients; qa_matrix guards it #193/#195). (2) But ATV-8.45 **still can't
play transcoded HEVC even as Main10** (real-device: Dennis mkv-Main10, Tim
mp4-Main10 — both failed) → a hard client limit, any container. So h264 is the
only working transcode codec on this client.

**Permanent for ATV-8.45:** `CLIENTFIX_DECISION_MODE=strip` → h264 1080p is the
only thing that plays a transcoded ATV-8.45 session (every HEVC variant fails,
even Main10). clientfix is **live in prod in strip mode** and **NOT retirable**
for this client — the worker Main10 fix doesn't unlock ATV. 4K on ATV only via
copy/Direct-Play — see [`KNOWN_ISSUES.md`](KNOWN_ISSUES.md).

### Custom PMS profiles (for the record)

PMS loads custom client profiles from
`<config>/Library/Application Support/Plex Media Server/Profiles/<name>.xml`
(persistent, survives Plex updates, **no** need to edit
`Resources/Profiles`). Restart PMS to load — in the lsio container,
`s6-svc -r /run/service/svc-plex` restarts PMS in-place (~5s, no pod
reschedule). A custom profile + `strip` is equivalent to a container-rewrite
mode but declarative; it does **not** unlock anything the client can't already
play (a clean hevc-4K profile still crashes ATV 8.45).
