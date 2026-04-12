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
	"time"

	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"encoding/json"

	governancev1alpha1 "github.com/ravisantoshgudimetla/aip-k8s/api/v1alpha1"
	"github.com/ravisantoshgudimetla/aip-k8s/internal/evaluation"
	"github.com/ravisantoshgudimetla/aip-k8s/internal/evaluation/fetchers"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
)

// AgentRequestReconciler reconciles a AgentRequest object
type AgentRequestReconciler struct {
	client.Client
	// APIReader is a direct (non-cached) API server reader used for the initial
	// Get in Reconcile. This ensures we always work with the latest resourceVersion
	// and avoids 409 conflicts caused by stale informer cache reads.
	APIReader            client.Reader
	Scheme               *runtime.Scheme
	Clock                func() time.Time // injectable for testing; defaults to time.Now
	Evaluator            evaluation.Evaluator
	TargetContextFetcher evaluation.TargetContextFetcher
}

func (r *AgentRequestReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=governance.aip.io,resources=agentrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=governance.aip.io,resources=agentrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=governance.aip.io,resources=agentrequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=governance.aip.io,resources=safetypolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=governance.aip.io,resources=auditrecords,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=governance.aip.io,resources=auditrecords/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=governance.aip.io,resources=governedresources,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.sh,resources=nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

func (r *AgentRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	var agentReq governancev1alpha1.AgentRequest
	// Use the direct API server reader (bypass cache) to get the authoritative
	// resourceVersion. This prevents 409 conflicts when the informer cache lags
	// behind a recent spec or status update.
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	if err := reader.Get(ctx, req.NamespacedName, &agentReq); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Snapshot the object state for merge patches. All status writes in this
	// reconcile use Patch (not Update) so they apply cleanly even when a
	// concurrent spec write (e.g. dashboard setting humanApproval) has changed
	// the resourceVersion since our Get.

	if agentReq.Status.Phase == governancev1alpha1.PhaseCompleted ||
		agentReq.Status.Phase == governancev1alpha1.PhaseFailed ||
		agentReq.Status.Phase == governancev1alpha1.PhaseDenied {
		return ctrl.Result{}, nil
	}

	// 1.1 GovernedResource Integrity Check
	if agentReq.Spec.GovernedResourceRef != nil {
		if err := r.checkGovernedResourceIntegrity(ctx, &agentReq); err != nil {
			return ctrl.Result{}, err
		}
		// If it denied the request, status was updated and audit records emitted.
		// Return early to exit this reconcile.
		if agentReq.Status.Phase == governancev1alpha1.PhaseDenied {
			return ctrl.Result{}, nil
		}
	}

	// 2. Check for Agent-triggered transitions
	handled, err := r.checkAgentTransitions(ctx, &agentReq)
	if err != nil {
		return ctrl.Result{}, err
	}
	if handled {
		return ctrl.Result{}, nil
	}

	// 3. Initialize Phase if empty
	if agentReq.Status.Phase == "" && !meta.IsStatusConditionTrue(agentReq.Status.Conditions, "RequestSubmitted") {
		log.FromContext(ctx).Info("Initializing AgentRequest phase to Pending", "name", agentReq.Name)
		base := agentReq.DeepCopy()
		agentReq.Status.Phase = governancev1alpha1.PhasePending
		agentRequestActive.Inc()
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhasePending).Inc()

		// Mark as submitted to avoid double auditing if the reconcile is re-triggered
		// before the phase update is fully visible.
		meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
			Type:    "RequestSubmitted",
			Status:  metav1.ConditionTrue,
			Reason:  "Initialization",
			Message: "Initial phase set to Pending",
		})

		if err := r.Status().Patch(ctx, &agentReq, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		// Emit initial AuditRecord. We return here so subsequent Reconcile will
		// enter the state machine with Phase=Pending.
		if err := r.emitAuditRecord(ctx, &agentReq, governancev1alpha1.AuditEventRequestSubmitted, "", governancev1alpha1.PhasePending); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 4. State Machine Evaluation
	switch agentReq.Status.Phase {
	case governancev1alpha1.PhasePending:
		return r.reconcilePending(ctx, &agentReq)
	case governancev1alpha1.PhaseApproved:
		return r.reconcileApproved(ctx, &agentReq)
	case governancev1alpha1.PhaseExecuting:
		return r.reconcileExecuting(ctx, &agentReq)
	}

	return ctrl.Result{}, nil
}

