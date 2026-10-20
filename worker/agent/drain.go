package main

// Graceful drain on SIGTERM/SIGINT (scaleplex#242).
//
// A worker DaemonSet rollout used to kill workers with live sessions and no
// warning. The orchestrator kept routing to the dying worker until its next
// /capability probe, and mid-stream recovery could land on a worker that was
// itself about to go. On a termination signal the worker now:
//
//  1. flips `draining` — /readyz turns 503 (the Service drops it), /capability
//     reports draining=true and ready=false (the orchestrator stops picking it),
//     and new /task requests get 503 (the orchestrator falls through to the next
//     candidate);
//  2. waits up to SCALEPLEX_DRAIN_SECONDS (default 10) for active sessions to
//     finish on their own;
//  3. exits. Any ffmpeg still running dies with the agent (Pdeathsig). The
//     orchestrator sees a mid-stream failure and resumes the session on another
//     worker.
//
// Keep SCALEPLEX_DRAIN_SECONDS below the pod's terminationGracePeriodSeconds
// (k8s default 30) so the worker exits on its own terms, not by SIGKILL.

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const defaultDrainSeconds = 10

// draining is 1 once a termination signal has been received.
var draining int32

func isDraining() bool { return atomic.LoadInt32(&draining) == 1 }

// drainTimeout reads SCALEPLEX_DRAIN_SECONDS (0 = exit immediately).
func drainTimeout() time.Duration {
	v := os.Getenv("SCALEPLEX_DRAIN_SECONDS")
	if v == "" {
		return defaultDrainSeconds * time.Second
	}
	var n int
	for _, c := range v {
		if c < '0' || c > '9' {
			return defaultDrainSeconds * time.Second
		}
		n = n*10 + int(c-'0')
	}
	return time.Duration(n) * time.Second
}

// waitForSessions polls activeCount until it reaches zero or timeout elapses.
// Returns the number of sessions still active when it gave up.
func waitForSessions(activeCount func() int, timeout, poll time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		n := activeCount()
		if n == 0 || !time.Now().Before(deadline) {
			return n
		}
		time.Sleep(poll)
	}
}

// serveWithDrain runs srv until a termination signal, then drains and exits.
func serveWithDrain(srv *http.Server) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
		return
	case sig := <-sigs:
		atomic.StoreInt32(&draining, 1)
		metricReady.Set(0)
		timeout := drainTimeout()
		log.Printf("drain: %s received, refusing new sessions; waiting up to %s for %d active",
			sig, timeout, registry.activeCount())
		left := waitForSessions(registry.activeCount, timeout, 250*time.Millisecond)
		if left > 0 {
			log.Printf("drain: timeout with %d session(s) still active — exiting; orchestrator recovers them elsewhere", left)
		} else {
			log.Printf("drain: no active sessions — exiting")
		}
		// Don't wait on open /task streams (they can run for hours): close
		// listeners and idle conns, give in-flight non-stream handlers a moment.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}
