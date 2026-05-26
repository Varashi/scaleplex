package capture

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/Varashi/scaleplex/cmd/optimize-corpus-gen/source"
)

// CellTag is the metadata the generator writes alongside each captured
// argv as a sidecar JSON. The corpus then sorts/filters by these axes
// to ask questions like "every cell where source HDR + HEVC-optimize
// ON" → "how many distinct argv shapes appeared?".
//
// One CellTag per capture. Multi-capture cells (e.g. Optimize jobs
// that emit two ffmpeg spawns) get one sidecar each, all referencing
// the same CellID.
type CellTag struct {
	// CellID groups every capture from a single Optimize trigger
	// together. Format: <RFC3339-nano timestamp>-<sourceRatingKey>-<targetTagID>.
	CellID string `json:"cell_id"`

	// CapturedAt is the timestamp the sidecar was written (i.e. when
	// the generator detected the new corpus file). Useful for replay
	// ordering across runs.
	CapturedAt time.Time `json:"captured_at"`

	// Source identifies the file PMS handed to ffmpeg. RatingKey + the
	// authoritative ffprobe Profile (NOT Plex's library metadata —
	// which goes stale after Tdarr).
	Source SourceRef `json:"source"`

	// Target identifies the Optimize preset PMS targeted (one cell of
	// the matrix). TagID is the parameter the Optimize trigger took;
	// Title is human-friendly.
	Target TargetRef `json:"target"`

	// Prefs records the PMS pref combination active at trigger time.
	// Keyed by the same IDs SaveSnapshot+Restore use (PrefMatrixIDs).
	Prefs map[string]string `json:"prefs"`

	// CaptureFile is the basename of the corpus capture this sidecar
	// is tagging. Cross-reference for tools that scan the dir.
	CaptureFile string `json:"capture_file"`
}

type SourceRef struct {
	RatingKey string          `json:"rating_key"`
	Title     string          `json:"title"`
	Probe     *source.Profile `json:"probe"`
}

type TargetRef struct {
	TagID int    `json:"tag_id"`
	Title string `json:"title"`
}

// Write serializes the tag to <captureFile>.optimize-cell.json
// alongside the original capture. Idempotent — overwrites if the
// sidecar already exists (e.g. on a re-run of the same cell).
func (t *CellTag) Write(captureFile string) error {
	t.CaptureFile = baseName(captureFile)
	if t.CapturedAt.IsZero() {
		t.CapturedAt = time.Now().UTC()
	}
	body, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	sidecar := strings.TrimSuffix(captureFile, ".json") + ".optimize-cell.json"
	return os.WriteFile(sidecar, body, 0o644)
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
