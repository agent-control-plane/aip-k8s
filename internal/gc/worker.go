package gc

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	governancev1alpha1 "github.com/ravisantoshgudimetla/aip-k8s/api/v1alpha1"
)

// +kubebuilder:rbac:groups=governance.aip.io,resources=agentdiagnostics,verbs=get;list;delete

const (
	resourceAgentDiagnostic = "agentdiagnostics"
	exportTypeOTLP          = "otlp"
	exportTypeNone          = "none"
	deleteCallbackTimeout   = 10 * time.Second
	backoffBase             = 5 * time.Second
	backoffMax              = 10 * time.Minute
)

// RateLimiter defines the subset of rate.Limiter needed by the worker.
type RateLimiter interface {
	Wait(ctx context.Context) error
}

// GCWorker runs a single GC scan for AgentDiagnostic resources.
type GCWorker struct {
	APIReader client.Reader
	Client    client.Client
	Config    GCConfig
	Now       func() time.Time
	Limiter   RateLimiter

	// Pool is the async export worker pool. If nil, export is skipped (no-op path).
	Pool *ExportPool
	// retryState tracks per-object export retry info (in-memory only).
	// Key: "<namespace>/<name>"
	retryState map[string]*retryRecord
	mu         sync.Mutex
}

// NewGCWorker initialises a new GCWorker with Phase 2 capabilities.
func NewGCWorker(apiReader client.Reader, c client.Client, cfg GCConfig, now func() time.Time, limiter RateLimiter, pool *ExportPool) *GCWorker {
	return &GCWorker{
		APIReader:  apiReader,
		Client:     c,
		Config:     cfg,
		Now:        now,
		Limiter:    limiter,
		Pool:       pool,
		retryState: make(map[string]*retryRecord),
	}
}

type retryRecord struct {
	attempts    int
	nextRetryAt time.Time
	firstFail   time.Time
}

// Run executes one full scan cycle.
func (w *GCWorker) Run(ctx context.Context) error {
	logger := log.FromContext(ctx).WithValues("resource", resourceAgentDiagnostic)
	start := w.Now()

	var countCheck governancev1alpha1.AgentDiagnosticList
	if err := w.APIReader.List(ctx, &countCheck, client.Limit(int64(w.Config.SafetyMinCount+1))); err != nil {
		return fmt.Errorf("gc safety-valve count: %w", err)
	}
	if len(countCheck.Items) < w.Config.SafetyMinCount && countCheck.Continue == "" {
		logger.Info("Safety valve: total object count below threshold, skipping GC",
			"count", len(countCheck.Items), "threshold", w.Config.SafetyMinCount)
		gcObjectsSkippedTotal.WithLabelValues(resourceAgentDiagnostic, "safety_valve").Inc()
		return nil
	}

	var continueToken string
	deleted, skipped, evaluated := 0, 0, 0

	for {
		var page governancev1alpha1.AgentDiagnosticList
		opts := []client.ListOption{client.Limit(w.Config.PageSize)}
		if continueToken != "" {
			opts = append(opts, client.Continue(continueToken))
		}
		if err := w.APIReader.List(ctx, &page, opts...); err != nil {
			return fmt.Errorf("gc list page: %w", err)
		}

		for i := range page.Items {
			diag := &page.Items[i]
			evaluated++
			gcScanObjectsEvaluatedTotal.WithLabelValues(resourceAgentDiagnostic).Inc()

			res, err := w.processDiagnostic(ctx, diag)
			if err != nil {
				return err
			}
			switch res {
			case resultDeleted:
				deleted++
			case resultSkipped:
				skipped++
			}
		}

		continueToken = page.Continue
		if continueToken == "" {
			break
		}
	}

	duration := w.Now().Sub(start)
	gcScanDurationSeconds.WithLabelValues(resourceAgentDiagnostic).Observe(duration.Seconds())
	logger.Info("GC scan complete", "evaluated", evaluated, "deleted", deleted, "skipped", skipped,
		"dryRun", w.Config.DryRun, "duration", duration.Round(time.Millisecond))
	return nil
}

type processResult int

const (
	resultNone processResult = iota
	resultDeleted
	resultSkipped
)

