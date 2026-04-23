package gc

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

// +kubebuilder:rbac:groups=governance.aip.io,resources=agentrequests,verbs=get;list;delete
// +kubebuilder:rbac:groups=governance.aip.io,resources=auditrecords,verbs=get;list;delete

const resourceAgentRequest = "agentrequests"

// RateLimiter defines the subset of rate.Limiter needed by the worker.
type RateLimiter interface {
	Wait(ctx context.Context) error
}

type processResult int

const (
	resultNone processResult = iota
	resultDeleted
	resultSkipped
)

// ARGCWorker runs GC for AgentRequest resources.
type ARGCWorker struct {
	APIReader client.Reader
	Client    client.Client
	Config    GCConfig
	Now       func() time.Time
	Limiter   RateLimiter
}

// Run executes one full scan cycle for AgentRequests.
func (w *ARGCWorker) Run(ctx context.Context) error {
	if w.Config.HardTTL == 0 {
		return nil
	}

	logger := log.FromContext(ctx).WithValues("resource", resourceAgentRequest)
	start := w.Now()

	// Safety valve: count terminal ARs
	var countList governancev1alpha1.AgentRequestList
	if err := w.APIReader.List(ctx, &countList, client.Limit(int64(w.Config.SafetyMinCount+1))); err != nil {
		return fmt.Errorf("ar gc safety-valve list: %w", err)
	}

	terminalCount := 0
	for _, ar := range countList.Items {
		if isTerminal(ar.Status.Phase) {
			terminalCount++
		}
	}

	if terminalCount < w.Config.SafetyMinCount && countList.Continue == "" {
		logger.Info("Safety valve: terminal AgentRequest count below threshold, skipping GC",
			"count", terminalCount, "threshold", w.Config.SafetyMinCount)
		gcObjectsSkippedTotal.WithLabelValues(resourceAgentRequest, "safety_valve").Inc()
		return nil
	}

	var continueToken string
	deleted, skipped, evaluated := 0, 0, 0

	for {
		var page governancev1alpha1.AgentRequestList
		opts := []client.ListOption{client.Limit(w.Config.PageSize)}
		if continueToken != "" {
			opts = append(opts, client.Continue(continueToken))
		}
		if err := w.APIReader.List(ctx, &page, opts...); err != nil {
			return fmt.Errorf("ar gc list page: %w", err)
		}

		for i := range page.Items {
			ar := &page.Items[i]
			evaluated++
			gcScanObjectsEvaluatedTotal.WithLabelValues(resourceAgentRequest).Inc()

			res, err := w.processRequest(ctx, ar)
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
	gcScanDurationSeconds.WithLabelValues(resourceAgentRequest).Observe(duration.Seconds())
	logger.Info("AR GC scan complete", "evaluated", evaluated, "deleted", deleted, "skipped", skipped,
		"dryRun", w.Config.DryRun, "duration", duration.Round(time.Millisecond))
	return nil
}

func (w *ARGCWorker) processRequest(ctx context.Context, ar *governancev1alpha1.AgentRequest) (processResult, error) {
	if !isTerminal(ar.Status.Phase) {
		return resultNone, nil
	}

	age := w.Now().Sub(ar.CreationTimestamp.Time)
	if age < w.Config.HardTTL {
		return resultNone, nil
	}

	logger := log.FromContext(ctx).WithValues("resource", resourceAgentRequest, "name", ar.Name, "namespace", ar.Namespace)

	if w.Config.DryRun {
		logger.Info("DRY-RUN: would delete AgentRequest (hard TTL exceeded)", "age", age.Round(time.Second))
		gcObjectsSkippedTotal.WithLabelValues(resourceAgentRequest, "dry_run").Inc()
		return resultSkipped, nil
	}

	if err := w.Limiter.Wait(ctx); err != nil {
		return resultNone, fmt.Errorf("ar gc rate limiter: %w", err)
	}

	// Delete associated AuditRecords first (paginated) using the indexed label selector.
	// This prevents an O(N*M) scan of the namespace for every AgentRequest deletion.
	var auditContinue string
	for {
		var auditPage governancev1alpha1.AuditRecordList
		listOpts := []client.ListOption{
			client.InNamespace(ar.Namespace),
			client.MatchingLabels{"aip.io/agentRequestRef": ar.Name},
			client.Limit(w.Config.PageSize),
		}
		if auditContinue != "" {
			listOpts = append(listOpts, client.Continue(auditContinue))
		}
		if err := w.APIReader.List(ctx, &auditPage, listOpts...); err != nil {
			return resultNone, fmt.Errorf("ar gc list auditrecords for %s/%s: %w", ar.Namespace, ar.Name, err)
		}
		for i := range auditPage.Items {
			if err := w.Client.Delete(ctx, &auditPage.Items[i]); client.IgnoreNotFound(err) != nil {
				logger.Error(err, "Failed to delete AuditRecord", "auditName", auditPage.Items[i].Name)
			}
		}
		auditContinue = auditPage.Continue
		if auditContinue == "" {
			break
		}
	}

	if err := w.Client.Delete(ctx, ar); client.IgnoreNotFound(err) != nil {
		logger.Error(err, "Failed to delete AgentRequest")
		return resultNone, nil
	}

	gcObjectsDeletedTotal.WithLabelValues(resourceAgentRequest, "hard_ttl").Inc()
	return resultDeleted, nil
}

func isTerminal(phase string) bool {
	switch phase {
	case governancev1alpha1.PhaseCompleted, governancev1alpha1.PhaseFailed,
		governancev1alpha1.PhaseDenied, governancev1alpha1.PhaseExpired:
		return true
	default:
		return false
	}
}
