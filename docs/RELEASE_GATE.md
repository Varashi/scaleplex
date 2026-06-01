# Release gate

What must be green before tagging `vX.Y.Z`. Procedural complement to
the cell-level data in [`TEST_MATRIX.md`](TEST_MATRIX.md) — that doc
enumerates the cells; this one tells you the order, mechanics, and
pass criteria.

The gate is layered: each tier catches a class of regressions cheaper
than the next. **Cheap-first** means a failure in T1 short-circuits
the heavy T2/T3/T4 work.

## T1 — unit tests (CI gate; fail-fast)

**Where:** `.github/workflows/build-worker.yaml`,
`build-orchestrator.yaml`, `build-shim.yaml`.

**Status check before tag:**

```bash
gh run list --branch main --workflow=build-worker --limit=1 --json conclusion --jq '.[].conclusion'
gh run list --branch main --workflow=build-orchestrator --limit=1 --json conclusion --jq '.[].conclusion'
gh run list --branch main --workflow=build-shim --limit=1 --json conclusion --jq '.[].conclusion'
# All three must be: "success"
```

**Required:**
- `go test -cover ./...` green in all three jobs
- `TestRewriterTagInventory` + `TestRewriterTagInventory_ConstsAreUnique`
  + `TestTagValues_Stable` green (drift-catch on the rewriter change-tag
  canon — see [`worker/agent/rewriter_tags.go`](../worker/agent/rewriter_tags.go))
- Worker `replay (rewriter-only, fixture corpus)` step green — runs
  the build-tagged replay tests against the committed 240-cell
  stratified Optimize fixture at `worker/agent/testdata/replay-corpus/`
  with `REPLAY_NO_FFMPEG=1`. Catches rewriter regressions per-PR
  without cluster access.
- Coverage % unchanged or improved vs prior release (manual eyeball,
  no automated trend yet)

**Cost:** ~1 minute, fully automated.

## T2 — replay corpus (manual until CI-wired)

**Where:** `worker/agent/replay_test.go`,
`worker/agent/replay_bitmap_unified_test.go`,
`worker/agent/rewriter_orthogonal_parity_test.go` (build-tag `replay`).
Corpus at `~/scaleplex-corpus/` (root + `optimize-sweep/` +
`optimize-validate-pr41/`).

**Step 1 — pre-tag corpus refresh:**

```bash
# Production captures (worker NFS + stderr logs from prod + plex-test):
~/git/scaleplex/cmd/argv-extract/sweep.sh

# Optional: synthetic Optimize matrix (1824 cells / 53 shapes) via
# the optimize-corpus-gen tool. Skip if the latest sweep is recent
# (see `~/scaleplex-corpus/optimize-sweep/` mtime).
cd ~/git/scaleplex/cmd/optimize-corpus-gen
# (See cmd/optimize-corpus-gen/README.md for the run command — needs
# plex-test PMS token + the synthetic test-clips media share.)
```

**Step 2 — local rewriter-only validation:**

```bash
cd ~/git/scaleplex/worker/agent
REPLAY_NO_FFMPEG=1 go test -tags=replay -v \
  -run 'TestReplayCorpus|TestReplayCorpus_BitmapOverlayUnified|TestReplayCorpus_OrthogonalEmitParity' \
  ./...
```

**Required:**
- `TestReplayCorpus`: 0 `FAIL bail` entries without a recognised `skip:`
  reason; 0 `FAIL argv` is enforced only in in-pod mode below
- `TestReplayCorpus_BitmapOverlayUnified`: 0 errors; reshape count > 0
- `TestReplayCorpus_OrthogonalEmitParity`: `diffOther == 0`,
  `considered > 0`

**Step 3 — in-pod replay (full ffmpeg + VAAPI + libass):**

```bash
go test -tags=replay -c -o /tmp/replay.test ./worker/agent
POD=$(kubectl -n plex-test get pod -l app.kubernetes.io/controller=worker -o jsonpath='{.items[0].metadata.name}')
kubectl -n plex-test cp /tmp/replay.test "$POD:/tmp/replay.test"
kubectl -n plex-test exec "$POD" -- /tmp/replay.test \
  -test.v -test.run TestReplayCorpus -test.timeout 30m
```

