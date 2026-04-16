package gc

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	gcObjectsDeletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aip_gc_objects_deleted_total",
		Help: "Total objects purged by the GC engine.",
	}, []string{"resource", "reason"}) // reason: "hard_ttl", "expired"

	gcObjectsSkippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aip_gc_objects_skipped_total",
		Help: "GC cycles or objects skipped by the GC engine per resource and reason.",
	}, []string{"resource", "reason"}) // reason: "dry_run", "safety_valve", "export_pending"

	gcExportFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aip_gc_export_failures_total",
		Help: "Total export failures by resource type.",
	}, []string{"resource"})

	gcScanDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "aip_gc_scan_duration_seconds",
		Help:    "Duration of a full GC scan cycle per resource type.",
		Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 120, 300},
	}, []string{"resource"})

	gcScanObjectsEvaluatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aip_gc_scan_objects_evaluated_total",
		Help: "Total objects scanned from etcd per GC cycle.",
	}, []string{"resource"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		gcObjectsDeletedTotal,
		gcObjectsSkippedTotal,
		gcExportFailuresTotal,
		gcScanDurationSeconds,
		gcScanObjectsEvaluatedTotal,
	)
}
