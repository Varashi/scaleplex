package main

// Dispatch, streaming and mid-stream recovery (#219, #242).
//
// A task is one long-lived POST from the PMS-side shim. The orchestrator picks
// a worker, streams its output back, and — if that worker dies after output
// has started flowing — resumes the session on another worker over the same
// PMS-facing connection.
//
// Failure handling, per attempt:
//   - worker refuses before accepting (503 at cap/draining, or connection
//     refused/reset): nothing reached PMS, so try the next candidate. A
//     connection failure also marks the worker down at once, so it stops
//     attracting sessions before the next /capability probe (#219).
//   - worker accepted, then the stream broke: recover onto another worker.
//     Recovery re-resolves DNS and re-probes the fleet, waiting up to
//     SCALEPLEX_RECOVERY_WAIT_SECONDS for a healthy candidate (#242): during
//     a rolling restart every cached address can be stale at once.
//   - PMS went away (write error / request context cancelled): stop. Not a
//     worker failure, not a recovery.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	// maxTaskBody bounds the /task envelope read (argv + env; KBs in practice).
	maxTaskBody = 8 << 20
	// defaultRecoveryWait is how long one recovery may wait for a healthy
	// worker to appear (SCALEPLEX_RECOVERY_WAIT_SECONDS). Covers a worker
	// rollout: new pod scheduled, pre-warmed, and probed.
	defaultRecoveryWait = 30 * time.Second
	// maxRecoveries caps how many times one session may be moved after its
	// stream broke mid-flight.
	maxRecoveries = 3
)

var recoveryWait = defaultRecoveryWait

// dispatchTicket tracks one dispatch attempt's contribution to a worker's
// in-flight load: pending until the worker answers, then accepted until the
// stream ends (counted only until a probe reflects it — see worker.inFlight).
type dispatchTicket struct {
	w        *worker
	accepted bool
	ended    bool
}

func (w *worker) begin() *dispatchTicket {
	w.dispatchBegin()
	return &dispatchTicket{w: w}
}

func (t *dispatchTicket) accept() {
	if t.accepted || t.ended {
		return
	}
	t.accepted = true
	t.w.dispatchEnd()
	t.w.mu.Lock()
	if t.w.accepted == nil {
		t.w.accepted = make(map[*dispatchTicket]time.Time)
	}
	t.w.accepted[t] = time.Now()
	t.w.mu.Unlock()
}

func (t *dispatchTicket) end() {
	if t.ended {
		return
	}
	t.ended = true
	if !t.accepted {
		t.w.dispatchEnd()
		return
	}
	t.w.mu.Lock()
	delete(t.w.accepted, t)
	t.w.mu.Unlock()
}

// pendingAndAcceptedLocked is inFlight for callers already holding w.mu.
func (w *worker) pendingAndAcceptedLocked() int {
	n := int(w.pending)
	for _, at := range w.accepted {
		if !at.Before(w.lastProbeStart) {
			n++
		}
	}
	return n
}

type outcome int

const (
	outDone       outcome = iota // upstream finished cleanly
	outRefused                   // worker never accepted; nothing reached PMS from this attempt
	outBroken                    // worker accepted, then the stream failed
	outClientGone                // PMS side went away
)

// pmsStream is the PMS-facing side of one task, shared across upstream swaps.
type pmsStream struct {
	w              http.ResponseWriter
	r              *http.Request
	flusher        http.Flusher
	sessionID      string
	args           []string
	headersWritten bool
	wroteAny       bool
	lastByte       byte
	refusedBy503   bool // last refusal was a 503 (vs. unreachable) — metrics only
}

func newPMSStream(w http.ResponseWriter, r *http.Request, sessionID string, body []byte) *pmsStream {
	ps := &pmsStream{w: w, r: r, sessionID: sessionID}
	ps.flusher, _ = w.(http.Flusher)
	var req taskRequest
	if json.Unmarshal(body, &req) == nil {
		ps.args = req.Args
	}
	return ps
}

func (ps *pmsStream) write(p []byte) error {
	if _, err := ps.w.Write(p); err != nil {
		return err
	}
	ps.wroteAny = true
	ps.lastByte = p[len(p)-1]
	if ps.flusher != nil {
		ps.flusher.Flush()
	}
	return nil
}

// streamFrom runs one upstream attempt against wk.
func (ps *pmsStream) streamFrom(wk *worker, body []byte) outcome {
	ticket := wk.begin()
	defer ticket.end()

	ctx, cancel := context.WithCancel(ps.r.Context())
	defer cancel()

	preq, err := http.NewRequestWithContext(ctx, http.MethodPost, wk.url+"/task", strings.NewReader(string(body)))
	if err != nil {
		log.Printf("session %s: build request for %s: %v", ps.sessionID, wk.url, err)
		return outRefused
	}
	preq.Header.Set("Content-Type", "application/json")
	resp, err := proxyClient.Do(preq)
	if err != nil {
		if ps.r.Context().Err() != nil {
			return outClientGone
		}
		log.Printf("session %s: worker %s unreachable: %v", ps.sessionID, wk.url, err)
		wk.markDown("dispatch: " + err.Error())
		ps.refusedBy503 = false
		return outRefused
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		log.Printf("session %s: worker %s refused (503: at capacity or draining), falling through", ps.sessionID, wk.url)
		ps.refusedBy503 = true
		return outRefused
	}

	ticket.accept()
	sessions.set(ps.sessionID, wk.url)
	defer sessions.del(ps.sessionID)

	// Checkpoint poller follows the CURRENT upstream (#219: it used to keep
	// polling the dead first worker after a recovery swap).
	go pollCheckpoint(ctx, wk.url, ps.sessionID, ps.args)

	if !ps.headersWritten {
		for k, v := range resp.Header {
			ps.w.Header()[k] = v
		}
		ps.w.WriteHeader(resp.StatusCode)
		ps.headersWritten = true
		if ps.flusher != nil {
			ps.flusher.Flush()
		}
	} else if ps.wroteAny && ps.lastByte != '\n' {
		// The previous worker died mid-line. Terminate it so its fragment
		// doesn't merge with the new worker's first line (#219).
		if err := ps.write([]byte("\n")); err != nil {
			return outClientGone
		}
	}

	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if err := ps.write(buf[:n]); err != nil {
				return outClientGone // PMS gone — no recovery
			}
		}
		if rerr == nil {
			continue
		}
		if rerr == io.EOF {
			return outDone
		}
		if ps.r.Context().Err() != nil {
			return outClientGone
		}
		log.Printf("session %s: stream from %s broke: %v", ps.sessionID, wk.url, rerr)
		wk.markDown("stream: " + rerr.Error())
		return outBroken
	}
}

