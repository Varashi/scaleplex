package main

// throttle — pass through PMS's per-session "go slow" signal to the
// worker's ffmpeg subprocess.
//
// Plex Transcoder's own throttle (fftools/ffmpeg.c after each progress
// PUT) checks the response body for the substring `canThrottle`. If
// present, it self-paces with usleep(100*ms) per encoded packet
// (libavcodec/eae.c "sloth mode"). PMS sends `canThrottle` once the
// client buffer is far enough ahead of playback that further GPU/disk
// burn would be wasted (skipped or abandoned segments).
//
// Plex's pacing path runs only on `-progressurl <http>` mode. The
// rewriter strips that and substitutes `-progress pipe:N` for our
// stdlib parser, which means Plex's built-in sloth never engages. We
// re-establish the same end-to-end behaviour by:
//
//   1. doPlexPUT reads the response body and updates a *throttleSignal.
//   2. throttleController polls the signal and pulses SIGSTOP / SIGCONT
//      on the ffmpeg process group (cmd has Setpgid:true).
//   3. While throttled, the controller duty-cycles 200 ms STOP / 50 ms
//      CONT so the progress pipe keeps draining (~20% throughput).
//      A pure SIGSTOP would deadlock — paused ffmpeg writes nothing,
//      reporter sees nothing, no PUT, no body, no way to learn that
//      PMS has cleared canThrottle.

import (
	"context"
	"log"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricThrottleState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scaleplex_worker_throttle_state",
		Help: "1 while PMS asserts canThrottle on the session, 0 otherwise.",
	}, []string{"session"})

	metricThrottleSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "scaleplex_worker_throttle_seconds_total",
		Help: "Cumulative wall time the worker spent in PMS-asserted throttle.",
	}, []string{"session"})

	// Depth = seconds since the current continuous throttle started.
	// Resets to 0 on exit. Drives duty-cycle escalation; expose for
	// dashboards so operators can spot parked / abandoned sessions
	// (depth keeps climbing while client never drains the buffer).
	metricThrottleDepthSeconds = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scaleplex_worker_throttle_depth_seconds",
		Help: "Seconds since the current continuous throttle began (0 when not throttled).",
	}, []string{"session"})
)

// dutyCycle returns (stop, cont) durations for the SIGSTOP/SIGCONT
// pulse, escalating as the throttle persists. CONT phase stays small
// so the progress pipe keeps draining and PMS-side PUTs keep firing
// — that's the only path by which canThrottle can be cleared, so we
// must not silence it even on the most aggressive tier.
//
// Tier rationale (validated 2026-05-10, escalation tightened 2026-05-11):
// Plex's `TranscoderThrottleBuffer` pref is intended as a HARD CAP via
// pause-and-resume — Plex-native ffmpeg fully sleeps the encode loop on
// canThrottle. Stock ffmpeg keeps running, so we approximate via
// SIGSTOP/SIGCONT pulses; the lighter the duty cycle, the closer we
// approximate a true pause. Escalate fast so the buffer stops growing
// quickly after the first canThrottle signal lands.
//   - <2s:    brief warmup before slamming hard; avoids spurious
//             pause-resume churn on transient signals.
//   - 2-15s:  moderate throttle (~5%) for typical buffer-ahead.
//   - 15s+:   aggressive (~1%) for sustained throttle — buffer drains.
func dutyCycle(depth time.Duration) (stop, cont time.Duration) {
	switch {
	case depth < 2*time.Second:
		return 200 * time.Millisecond, 50 * time.Millisecond
	case depth < 15*time.Second:
		return 1000 * time.Millisecond, 50 * time.Millisecond
	default:
		return 5000 * time.Millisecond, 50 * time.Millisecond
	}
}

// throttleSignal is the shared state between progress_report (writer)
// and throttleController (reader). Atomic int32 0=off, 1=on.
type throttleSignal struct{ v atomic.Int32 }

func (s *throttleSignal) set(on bool) {
	if on {
		s.v.Store(1)
	} else {
		s.v.Store(0)
	}
}

func (s *throttleSignal) on() bool { return s.v.Load() == 1 }