func (r *AgentRequestReconciler) checkGovernedResourceIntegrity(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) error {
	var gr governancev1alpha1.GovernedResource
	if err := r.Get(ctx, types.NamespacedName{Name: agentReq.Spec.GovernedResourceRef.Name}, &gr); err != nil {
		if errors.IsNotFound(err) {
			log.FromContext(ctx).Info("Denying AgentRequest because its GovernedResource was deleted", "name", agentReq.Name, "governedResource", agentReq.Spec.GovernedResourceRef.Name)
			fromPhase := agentReq.Status.Phase
			base := agentReq.DeepCopy()
			agentReq.Status.Phase = governancev1alpha1.PhaseDenied
			agentReq.Status.Denial = &governancev1alpha1.DenialResponse{
				Code:    governancev1alpha1.DenialCodeGovernedResourceDeleted,
				Message: fmt.Sprintf("The GovernedResource %q that admitted this request was deleted.", agentReq.Spec.GovernedResourceRef.Name),
			}
			agentRequestDeniedTotal.WithLabelValues(governancev1alpha1.DenialCodeGovernedResourceDeleted).Inc()
			agentRequestActive.Dec()
			agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseDenied).Inc()
			if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
				return err
			}
			if err := r.releaseLock(ctx, agentReq); err != nil {
				return err
			}
			if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestDenied, fromPhase, governancev1alpha1.PhaseDenied); err != nil {
				return err
			}
			return nil
		}
		return err
	}

	// Deny if the GovernedResource has been recreated or mutated (generation mismatch).
	if gr.Generation != agentReq.Spec.GovernedResourceRef.Generation {
		log.FromContext(ctx).Info("Denying AgentRequest due to GovernedResource generation mismatch",
			"name", agentReq.Name,
			"expectedGeneration", agentReq.Spec.GovernedResourceRef.Generation,
			"currentGeneration", gr.Generation)
		fromPhase := agentReq.Status.Phase
		base := agentReq.DeepCopy()
		agentReq.Status.Phase = governancev1alpha1.PhaseDenied
		agentReq.Status.Denial = &governancev1alpha1.DenialResponse{
			Code: governancev1alpha1.DenialCodeGenerationMismatch,
			Message: fmt.Sprintf(
				"GovernedResource %q generation has changed (expected %d, current %d); the policy binding is no longer valid.",
				agentReq.Spec.GovernedResourceRef.Name,
				agentReq.Spec.GovernedResourceRef.Generation,
				gr.Generation,
			),
		}
		agentRequestDeniedTotal.WithLabelValues(governancev1alpha1.DenialCodeGenerationMismatch).Inc()
		agentRequestActive.Dec()
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseDenied).Inc()
		if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
			return err
		}
		if err := r.releaseLock(ctx, agentReq); err != nil {
			return err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestDenied, fromPhase, governancev1alpha1.PhaseDenied); err != nil {
			return err
		}
		return nil
	}

	return nil
}