// dispatchTask sends the task to the best worker, falling through refusals,
// and recovers mid-stream failures.
func dispatchTask(w http.ResponseWriter, r *http.Request, body []byte, sessionID string) {
	ps := newPMSStream(w, r, sessionID, body)

	// Candidate order computed ONCE (round-robin needs a stable rotation).
	order := pl.schedule()
	if len(order) == 0 {
		metricDispatchTotal.WithLabelValues("no_workers").Inc()
		http.Error(w, "all workers at capacity", http.StatusServiceUnavailable)
		return
	}
	for i, wk := range order {
		log.Printf("session %s: try worker %s (load=%.3f, backend=%s, attempt=%d/%d)",
			sessionID, wk.url, wk.load(), wk.backendStr(), i+1, len(order))
		switch ps.streamFrom(wk, body) {
		case outRefused:
			if ps.refusedBy503 {
				metricDispatchTotal.WithLabelValues("fallthrough_503").Inc()
			} else {
				metricDispatchTotal.WithLabelValues("fallthrough_unreachable").Inc()
			}
			continue
		case outDone:
			metricDispatchAttempts.Observe(float64(i + 1))
			metricDispatchTotal.WithLabelValues("success").Inc()
			return
		case outClientGone:
			metricDispatchAttempts.Observe(float64(i + 1))
			metricDispatchTotal.WithLabelValues("success").Inc()
			return
		case outBroken:
			metricDispatchAttempts.Observe(float64(i + 1))
			metricDispatchTotal.WithLabelValues("success").Inc()
			ps.recover(wk, body)
			return
		}
	}
	metricDispatchTotal.WithLabelValues("all_at_cap").Inc()
	metricDispatchAttempts.Observe(float64(len(order)))
	http.Error(w, "all workers at capacity", http.StatusServiceUnavailable)
}

// recover moves a session whose stream broke onto another worker, resuming
// from the checkpoint cache. The PMS-facing connection stays open.
func (ps *pmsStream) recover(failed *worker, body []byte) {
	// broken: workers whose stream broke for this session — never reused for
	// it (a worker whose ffmpeg crashed can still pass /capability probes).
	// refused: workers that refused the connection — eligible again once a
	// newer probe shows them healthy (e.g. a pod that finished restarting).
	broken := map[string]bool{failed.url: true}
	refused := map[string]time.Time{}
	from := failed.url
	for moves := 1; moves <= maxRecoveries; moves++ {
		deadline := time.Now().Add(recoveryWait)
		for {
			if ps.r.Context().Err() != nil {
				log.Printf("session %s: client gone during recovery", ps.sessionID)
				return
			}
			next := waitForRecoveryWorker(ps.r.Context(), broken, refused, deadline)
			if next == nil {
				log.Printf("session %s: mid-stream failure on %s, no healthy worker within %s", ps.sessionID, from, recoveryWait)
				metricDispatchTotal.WithLabelValues("recovery_failed").Inc()
				return
			}
			log.Printf("session %s: mid-stream failure on %s, recovering to %s (move=%d)", ps.sessionID, from, next.url, moves)
			body = withResume(body)
			switch ps.streamFrom(next, body) {
			case outDone:
				metricDispatchTotal.WithLabelValues("recovered").Inc()
				return
			case outClientGone:
				return
			case outRefused:
				refused[next.url] = time.Now() // try another candidate, same deadline
				continue
			case outBroken:
				metricDispatchTotal.WithLabelValues("recovered").Inc()
				broken[next.url] = true
				from = next.url
			}
			break // broke again → next move with a fresh deadline
		}
	}
	log.Printf("session %s: recovery exhausted after %d moves", ps.sessionID, maxRecoveries)
	metricDispatchTotal.WithLabelValues("recovery_failed").Inc()
}

// waitForRecoveryWorker returns the best healthy worker this session hasn't
// broken on (a refused one only once a newer probe shows it healthy),
// re-resolving DNS and re-probing the fleet until one shows up or the deadline
// passes.
func waitForRecoveryWorker(ctx context.Context, broken map[string]bool, refused map[string]time.Time, deadline time.Time) *worker {
	pick := func() *worker {
		for _, w := range pl.schedule() {
			if broken[w.url] {
				continue
			}
			if t, ok := refused[w.url]; ok && !w.healthySince(t) {
				continue
			}
			return w
		}
		return nil
	}
	for {
		if w := pick(); w != nil {
			return w
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return nil
		}
		pl.rediscover()
		if w := pick(); w != nil {
			return w
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

// withResume injects resume flags from the checkpoint cache (idempotent).
func withResume(body []byte) []byte {
	var req taskRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return body
	}
	newArgs, resumed := resumeIfApplicable(req.Args)
	if !resumed {
		return body
	}
	req.Args = newArgs
	nb, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return nb
}
