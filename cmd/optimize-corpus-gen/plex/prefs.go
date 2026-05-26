package plex

import (
	"fmt"
	"net/url"
	"strings"
)

// Pref is one PMS setting from GET /:/prefs.
//
// The fields here are the subset the generator actually reads. PMS's
// JSON encoding for `value` is polymorphic: bools come as Go `bool`,
// ints as `float64`, strings as `string`. We accept any of them and
// stringify on Get(); SetPref converts the caller's string back to
// whatever form the setter wants (PMS accepts query-string values
// generically, so a plain string round-trips for every type the
// generator touches).
type Pref struct {
	ID    string      `json:"id"`
	Label string      `json:"label"`
	Type  string      `json:"type"` // "bool", "int", "text", "enum"
	Value interface{} `json:"value"`
}

// StringValue returns Value as a string, using the canonical form
// PMS's setter accepts ("true"/"false" for bools, decimal for ints).
func (p Pref) StringValue() string {
	switch v := p.Value.(type) {
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		// Plex int prefs decode as float64 from JSON; print without trailing zeros.
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// AllPrefs returns every pref the server exposes, keyed by ID for
// O(1) lookup. Used by SaveSnapshot at startup so the generator can
// restore on exit.
func (c *Client) AllPrefs() (map[string]Pref, error) {
	var resp struct {
		MediaContainer struct {
			Setting []Pref `json:"Setting"`
		} `json:"MediaContainer"`
	}
	if err := c.do("GET", "/:/prefs", nil, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]Pref, len(resp.MediaContainer.Setting))
	for _, p := range resp.MediaContainer.Setting {
		out[p.ID] = p
	}
	return out, nil
}

// SetPref writes one pref via PUT /:/prefs?ID=value. Plex accepts a
// query-string value for every pref type the generator touches
// (bool/int/enum/text). Returns an error if the server rejects.
func (c *Client) SetPref(id, value string) error {
	q := url.Values{}
	q.Set(id, value)
	return c.do("PUT", "/:/prefs", q, nil)
}

// SetPrefs writes a batch of prefs in one PUT — Plex accepts multiple
// ID=value pairs in a single request, which is both faster and
// atomically applied. Returns the first ID that failed (if any) plus
// the underlying error.
func (c *Client) SetPrefs(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	q := url.Values{}
	for k, v := range values {
		q.Set(k, v)
	}
	if err := c.do("PUT", "/:/prefs", q, nil); err != nil {
		var keys []string
		for k := range values {
			keys = append(keys, k)
		}
		return fmt.Errorf("set prefs [%s]: %w", strings.Join(keys, ","), err)
	}
	return nil
}

// PrefMatrixIDs lists the pref keys the generator toggles when sweeping
// the Optimize corpus. Kept in one place so SaveSnapshot can grab them,
// the matrix expander can enumerate them, and Restore knows exactly
// which keys to roll back (touching unrelated prefs would alter user
// behavior outside the corpus run).
//
//   - HardwareAcceleratedCodecs    — HW decode on/off (umbrella)
//   - HardwareAcceleratedEncoders  — HW encode on/off
//   - TranscoderToneMapping        — HW HDR tonemap on/off (only meaningful when HW codecs on)
//   - TranscoderHEVCEncodingMode   — HEVC for standard transcodes ("always"|"never"|"auto")
//   - TranscoderHEVCOptimize       — HEVC for Optimize jobs (independent of standard HEVC pref!)
//
// The HEVC pair was the user's catch: TranscoderHEVCEncodingMode gates
// standard transcoding HEVC; Optimize ignores it and consults
// TranscoderHEVCOptimize instead — so an Optimize matrix that toggles
// only the standard pref misses half the cells.
var PrefMatrixIDs = []string{
	"HardwareAcceleratedCodecs",
	"HardwareAcceleratedEncoders",
	"TranscoderToneMapping",
	"TranscoderHEVCEncodingMode",
	"TranscoderHEVCOptimize",
}

// Snapshot captures the current values of PrefMatrixIDs so the run can
// restore them on exit. Stringified via StringValue so the restore path
// re-uses SetPrefs unchanged.
type Snapshot struct {
	Values map[string]string
}

// SaveSnapshot reads the current values of every pref in PrefMatrixIDs.
// Returns a Snapshot suitable for Restore.
func (c *Client) SaveSnapshot() (*Snapshot, error) {
	all, err := c.AllPrefs()
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Values: make(map[string]string, len(PrefMatrixIDs))}
	for _, id := range PrefMatrixIDs {
		p, ok := all[id]
		if !ok {
			return nil, fmt.Errorf("pref %s not found on server (PMS version skew?)", id)
		}
		snap.Values[id] = p.StringValue()
	}
	return snap, nil
}

// Restore writes the snapshotted values back. Idempotent — safe to call
// multiple times. Used by both the normal-exit cleanup path and the
// SIGINT/SIGTERM handler.
func (c *Client) Restore(snap *Snapshot) error {
	if snap == nil || len(snap.Values) == 0 {
		return nil
	}
	return c.SetPrefs(snap.Values)
}
