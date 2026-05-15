# Client test matrix

Live-test sheet for validating scaleplex worker behavior across Plex
clients. Run against a PMS with the scaleplex `DOCKER_MODS` bundle
active — every transcode session flows through the scaleplex
orchestrator → worker pods.

## Pre-flight

```bash
# All workers on the same image, ready
kubectl -n <namespace> get pods -l app.kubernetes.io/controller=worker \
  -o custom-columns=NAME:.metadata.name,IMAGE:.spec.containers[0].image,READY:.status.containerStatuses[0].ready

# No leftover sessions
kubectl -n <namespace> exec deploy/<pms-deployment> -- \
  curl -s "http://localhost:32400/status/sessions?X-Plex-Token=<token>" \
  | grep -oE 'sessionKey="[^"]*"' | wc -l

# Image SHA written into rewriter logs (correlate with k8s commit log)
kubectl -n <namespace> logs -l app.kubernetes.io/controller=worker --tail=1 \
  | grep -oE 'scaleplex-agent v[a-f0-9]+' | head -1
```

## Test sources (validated live, 2026-05-10)

Pick from these — Big Hero 6 is the canonical HDR4K reference (used for
tonemap injection verification).

| Source | rk | Codec / audio | Use case |
|---|---|---|---|
| Big Hero 6 (2014) | 784 | HEVC 4K HDR + **TrueHD Atmos 7.1** | HW-passthrough HDR→SDR tonemap, TrueHD swap |
| Legend (2015) 4K | 7204 | AV1 (Tdarr) + EAC3 | AV1 HW-decode, EAC3 swap |
| The Accountant 4K | 13961 | AV1 HDR10+ + EAC3 | HDR pass-through (HEVC client) or tonemap (h264 client) |
| Bob's Burgers S16E12 | (varies) | AV1 + EAC3 5.1 | Optimize jobs ref |

Force HW probe failure to flex bare-decoder path: temporarily set
`HardwareAcceleratedCodecs=0` in PMS prefs, restart PMS, hit one of
the above. Reset after.

## Common golden-path test cases (per client)

Each client should walk through these in order. A FAIL on any case
means stop, capture data (see "Failure capture" below), continue only
if recoverable.

| # | Case | Source | What's exercised |
|---|---|---|---|
| 1 | Direct play | any matching client capabilities | sanity — scaleplex must not interfere |
| 2 | Audio-only transcode | TrueHD source → AAC client (Big Hero 6) | partial path: `swapEAEAudioDecoders`, no video filter |
| 3 | Video transcode (HDR pass-through) | rk=13961 → HEVC-capable client at 1080p | HW-decode + HW-encode, HDR stays p010, no tonemap |
| 4 | Video transcode (HDR→SDR tonemap) | rk=784 → h264-only client at 720p | `filter:hw-passthrough-tonemap-injected` MUST fire |
| 5 | Seek mid-stream | any video transcode (case 3 or 4) | force_key_frames offset, segment renumber, tfdt patch (DASH) / CSV rewrite (HLS) |
| 6 | Forced subtitle burn-in | source with SRT or PGS sub | `subtitle:bitmap:*` or `subtitle:embedded-extract:*` |

## Per-client setup + checks

### Plex for Windows (desktop native)

- **Install:** Microsoft Store or plex.tv/desktop. Sign in with same
  Plex account. Server should appear automatically.
- **Force test PMS:** `Settings → General → Advanced → Show Advanced` — select the scaleplex-backed server.
- **Protocol:** Plex for Windows uses **DASH** for transcodes (same path as Plex Web Chrome).
- **Force transcode for case 3/4:** Quality menu → "Convert Automatically" → select 4 Mbps 720p. For HDR→SDR force, use a non-HEVC-capable quality.
- **Likely transcode trigger:** TrueHD source (case 2 already) or 4K → 1080p quality switch.

### Plex Web — Firefox

- **URL:** `https://app.plex.tv/desktop` in Firefox.
- **Protocol:** DASH via MSE — same path as Chrome but Firefox's
  SourceBuffer behavior differs on seek.
- **Verify-Firefox-specific:** open `about:debugging` → media-internals if available, or use Chrome DevTools-style breakpoint via Firefox web tools. Seek timing on Firefox sometimes shows different `BUFFERING_HAVE_NOTHING` patterns than Chrome.
- **Risk areas:** sidx box parsing on seek chunks (Firefox stricter than Chrome on box ordering inside a CMAF segment).

### LG webOS (TV)