// throttleController watches sig and pulses SIGSTOP/SIGCONT on pgid
// (negative pid = process group, signals all ffmpeg threads) until
// ctx is done. cmd's SysProcAttr.Setpgid must be true (already the
// case for the ffmpeg spawn in main.go).
//
// Behaviour:
//   - sig false: ensure ffmpeg is running (SIGCONT once on transition),
//     poll for next state change.
//   - sig true: duty-cycle SIGSTOP(200ms)/SIGCONT(50ms) → ~20% throughput.
//     Resets to running on transition back to false.
//
// On signal failure (process already gone), exits silently. A spurious
// SIGCONT on a non-existent pid returns ESRCH — non-fatal.
func throttleController(ctx context.Context, sessionID string, pid int, sig *throttleSignal) {
	pgid := -pid // syscall.Kill semantics: negative = process group
	wasOn := false
	throttledStart := time.Now() // only meaningful while wasOn=true
	lastOff := time.Time{}       // time of last ON→OFF transition
	gauge := metricThrottleState.WithLabelValues(sessionID)
	counter := metricThrottleSeconds.WithLabelValues(sessionID)
	depthGauge := metricThrottleDepthSeconds.WithLabelValues(sessionID)

	// Sticky depth window: PMS's canThrottle signal flaps ON/OFF every
	// few seconds as the client buffer hovers near the
	// TranscoderThrottleBuffer threshold. Without hysteresis, depth
	// resets on every OFF and we stay stuck at the lightest tier
	// (200/50, ~20% throughput at 15x realtime = +20s buffer per second
	// of wall — buffer climbs to 10+ min in minutes). With sticky depth
	// across brief OFFs (<10s gap), we accumulate enough cumulative ON
	// time to escalate to the heavier tiers (~1% at 15s+).
	const stickyOffGrace = 10 * time.Second

	flush := func() {
		_ = syscall.Kill(pgid, syscall.SIGCONT)
		if wasOn {
			counter.Add(time.Since(throttledStart).Seconds())
			gauge.Set(0)
		}
		depthGauge.Set(0)
	}
	defer flush()

	for {
		if ctx.Err() != nil {
			return
		}
		on := sig.on()

		if on != wasOn {
			if on {
				if !lastOff.IsZero() && time.Since(lastOff) < stickyOffGrace {
					// Brief OFF — keep prior depth so we escalate
					// across flaps instead of restarting at light tier.
					log.Printf("session %s: throttle ON (resumed after %.1fs gap, depth %.1fs)",
						sessionID, time.Since(lastOff).Seconds(), time.Since(throttledStart).Seconds())
				} else {
					log.Printf("session %s: throttle ON (PMS canThrottle)", sessionID)
					throttledStart = time.Now()
				}
				gauge.Set(1)
			} else {
				depth := time.Since(throttledStart)
				log.Printf("session %s: throttle OFF (was throttled %.1fs)", sessionID, depth.Seconds())
				counter.Add(depth.Seconds())
				gauge.Set(0)
				depthGauge.Set(0)
				lastOff = time.Now()
				if err := syscall.Kill(pgid, syscall.SIGCONT); err != nil && err != syscall.ESRCH {
					log.Printf("session %s: SIGCONT pgid=%d: %v", sessionID, pgid, err)
				}
			}
			wasOn = on
		}

		if !on {
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}

		// Throttled: pulse SIGSTOP/SIGCONT. Duty cycle escalates with
		// time-in-throttle (see dutyCycle()): light pause for normal
		// buffer-ahead; aggressive throttle once we cross zombie /
		// abandoned thresholds. Bail out immediately on ctx cancellation
		// between phases.
		depth := time.Since(throttledStart)
		depthGauge.Set(depth.Seconds())
		stopFor, contFor := dutyCycle(depth)

		if err := syscall.Kill(pgid, syscall.SIGSTOP); err != nil {
			if err == syscall.ESRCH {
				return // process gone
			}
			log.Printf("session %s: SIGSTOP pgid=%d: %v", sessionID, pgid, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(stopFor):
		}
		if err := syscall.Kill(pgid, syscall.SIGCONT); err != nil {
			if err == syscall.ESRCH {
				return
			}
			log.Printf("session %s: SIGCONT pgid=%d: %v", sessionID, pgid, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(contFor):
		}
	}
}