**Required:**
- 0 `FAIL argv` (ffmpeg fast-fail on argv parse / filter graph / encoder
  open)
- 0 `TIMEOUT` past the 10s per-cell budget
- `FAIL run` / `SKIP` acceptable when source file is absent on the
  plex-test PMS (real-prod captures reference paths that may not exist
  on the test PMS)

**Step 4 — in-pod dash-muxer e2e (`TestReplayCorpus_DashMuxer`, #148):**

Runs the *real* `-f dash` muxer against a fake-PMS `httptest` server and
asserts ffmpeg PUT the manifest to the rewritten `-manifest_name` URL +
exited 0 — the muxer-side network path `TestReplayCorpus` can't reach
(it strips `-manifest_name`). Catches PR #144's Bug B class (loopback
manifest URL → ECONNREFUSED → exit-145). Same built binary:

```bash
kubectl -n plex-test exec "$POD" -- /tmp/replay.test \
  -test.v -test.run TestReplayCorpus_DashMuxer -test.timeout 20m
```

**Required:**
- Each run cell: `manifest PUT(s) to fake-PMS, ffmpeg exit 0`
- `ran == 0` (whole test SKIP) only acceptable when the corpus has no
  dash-shape cell with a present source (e.g. fixture-only on a host
  without the synth clips); against `~/scaleplex-corpus` in-pod it must
  run ≥1 cell. Knobs: `REPLAY_DASH_MAX` (default 6), `REPLAY_TIMEOUT`.

**Cost:** ~20 minutes local, ~45 minutes in-pod (depends on corpus size).

## T3 — API-driven matrix (`test/qa_matrix.py`)

**Where:** `test/qa_matrix.py`. Drives real Plex transcode sessions on
the `plex-test` PMS across the server-pref matrix (HW decode/encode,
HEVC mode, tonemapping) × scaleplex `FORCE_HW` × representative content.

**Setup:**

```bash
export PLEX_TOKEN=$(...)  # plex-test PMS token
export BACKBONE_RK=<known HDR4K rating-key on plex-test library>
# (optional) export PLEX_URL=http://172.16.4.106:32400  # default plex-test LB
```

**Run:**

```bash
cd ~/git/scaleplex
python3 test/qa_matrix.py --force-hw 1,0
# 32 cells (16 server × 2 FORCE_HW) × 1 backbone item
# ~3 min wallclock + 2 worker rolls
```

**Required:**
- `32/32 PASS`
- Worker stderr `rewriter applied:` lines show non-empty tag list for
  each cell (no silent identity-rewrite)
- No `-38` / `218` / `234` / `Conversion failed` / `Error reinitializing`
  in worker logs
- No `skip:` bails for the backbone item

**Post-run:** restore plex-test PMS prefs to prod-equivalent
(`HardwareAcceleratedCodecs=1, HardwareAcceleratedEncoders=1,
TranscoderHEVCEncodingMode=hevc-sources, TranscoderToneMapping=1`).
`qa_matrix.py` leaves prefs in the last-tested state — manual restore
required.

**Known limitations:** drives one backbone item; doesn't iterate sub
streams or alternate client profiles. Sub-burn verification + multi-
client profile matrix tracked in `project_scaleplex_pre_release_testing_review.md`
as a post-v1.7.0 feature.

**Cost:** ~3 minutes; needs a quiet plex-test (sessions are mutating).

## T4 — live sweep on physical clients

**Where:** [`TEST_MATRIX.md`](TEST_MATRIX.md) "Release gate" section —
the per-release 11-cell sweep.

**Procedure per cell:** see
[`CLIENT_TEST_MATRIX.md`](CLIENT_TEST_MATRIX.md) — install / sign-in /
force-transcode hints per client, the worker-side PASS verification
(rewriter-tag log grep + `/proc/$(pgrep ffmpeg)/cmdline`), and the
failure-capture commands (dump argv + stderr + first segment, drop
into corpus for T2 replay forever after).

