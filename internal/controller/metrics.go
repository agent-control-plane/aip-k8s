package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// AgentRequest lifecycle metrics

	agentRequestTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aip_agentrequest_total",
			Help: "Total number of AgentRequest phase transitions.",
		},
		[]string{"phase"},
	)

	agentRequestDeniedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aip_agentrequest_denied_total",
			Help: "Total number of AgentRequests denied, by denial reason code.",
		},
		[]string{"code"},
	)

	agentRequestEvalDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "aip_agentrequest_evaluation_duration_seconds",
		Help:    "Duration of SafetyPolicy evaluation for AgentRequests.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})

	agentRequestActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "aip_agentrequest_active",
		Help: "Number of AgentRequests currently in-flight (not yet in a terminal phase).",
	})

	// OpsLock metrics

	opsLockContentionTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "aip_opslock_contention_total",
		Help: "Total number of OpsLock acquisition failures due to contention or timeout.",
	})

	opsLockExpiredTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "aip_opslock_expired_total",
		Help: "Total number of OpsLock TTL expirations detected during execution.",
	})

	opsLockActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "aip_opslock_active",
		Help: "Number of OpsLocks currently held.",
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		agentRequestTotal,
		agentRequestDeniedTotal,
		agentRequestEvalDuration,
		agentRequestActive,
		opsLockContentionTotal,
		opsLockExpiredTotal,
		opsLockActive,
	)
}
