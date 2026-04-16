package gc

import "time"

// GCConfig holds all Phase 1 configuration. Populated from CLI flags in cmd/main.go.
// The full gc: YAML shape (retentionDays, export) is defined here as unexported fields
// so it exists in the config struct and can be wired in Phase 2 without a breaking change.
// Phase 1 only reads the fields listed below.
type GCConfig struct {
	Enabled                bool
	Interval               time.Duration
	DryRun                 bool
	DiagnosticHardTTL      time.Duration
	DiagnosticRetentionTTL time.Duration // soft retention window; 0 means disabled (hard TTL only)
	ExportType             string        // "none" or "otlp"; default "none"
	OTLPEndpoint           string        // gRPC endpoint e.g. "otel-collector:4317"
	OTLPInsecure           bool          // if true, use insecure connection for OTLP
	Concurrency            int           // export worker pool size per resource; default 5
	PageSize               int64
	DeleteRatePerSec       float64
	SafetyMinCount         int // skip GC if total object count is below this threshold
}

// DefaultGCConfig returns safe production defaults.
// DryRun defaults to true — operators must explicitly set --gc-dry-run=false.
func DefaultGCConfig() GCConfig {
	return GCConfig{
		Enabled:                false,
		Interval:               time.Hour,
		DryRun:                 true,
		DiagnosticHardTTL:      14 * 24 * time.Hour,
		DiagnosticRetentionTTL: 0,
		ExportType:             "none",
		OTLPEndpoint:           "",
		OTLPInsecure:           false,
		Concurrency:            5,
		PageSize:               500,
		DeleteRatePerSec:       100,
		SafetyMinCount:         10,
	}
}
