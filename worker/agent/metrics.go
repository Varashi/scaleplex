package main

// Prometheus metrics — exposed at /metrics for any scraper.
//
// Buckets are chosen for transcode-shaped workloads:
//   - session_duration_seconds covers single-clip transcodes (a few s)
//     up to a full feature (~7000s).
//   - first_segment_seconds is the latency-budget bucket; LATENCY.md
//     targets ≤2-4s warm/cold, so buckets cluster densely around there.
//   - speed_ratio buckets straddle the 1.0× realtime line so a histogram
//     readout makes "how many sessions are sub-realtime" trivial.

import (
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricActiveSessions = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "scaleplex_worker_active_sessions",
		Help: "Currently running ffmpeg sessions on this worker.",
	})

	metricMaxSessions = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "scaleplex_worker_max_sessions",
		Help: "Soft cap on concurrent ffmpeg spawns (0 = unlimited).",
	})

	metricReady = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "scaleplex_worker_ready",
		Help: "1 once pre-warm completes and /readyz flips green.",
	})

	metricPrewarmSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "scaleplex_worker_prewarm_seconds",
		Help: "Wall-clock duration of the pre-warm pass at boot.",
	})

	metricFFmpegOK = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "scaleplex_worker_ffmpeg_ok",
		Help: "1 if ffmpeg binary is found at startup.",
	})

	metricSessionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "scaleplex_worker_sessions_total",
		Help: "Sessions completed, labelled by outcome.",
	}, []string{"outcome"}) // success | error | killed

	metricRewriteApplied = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "scaleplex_worker_rewrite_total",
		Help: "Argument-rewrite outcomes (applied or skip-reason).",
	}, []string{"result"}) // applied | skip:<reason>

	metricSessionDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "scaleplex_worker_session_duration_seconds",
		Help:    "Wall-clock duration of a session from spawn to exit.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600, 7200},
	})

	metricFirstSegmentSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "scaleplex_worker_first_segment_seconds",
		Help:    "Time from ffmpeg spawn to first .m4s/.ts segment file.",
		Buckets: []float64{0.5, 1, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10, 15, 30},
	})

	metricFinalSpeed = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "scaleplex_worker_session_speed_ratio",
		Help:    "Final ffmpeg speed= ratio (×realtime) per session.",
		Buckets: []float64{0.25, 0.5, 0.75, 0.9, 1.0, 1.2, 1.5, 2, 3, 5, 8, 12},
	})
)

// recordSpeedFromOutput parses the last `speed= Xx` from a captured
// ffmpeg progress stream and observes the histogram. No-op if the
// output is empty or malformed.
func recordSpeedFromOutput(output string) {
	last := lastSpeedX(output)
	if last <= 0 {
		return
	}
	metricFinalSpeed.Observe(last)
}

// lastSpeedX scans a buffer (with embedded \r between progress lines)
// for the final `speed= <float>x` value.
func lastSpeedX(s string) float64 {
	var lastVal float64
	for {
		i := strings.Index(s, "speed=")
		if i < 0 {
			return lastVal
		}
		s = s[i+len("speed="):]
		// skip leading spaces
		j := 0
		for j < len(s) && s[j] == ' ' {
			j++
		}
		s = s[j:]
		// read digits + dot
		k := 0
		for k < len(s) && (s[k] == '.' || (s[k] >= '0' && s[k] <= '9')) {
			k++
		}
		if k == 0 {
			continue
		}
		var v float64
		var dec float64 = 1
		seenDot := false
		for _, c := range s[:k] {
			if c == '.' {
				seenDot = true
				continue
			}
			d := float64(c - '0')
			if seenDot {
				dec *= 10
				v += d / dec
			} else {
				v = v*10 + d
			}
		}
		if k < len(s) && s[k] == 'x' {
			lastVal = v
		}
		s = s[k:]
	}
}

// recorderMu serializes histogram observations from concurrent task
// goroutines (Prometheus client is thread-safe but we also need to
// observe a single value once per session reliably).
var recorderMu sync.Mutex
