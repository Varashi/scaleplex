# QA matrix — Tier-2 transcoder-error harness

`qa_matrix.py` drives **real Plex transcode sessions** on a test PMS across the
full server-pref matrix × scaleplex `FORCE_HW` × representative content, then
auto-verifies each on the worker side. It catches transcoder errors and
branch-shape regressions **without a human** — argv-parse failures, filter-graph
build errors, device-binding failures, encoder-open errors, late libass/
fontconfig fatals.

It is **Tier-2** in the release gate: structural correctness, not perception.
Quality / smoothness / visual-burn correctness stay a **Tier-3 human pass**.

## Run

```bash
PLEX_TOKEN=<token> python3 test/qa_matrix.py [--quick]
```

Token is the test PMS's `PlexOnlineToken`:

```bash
kubectl exec -n plex-test <pms-pod> -c app -- \
  grep -o 'PlexOnlineToken="[^"]*"' \
  '/config/Library/Application Support/Plex Media Server/Preferences.xml'
```

> **`--quick` rolls the worker DaemonSet to `SCALEPLEX_FORCE_HW=1`** (via
> `kubectl set env` + rollout). **Capture the prior value and restore it
> afterward:**
> ```bash
> kubectl get ds plex-test-worker -n plex-test \
>   -o jsonpath='{range .spec.template.spec.containers[0].env[?(@.name=="SCALEPLEX_FORCE_HW")]}{.value}{end}'
> # … run …
> kubectl set env ds/plex-test-worker -n plex-test SCALEPLEX_FORCE_HW=<prior>
> ```

A `--quick` is ~9 cases × 4 server-combos × ~1.5 protocols ≈ **56 cells, ~30
min**. Python block-buffers stdout to a pipe, so output flushes on exit — an
in-progress run looks empty in a redirected log; that's normal.

### Why API-driven

The server prefs change the argv **Plex** generates, so the harness lets Plex
generate them (a captured Plex-Web request template with `directStream=0` forces
a full transcode). Recognised `X-Plex-*` headers load a base profile.

## Verdicts

| verdict | meaning |
|---|---|
| **PASS** | worker spawned, produced a first segment, **and survived the soak** with no fatal / non-zero `ffmpeg exit:` |
| **FAIL** | spawned but errored (incl. a *late* soak-window exit), or produced no segment |
| **NODISPATCH** | PMS decided transcode but no ffmpeg spawned — the cell went **unvalidated**. A hard FAIL by default |
| **SKIP** | PMS chose direct-play/copy (no transcode to verify), or a client-profile 400 |

### The soak (why first-segment isn't enough)

For DASH/HLS the init segment (moov box) is written **before** the video
pipeline processes a frame, so a first-frame fatal — e.g. a libass/fontconfig
**exit 145** — fires ~1 s *after* `first segment ready`. The verifier therefore
keeps watching for `--soak-seconds` (default 8) after the segment and FAILs on
any late fatal. A run is GREEN only if every cell survives its soak.

### Fatal-exit set

`-38 / 218 / 234`, **exit 145** (libass/fontconfig init), **exit 8** (unknown
decoder/encoder/BSF/filter), and **any non-zero `ffmpeg exit:`**.

### NODISPATCH is loud + reclassified

A NODISPATCH means an intended cell wasn't actually checked, so it **fails the
run** (no silent green) unless `--allow-nodispatch`. It's reclassified by
observed state so the root cause is visible:

| class | meaning |
|---|---|
| `PMS_NO_TRANSCODE` | the prefs flip didn't take / PMS chose copy |
| `ORCH_NOT_NOTIFIED` | the shim never POSTed `/task` to the orchestrator |
| `WORKER_NEVER_SPAWNED` | orchestrator got the task, PMS transcoding, no worker spawn |
| `LOGS_UNAVAILABLE` | log sources empty (e.g. docker-only worker mode, no k8s access) — unattributable |

Robustness: a no-spawn re-polls out to **2× settle** (same session, no second
trigger) before being classified, so a slow av1→hevc HW init (12-15 s) can't
false-miss. Each pref-combo is pre-warmed (`/:/prefs` read-back of the
`HardwareAccelerated*` bools) before its first cell.

## Axes

`server-pref` (HW decode/encode, HEVC-mode, tonemap) × `FORCE_HW` × `content`
(resolution / HDR) × **sub-burn** `{text, bitmap, ass} × {embedded, external}`.

Sub sources are discovered from the library:

- **external** (sidecar) streams carry `key="/library/streams/.."` — e.g. a
  bazarr `.srt` next to the file. PMS sends `-i temp.srt` + `-map_inlineass`.
- **embedded** streams have no `key` — muxed in the container, `-map 0:s:N`.
- **ASS** lives embedded in the **anime** library (styled/animated dialogue +
  signs — stresses libass far harder than SRT). Sourced via `ASS_SECTION`.

A startup **content audit** lists probed sub-burn shapes the library has no
content for, so a gap reads as a content gap — not a silent skip.

## Environment

| var | default | purpose |
|---|---|---|
| `PLEX_TOKEN` | — | **required** — test PMS admin token |
| `PLEX_URL` | `http://172.16.4.106:32400` | test PMS (plex-test LB) |
| `NS` / `WORKER_DS` | `plex-test` / `plex-test-worker` | k8s worker DaemonSet |
| `SECTION` | `1` | movie section for content discovery |
| `ASS_SECTION` | `3` | section to mine for an embedded-ASS cell (`""` disables) |
| `WORKER_MODE` | `auto` | `k8s` / `docker` / `auto` (combines both; external push workers over SSH) |
| `--settle` | `20` | secs to wait per session for spawn / first segment |
| `--soak-seconds` | `8` | post-segment watch for a late fatal (`0` disables) |
| `--allow-nodispatch` | off | don't fail the run on NODISPATCH cells |
| `--quick` | off | small slice, `FORCE_HW=1` only |

## Gotchas

- **Worker-log correlation is by source FILENAME, not Plex title.** PMS derives
  the transcode SessionID from the file basename; a localized title diverges
  (title *"40-45 De Musical"* vs file *"40-45, the Musical (2025)"*). Matching on
  the title silently mis-correlates → false NODISPATCH. `source_corr_key()` uses
  an alphanumeric prefix of the filename (survives PMS's separator normalization
  and SessionID truncation).
- **`TranscoderToneMapping=0` is degenerate** — it's an `advanced="1"` pref PMS
  pins at `1` via `/:/prefs`; both values of the `[1,0]` axis run as `1` (the
  HDR-tonemap path is never actually disabled). Tracked in #160. (This is also
  why pre-warm confirms only `HardwareAccelerated*` bools — PMS normalizes the
  `TranscoderHEVCEncodingMode` enum, `hevc-sources` → `always`.)

## Relationship to other gates

- **Replay** (`worker/agent/replay_test.go`, `-tags=replay`) dry-runs the
  rewriter against the captured argv corpus — no real ffmpeg/worker. Runs in CI
  against the committed `testdata/replay-corpus` fixture. qa_matrix is the live
  complement; it is **manual** against plex-test today (CI-arming is an open gap).
- **Tier-3** is the human pass — actual playback, subtitle render fidelity, A/V
  sync, smoothness.
