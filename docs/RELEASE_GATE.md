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
~/git/scaleplex/cmd/argv-extract/sweep.sh
# Pulls latest worker NFS captures + worker stderr logs from prod + plex-test
# Idempotent — only adds new sessions
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

After T1–T5 green:

```bash
cd ~/git/scaleplex
git tag -a v<X.Y.Z> -m "scaleplex v<X.Y.Z>"
git push origin v<X.Y.Z>
# wait for build-worker.yaml to produce sha-<short>
crane tag ghcr.io/varashi/scaleplex_worker:sha-<short> v<X.Y.Z>
# crane tag (NOT podman) — see feedback_podman_retag_breaks_lsio_dockermods
```

Then GitOps bump in `~/git/k8s/cluster-talos/kubernetes/apps/plex/.../`
(image tag pin), commit, push, Flux reconcile.

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

### Tag
- [ ] git tag v<X.Y.Z>
- [ ] crane tag worker image
- [ ] GitOps bump in k8s repo
```