func (r *AgentRequestReconciler) checkAgentTransitions(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) (bool, error) {
	// Agent completed successfully
	if meta.IsStatusConditionTrue(agentReq.Status.Conditions, governancev1alpha1.ConditionCompleted) && agentReq.Status.Phase != governancev1alpha1.PhaseCompleted {
		fromPhase := agentReq.Status.Phase
		base := agentReq.DeepCopy()
		agentReq.Status.Phase = governancev1alpha1.PhaseCompleted
		agentRequestActive.Dec()
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseCompleted).Inc()
		if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
			return false, err
		}
		if err := r.releaseLock(ctx, agentReq); err != nil {
			return false, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestCompleted, fromPhase, governancev1alpha1.PhaseCompleted); err != nil {
			return false, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventLockReleased, "", ""); err != nil {
			return false, err
		}
		return true, nil
	}

	// Agent failed
	if meta.IsStatusConditionTrue(agentReq.Status.Conditions, governancev1alpha1.ConditionFailed) && agentReq.Status.Phase != governancev1alpha1.PhaseFailed {
		fromPhase := agentReq.Status.Phase
		base := agentReq.DeepCopy()
		agentReq.Status.Phase = governancev1alpha1.PhaseFailed
		agentRequestActive.Dec()
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseFailed).Inc()
		if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
			return false, err
		}
		if err := r.releaseLock(ctx, agentReq); err != nil {
			return false, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestFailed, fromPhase, governancev1alpha1.PhaseFailed); err != nil {
			return false, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventLockReleased, "", ""); err != nil {
			return false, err
		}
		return true, nil
	}

	// Agent signals it started executing
	if meta.IsStatusConditionTrue(agentReq.Status.Conditions, governancev1alpha1.ConditionExecuting) && agentReq.Status.Phase == governancev1alpha1.PhaseApproved {
		fromPhase := agentReq.Status.Phase
		base := agentReq.DeepCopy()
		agentReq.Status.Phase = governancev1alpha1.PhaseExecuting
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseExecuting).Inc()
		if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
			return false, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventLockAcquired, "", ""); err != nil {
			return false, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestExecuting, fromPhase, governancev1alpha1.PhaseExecuting); err != nil {
			return false, err
		}
		return true, nil
	}

	return false, nil
}

func generateLeaseName(targetURI string) string {
	hash := sha256.Sum256([]byte(targetURI))
	hexHash := hex.EncodeToString(hash[:])

	name := fmt.Sprintf("aip-lock-%s", hexHash)
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

func matchesSelector(grLabels map[string]string, policy *governancev1alpha1.SafetyPolicy) bool {
	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.GovernedResourceSelector)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(grLabels))
}