**Required per cell:**
- Playback starts within ~10s of the session creating
- No buffering / freezing during the first 30s of play
- No client-side error toast
- Seek (forward and backward) resumes within ~5s
- Worker `rewriter applied:` line contains the expected `Tag*` /
  `TagPrefix*` strings for the cell shape (see "Tag(s) expected"
  column in the release-gate table)
- Closes-out cleanly on stop (no zombie worker process)

**Required across all cells:**
- 11/11 PASS, OR every FAIL has a `[KNOWN: <slug>]` entry in
  [`KNOWN_ISSUES.md`](KNOWN_ISSUES.md) (re-confirming a known-broken
  is acceptable; encountering a *new* failure is not)

**Cost:** ~35 min for one operator with the physical fleet.

## T5 — debug-build gate (conditional)

**When:** if the release includes any change to `scaleplex-ffmpeg/`
(fork patches, debian rules, base image). Skip for pure rewriter /
orchestrator / shim releases.

**Procedure:**

```bash
# On SKW-Build (per reference_skw_build_node.md):
cd ~/scaleplex-ffmpeg
DEB_BUILD_OPTIONS=nostrip ./build.sh
# Produces an unstripped deb suitable for in-pod gdb
```

Deploy the resulting image to `plex-test` worker DS, run the touched-
patch-specific live validation (e.g. for patch 0120 → sub-burn
startup; for patch 0121 → PGS cue clear; for patch 0122 →
sched-sinkless interactions; for patch 0107 → matroska Duration
field). Capture the validation in the PR description.

**After validation:** rebuild stripped (default `./build.sh`) for the
production deb.

**See:** memory `feedback_ffmpeg_fork_debug_build_before_release`,
[`scaleplex-ffmpeg/patches/`](../scaleplex-ffmpeg/patches/) for the
fork patch list.

## Tagging

After T1–T5 green. **release-please** (`.github/workflows/release-please.yaml`)
watches `main` for Conventional Commits and maintains a draft Release PR
titled `chore(main): release X.Y.Z`. The bot computes the version bump
from commit types (`feat:` → minor, `fix:` → patch, `feat!:`/`BREAKING
CHANGE:` → major). The git tag + GitHub Release are created
automatically when the Release PR merges. Image retagging stays manual
via `crane` (the gh CLI's token has `read:packages` only — push needs
`SECRET_GHCR_PUSH_TOKEN` from BWS).

### Release-PR pre-merge edits

The auto-generated CHANGELOG entry is one bullet per merged PR. Before
merging the Release PR, edit `CHANGELOG.md` in that PR's branch to:

1. **Prepend an Images-shipped table** immediately after the version
   header. The git tag tracks the worker stream only; the table tells
   readers which of the three images actually bumped.

   ```markdown
   ## [X.Y.Z](https://github.com/Varashi/scaleplex/compare/vPREV...vX.Y.Z) (DATE)

   **Images shipped:**

   | Image | Tag | Notes |
   |---|---|---|
   | `ghcr.io/varashi/scaleplex_worker` | `vX.Y.Z` | new — <one-line> |
   | `ghcr.io/varashi/scaleplex_orchestrator` | `vA.B.C` | new — <one-line>, or `unchanged` |
   | `ghcr.io/varashi/scaleplex_pms_dockermod` | `vD.E.F` | new — <one-line>, or `unchanged` |
   ```

2. **Add narrative paragraphs** describing what shipped at a level
   matching prior releases (see v1.7.0/v1.7.2 for examples — terse
   one-bullet auto-CHANGELOG is too thin for users diffing release
   pages).

### Merge + post-merge sync

Merge the Release PR. release-please creates the git tag + GH Release
in ~30s. **The GH Release body is the auto-generated bullets, NOT the
CHANGELOG.md section** (release-please's quirk — it doesn't re-read
the file after PR commit). Sync it:

```bash
# Extract the new version's section from CHANGELOG.md and overwrite the
# GH Release body with it.
awk '/^## \[X.Y.Z\]/{flag=1; next} /^## /{flag=0} flag' CHANGELOG.md > /tmp/release-body.md
gh release edit vX.Y.Z -R Varashi/scaleplex --notes-file /tmp/release-body.md
```

