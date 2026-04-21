package gc

import "time"

// GCConfig holds GC configuration. Populated from CLI flags in cmd/main.go.
// Only terminal-phase AgentRequests (Completed, Failed, Denied, Expired) are eligible.
// AuditRecords are cascade-deleted automatically via OwnerReferences on a real cluster;
// the GC worker also deletes them explicitly for envtest compatibility.
type GCConfig struct {
	Enabled          bool
	Interval         time.Duration
	DryRun           bool
	HardTTL          time.Duration // 0 means disabled
	PageSize         int64
	DeleteRatePerSec float64
	SafetyMinCount   int // skip GC if terminal AgentRequest count is below this threshold
}

// AgentRequestGCEnabled reports whether AgentRequest GC is active.
// GC is disabled when HardTTL is zero regardless of the Enabled flag.
func (c GCConfig) AgentRequestGCEnabled() bool {
	return c.Enabled && c.HardTTL > 0
}

// DefaultGCConfig returns safe production defaults.
// DryRun defaults to true — operators must explicitly set --gc-dry-run=false.
func DefaultGCConfig() GCConfig {
	return GCConfig{
		Enabled:          false,
		Interval:         time.Hour,
		DryRun:           true,
		HardTTL:          0,
		PageSize:         500,
		DeleteRatePerSec: 100,
		SafetyMinCount:   10,
	}
}