func (r *AgentRequestReconciler) reconcilePending(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	policyEvaluated := meta.IsStatusConditionTrue(agentReq.Status.Conditions, governancev1alpha1.ConditionPolicyEvaluated)
	requiresApproval := meta.IsStatusConditionTrue(agentReq.Status.Conditions, governancev1alpha1.ConditionRequiresApproval)
	hasHumanApproval := agentReq.Spec.HumanApproval != nil
	logger.Info("reconcilePending", "name", agentReq.Name, "policyEvaluated", policyEvaluated, "requiresApproval", requiresApproval, "hasHumanApproval", hasHumanApproval)

	// Guard: If policies are already evaluated, skip to lock acquisition or wait for approval
	if policyEvaluated {
		// If it's blocked on approval, check spec for a human decision
		if requiresApproval {
			return r.handleApprovalWait(ctx, agentReq)
		}
		// Otherwise, it must have been an "Allow" result and we're now in lock acquisition mode
		fromPhase := agentReq.Status.Phase // Should be Pending
		return r.handleLockAcquisition(ctx, agentReq, fromPhase)
	}

	logger.Info("Evaluating policies for AgentRequest", "name", agentReq.Name, "generation", agentReq.Generation)

	var policyList governancev1alpha1.SafetyPolicyList
	// Use APIReader if available to avoid Informer cache lag for policies
	// just applied by demo scripts.
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	if err := reader.List(ctx, &policyList, client.InNamespace(agentReq.Namespace)); err != nil {
		logger.Error(err, "Failed to list SafetyPolicies")
		return ctrl.Result{}, err
	}

	var (
		hasGovernedResource bool
		grLabels            map[string]string
	)
	if agentReq.Spec.GovernedResourceRef != nil {
		var gr governancev1alpha1.GovernedResource
		if err := reader.Get(ctx, types.NamespacedName{Name: agentReq.Spec.GovernedResourceRef.Name}, &gr); err != nil {
			logger.Error(err, "Failed to get GovernedResource for policy matching", "name", agentReq.Spec.GovernedResourceRef.Name)
			return ctrl.Result{}, err
		}
		hasGovernedResource = true
		grLabels = gr.Labels
		if grLabels == nil {
			grLabels = map[string]string{}
		}
	}

	evalOpts := []governancev1alpha1.SafetyPolicy{}
	for _, p := range policyList.Items {
		// Evaluate policy if:
		// - There's no GovernedResource (global policy evaluation), OR
		// - There's a GovernedResource and it matches the policy's selector
		if !hasGovernedResource || matchesSelector(grLabels, &p) {
			evalOpts = append(evalOpts, p)
		}
	}

	targetCtx, cascadeCtxs, err := r.fetchTargetContext(ctx, agentReq)
	if err != nil {
		logger.Error(err, "Failed to fetch target context, proceeding with empty context")
	}

	// Step 1 - Fetch Provider Context based on GovernedResource
	if agentReq.Spec.GovernedResourceRef != nil {
		var gr governancev1alpha1.GovernedResource
		if err := reader.Get(ctx, types.NamespacedName{Name: agentReq.Spec.GovernedResourceRef.Name}, &gr); err != nil {
			logger.Error(err, "Failed to get GovernedResource for provider context", "name", agentReq.Spec.GovernedResourceRef.Name)
			return ctrl.Result{}, err
		}
		providerCtx, err := r.fetchContextFromProvider(ctx, agentReq, &gr)
		if err != nil {
			logger.Error(err, "Provider context fetch failed")
		} else if providerCtx != nil {
			agentReq.Status.ProviderContext = providerCtx
		}
	}

	// Persist what the control plane verified so the dashboard can show
	// agent-declared vs control-plane-verified side by side.
	if targetCtx != nil {
		agentReq.Status.ControlPlaneVerification = &governancev1alpha1.ControlPlaneVerification{
			TargetExists:        targetCtx.Exists,
			HasActiveEndpoints:  targetCtx.HasActiveEndpoints,
			ActiveEndpointCount: targetCtx.ActiveEndpointCount,
			ReadyReplicas:       targetCtx.ReadyReplicas,
			SpecReplicas:        targetCtx.SpecReplicas,
			DownstreamServices:  targetCtx.DownstreamServices,
			FetchedAt:           metav1.NewTime(r.now()),
		}
	}

	evalTimer := prometheus.NewTimer(agentRequestEvalDuration)
	result, err := r.Evaluator.Evaluate(ctx, agentReq, evalOpts, targetCtx, cascadeCtxs)
	evalTimer.ObserveDuration()
	if err != nil {
		logger.Error(err, "Evaluation failed")
		return ctrl.Result{}, err
	}
	logger.Info("Evaluation complete", "name", agentReq.Name, "action", result.Action, "message", result.Message)

	auditEvals := []governancev1alpha1.AuditPolicyEvaluation{}
	for _, pr := range result.PolicyResults {
		auditEvals = append(auditEvals, governancev1alpha1.AuditPolicyEvaluation{
			PolicyName: pr.PolicyName,
			RuleName:   pr.RuleName,
			Result:     pr.Result,
		})
	}

	policyEvalAudit := &governancev1alpha1.AuditRecord{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: agentReq.Name + "-audit-",
			Namespace:    agentReq.Namespace,
		},
		Spec: governancev1alpha1.AuditRecordSpec{
			Timestamp:         metav1.NewTime(r.now()),
			AgentIdentity:     agentReq.Spec.AgentIdentity,
			AgentRequestRef:   agentReq.Name,
			Event:             governancev1alpha1.AuditEventPolicyEvaluated,
			Action:            agentReq.Spec.Action,
			TargetURI:         agentReq.Spec.Target.URI,
			Reason:            agentReq.Spec.Reason,
			PolicyEvaluations: auditEvals,
		},
	}

	if err := ctrl.SetControllerReference(agentReq, policyEvalAudit, r.Scheme); err != nil {
		logger.Error(err, "Failed to set owner reference for policy.evaluated AuditRecord")
	}

	return r.handleEvaluationResult(ctx, agentReq, result, policyEvalAudit)
}

