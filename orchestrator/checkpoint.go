package main

// checkpoint cache — per-Plex-session resume state.
//
// Phase 4b/4d: when an ffmpeg session dies mid-stream (worker pod
// crash, manual kill, voluntary swap) Plex's transcoder supervisor
// usually re-spawns ffmpeg to keep serving the client. Plex generates
// a NEW shim session_id for the retry but keeps the SAME progressurl
// (which carries Plex's own per-transcode-session UUID). We cache
// `{last_seq, segment_time, original_ss}` keyed by that UUID. When
// the new POST /task arrives we recognise the resume case and inject
// `-ss <next_pts>` + `-segment_start_number <last+1>` so the worker
// picks up exactly where the old one stopped, instead of redoing the
// chunks the client has already buffered.
//
// We deliberately do NOT try to keep the PMS-facing HTTP body alive
// through a worker swap (that needs an orchestrator-side stream mux —
// real complexity for marginal gain on a 1-3 concurrent homelab).
// The PMS-facing stream dies with the source ffmpeg; Plex's natural
// retry pipeline re-establishes everything.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// progressURL: http://127.0.0.1:32400/video/:/transcode/session/<plex-session-uuid>/<job-uuid>/progress
// Plex normalises the first UUID across ffmpeg restarts; the second
// (job UUID) changes per spawn. Match upper- and lowercase hex.
var progressSessionRE = regexp.MustCompile(`/transcode/session/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})/`)

// plexSessionFromArgs returns the Plex transcode-session UUID
// (lowercased) parsed from the -progressurl argument. Empty string
// means we couldn't find one — caller should fall through to fresh
// spawn behaviour.
func plexSessionFromArgs(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-progressurl" {
			m := progressSessionRE.FindStringSubmatch(args[i+1])
			if m != nil {
				return strings.ToLower(m[1])
			}
		}
	}
	return ""
}

// initialSeekFromArgs returns the value of `-ss <T>` placed before the
// first `-i` (input seek). Output-side -ss flags are ignored. 0 means
// no input seek, which is also what we get when the field is absent.
func initialSeekFromArgs(args []string) float64 {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-i" {
			break
		}
		if args[i] == "-ss" {
			if v, err := strconv.ParseFloat(args[i+1], 64); err == nil {
				return v
			}
		}
	}
	return 0
}

// segmentTimeFromArgs reads `-segment_time <N>` from the argv. Default
// 1.0 — Plex's HLS mux emits 1-second segments by default, and that's
// what we see across the captured corpus.
func segmentTimeFromArgs(args []string) float64 {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-segment_time" {
			if v, err := strconv.ParseFloat(args[i+1], 64); err == nil && v > 0 {
				return v
			}
		}
	}
	return 1.0
}

type resumeHint struct {
	OriginalSeek float64   // value of `-ss` in the original session, 0 if absent
	SegmentTime  float64   // HLS segment duration
	LastSeq      int64     // highest segment seq emitted
	UpdatedAt    time.Time // last successful poll
}

type checkpointCache struct {
	mu  sync.Mutex
	m   map[string]*resumeHint // key = lowercased plex-session-uuid
	ttl time.Duration
}

func newCheckpointCache(ttl time.Duration) *checkpointCache {
	return &checkpointCache{m: make(map[string]*resumeHint), ttl: ttl}
}

func (c *checkpointCache) put(plexID string, h *resumeHint) {
	if plexID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[plexID] = h
}

func (c *checkpointCache) get(plexID string) *resumeHint {
	if plexID == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.m[plexID]
	if h == nil {
		return nil
	}
	if time.Since(h.UpdatedAt) > c.ttl {
		delete(c.m, plexID)
		return nil
	}
	return h
}

// drop is called when a session is known to have ended cleanly (so we
// don't accidentally resume the next unrelated session that happens to
// land on the same PMS-side UUID — rare but possible after an ffmpeg
// graceful-exit).
func (c *checkpointCache) drop(plexID string) {
	if plexID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, plexID)
}

// pollCheckpoint runs while a session is dispatched-here-via-orchestrator.
// Polls the worker's /task/<id>/checkpoint every 2s and updates the cache.
// Exits when ctx is cancelled (handleTask's context tied to PMS-facing
// stream lifetime).
func pollCheckpoint(ctx context.Context, workerURL, sessionID string, args []string) {
	plexID := plexSessionFromArgs(args)
	if plexID == "" {
		return // no progressurl → no resume key
	}
	origSeek := initialSeekFromArgs(args)
	segTime := segmentTimeFromArgs(args)

	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			seq, ok := fetchLastSeq(ctx, client, workerURL, sessionID)
			if !ok {
				continue
			}
			cpCache.put(plexID, &resumeHint{
				OriginalSeek: origSeek,
				SegmentTime:  segTime,
				LastSeq:      seq,
				UpdatedAt:    time.Now(),
			})
		}
	}
}