### Image retagging (crane)

For each image whose code changed since the previous release, retag the
`sha-<short>` (of the merge commit) to the new image version. **Use
`crane tag` — NEVER `docker pull+tag+push` or `podman pull+tag+push`**
(those corrupt LSIO docker-mod images, see
`feedback_podman_retag_breaks_lsio_dockermods`).

```bash
# Auth (uses SECRET_GHCR_PUSH_TOKEN from BWS — the gh CLI token has
# read:packages + delete:packages only; no write:packages):
PUSH_TOKEN=$(bws secret get 8e6f3b91-17c1-4c76-bba2-b42c01780722 \
  | python3 -c "import json,sys; print(json.loads(sys.stdin.read())['value'])")
echo "$PUSH_TOKEN" | crane auth login ghcr.io -u Varashi --password-stdin

# Retag whichever images bumped (skip lines for unchanged images):
crane tag ghcr.io/varashi/scaleplex_worker:sha-<short>       vX.Y.Z
crane tag ghcr.io/varashi/scaleplex_orchestrator:sha-<short> vA.B.C
crane tag ghcr.io/varashi/scaleplex_pms_dockermod:sha-<short> vD.E.F

# Verify digests match:
crane digest ghcr.io/varashi/scaleplex_worker:vX.Y.Z
crane digest ghcr.io/varashi/scaleplex_worker:sha-<short>
```

Per-image cadence:
- `scaleplex_worker` — fast (matches git tag stream)
- `scaleplex_orchestrator` — slow (last bumped v1.2.1 → v1.3.0 in v1.8.0)
- `scaleplex_pms_dockermod` — slow (only when `shim/` changes)

### GitOps bump (prod)

```bash
$EDITOR ~/git/k8s/cluster-talos/kubernetes/apps/media/plex/app/helmrelease.yaml
# Update scaleplex_worker tag + sha pin.
git -C ~/git/k8s add . && git -C ~/git/k8s commit -m "..." && git -C ~/git/k8s push
# Flux webhook fires; reconcile in ~30s.
```

## Per-release checklist template

Copy into the release PR description:

```markdown
## Release gate (v<X.Y.Z>)

### T1 — unit tests (CI)
- [ ] build-worker.yaml — success
- [ ] build-orchestrator.yaml — success
- [ ] build-shim.yaml — success
- [ ] Coverage % vs prior release: worker NN.N% / orch NN.N% / shim/cmd/relay NN.N% / shim/cmd/shim NN.N%

### T2 — replay corpus
- [ ] Pre-tag sweep.sh
- [ ] Local REPLAY_NO_FFMPEG=1: BitmapOverlayUnified ✓ / OrthogonalEmitParity ✓ / TestReplayCorpus (no unrecognised bails) ✓
- [ ] In-pod plex-test: 0 FAIL argv, 0 TIMEOUT

### T3 — qa_matrix.py
- [ ] 32/32 PASS
- [ ] plex-test prefs restored

### T4 — live sweep (TEST_MATRIX.md "Release gate" section)
- [ ] Cells 1-11 walked
- [ ] Failures all have `[KNOWN: …]` cross-link

### T5 — debug-build (if fork changed)
- [ ] DEB_BUILD_OPTIONS=nostrip live validation on plex-test
- [ ] Stripped deb produced for release

### Release
- [ ] release-please Release PR opened (`chore(main): release X.Y.Z`)
- [ ] CHANGELOG.md edited in Release PR with **Images-shipped table** + narrative paragraphs
- [ ] Release PR merged → `vX.Y.Z` git tag + GitHub Release auto-created
- [ ] GH Release body synced from CHANGELOG.md via `gh release edit --notes-file`
- [ ] `crane tag` applied to each image whose code changed (worker / orchestrator / pms_dockermod) — auth via `SECRET_GHCR_PUSH_TOKEN` from BWS
- [ ] GitOps bump in `~/git/k8s/cluster-talos/kubernetes/apps/media/plex/...` (image tag + sha pin), commit + push
```