func (r *AgentRequestReconciler) handleApprovalWait(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if agentReq.Spec.HumanApproval != nil {
		fromPhase := agentReq.Status.Phase

		// 5. If same (or Re-eval returned Allow), proceed
		switch agentReq.Spec.HumanApproval.Decision {
		case "approved":
			logger.Info("Human approved AgentRequest via spec", "name", agentReq.Name)
			meta.RemoveStatusCondition(&agentReq.Status.Conditions, governancev1alpha1.ConditionRequiresApproval)
			return r.handleLockAcquisition(ctx, agentReq, fromPhase)
		case "denied":
			logger.Info("Human denied AgentRequest via spec", "name", agentReq.Name)
			base := agentReq.DeepCopy()
			agentReq.Status.Phase = governancev1alpha1.PhaseDenied
			agentReq.Status.Denial = &governancev1alpha1.DenialResponse{
				Code:    governancev1alpha1.DenialCodePolicyViolation,
				Message: "Denied by human reviewer",
			}
			agentRequestDeniedTotal.WithLabelValues(governancev1alpha1.DenialCodePolicyViolation).Inc()
			agentRequestActive.Dec()
			agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseDenied).Inc()
			meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
				Type:    governancev1alpha1.ConditionApproved,
				Status:  metav1.ConditionFalse,
				Reason:  "ManualDenial",
				Message: "Denied by human reviewer",
			})
			if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestDenied, fromPhase, governancev1alpha1.PhaseDenied); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}
	logger.Info("AgentRequest awaiting manual approval", "name", agentReq.Name)
	return ctrl.Result{}, nil
}

func (r *AgentRequestReconciler) handleEvaluationResult(ctx context.Context, agentReq *governancev1alpha1.AgentRequest, result *evaluation.Result, policyEvalAudit *governancev1alpha1.AuditRecord) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fromPhase := agentReq.Status.Phase

	// Capture base before any mutations so the patch diff includes
	// ConditionPolicyEvaluated and all action-specific conditions atomically.
	base := agentReq.DeepCopy()

	meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
		Type:    governancev1alpha1.ConditionPolicyEvaluated,
		Status:  metav1.ConditionTrue,
		Reason:  "EvaluationComplete",
		Message: result.Message,
	})

	switch result.Action {
	case governancev1alpha1.ResultDeny:
		agentReq.Status.Phase = governancev1alpha1.PhaseDenied
		agentReq.Status.Denial = &governancev1alpha1.DenialResponse{
			Code:          result.Code,
			Message:       result.Message,
			PolicyResults: result.PolicyResults,
		}
		agentRequestDeniedTotal.WithLabelValues(result.Code).Inc()
		agentRequestActive.Dec()
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseDenied).Inc()

		meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
			Type:    governancev1alpha1.ConditionApproved,
			Status:  metav1.ConditionFalse,
			Reason:  "Denied",
			Message: result.Message,
		})

	case governancev1alpha1.ResultRequireApproval:
		meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
			Type:    governancev1alpha1.ConditionRequiresApproval,
			Status:  metav1.ConditionTrue,
			Reason:  "ApprovalRequired",
			Message: result.Message,
		})
	}

	// Single status patch: commits ConditionPolicyEvaluated + action-specific conditions atomically
	if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
		logger.Error(err, "Failed to update Status with evaluation results")
		return ctrl.Result{}, err
	}

	// Now emit the audit record(s) safely. Using a dedicated patch for the audit creation
	// to ensure we don't return from handleLockAcquisition without the audit trace.
	if err := r.Create(ctx, policyEvalAudit); err != nil {
		logger.Error(err, "Failed to create policy.evaluated AuditRecord")
	}

	if result.Action == governancev1alpha1.ResultDeny {
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestDenied, fromPhase, governancev1alpha1.PhaseDenied); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if result.Action == governancev1alpha1.ResultRequireApproval {
		// Emit record for entering manual approval phase
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventPolicyEvaluated, fromPhase, agentReq.Status.Phase); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Default: Allow -> handle lock
	return r.handleLockAcquisition(ctx, agentReq, fromPhase)
}

