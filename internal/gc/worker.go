package gc

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	governancev1alpha1 "github.com/ravisantoshgudimetla/aip-k8s/api/v1alpha1"
)

// +kubebuilder:rbac:groups=governance.aip.io,resources=agentdiagnostics,verbs=get;list;delete

const resourceAgentDiagnostic = "agentdiagnostics"

// RateLimiter defines the subset of rate.Limiter needed by the worker.
type RateLimiter interface {
	Wait(ctx context.Context) error
}

// GCWorker runs a single GC scan for AgentDiagnostic resources.
type GCWorker struct {
	// APIReader is a direct client that bypasses the informer cache.
	// Must be mgr.GetAPIReader() — never mgr.GetClient() — to avoid stale reads.
	APIReader client.Reader
	// Client is used for Delete operations.
	Client  client.Client
	Config  GCConfig
	Now     func() time.Time
	Limiter RateLimiter
}

// Run executes one full scan cycle. It is called by GCManager on every interval tick.
// It is safe to call concurrently but GCManager never does so.
func (w *GCWorker) Run(ctx context.Context) error {
	logger := log.FromContext(ctx).WithValues("resource", resourceAgentDiagnostic)
	start := w.Now()

	// --- Safety valve: count total objects cluster-wide ---
	// List with limit=SafetyMinCount+1 to avoid fetching the full corpus just for counting.
	// If we receive fewer than SafetyMinCount items and no continuation token, skip.
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

	// --- Paginated scan ---
	var continueToken string
	deleted, skipped, evaluated := 0, 0, 0
	hardTTLCutoff := w.Now().Add(-w.Config.DiagnosticHardTTL)

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

			// Hard TTL check: delete if creationTimestamp + hardTTL <= now (equality = expired)
			if diag.CreationTimestamp.After(hardTTLCutoff) {
				// Not yet expired
				continue
			}

			age := w.Now().Sub(diag.CreationTimestamp.Time)

			if w.Config.DryRun {
				logger.Info("DRY-RUN: would delete AgentDiagnostic (hard TTL exceeded)",
					"name", diag.Name, "namespace", diag.Namespace,
					"age", age.Round(time.Second),
					"hardTTL", w.Config.DiagnosticHardTTL)
				gcObjectsSkippedTotal.WithLabelValues(resourceAgentDiagnostic, "dry_run").Inc()
				skipped++
				continue
			}

			// Acquire 1 rate-limiter token before each Delete
			if err := w.Limiter.Wait(ctx); err != nil {
				// ctx cancelled; abort cleanly
				return fmt.Errorf("gc rate limiter: %w", err)
			}

			if err := w.Client.Delete(ctx, diag); client.IgnoreNotFound(err) != nil {
				logger.Error(err, "Failed to delete AgentDiagnostic",
					"name", diag.Name, "namespace", diag.Namespace)
				// Do not return; continue to next object. Log the error and move on.
				continue
			}

			logger.V(1).Info("Deleted AgentDiagnostic",
				"name", diag.Name, "namespace", diag.Namespace, "age", age.Round(time.Second))
			gcObjectsDeletedTotal.WithLabelValues(resourceAgentDiagnostic, "hard_ttl").Inc()
			deleted++
		}

		continueToken = page.Continue
		if continueToken == "" {
			break
		}
	}

	duration := w.Now().Sub(start)
	gcScanDurationSeconds.WithLabelValues(resourceAgentDiagnostic).Observe(duration.Seconds())
	logger.Info("GC scan complete",
		"evaluated", evaluated, "deleted", deleted, "skipped", skipped,
		"dryRun", w.Config.DryRun, "duration", duration.Round(time.Millisecond))
	return nil
}
