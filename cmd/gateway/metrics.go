package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	gatewayRequestTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aip_gateway_request_total",
			Help: "Total number of HTTP requests handled by the AIP gateway.",
		},
		[]string{"method", "path", "status_code"},
	)
	gatewayRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aip_gateway_request_duration_seconds",
			Help:    "Duration of HTTP requests handled by the AIP gateway.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"method", "path", "status_code"},
	)

	diagnosticCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aip_diagnostic_created_total",
			Help: "Total number of AgentDiagnostic records created.",
		},
		[]string{"agent_identity"},
	)
	diagnosticDedupTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "aip_diagnostic_dedup_total",
		Help: "Total number of AgentDiagnostic creation requests rejected as duplicates.",
	})
	diagnosticVerdictTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aip_diagnostic_verdict_total",
			Help: "Total number of AgentDiagnostic verdicts recorded, by verdict value.",
		},
		[]string{"verdict"},
	)
)

func init() {
	prometheus.MustRegister(
		gatewayRequestTotal,
		gatewayRequestDuration,
		diagnosticCreatedTotal,
		diagnosticDedupTotal,
		diagnosticVerdictTotal,
	)
}

// metricsMiddleware records request count and duration for every HTTP request.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		status := strconv.Itoa(rw.status)
		path := normalizePath(r)
		gatewayRequestTotal.WithLabelValues(r.Method, path, status).Inc()
		gatewayRequestDuration.WithLabelValues(r.Method, path, status).Observe(time.Since(start).Seconds())
	})
}

// normalizePath returns the matched route pattern when available (Go 1.22+),
// falling back to the raw path. This prevents high-cardinality labels from
// path parameters like /agent-requests/{name}.
func normalizePath(r *http.Request) string {
	if pattern := r.Pattern; pattern != "" {
		return pattern
	}
	return "/__unmatched__"
}

// metricsHandler returns the default Prometheus metrics handler.
func metricsHandler() http.Handler {
	return promhttp.Handler()
}
