/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

const (
	defaultGraduationPolicyName = "default"
)

// AgentTrustProfileReconciler maintains AgentTrustProfile status based on
// DiagnosticAccuracySummary and terminal AgentRequest verdicts.
type AgentTrustProfileReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Clock     func() time.Time
}

// +kubebuilder:rbac:groups=governance.aip.io,resources=agenttrustprofiles,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=governance.aip.io,resources=agenttrustprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=governance.aip.io,resources=agentgraduationpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=governance.aip.io,resources=diagnosticaccuracysummaries,verbs=get;list;watch
// +kubebuilder:rbac:groups=governance.aip.io,resources=agentrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=governance.aip.io,resources=auditrecords,verbs=create

func (r *AgentTrustProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	profileNN := types.NamespacedName{Name: req.Name, Namespace: req.Namespace}
	profile, err := r.getOrBootstrapProfile(ctx, profileNN)
	if err != nil {
		return ctrl.Result{}, err
	}
	if profile.Spec.AgentIdentity == "" {
		return ctrl.Result{}, nil
	}

	agentID := profile.Spec.AgentIdentity
	ns := profile.Namespace

	// Read DiagnosticAccuracySummary.
	var summary governancev1alpha1.DiagnosticAccuracySummary
	summaryNN := types.NamespacedName{Name: summaryNameForAgent(agentID), Namespace: ns}
	var hasSummary bool
	if err := r.Get(ctx, summaryNN, &summary); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else {
		hasSummary = true
	}

	// Read the graduation policy.
	var policy governancev1alpha1.AgentGraduationPolicy
	policyNN := types.NamespacedName{Name: defaultGraduationPolicyName, Namespace: ns}
	levelFound := true
	if err := r.Get(ctx, policyNN, &policy); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		levelFound = false
	}

	// Compute recentAccuracy from the evaluation window.
	var recentAccuracy float64
	var totalReviewed int64
	if hasSummary && summary.Status.DiagnosticAccuracy != nil {
		recentAccuracy = *summary.Status.DiagnosticAccuracy
		totalReviewed = summary.Status.TotalReviewed
	}

	// If we have a policy with an evaluation window, compute rolling accuracy.
	if levelFound && len(policy.Spec.Levels) > 0 && policy.Spec.EvaluationWindow.Count > 0 {
		ra, tr, err := r.computeRollingAccuracy(ctx, ns, agentID, policy.Spec.EvaluationWindow.Count)
		if err != nil {
			logger.Error(err, "Failed to compute rolling accuracy, falling back to all-time")
		} else if ra != nil {
			recentAccuracy = *ra
			totalReviewed = tr
		}
	}

	// Compute a separate rolling accuracy for demotion evaluation using DemotionPolicy.WindowSize.
	// Falls back to recentAccuracy (EvaluationWindow) if WindowSize is not set.
	demotionAccuracy := r.computeDemotionAccuracy(ctx, ns, agentID, recentAccuracy, levelFound, policy, logger)

	// Count terminal executions.
	totalExecutions, successRate, err := r.countTerminalExecutions(ctx, ns, agentID)
	if err != nil {
		logger.Error(err, "Failed to count terminal executions")
	}

	// Resolve trust level.
	newLevel := r.resolveTrustLevel(recentAccuracy, totalExecutions, policy)
	oldLevel := profile.Status.TrustLevel
	if oldLevel == "" {
		oldLevel = governancev1alpha1.TrustLevelObserver
	}

	// Guard demotions behind grace period and accuracy-band threshold.
	// Promotions are always applied immediately.
	demoted := false
	if levelFound {
		newRank, _ := governancev1alpha1.TrustLevelRank(newLevel)
		oldRank, _ := governancev1alpha1.TrustLevelRank(oldLevel)
		if newRank < oldRank {
			if r.checkDemotion(&profile, oldLevel, demotionAccuracy, policy) {
				demoted = true
			} else {
				newLevel = oldLevel // grace period active — hold at current level
			}
		}
	}

	// Build the new status.
	now := metav1.NewTime(r.now())
	base := profile.DeepCopy()
	profile.Status.TrustLevel = newLevel
	profile.Status.DiagnosticAccuracy = summary.Status.DiagnosticAccuracy
	profile.Status.RecentAccuracy = &recentAccuracy
	profile.Status.TotalReviewed = totalReviewed
	profile.Status.TotalExecutions = totalExecutions
	profile.Status.SuccessRate = successRate
	profile.Status.LastEvaluatedAt = &now

	if newLevel != oldLevel {
		newRank, _ := governancev1alpha1.TrustLevelRank(newLevel)
		oldRank, _ := governancev1alpha1.TrustLevelRank(oldLevel)
		if newRank > oldRank {
			profile.Status.LastPromotedAt = &now
		} else {
			profile.Status.LastDemotedAt = &now
		}
	}

	if err := r.Status().Patch(ctx, &profile, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	// Emit audit record if trust level changed.
	if newLevel != oldLevel {
		if err := r.emitTrustProfileAuditWithRetry(ctx, &profile, oldLevel, newLevel, demoted); err != nil {
			logger.Error(err, "Failed to emit trust profile audit record after retries")
		}
	}

	return ctrl.Result{}, nil
}

