package matrix

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Manifest tracks per-cell run status across a long sweep so SIGINT-
// resume picks up where it left off without re-triggering captured
// cells (which would create "media version already exists" collisions).
//
// Persisted as <corpusDir>/manifest.json — append-on-progress, durable
// fsync after each cell completes.
type Manifest struct {
	StartedAt   time.Time              `json:"started_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
	CellResults map[string]CellResult  `json:"cell_results"` // keyed by Cell.ID
	mu          sync.Mutex             `json:"-"`
	path        string                 `json:"-"`
}

// CellResult is the outcome of one cell run.
type CellResult struct {
	Status      string    `json:"status"` // "captured" | "timeout" | "error" | "skipped"
	Captures    []string  `json:"captures,omitempty"` // local paths of captured argv files
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// LoadOrInit reads <dir>/manifest.json or returns a fresh one. Safe to
// call on an empty dir.
func LoadOrInit(dir string) (*Manifest, error) {
	path := filepath.Join(dir, "manifest.json")
	m := &Manifest{
		StartedAt:   time.Now().UTC(),
		CellResults: map[string]CellResult{},
		path:        path,
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(body, m); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %w", path, err)
	}
	m.path = path
	if m.CellResults == nil {
		m.CellResults = map[string]CellResult{}
	}
	return m, nil
}

// Done reports whether a cell has been completed (any non-error status
// the runner shouldn't retry — captured/skipped). Timeout and error
// states are NOT considered done so resume retries them.
func (m *Manifest) Done(cellID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.CellResults[cellID]
	if !ok {
		return false
	}
	return r.Status == "captured" || r.Status == "skipped"
}

// Record persists one cell's result. Calls Sync after write so a
// SIGKILL between cells loses at most the in-flight cell.
func (m *Manifest) Record(cellID string, r CellResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CellResults[cellID] = r
	m.UpdatedAt = time.Now().UTC()
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// Summary returns counters: total, captured, timeout, error, skipped.
type Summary struct {
	Total     int
	Captured  int
	Timeout   int
	Error     int
	Skipped   int
	Remaining int
}

func (m *Manifest) Summary(totalCells int) Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Summary{Total: totalCells}
	for _, r := range m.CellResults {
		switch r.Status {
		case "captured":
			s.Captured++
		case "timeout":
			s.Timeout++
		case "error":
			s.Error++
		case "skipped":
			s.Skipped++
		}
	}
	s.Remaining = s.Total - s.Captured - s.Skipped
	if s.Remaining < 0 {
		s.Remaining = 0
	}
	return s
}