- **Install:** LG Content Store → search "Plex".
- **Sign in:** uses Plex.tv code-pair flow (TV shows 4-char code, enter on plex.tv/link from phone/laptop).
- **Protocol:** **HLS mpegts** is the default (TV doesn't do DASH).
- **HDR behavior:** webOS TVs typically negotiate HDR10/Dolby Vision pass-through if supported; with `HardwareAcceleratedCodecs=1` PMS may try HEVC HDR direct stream first.
- **Force transcode for case 4:** lower quality picker → 4 Mbps 720p (most TVs will land here when Wi-Fi-limited).
- **Risk areas:** HLS mkv-container fallback when codec/audio combo can't fit mpegts (already validated on Android; LG may use mpegts more aggressively). 

### Plex for iOS / iPadOS

- **Install:** App Store. Sign in.
- **Force test PMS:** Settings → Advanced → enable "Show Advanced" → server picker.
- **Protocol:** HLS mpegts.
- **Risk areas:** AV1 not supported on most iPhones → forced HW-decode → HW-encode hevc/h264. Sub-burn on iOS — has its own font fallback peculiarities.

### PS4 (game console)

- **Install:** PS Store → Plex.
- **Sign in:** code-pair via plex.tv/link.
- **Protocol:** HLS mpegts only. **No HEVC support** — PS4 (non-Pro) is h264 hardware-only. PS4 Pro adds HEVC for streaming apps but Plex client may still negotiate h264.
- **Implies:** all 4K HEVC/AV1 sources MUST transcode through scaleplex.
- **Audio limits:** AAC + AC3; TrueHD/DTS-MA → AC3 transcode.
- **Risk areas:** strict mpegts compliance; PS4 is intolerant of stream-spec discontinuities. Test seek heavily.

### Plex for Apple TV (tvOS) — optional

- Similar to iOS but with HEVC/HDR support on AppleTV 4K+. Use only
  if you have an AppleTV.

## Worker-side PASS verification

After each test case, capture rewriter tags + ffmpeg cmdline from the
worker that took the session. The pod name is in PMS's session log;
easier to just grep all 3 worker pods:

```bash
# Per-session rewriter tags (replace SESSION_PREFIX with first part of session UUID,
# or filter on the basename of the source file)
kubectl -n <namespace> logs -l app.kubernetes.io/controller=worker --since=2m \
  | grep -E "rewriter applied:" | tail -5

# Live ffmpeg cmdline (for verifying filter chain / hwaccel)
for p in $(kubectl -n <namespace> get pod -l app.kubernetes.io/controller=worker -o jsonpath='{.items[*].metadata.name}'); do
  out=$(kubectl -n <namespace> exec "$p" -- sh -c 'cat /proc/$(pgrep ffmpeg | head -1)/cmdline 2>/dev/null | tr "\0" " "' 2>&1)
  if [ -n "$out" ] && [ "$out" != " " ]; then
    echo "=== $p ==="
    echo "$out" | tr ' ' '\n' | grep -E "^-(codec|hwaccel|filter|init_hw|f|c)" -A1 | head -40
  fi
done
```

### Expected rewriter tags per case

| Case | Expected tags (minimum) |
|---|---|
| 1 (direct play) | no session — sanity check |
| 2 (audio-only) | `audio:<src>_eae-><dst>`, `drop:-eae_prefix:N` |
| 3 (HDR pass-through) | `decode:hw-passthrough:hevc`, `encode:hw-passthrough:hevc_vaapi`, `video:hdr-source(smpte2084)`, NO `tonemap-injected` |
| 4 (HDR→SDR tonemap) | `decode:hw-passthrough:hevc`, `encode:hw-passthrough:h264_vaapi`, `video:hdr-source(smpte2084)`, **`filter:hw-passthrough-tonemap-injected`** |
| 5 (seek) | tags from prior case + `seek-offset:captured=<T>s`, segment renumber tags |
| 6 (sub-burn) | `subtitle:bitmap:<spec>` OR `subtitle:embedded-extract:<spec>` OR `subtitle:sidecar-staged`; HW path adds `hw-decode:filter:inlineass->subtitles` |

## Failure capture

If a case fails (buffering, error toast, wrong colors, audio dropout):

```bash
# 1. Dump the full ffmpeg cmdline of the broken session
WORKER=<worker-pod>
kubectl -n <namespace> exec $WORKER -- sh -c 'cat /proc/$(pgrep ffmpeg | head -1)/cmdline | tr "\0" "\n"' > /tmp/broken.argv

# 2. Find the rewriter input argv (corpus capture)
kubectl -n <namespace> exec $WORKER -- sh -c 'ls -lt /scaleplex-corpus/ | head -5'
# Copy the most recent JSON locally for debugging
kubectl -n <namespace> cp $WORKER:/scaleplex-corpus/<latest>.json /tmp/broken-input.json

# 3. ffmpeg stderr — last 200 lines
kubectl -n <namespace> exec $WORKER -- sh -c 'find /transcode -name "*.log" -mmin -2 | head -1 | xargs tail -200' > /tmp/broken.stderr

# 4. PMS log slice
kubectl -n <namespace> exec deploy/<pms-deployment> -- \
  tail -200 "/config/Library/Application Support/Plex Media Server/Logs/Plex Media Server.log"

# 5. Browser/client diagnostics
#    Plex Web Chrome: chrome://media-internals → most recent session → save full JSON
#    Plex Web Firefox: about:debugging → equivalent
#    Apps: enable verbose log in Settings → Crash reports
```

Drop captured files into a new ticket / scratch dir for later replay
via `worker/agent/replay_test.go`.

## Test order recommendation

Walk clients in this order — covers maximum risk surface fastest:

1. **Plex for Windows** — common desktop client, DASH path.
2. **LG webOS** — living-room TV client, biggest UX cost if broken.
3. **PS4** — strict mpegts compliance flush.
4. **Plex Web Firefox** — alternate MSE.
5. **iOS** — if you have an iPhone/iPad handy.

Each client takes ~15 min for the 6 cases.