// fetchLastSeq calls GET /task/<id>/checkpoint on the worker.
// Returns (seq, true) on success; (0, false) on transport/HTTP error
// or unexpected JSON.
func fetchLastSeq(ctx context.Context, c *http.Client, workerURL, sessionID string) (int64, bool) {
	url := workerURL + "/task/" + sessionID + "/checkpoint"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, false
	}
	var cp struct {
		LastSegmentSeq int64 `json:"last_segment_seq"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cp); err != nil {
		return 0, false
	}
	return cp.LastSegmentSeq, true
}

// cpCache is the process-wide checkpoint cache. 5-minute TTL covers
// Plex's transcoder supervisor restart latency (typically <30s) with
// margin for human-driven retries.
var cpCache = newCheckpointCache(5 * time.Minute)

// resumeIfApplicable returns argv with resume flags injected when the
// caller's plex-session-uuid is in the cache AND the new request's
// initial seek matches the original (so we don't clobber a deliberate
// user-driven re-seek with stale resume state).
//
// Returns (newArgs, true) on injection, (origArgs, false) otherwise.
//
// Mutations:
//   - -ss before -i: replaces value or inserts.
//   - -copyts: inserted before -i if absent (preserves PTS so PMS sees
//     contiguous timestamps across the swap).
//   - -segment_start_number: replaces value or appends to the output
//     section. Worker re-renumbers segments via segwatch starting from
//     this seed.
func resumeIfApplicable(args []string) ([]string, bool) {
	plexID := plexSessionFromArgs(args)
	hint := cpCache.get(plexID)
	if hint == nil || hint.LastSeq <= 0 {
		return args, false
	}
	newSeek := initialSeekFromArgs(args)
	if !floatEq(newSeek, hint.OriginalSeek, 0.05) {
		// Client re-seeked deliberately; resume would land at the wrong
		// position. Fresh spawn, drop the stale hint.
		cpCache.drop(plexID)
		return args, false
	}
	resumePTS := hint.OriginalSeek + float64(hint.LastSeq)*hint.SegmentTime
	nextSeq := hint.LastSeq + 1

	out := injectResumeFlags(args, resumePTS, nextSeq)
	log.Printf("session resume: plex=%s last_seq=%d → -ss %.3f -segment_start_number %d",
		plexID, hint.LastSeq, resumePTS, nextSeq)
	return out, true
}

func floatEq(a, b, tol float64) bool {
	if a > b {
		return a-b <= tol
	}
	return b-a <= tol
}

// injectResumeFlags rewrites the input-seek and segment-start-number
// flags. Pure function — unit tested directly.
func injectResumeFlags(args []string, resumePTS float64, nextSeq int64) []string {
	out := make([]string, len(args))
	copy(out, args)

	// Locate -i. Everything before is input flags; input-seek goes there.
	iIdx := -1
	for i, a := range out {
		if a == "-i" {
			iIdx = i
			break
		}
	}

	resumeSS := strconv.FormatFloat(resumePTS, 'f', 3, 64)
	resumeSeq := strconv.FormatInt(nextSeq, 10)

	// Replace existing -ss before -i, else insert one.
	replacedSS := false
	for i := 0; i < iIdx && i < len(out)-1; i++ {
		if out[i] == "-ss" {
			out[i+1] = resumeSS
			replacedSS = true
			break
		}
	}
	if !replacedSS && iIdx >= 0 {
		out = append(out[:iIdx], append([]string{"-ss", resumeSS}, out[iIdx:]...)...)
		iIdx += 2
	}

	// Insert -copyts before -i if absent.
	if iIdx >= 0 {
		hasCopyts := false
		for i := 0; i < iIdx; i++ {
			if out[i] == "-copyts" {
				hasCopyts = true
				break
			}
		}
		if !hasCopyts {
			out = append(out[:iIdx], append([]string{"-copyts"}, out[iIdx:]...)...)
		}
	}

	// Replace existing -segment_start_number, else append.
	replacedSeq := false
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "-segment_start_number" {
			out[i+1] = resumeSeq
			replacedSeq = true
			break
		}
	}
	if !replacedSeq {
		out = append(out, "-segment_start_number", resumeSeq)
	}
	return out
}