// computeDemotionAccuracy computes accuracy over DemotionPolicy.WindowSize for demotion
// evaluation. Falls back to recentAccuracy if WindowSize is unset or the computation fails.
// getOrBootstrapProfile finds the AgentTrustProfile by name or bootstraps
// a new one from DiagnosticAccuracySummary or terminal AgentRequests.
func (r *AgentTrustProfileReconciler) getOrBootstrapProfile(
	ctx context.Context, profileNN types.NamespacedName,
) (governancev1alpha1.AgentTrustProfile, error) {
	var profile governancev1alpha1.AgentTrustProfile
	if err := r.Get(ctx, profileNN, &profile); err != nil {
		if !errors.IsNotFound(err) {
			return profile, err
		}

		var agentID string
		var summary governancev1alpha1.DiagnosticAccuracySummary
		if err := r.Get(ctx, profileNN, &summary); err != nil {
			if !errors.IsNotFound(err) {
				return profile, err
			}

			var arList governancev1alpha1.AgentRequestList
			if err := r.List(ctx, &arList,
				client.InNamespace(profileNN.Namespace),
				client.MatchingLabels{"aip.io/profileName": profileNN.Name},
			); err != nil {
				return profile, err
			}
			if len(arList.Items) == 0 {
				var allARs governancev1alpha1.AgentRequestList
				if err := r.List(ctx, &allARs, client.InNamespace(profileNN.Namespace)); err != nil {
					return profile, err
				}
				for _, ar := range allARs.Items {
					if governancev1alpha1.ProfileNameForAgent(ar.Spec.AgentIdentity) == profileNN.Name {
						agentID = ar.Spec.AgentIdentity
						break
					}
				}
				if agentID == "" {
					return profile, nil
				}
			} else {
				agentID = arList.Items[0].Spec.AgentIdentity
			}
		} else {
			agentID = summary.Spec.AgentIdentity
		}

		log.FromContext(ctx).Info("Bootstrapping AgentTrustProfile", "agentIdentity", agentID)
		profile = governancev1alpha1.AgentTrustProfile{
			ObjectMeta: metav1.ObjectMeta{
				Name:      profileNN.Name,
				Namespace: profileNN.Namespace,
			},
			Spec: governancev1alpha1.AgentTrustProfileSpec{
				AgentIdentity: agentID,
			},
		}
		if err := r.Create(ctx, &profile); err != nil {
			if !errors.IsAlreadyExists(err) {
				return profile, err
			}
		}
		// Bypass cache — informer may not have the object yet.
		if err := r.APIReader.Get(ctx, profileNN, &profile); err != nil {
			return profile, err
		}
	}
	return profile, nil
}

func (r *AgentTrustProfileReconciler) computeDemotionAccuracy(
	ctx context.Context,
	ns, agentID string,
	recentAccuracy float64,
	levelFound bool,
	policy governancev1alpha1.AgentGraduationPolicy,
	logger interface{ Error(error, string, ...any) },
) float64 {
	if !levelFound || policy.Spec.DemotionPolicy.WindowSize <= 0 {
		return recentAccuracy
	}
	da, _, err := r.computeRollingAccuracy(ctx, ns, agentID, policy.Spec.DemotionPolicy.WindowSize)
	if err != nil {
		logger.Error(err, "Failed to compute demotion rolling accuracy, falling back to evaluation window")
		return recentAccuracy
	}
	if da == nil {
		return recentAccuracy
	}
	return *da
}