func (r *AgentRequestReconciler) handleLockAcquisition(ctx context.Context, agentReq *governancev1alpha1.AgentRequest, fromPhase string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Idempotency check: if we are already in Approved phase, we don't need to acquire again
	// unless we are implementing re-entrant or renewal logic here.
	if agentReq.Status.Phase == governancev1alpha1.PhaseApproved {
		return ctrl.Result{}, nil
	}

	// Attempt to acquire OpsLock via Lease
	leaseName := generateLeaseName(agentReq.Spec.Target.URI)
	holderIdentity := fmt.Sprintf("%s/%s", agentReq.Spec.AgentIdentity, agentReq.Name)

	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: agentReq.Namespace,
			Labels: map[string]string{
				"governance.aip.io/managed-by": "aip-controller",
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To(holderIdentity),
			LeaseDurationSeconds: ptr.To(int32(300)), // Default 5 minutes
			AcquireTime:          &metav1.MicroTime{Time: r.now()},
			RenewTime:            &metav1.MicroTime{Time: r.now()},
		},
	}

	err := r.Create(ctx, lease)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Lock contention check
			existingLease := &coordinationv1.Lease{}
			if getErr := r.Get(ctx, types.NamespacedName{Name: leaseName, Namespace: agentReq.Namespace}, existingLease); getErr != nil {
				logger.Error(getErr, "Failed to get existing lease for conflict resolution")
				return ctrl.Result{}, getErr
			}

			// Ensure that the holder is not ourselves (re-entrant success handling)
			if existingLease.Spec.HolderIdentity != nil && *existingLease.Spec.HolderIdentity == holderIdentity {
				// We somehow already own the lock
				logger.Info("AgentRequest already holds the lease", "lease", leaseName)
			} else {
				// Check timeout
				waitLimit := r.now().Add(-60 * time.Second) // 60 second timeout
				if agentReq.CreationTimestamp.Time.Before(waitLimit) {
					// Timeout exceeded
					logger.Info("Lock wait timeout exceeded", "lease", leaseName)
					opsLockContentionTotal.Inc()
					base := agentReq.DeepCopy()
					agentReq.Status.Phase = governancev1alpha1.PhaseDenied
					agentReq.Status.Denial = &governancev1alpha1.DenialResponse{
						Code:    governancev1alpha1.DenialCodeLockTimeout,
						Message: fmt.Sprintf("Failed to acquire lock for %s within 60s timeout. Lock held by: %s", agentReq.Spec.Target.URI, ptr.Deref(existingLease.Spec.HolderIdentity, "unknown")),
					}
					agentRequestDeniedTotal.WithLabelValues(governancev1alpha1.DenialCodeLockTimeout).Inc()
					agentRequestActive.Dec()
					agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseDenied).Inc()

					meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
						Type:    governancev1alpha1.ConditionApproved,
						Status:  metav1.ConditionFalse,
						Reason:  "LockTimeout",
						Message: "Lock acquisition timed out",
					})

					if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
						return ctrl.Result{}, err
					}

					if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestDenied, fromPhase, governancev1alpha1.PhaseDenied); err != nil {
						return ctrl.Result{}, err
					}

					return ctrl.Result{}, nil
				}

				// Requeue to wait for the lock
				opsLockContentionTotal.Inc()
				logger.Info("Lock contention, requeueing", "lease", leaseName)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		} else {
			logger.Error(err, "Failed to create Lease for OpsLock")
			return ctrl.Result{}, err
		}
	}

	// Lock acquired successfully!
	opsLockActive.Inc()
	agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseApproved).Inc()
	base := agentReq.DeepCopy()
	meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
		Type:    governancev1alpha1.ConditionApproved,
		Status:  metav1.ConditionTrue,
		Reason:  "Approved",
		Message: "Request approved and OpsLock acquired",
	})

	agentReq.Status.Phase = governancev1alpha1.PhaseApproved
	if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestApproved, fromPhase, governancev1alpha1.PhaseApproved); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *AgentRequestReconciler) reconcileApproved(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) (ctrl.Result, error) {
	log.FromContext(ctx).Info("AgentRequest is Approved. Waiting for agent to acquire lock and signal Executing.", "name", agentReq.Name)

	// In Phase 4, the controller will participate in lease management here.
	// For Phase 2, we just return and wait for the agent to patch the Executing condition.
	return ctrl.Result{}, nil
}

