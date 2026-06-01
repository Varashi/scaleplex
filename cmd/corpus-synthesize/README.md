# corpus-synthesize

Emits synthetic [replay-corpus](../../worker/agent/testdata/replay-corpus)
cells for argv-shape combinations the **organic** capture corpus never
produced — specifically the `hw-subburn-transcode` class (an
`inlineass=` sub-burn filtergraph paired with a `*_vaapi` video
encoder).

## Why

PR #144 fixed three latent bugs that combined to break Plex Web
force-burn on the Plex Versions / Optimized-for-TV file class. The
triggering argv combined `:#0xNN` stream-specs + `-f dash` +
`-filter_complex inlineass=` + a HW encoder. **Zero** corpus entries
matched all four — Frank's household rarely force-burns, so the combo
was never captured, and replay can only test what the corpus contains
(#150). PR #152 (#147) then added a must-reshape bail assertion for this
class, but with no fixture cell in the shape it never fired against the
CI corpus — only the unit test exercised it (#153).

This generator fills that gap deterministically.

## How it differs from `optimize-corpus-gen`

`optimize-corpus-gen` drives a **live** PMS through Optimize jobs and
*captures* the real argv each produces. `corpus-synthesize` needs no
cluster and no Plex: it *transforms* a checked-in **sanitized real
capture** (`templates/base__*.json`) along a small axis matrix. Output
is fully deterministic.

## Faithful-fragment policy

A synthetic cell is only worth committing if it's an argv PMS would
*actually* emit — otherwise replay tests the rewriter against argvs it
will never see (false confidence or false failure). So synthesis is
restricted to transforms of a real capture whose result stays a
documented PMS form:

| Axis | Values | Faithful because |
|---|---|---|
| `StreamSpec`  | `ordinal` / `hex` | `:#0xNN` is Plex's high-program-ID stream selector (confirmed in the organic corpus on m2ts / Plex Versions); the #144 trigger combined it with dash + inlineass. |
| `DecodeCodec` | `av1` / `hevc`    | both decode through the same vaapi hwaccel path; only the `-codec` value differs. |
| `EncodeCodec` | `hevc_vaapi` / `h264_vaapi` | output encoder token swap. |

2 × 2 × 2 = **8 cells**, all on the external-sidecar-SRT + dash base.

### Deliberately not synthesized yet

No faithful fragment template exists (the organic corpus has no
`inlineass` cell in these shapes to derive from), so these are **not**
hand-written — adding one means dropping a real sanitized capture under
`templates/` plus a new axis in `matrix.go`:

- HLS (`ssegment`/`segment`) sub-burn muxer tail
- embedded (muxed) text/ASS sub-source (`-map 0:s:N`, single `-i`)
- HDR tonemap variants (opencl / vaapi tonemap node)
- NVENC / AMF cross-backend encoders

Tracked as #150 follow-ups.

## Usage

```bash
# preview the matrix, write nothing
go run ./cmd/corpus-synthesize -list

# (re)generate the fixture cells into the replay corpus
go run ./cmd/corpus-synthesize -out-dir worker/agent/testdata/replay-corpus
```

Each cell is written as `synth__<decode>__<encode>__<spec>__dash-extsrt.json`
with `"capture_source": "synthesized"` and `"synthesized": true` so it's
never mistaken for an organic capture. PR CI's `TestReplayCorpus`
(`worker/agent/build-worker.yaml`) picks them up automatically — a
regression that makes the rewriter bail on this class flips the cell
PASS → FAIL.

## Validating after regeneration

```bash
cd worker/agent
REPLAY_CORPUS_DIR=$PWD/testdata/replay-corpus REPLAY_NO_FFMPEG=1 \
  go test -tags=replay -run TestReplayCorpus -v
```

All `source=synthesized` cells must report `PASS rewrite`. A `FAIL bail`
means either the rewriter regressed on the `hw-subburn-transcode` class
or the embedded base template drifted out of the shape.