// computeRollingAccuracy reads the last N verdicted AuditRecords and computes accuracy.
func (r *AgentTrustProfileReconciler) computeRollingAccuracy(ctx context.Context, ns, agentID string, count int64) (*float64, int64, error) {
	var auditList governancev1alpha1.AuditRecordList
	if err := r.List(ctx, &auditList, client.InNamespace(ns), client.MatchingLabels{"aip.io/agentIdentity": agentID}); err != nil {
		return nil, 0, err
	}

	// Filter and sort by Timestamp descending.
	type verdictEntry struct {
		verdict    string
		reasonCode string
		timestamp  *metav1.Time
	}
	entries := make([]verdictEntry, 0, len(auditList.Items))
	skipReasons := map[string]bool{
		"bad_timing": true, "scope_too_broad": true,
		"precautionary": true, "policy_block": true,
	}

	for _, a := range auditList.Items {
		if a.Spec.Event != governancev1alpha1.AuditEventVerdictSubmitted {
			continue
		}
		verdict := a.Spec.Annotations["verdict"]
		reasonCode := a.Spec.Annotations["verdictReasonCode"]

		if skipReasons[reasonCode] {
			continue
		}
		entries = append(entries, verdictEntry{
			verdict:    verdict,
			reasonCode: reasonCode,
			timestamp:  &a.Spec.Timestamp,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].timestamp == nil && entries[j].timestamp == nil {
			return false
		}
		if entries[i].timestamp == nil {
			return false
		}
		if entries[j].timestamp == nil {
			return true
		}
		return entries[i].timestamp.After(entries[j].timestamp.Time)
	})

	if int64(len(entries)) > count {
		entries = entries[:count]
	}

	if len(entries) == 0 {
		return nil, 0, nil
	}

	var correct, partial, total int64
	for _, e := range entries {
		total++
		switch e.verdict {
		case verdictCorrect:
			correct++
		case verdictPartial:
			partial++
		}
	}

	acc := float64(correct) + 0.5*float64(partial)
	if total > 0 {
		val := acc / float64(total)
		return &val, total, nil
	}
	return nil, 0, nil
}

// countTerminalExecutions counts Completed and Failed AgentRequests for the agent.
// Lists all ARs and filters by Spec.AgentIdentity so that pre-existing ARs
// without the aip.io/agentIdentity label are still counted correctly.
func (r *AgentTrustProfileReconciler) countTerminalExecutions(ctx context.Context, ns, agentID string) (int64, *float64, error) {
	var list governancev1alpha1.AgentRequestList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return 0, nil, err
	}

	var total, completed int64
	for _, ar := range list.Items {
		if ar.Spec.AgentIdentity != agentID {
			continue
		}
		if ar.Status.Phase == governancev1alpha1.PhaseCompleted ||
			ar.Status.Phase == governancev1alpha1.PhaseFailed {

			// Only count requests that actually reached the Executing state
			// (excludes Observer/AwaitingVerdict grading-only requests)
			if meta.IsStatusConditionTrue(ar.Status.Conditions, governancev1alpha1.ConditionExecuting) {
				total++
				if ar.Status.Phase == governancev1alpha1.PhaseCompleted {
					completed++
				}
			}
		}
	}

	if total == 0 {
		return 0, nil, nil
	}
	rate := float64(completed) / float64(total)
	return total, &rate, nil
}

// resolveTrustLevel picks the highest level where both accuracy and executions meet the minimum.
func (r *AgentTrustProfileReconciler) resolveTrustLevel(recentAccuracy float64, totalExecutions int64, policy governancev1alpha1.AgentGraduationPolicy) string {
	if !policyFound(policy) {
		return governancev1alpha1.TrustLevelObserver
	}

	// Iterate from highest to lowest.
	for i := len(policy.Spec.Levels) - 1; i >= 0; i-- {
		level := policy.Spec.Levels[i]
		if levelMeets(level, recentAccuracy, totalExecutions) {
			return level.Name
		}
	}
	return governancev1alpha1.TrustLevelObserver
}

// checkDemotion returns true if the agent should be demoted from its current level.
func (r *AgentTrustProfileReconciler) checkDemotion(profile *governancev1alpha1.AgentTrustProfile, oldLevel string, recentAccuracy float64, policy governancev1alpha1.AgentGraduationPolicy) bool {
	// 1. Evaluate GracePeriod if configured
	if policy.Spec.DemotionPolicy.GracePeriod != "" {
		grace, err := time.ParseDuration(policy.Spec.DemotionPolicy.GracePeriod)
		if err != nil {
			log.Log.Error(err, "AgentGraduationPolicy has unparseable DemotionPolicy.GracePeriod — grace period skipped",
				"policy", policy.Name, "value", policy.Spec.DemotionPolicy.GracePeriod)
		} else {
			lastPromoted := profile.Status.LastPromotedAt
			if lastPromoted == nil {
				// If never promoted, we use creation timestamp as a fallback
				lastPromoted = &metav1.Time{Time: profile.CreationTimestamp.Time}
			}
			if r.now().Before(lastPromoted.Add(grace)) {
				// Still within grace period, skip demotion
				return false
			}
		}
	}

	// 2. Evaluate accuracy-band thresholds. DemotionBuffer (per-level) takes precedence
	// over AccuracyDropThreshold (global fallback from DemotionPolicy).
	for _, level := range policy.Spec.Levels {
		if level.Name != oldLevel || level.Accuracy == nil || level.Accuracy.Min == nil {
			continue
		}
		buffer := policy.Spec.DemotionPolicy.AccuracyDropThreshold
		if level.Accuracy.DemotionBuffer != nil {
			buffer = *level.Accuracy.DemotionBuffer
		}
		return recentAccuracy < *level.Accuracy.Min-buffer
	}
	return false
}