func (w *GCWorker) processDiagnostic(ctx context.Context, diag *governancev1alpha1.AgentDiagnostic) (processResult, error) {
	logger := log.FromContext(ctx).WithValues("resource", resourceAgentDiagnostic)
	key := fmt.Sprintf("%s/%s", diag.Namespace, diag.Name)
	age := w.Now().Sub(diag.CreationTimestamp.Time)

	// 1. age < retentionWindow (soft TTL)
	if w.Config.DiagnosticRetentionTTL > 0 && age < w.Config.DiagnosticRetentionTTL {
		return resultNone, nil
	}

	// 2. age >= hardTTL
	if age >= w.Config.DiagnosticHardTTL {
		if w.Config.DryRun {
			logger.Info("DRY-RUN: would delete AgentDiagnostic (hard TTL exceeded)",
				"name", diag.Name, "namespace", diag.Namespace, "age", age.Round(time.Second))
			gcObjectsSkippedTotal.WithLabelValues(resourceAgentDiagnostic, "dry_run").Inc()
			return resultSkipped, nil
		}
		if err := w.Limiter.Wait(ctx); err != nil {
			return resultNone, fmt.Errorf("gc rate limiter: %w", err)
		}
		if err := w.Client.Delete(ctx, diag); client.IgnoreNotFound(err) != nil {
			logger.Error(err, "Failed to delete AgentDiagnostic", "name", diag.Name)
			return resultNone, nil
		}
		gcObjectsDeletedTotal.WithLabelValues(resourceAgentDiagnostic, "hard_ttl").Inc()
		w.mu.Lock()
		delete(w.retryState, key)
		w.mu.Unlock()
		return resultDeleted, nil
	}

	// 3. Pool == nil OR Config.ExportType == "none"
	if w.Pool == nil || w.Config.ExportType == exportTypeNone {
		if w.Config.DiagnosticRetentionTTL > 0 && age >= w.Config.DiagnosticRetentionTTL {
			if w.Config.DryRun {
				logger.Info("DRY-RUN: would delete AgentDiagnostic (soft TTL exceeded, no export)",
					"name", diag.Name, "namespace", diag.Namespace, "age", age.Round(time.Second))
				gcObjectsSkippedTotal.WithLabelValues(resourceAgentDiagnostic, "dry_run").Inc()
				return resultSkipped, nil
			}
			if err := w.Limiter.Wait(ctx); err != nil {
				return resultNone, fmt.Errorf("gc rate limiter: %w", err)
			}
			if err := w.Client.Delete(ctx, diag); client.IgnoreNotFound(err) != nil {
				logger.Error(err, "Failed to delete AgentDiagnostic", "name", diag.Name)
				return resultNone, nil
			}
			gcObjectsDeletedTotal.WithLabelValues(resourceAgentDiagnostic, "expired").Inc()
			return resultDeleted, nil
		}
		return resultNone, nil
	}

	// 4. Export path
	if w.Config.DiagnosticRetentionTTL <= 0 {
		return resultNone, nil
	}

	w.mu.Lock()
	if r, exists := w.retryState[key]; exists && w.Now().Before(r.nextRetryAt) {
		w.mu.Unlock()
		gcObjectsSkippedTotal.WithLabelValues(resourceAgentDiagnostic, "export_pending").Inc()
		return resultSkipped, nil
	}
	w.mu.Unlock()

	objToExport := diag.DeepCopy()
	success := w.Pool.Submit(objToExport, func() {
		if w.Config.DryRun {
			return
		}
		delCtx, cancel := context.WithTimeout(context.Background(), deleteCallbackTimeout)
		defer cancel()
		if err := w.Limiter.Wait(delCtx); err != nil {
			logger.Error(err, "Failed to wait for rate limiter in export success callback",
				"name", objToExport.Name, "namespace", objToExport.Namespace)
			return
		}
		if err := w.Client.Delete(delCtx, objToExport); client.IgnoreNotFound(err) != nil {
			logger.Error(err, "Failed to delete AgentDiagnostic in export success callback",
				"name", objToExport.Name, "namespace", objToExport.Namespace)
			return
		}
		gcObjectsDeletedTotal.WithLabelValues(resourceAgentDiagnostic, "expired").Inc()
		w.mu.Lock()
		delete(w.retryState, key)
		w.mu.Unlock()
	}, func(err error) {
		gcExportFailuresTotal.WithLabelValues(resourceAgentDiagnostic).Inc()
		w.mu.Lock()
		r, exists := w.retryState[key]
		if !exists {
			r = &retryRecord{firstFail: w.Now()}
			w.retryState[key] = r
		}
		r.attempts++
		r.nextRetryAt = w.Now().Add(nextBackoff(r.attempts))
		w.mu.Unlock()
	})

	if !success {
		logger.V(1).Info("Export pool full, skipping AgentDiagnostic", "name", diag.Name)
		gcObjectsSkippedTotal.WithLabelValues(resourceAgentDiagnostic, "export_pending").Inc()
		return resultSkipped, nil
	}
	return resultNone, nil
}

func nextBackoff(attempts int) time.Duration {
	shift := min(attempts, 10)
	delay := backoffBase * time.Duration(1<<uint(shift))
	var b [8]byte
	// Errors from rand.Read are ignored as they only affect jitter magnitude,
	// not the core exponential backoff behavior.
	_, _ = rand.Read(b[:])
	f := float64(binary.LittleEndian.Uint64(b[:])&((1<<53)-1)) / float64(1<<53)
	jitter := time.Duration(float64(delay) * 0.2 * (2*f - 1))
	final := max(0, delay+jitter)
	return min(final, backoffMax)
}