func (r *AgentRequestReconciler) reconcileExecuting(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) (ctrl.Result, error) {
	// Fetch the Lease
	leaseName := generateLeaseName(agentReq.Spec.Target.URI)
	lease := &coordinationv1.Lease{}

	err := r.Get(ctx, types.NamespacedName{Name: leaseName, Namespace: agentReq.Namespace}, lease)
	if err != nil {
		if errors.IsNotFound(err) {
			// Lease is missing! This shouldn't happen during execution unless deleted manually
			log.FromContext(ctx).Error(err, "Lease missing during execution", "lease", leaseName)
			// Return nil as whatever deleted it might have resolved the intent or we wait for Agent completion
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Check Lease expiration
	if lease.Spec.RenewTime != nil && lease.Spec.LeaseDurationSeconds != nil {
		expirationTime := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
		if r.now().After(expirationTime) {
			log.FromContext(ctx).Info("AgentRequest execution timed out (Lease expired)", "name", agentReq.Name)
			opsLockExpiredTotal.Inc()
			agentRequestActive.Dec()
			agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseFailed).Inc()
			fromPhase := agentReq.Status.Phase
			base := agentReq.DeepCopy()
			meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
				Type:    governancev1alpha1.ConditionFailed,
				Status:  metav1.ConditionTrue,
				Reason:  governancev1alpha1.DenialCodeLockTimeout,
				Message: "Heartbeat timeout: Agent failed to renew OpsLock lease before expiration",
			})
			agentReq.Status.Phase = governancev1alpha1.PhaseFailed
			if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}

			// Try to delete the expired lease
			if err := r.Delete(ctx, lease); err != nil && !errors.IsNotFound(err) {
				log.FromContext(ctx).Error(err, "Failed to delete expired lease", "lease", leaseName)
			}

			if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventLockExpired, fromPhase, governancev1alpha1.PhaseFailed); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestFailed, fromPhase, governancev1alpha1.PhaseFailed); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}
	}

	// Re-queue slowly to monitor the executing state
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// releaseLock deletes the Kubernetes Lease backing the OpsLock for this request's target.
// It is idempotent: a missing lease is not an error.
func (r *AgentRequestReconciler) releaseLock(ctx context.Context, req *governancev1alpha1.AgentRequest) error {
	leaseName := generateLeaseName(req.Spec.Target.URI)
	lease := &coordinationv1.Lease{}
	err := r.Get(ctx, types.NamespacedName{Name: leaseName, Namespace: req.Namespace}, lease)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	// Only delete if we are the holder
	holderIdentity := fmt.Sprintf("%s/%s", req.Spec.AgentIdentity, req.Name)
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != holderIdentity {
		return nil
	}
	if err := r.Delete(ctx, lease); err != nil && !errors.IsNotFound(err) {
		return err
	}
	opsLockActive.Dec()
	return nil
}

// emitAuditRecord creates a new AuditRecord CR
func (r *AgentRequestReconciler) emitAuditRecord(ctx context.Context, req *governancev1alpha1.AgentRequest, eventType string, fromPhase string, toPhase string) error {
	auditLabels := map[string]string{}
	if corrID, ok := req.Labels["aip.io/correlationID"]; ok && corrID != "" {
		auditLabels["aip.io/correlationID"] = corrID
	}

	audit := &governancev1alpha1.AuditRecord{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: req.Name + "-audit-",
			Namespace:    req.Namespace,
			Labels:       auditLabels,
		},
		Spec: governancev1alpha1.AuditRecordSpec{
			Timestamp:       metav1.NewTime(r.now()),
			AgentIdentity:   req.Spec.AgentIdentity,
			AgentRequestRef: req.Name,
			Event:           eventType,
			Action:          req.Spec.Action,
			TargetURI:       req.Spec.Target.URI,
			Reason:          req.Spec.Reason,
		},
	}

	if fromPhase != "" && toPhase != "" {
		audit.Spec.PhaseTransition = &governancev1alpha1.PhaseTransition{
			From: fromPhase,
			To:   toPhase,
		}
	}

	// Set OwnerReference for garbage collection
	if err := ctrl.SetControllerReference(req, audit, r.Scheme); err != nil {
		log.FromContext(ctx).Error(err, "Failed to set owner reference for AuditRecord")
		// Continue anyway, it's just GC
	}

	if err := r.Create(ctx, audit); err != nil {
		log.FromContext(ctx).Error(err, "Failed to create AuditRecord")
		return err
	}

	return nil
}