func (r *AgentTrustProfileReconciler) emitTrustProfileAuditWithRetry(ctx context.Context, profile *governancev1alpha1.AgentTrustProfile, oldLevel, newLevel string, demoted bool) error {
	eventType := governancev1alpha1.AuditEventTrustProfileUpdated
	now := r.now()
	audit := &governancev1alpha1.AuditRecord{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deterministicAuditName(profile.Name, eventType, oldLevel, newLevel),
			Namespace: profile.Namespace,
			Labels: map[string]string{
				"aip.io/agentIdentity": profile.Spec.AgentIdentity,
			},
			Annotations: map[string]string{
				"governance.aip.io/agentTrustProfileRef": profile.Name,
			},
		},
		Spec: governancev1alpha1.AuditRecordSpec{
			Timestamp:       metav1.NewTime(now),
			AgentIdentity:   profile.Spec.AgentIdentity,
			AgentRequestRef: "-",
			Event:           eventType,
			Action:          "trust-evaluation",
			TargetURI:       "agent-trust-profile",
			Reason:          fmt.Sprintf("Trust level changed from %s to %s", oldLevel, newLevel),
			Annotations: map[string]string{
				"oldLevel": oldLevel,
				"newLevel": newLevel,
				"demoted":  fmt.Sprintf("%t", demoted),
			},
		},
	}
	if err := ctrl.SetControllerReference(profile, audit, r.Scheme); err != nil {
		log.FromContext(ctx).Error(err, "Failed to set owner reference for trust profile AuditRecord")
	}
	var lastErr error
	for range 3 {
		if err := r.Create(ctx, audit); err == nil || errors.IsAlreadyExists(err) {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func (r *AgentTrustProfileReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func policyFound(policy governancev1alpha1.AgentGraduationPolicy) bool {
	return policy.Name != "" && len(policy.Spec.Levels) > 0
}

func levelMeets(level governancev1alpha1.GraduationLevel, accuracy float64, executions int64) bool {
	if level.Accuracy != nil && level.Accuracy.Min != nil {
		if accuracy < *level.Accuracy.Min {
			return false
		}
	}
	if level.Executions != nil && level.Executions.Min != nil {
		if executions < *level.Executions.Min {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentTrustProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watch DiagnosticAccuracySummary updates and terminal AgentRequest phase changes.
	dasPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		das, ok := obj.(*governancev1alpha1.DiagnosticAccuracySummary)
		if !ok || das.Status.DiagnosticAccuracy == nil {
			return false
		}
		return true
	})

	arPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		ar, ok := obj.(*governancev1alpha1.AgentRequest)
		if !ok {
			return false
		}
		return ar.Status.Phase == governancev1alpha1.PhaseCompleted ||
			ar.Status.Phase == governancev1alpha1.PhaseFailed ||
			ar.Status.Phase == governancev1alpha1.PhaseDenied
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&governancev1alpha1.AgentTrustProfile{}).
		Watches(&governancev1alpha1.DiagnosticAccuracySummary{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				das := obj.(*governancev1alpha1.DiagnosticAccuracySummary)
				profileName := summaryNameForAgent(das.Spec.AgentIdentity)
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Name:      profileName,
						Namespace: das.Namespace,
					},
				}}
			}),
			builder.WithPredicates(dasPredicate),
		).
		Watches(&governancev1alpha1.AgentRequest{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				ar := obj.(*governancev1alpha1.AgentRequest)
				profileName := summaryNameForAgent(ar.Spec.AgentIdentity)
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Name:      profileName,
						Namespace: ar.Namespace,
					},
				}}
			}),
			builder.WithPredicates(arPredicate),
		).
		Watches(&governancev1alpha1.AgentGraduationPolicy{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				// The policy name must be "default".
				if obj.GetName() != defaultGraduationPolicyName {
					return nil
				}
				var profiles governancev1alpha1.AgentTrustProfileList
				if err := mgr.GetClient().List(ctx, &profiles, client.InNamespace(obj.GetNamespace())); err != nil {
					return nil
				}
				var reqs []reconcile.Request
				for _, p := range profiles.Items {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      p.Name,
							Namespace: p.Namespace,
						},
					})
				}
				return reqs
			}),
		).
		Named("agenttrustprofile").
		Complete(r)
}
