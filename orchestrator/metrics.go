package main

// Prometheus metrics exposed on /metrics for any scraper.

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricWorkers = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scaleplex_orch_workers",
		Help: "Workers known to the orchestrator, by health.",
	}, []string{"healthy"}) // "true" | "false"

	metricWorkerActiveSessions = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "scaleplex_orch_active_sessions",
		Help: "Sum of active_sessions reported across all healthy workers.",
	})

	metricDispatchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "scaleplex_orch_dispatch_total",
		Help: "Task dispatches by outcome.",
	}, []string{"outcome"}) // success | fallthrough_503 | error | no_workers | all_at_cap

	metricDispatchAttempts = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "scaleplex_orch_dispatch_attempts",
		Help:    "Number of workers tried before a successful dispatch (or all-at-cap).",
		Buckets: []float64{1, 2, 3, 4, 5, 8, 12},
	})

	metricKillTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "scaleplex_orch_kill_total",
		Help: "POST /task/<id>/kill outcomes.",
	}, []string{"outcome"}) // success | not_found | error
)

// updateWorkerMetrics is called after each probeAll cycle to keep the
// gauges in sync with the worker pool.
func updateWorkerMetrics() {
	healthy, unhealthy := 0, 0
	totalActive := 0
	for _, w := range pl.list() {
		active, _, _, ok := w.snapshot()
		if ok {
			healthy++
			totalActive += active
		} else {
			unhealthy++
		}
	}
	metricWorkers.WithLabelValues("true").Set(float64(healthy))
	metricWorkers.WithLabelValues("false").Set(float64(unhealthy))
	metricWorkerActiveSessions.Set(float64(totalActive))
}
