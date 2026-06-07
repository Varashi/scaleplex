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
  remote clients (WAN port-forward → LB) and LAN clients.
- **Gateway** (`HTTPRoute /video/:/transcode/universal/decision` → clientfix):
  catches clients on the custom access URL (`plex.boeye.net`).

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