// fetchTargetContext retrieves live cluster state for the AgentRequest's target URI.
// Returns an empty TargetContext (not an error) when no fetcher is configured,
// so existing deployments without a fetcher degrade gracefully.
//
// After fetching primary target state, it cross-checks the agent's declared
// cascade model: each declared affected target is independently verified against
// the cluster. Only targets that actually exist and are live are reported in
// DownstreamServices — the control plane does not trust the agent's declarations,
// it verifies them.
func (r *AgentRequestReconciler) fetchTargetContext(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) (*evaluation.TargetContext, map[string]*evaluation.TargetContext, error) {
	if r.TargetContextFetcher == nil {
		return &evaluation.TargetContext{}, nil, nil
	}

	targetCtx, err := r.TargetContextFetcher.Fetch(ctx, agentReq.Spec.Target.URI, agentReq.Namespace)
	if err != nil {
		return targetCtx, nil, err
	}

	cascadeCtxs := make(map[string]*evaluation.TargetContext)

	// Verify each target declared in the agent's cascade model against live
	// cluster state. We populate DownstreamServices with names that actually
	// exist and are healthy — giving human reviewers ground truth, not just
	// the agent's assertion.
	if agentReq.Spec.CascadeModel != nil {
		for _, affected := range agentReq.Spec.CascadeModel.AffectedTargets {
			cascadeCtx, err := r.TargetContextFetcher.Fetch(ctx, affected.URI, agentReq.Namespace)
			if err != nil || cascadeCtx == nil || !cascadeCtx.Exists {
				continue
			}
			cascadeCtxs[affected.URI] = cascadeCtx

			parsed := evaluation.ParseTargetURI(affected.URI)
			name := parsed.Name
			if name == "" {
				name = affected.URI
			}
			targetCtx.DownstreamServices = append(targetCtx.DownstreamServices, name)
		}
	}

	return targetCtx, cascadeCtxs, nil
}

// fetchContextFromProvider dispatches to the named fetcher and performs runtime schema validation.
func (r *AgentRequestReconciler) fetchContextFromProvider(ctx context.Context, agentReq *governancev1alpha1.AgentRequest, gr *governancev1alpha1.GovernedResource) (*apiextensionsv1.JSON, error) {
	fetcherName := gr.Spec.ContextFetcher
	if fetcherName == "" || fetcherName == "none" {
		return nil, nil
	}

	var fetcher func(context.Context, client.Client, string) (*apiextensionsv1.JSON, error)
	switch fetcherName {
	case "k8s-deployment":
		fetcher = fetchers.FetchK8sDeployment
	case "karpenter":
		fetcher = fetchers.FetchKarpenter
	case "github":
		fetcher = fetchers.FetchGitHub
	default:
		return nil, fmt.Errorf("unknown fetcher: %s", fetcherName)
	}

	fetchedJSON, err := fetcher(ctx, r.Client, agentReq.Spec.Target.URI)
	if err != nil {
		return nil, err
	}

	if gr.Spec.ContextSchema != nil {
		// 1. Decode contextSchema JSON into structural schema
		var propsv1 apiextensionsv1.JSONSchemaProps
		if err := json.Unmarshal(gr.Spec.ContextSchema.Raw, &propsv1); err != nil {
			return nil, fmt.Errorf("failed to unmarshal contextSchema: %w", err)
		}

		var props apiextensions.JSONSchemaProps
		if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&propsv1, &props, nil); err != nil {
			return nil, fmt.Errorf("failed to convert schema: %w", err)
		}

		// 2. Wrap in validator
		validator, _, err := validation.NewSchemaValidator(&props)
		if err != nil {
			return nil, fmt.Errorf("failed to create schema validator: %w", err)
		}

		// 3. Validate the fetched JSON against it
		var obj any
		if err := json.Unmarshal(fetchedJSON.Raw, &obj); err != nil {
			return nil, fmt.Errorf("failed to unmarshal fetched JSON: %w", err)
		}

		res := validator.Validate(obj)
		if !res.IsValid() {
			// Validation fails
			meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
				Type:    "FetcherSchemaViolation",
				Status:  metav1.ConditionTrue,
				Reason:  "SchemaMismatch",
				Message: fmt.Sprintf("fetcher '%s' returned invalid JSON: %v", fetcherName, res.Errors[0]),
			})
			return nil, nil
		}
	}

	return fetchedJSON, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&governancev1alpha1.AgentRequest{}).
		Named("agentrequest").
		Complete(r)
}
