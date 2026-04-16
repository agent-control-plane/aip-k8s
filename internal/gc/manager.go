package gc

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// GCManager is a controller-runtime Runnable that runs all GC workers on a fixed interval.
// It relies on controller-manager leader election — mgr.Add(gcManager) ensures only
// the leader replica runs GC. No additional coordination is needed.
type GCManager struct {
	APIReader client.Reader
	Client    client.Client
	Config    GCConfig
	// Now is the clock function. Set to time.Now in production; injectable in tests.
	Now      func() time.Time
	Exporter Exporter // nil means no-op (NoopExporter used)

	mu          sync.RWMutex
	lastCheckIn time.Time
}

// Check implements healthz.Checker.
func (m *GCManager) Check(_ *http.Request) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.Config.Enabled {
		return nil
	}

	// If we haven't checked in for 2 intervals, consider it unhealthy.
	// Add a grace period of 1 minute.
	if !m.lastCheckIn.IsZero() && m.Now().Sub(m.lastCheckIn) > (m.Config.Interval*2+time.Minute) {
		return fmt.Errorf("GC cycle has not run for %v", m.Now().Sub(m.lastCheckIn))
	}
	return nil
}

// Start implements manager.Runnable. Blocks until ctx is cancelled.
// Logs a startup warning if dry-run is disabled (data will be permanently deleted).
func (m *GCManager) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("gc-manager")

	if !m.Config.Enabled {
		logger.Info("GC engine is disabled (--gc-enabled=false); no cleanup will run")
		<-ctx.Done()
		return nil
	}

	m.mu.Lock()
	m.lastCheckIn = m.Now()
	m.mu.Unlock()

	if !m.Config.DryRun {
		logger.Info("WARNING: GC dry-run is disabled — AgentDiagnostics will be permanently deleted after hard TTL",
			"hardTTL", m.Config.DiagnosticHardTTL)
	} else {
		logger.Info("GC engine starting in dry-run mode — no objects will be deleted",
			"interval", m.Config.Interval, "hardTTL", m.Config.DiagnosticHardTTL)
	}

	exporter := m.Exporter
	if exporter == nil {
		exporter = NoopExporter{}
	}

	var pool *ExportPool
	if m.Config.ExportType == "otlp" && m.Config.DiagnosticRetentionTTL > 0 {
		pool = NewExportPool(ctx, m.Config.Concurrency, exporter)
		defer pool.Stop()
		logger.Info("GC export pool started", "concurrency", m.Config.Concurrency,
			"exportType", m.Config.ExportType)
	}

	if m.Config.DiagnosticRetentionTTL > 0 {
		logger.Info("GC soft retention enabled",
			"retentionTTL", m.Config.DiagnosticRetentionTTL,
			"exportType", m.Config.ExportType)
	}

	// Burst must be >= 1; int() truncates so 0.5 → 0 which deadlocks every Wait.
	burst := max(1, int(m.Config.DeleteRatePerSec))
	worker := NewGCWorker(m.APIReader, m.Client, m.Config, m.Now,
		rate.NewLimiter(rate.Limit(m.Config.DeleteRatePerSec), burst), pool)

	ticker := time.NewTicker(m.Config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("GC manager stopping")
			return nil
		case <-ticker.C:
			logger.V(1).Info("GC cycle starting")
			if err := worker.Run(ctx); err != nil {
				// Log and continue — a single cycle failure must not crash the manager.
				logger.Error(err, "GC cycle failed")
			}
			m.mu.Lock()
			m.lastCheckIn = m.Now()
			m.mu.Unlock()
		}
	}
}
