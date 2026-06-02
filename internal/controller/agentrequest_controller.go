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
	"maps"
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

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/evaluation"
	"github.com/agent-control-plane/aip-k8s/internal/evaluation/fetchers"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
)

const (
	// defaultLockWaitTimeout is the fallback maximum time a request will wait in
	// Pending to acquire the OpsLock Lease when no LockWaitDuration is configured.
	defaultLockWaitTimeout = 60 * time.Second
	// awaitingVerdictTTL is the maximum time an ungraded AwaitingVerdict request
	// remains active before the controller expires it. Expired requests are excluded
	// from accuracy counts (graduation ladder spec §4.2).
	awaitingVerdictTTL = 168 * time.Hour // 7 days
)

// AgentRequestReconciler reconciles a AgentRequest object
type AgentRequestReconciler struct {
	client.Client
	// APIReader is a direct (non-cached) API server reader used for the initial
	// Get in Reconcile. This ensures we always work with the latest resourceVersion
	// and avoids 409 conflicts caused by stale informer cache reads.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Clock     func() time.Time // injectable for testing; defaults to time.Now
	// OpsLockDuration controls how long an OpsLock Lease remains valid before the
	// controller declares heartbeat timeout and fails the AgentRequest. Zero or
	// negative values fall back to defaultOpsLockDuration (5 minutes). The controller
	// renews the lease at half this interval, so agents have up to OpsLockDuration
	// to complete execution before the lock expires.
	OpsLockDuration time.Duration
	// LockWaitDuration is the maximum time a Pending request will wait to acquire the
	// OpsLock Lease before being Denied with LOCK_TIMEOUT. Zero falls back to
	// defaultLockWaitTimeout (60s). Configurable so e2e tests can use a shorter value
	// (e.g. 20s) to ensure the test completes well before the GC hard-TTL window.
	LockWaitDuration time.Duration
	// ApprovedTimeout is the maximum time a request can stay in Approved phase before
	// the controller times it out to PhaseFailed (agent never started execution).
	// Zero or negative falls back to 5 minutes.
	ApprovedTimeout time.Duration

	Evaluator            evaluation.Evaluator
	TargetContextFetcher evaluation.TargetContextFetcher
	GitHubMCPFetcher     *fetchers.GitHubMCPFetcher
}

// lockWaitTimeout returns the configured LockWaitDuration, falling back to
// defaultLockWaitTimeout if zero or negative.
func (r *AgentRequestReconciler) lockWaitTimeout() time.Duration {
	if r.LockWaitDuration > 0 {
		return r.LockWaitDuration
	}
	return defaultLockWaitTimeout
}

func (r *AgentRequestReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// deterministicAuditName generates a stable, DNS-compliant name for an AuditRecord
// based on the resource name, event type, and the state transition (stateA→stateB).
// Using state instead of time means retries for the same transition produce the same
// name, so AlreadyExists fires correctly instead of creating a duplicate record.
func deterministicAuditName(resourceName, eventType, stateA, stateB string) string {
	input := resourceName + "|" + eventType + "|" + stateA + "|" + stateB
	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", hash)[:53]
}

const defaultOpsLockDuration = 5 * time.Minute
const defaultApprovedTimeout = 5 * time.Minute

func (r *AgentRequestReconciler) opsLockDurationOrDefault() time.Duration {
	if r.OpsLockDuration <= 0 {
		return defaultOpsLockDuration
	}
	return r.OpsLockDuration
}

func (r *AgentRequestReconciler) approvedTimeoutOrDefault() time.Duration {
	if r.ApprovedTimeout <= 0 {
		return defaultApprovedTimeout
	}
	return r.ApprovedTimeout
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

	if agentReq.Status.Phase == governancev1alpha1.PhaseCompleted {
		// Backfill: a late PUT /result may have written Status.Result after the
		// request.completed AuditRecord was already emitted (with no Details).
		// Patch the existing record so the audit trail is complete.
		if agentReq.Status.Result != nil {
			if err := r.backfillCompletedAuditResult(ctx, &agentReq); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if agentReq.Status.Phase == governancev1alpha1.PhaseFailed ||
		agentReq.Status.Phase == governancev1alpha1.PhaseDenied ||
		agentReq.Status.Phase == governancev1alpha1.PhaseExpired ||
		agentReq.Status.Phase == governancev1alpha1.PhaseObserved {
		return ctrl.Result{}, nil
	}

	// 1.1 GovernedResource Integrity Check
	if governedResourceRef(&agentReq) != nil {
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
	if handled, err := r.initializePhase(ctx, &agentReq, reader); handled || err != nil {
		return ctrl.Result{}, err
	}

	// 4. State Machine Evaluation
	switch agentReq.Status.Phase {
	case governancev1alpha1.PhasePending:
		return r.reconcilePending(ctx, &agentReq)
	case governancev1alpha1.PhaseApproved:
		return r.reconcileApproved(ctx, &agentReq)
	case governancev1alpha1.PhaseExecuting:
		return r.reconcileExecuting(ctx, &agentReq)
	case governancev1alpha1.PhaseAwaitingVerdict:
		return r.reconcileAwaitingVerdict(ctx, &agentReq)
	}

	return ctrl.Result{}, nil
}

// governedResourceRef returns the effective GovernedResourceRef for an AgentRequest.
// It reads from Status first (canonical), falling back to Spec (deprecated) for
// requests created before the field was added to Status.
func governedResourceRef(agentReq *governancev1alpha1.AgentRequest) *governancev1alpha1.GovernedResourceRef {
	if agentReq.Status.GovernedResourceRef != nil {
		return agentReq.Status.GovernedResourceRef
	}
	return agentReq.Spec.GovernedResourceRef
}

func (r *AgentRequestReconciler) checkGovernedResourceIntegrity(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) error {
	ref := governedResourceRef(agentReq)
	var gr governancev1alpha1.GovernedResource
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &gr); err != nil {
		if errors.IsNotFound(err) {
			log.FromContext(ctx).Info("Denying AgentRequest because its GovernedResource was deleted", "name", agentReq.Name, "governedResource", ref.Name)
			fromPhase := agentReq.Status.Phase
			base := agentReq.DeepCopy()
			agentReq.Status.Phase = governancev1alpha1.PhaseDenied
			agentReq.Status.Denial = &governancev1alpha1.DenialResponse{
				Code:    governancev1alpha1.DenialCodeGovernedResourceDeleted,
				Message: fmt.Sprintf("The GovernedResource %q that admitted this request was deleted.", ref.Name),
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
	if gr.Generation != ref.Generation {
		log.FromContext(ctx).Info("Denying AgentRequest due to GovernedResource generation mismatch",
			"name", agentReq.Name,
			"expectedGeneration", ref.Generation,
			"currentGeneration", gr.Generation)
		fromPhase := agentReq.Status.Phase
		base := agentReq.DeepCopy()
		agentReq.Status.Phase = governancev1alpha1.PhaseDenied
		agentReq.Status.Denial = &governancev1alpha1.DenialResponse{
			Code: governancev1alpha1.DenialCodeGenerationMismatch,
			Message: fmt.Sprintf(
				"GovernedResource %q generation has changed (expected %d, current %d); the policy binding is no longer valid.",
				ref.Name,
				ref.Generation,
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
		// Re-read via APIReader to ensure MergeFrom(base) does not zero out
		// Status.Result written by the gateway between the initial reconcile
		// fetch and this patch.
		var fresh governancev1alpha1.AgentRequest
		if err := r.APIReader.Get(ctx, types.NamespacedName{Name: agentReq.Name, Namespace: agentReq.Namespace}, &fresh); err != nil {
			return false, fmt.Errorf("re-reading AgentRequest for Result: %w", err)
		}
		agentReq.Status.Result = fresh.Status.Result
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
		log.FromContext(ctx).Info("Agent signaled Executing, transitioning phase", "from", fromPhase, "to", governancev1alpha1.PhaseExecuting)
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

// initializePhase sets the initial Phase on a newly submitted AgentRequest and emits
// the first AuditRecord. Returns (true, nil) so Reconcile returns immediately;
// (false, nil) when phase is already set (no-op); (false, err) on transient errors.
func (r *AgentRequestReconciler) initializePhase(
	ctx context.Context,
	agentReq *governancev1alpha1.AgentRequest,
	reader client.Reader,
) (bool, error) {
	if agentReq.Status.Phase != "" || meta.IsStatusConditionTrue(agentReq.Status.Conditions, "RequestSubmitted") {
		return false, nil
	}

	// Gateway-denied: request was blocked at admission before policy evaluation.
	// Transition directly to Denied and emit a request.denied AuditRecord so that
	// scope escalation attempts are visible in the audit trail.
	if denialCode, ok := agentReq.Annotations[governancev1alpha1.AnnotationGatewayDenied]; ok && denialCode != "" {
		log.FromContext(ctx).Info("Transitioning gateway-denied AgentRequest to Denied", "name", agentReq.Name, "code", denialCode)
		base := agentReq.DeepCopy()
		agentReq.Status.Phase = governancev1alpha1.PhaseDenied
		agentReq.Status.Denial = &governancev1alpha1.DenialResponse{
			Code:    denialCode,
			Message: fmt.Sprintf("Blocked at gateway admission: %s", denialCode),
		}
		agentRequestDeniedTotal.WithLabelValues(denialCode).Inc()
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseDenied).Inc()
		meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
			Type:    "RequestSubmitted",
			Status:  metav1.ConditionTrue,
			Reason:  "GatewayDenied",
			Message: fmt.Sprintf("Blocked at gateway admission: %s", denialCode),
		})
		if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
			return false, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestDenied, "", governancev1alpha1.PhaseDenied); err != nil {
			return false, err
		}
		return true, nil
	}

	// Observe-mode requests are recorded as observations only; they skip
	// SafetyPolicy eval, OpsLock, and human approval and are immediately terminal.
	if agentReq.Spec.Mode == governancev1alpha1.ModeObserve {
		log.FromContext(ctx).Info("Initializing AgentRequest phase", "name", agentReq.Name, "phase", governancev1alpha1.PhaseObserved, "mode", governancev1alpha1.ModeObserve)
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseObserved).Inc()
		base := agentReq.DeepCopy()
		agentReq.Status.Phase = governancev1alpha1.PhaseObserved
		meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
			Type:    "RequestSubmitted",
			Status:  metav1.ConditionTrue,
			Reason:  "ObserveMode",
			Message: "Request recorded as observation; governance lifecycle skipped",
		})
		if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
			return false, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestObserved, "", governancev1alpha1.PhaseObserved); err != nil {
			return false, err
		}
		return true, nil
	}

	phase := governancev1alpha1.PhasePending
	reason := "Initialization"
	message := "Initial phase set to Pending"

	// Gateway trust gate: Observer/Advisor levels cannot execute; route directly to
	// AwaitingVerdict so a human reviewer can grade the request.
	if agentReq.Annotations[governancev1alpha1.AnnotationCanExecute] == "false" {
		phase = governancev1alpha1.PhaseAwaitingVerdict
		reason = "TrustGateBlock"
		message = "Agent trust level does not permit execution; routing to AwaitingVerdict"
	} else if ref := governedResourceRef(agentReq); ref != nil {
		var gr governancev1alpha1.GovernedResource
		if err := reader.Get(ctx, types.NamespacedName{Name: ref.Name}, &gr); err == nil {
			if gr.Spec.SoakMode {
				phase = governancev1alpha1.PhaseAwaitingVerdict
				reason = "SoakModeAdmission"
				message = "Initial phase set to AwaitingVerdict due to GovernedResource SoakMode"
			}
		} else if !errors.IsNotFound(err) {
			return false, fmt.Errorf("fetching GovernedResource %s: %w",
				ref.Name, err)
		}
	}

	log.FromContext(ctx).Info("Initializing AgentRequest phase", "name", agentReq.Name, "phase", phase)
	base := agentReq.DeepCopy()
	agentReq.Status.Phase = phase
	if phase == governancev1alpha1.PhasePending || phase == governancev1alpha1.PhaseAwaitingVerdict {
		agentRequestActive.Inc()
	}
	agentRequestTotal.WithLabelValues(phase).Inc()

	meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
		Type:    "RequestSubmitted",
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})

	if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
		return false, err
	}
	if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestSubmitted, "", phase); err != nil {
		return false, err
	}
	return true, nil
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
	if ref := governedResourceRef(agentReq); ref != nil {
		var gr governancev1alpha1.GovernedResource
		if err := reader.Get(ctx, types.NamespacedName{Name: ref.Name}, &gr); err != nil {
			logger.Error(err, "Failed to get GovernedResource for policy matching", "name", ref.Name)
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
	if ref := governedResourceRef(agentReq); ref != nil && agentReq.Status.ProviderContext == nil {
		var gr governancev1alpha1.GovernedResource
		if err := reader.Get(ctx, types.NamespacedName{Name: ref.Name}, &gr); err != nil {
			logger.Error(err, "Failed to get GovernedResource for provider context", "name", ref.Name)
			return ctrl.Result{}, err
		}
		providerCtx, err := r.fetchContextFromProvider(ctx, agentReq, &gr)
		if err != nil {
			logger.Error(err, "Provider context fetch failed")
		} else if providerCtx != nil {
			providerBase := agentReq.DeepCopy()
			agentReq.Status.ProviderContext = providerCtx
			if patchErr := r.Status().Patch(ctx, agentReq, client.MergeFrom(providerBase)); patchErr != nil {
				logger.Error(patchErr, "Failed to persist ProviderContext to status")
				return ctrl.Result{}, patchErr
			}
			// Requeue so the next reconcile reads a fresh object with the
			// persisted ProviderContext before running evaluation.
			return ctrl.Result{Requeue: true}, nil
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

	policyEvalLabels := map[string]string{
		"aip.io/agentRequestRef": agentReq.Name,
	}
	if agentIdentity, ok := agentReq.Labels["aip.io/agentIdentity"]; ok && agentIdentity != "" {
		policyEvalLabels["aip.io/agentIdentity"] = agentIdentity
	}
	if corrID, ok := agentReq.Labels["aip.io/correlationID"]; ok && corrID != "" {
		policyEvalLabels["aip.io/correlationID"] = corrID
	}

	// Capture fromPhase before any mutations; used as stable name anchor.
	evalFromPhase := agentReq.Status.Phase
	now := r.now()

	policyEvalAudit := &governancev1alpha1.AuditRecord{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deterministicAuditName(agentReq.Name, governancev1alpha1.AuditEventPolicyEvaluated, evalFromPhase, ""),
			Namespace: agentReq.Namespace,
			Labels:    policyEvalLabels,
		},
		Spec: governancev1alpha1.AuditRecordSpec{
			Timestamp:         metav1.NewTime(now),
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
			denyAnnotations := map[string]string{}
			if agentReq.Spec.HumanApproval.ApprovedBy != "" {
				denyAnnotations["approvedBy"] = agentReq.Spec.HumanApproval.ApprovedBy
			}
			if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestDenied, fromPhase, governancev1alpha1.PhaseDenied, denyAnnotations); err != nil {
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
		// Trusted/Autonomous agents (via gateway trust gate) skip human approval.
		if agentReq.Annotations[governancev1alpha1.AnnotationRequiresHumanApproval] == "false" {
			logger.Info("Auto-approving AgentRequest via trust gate", "name", agentReq.Name, "trustLevel", agentReq.Annotations[governancev1alpha1.AnnotationEffectiveTrustLevel])
			meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
				Type:    governancev1alpha1.ConditionApproved,
				Status:  metav1.ConditionTrue,
				Reason:  "AutoApprovedByTrustGate",
				Message: "Agent trust level satisfies GovernedResource requirements; human approval not required",
			})
			if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
				logger.Error(err, "Failed to update Status with evaluation results")
				return ctrl.Result{}, err
			}
			if err := r.createAuditWithRetry(ctx, policyEvalAudit); err != nil {
				logger.Error(err, "Failed to create policy.evaluated AuditRecord after retries")
			}
			return ctrl.Result{}, nil
		}
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
	// Attach the phase transition to the policy.evaluated record so the audit
	// trail shows a single rich record per evaluation (not one per-detail + one per-transition).
	policyEvalAudit.Spec.PhaseTransition = &governancev1alpha1.PhaseTransition{
		From: fromPhase,
		To:   agentReq.Status.Phase,
	}

	if err := r.createAuditWithRetry(ctx, policyEvalAudit); err != nil {
		logger.Error(err, "Failed to create policy.evaluated AuditRecord after retries")
	}

	if result.Action == governancev1alpha1.ResultDeny {
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestDenied, fromPhase, governancev1alpha1.PhaseDenied); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if result.Action == governancev1alpha1.ResultRequireApproval {
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

	duration := r.opsLockDurationOrDefault()
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
			LeaseDurationSeconds: ptr.To(int32(duration / time.Second)),
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

			alreadyOwned := existingLease.Spec.HolderIdentity != nil && *existingLease.Spec.HolderIdentity == holderIdentity
			leaseExpired := false
			if existingLease.Spec.RenewTime != nil && existingLease.Spec.LeaseDurationSeconds != nil {
				expiry := existingLease.Spec.RenewTime.Add(time.Duration(*existingLease.Spec.LeaseDurationSeconds) * time.Second)
				leaseExpired = r.now().After(expiry)
			}

			if alreadyOwned {
				// We somehow already own the lock — fall through to lock acquired.
				logger.Info("AgentRequest already holds the lease", "lease", leaseName)
			} else if leaseExpired {
				// Previous holder's lease has expired; take it over.
				logger.Info("Taking over expired lease", "lease", leaseName, "expiredHolder", ptr.Deref(existingLease.Spec.HolderIdentity, "unknown"))
				base := existingLease.DeepCopy()
				existingLease.Spec.HolderIdentity = ptr.To(holderIdentity)
				existingLease.Spec.AcquireTime = &metav1.MicroTime{Time: r.now()}
				existingLease.Spec.RenewTime = &metav1.MicroTime{Time: r.now()}
				existingLease.Spec.LeaseDurationSeconds = ptr.To(int32(duration / time.Second))
				if patchErr := r.Patch(ctx, existingLease, client.MergeFrom(base)); patchErr != nil {
					logger.Error(patchErr, "Failed to take over expired lease")
					return ctrl.Result{}, patchErr
				}
				// Fall through to lock acquired.
			} else {
				// Lock is held and not yet expired — check how long we've been waiting.
				lockTimeout := r.lockWaitTimeout()
				waitLimit := r.now().Add(-lockTimeout)
				if agentReq.CreationTimestamp.Time.Before(waitLimit) {
					// Timeout exceeded
					logger.Info("Lock wait timeout exceeded", "lease", leaseName)
					opsLockContentionTotal.Inc()
					base := agentReq.DeepCopy()
					agentReq.Status.Phase = governancev1alpha1.PhaseDenied
					agentReq.Status.Denial = &governancev1alpha1.DenialResponse{
						Code:    governancev1alpha1.DenialCodeLockTimeout,
						Message: fmt.Sprintf("Failed to acquire lock for %s within %v timeout. Lock held by: %s", agentReq.Spec.Target.URI, lockTimeout, ptr.Deref(existingLease.Spec.HolderIdentity, "unknown")),
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

	approvalAnnotations := map[string]string{}
	if agentReq.Spec.HumanApproval != nil && agentReq.Spec.HumanApproval.ApprovedBy != "" {
		approvalAnnotations["approvedBy"] = agentReq.Spec.HumanApproval.ApprovedBy
	}
	if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestApproved, fromPhase, governancev1alpha1.PhaseApproved, approvalAnnotations); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *AgentRequestReconciler) reconcileApproved(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	approvedCond := meta.FindStatusCondition(agentReq.Status.Conditions, governancev1alpha1.ConditionApproved)
	if approvedCond == nil || approvedCond.Status != metav1.ConditionTrue {
		// Defensive: shouldn't be in PhaseApproved without the condition, but requeue to self-heal.
		logger.Info("AgentRequest is Approved but missing Approved condition", "name", agentReq.Name)
		return ctrl.Result{}, nil
	}

	approvedTimeout := r.approvedTimeoutOrDefault()
	deadline := approvedCond.LastTransitionTime.Add(approvedTimeout)
	now := r.now()

	if now.After(deadline) {
		logger.Info("AgentRequest timed out in Approved phase (agent never started execution)", "name", agentReq.Name)
	} else {
		// Heartbeat the OpsLock lease so it stays alive until the agent picks up
		// the JWT and transitions to Executing. reconcileExecuting takes over
		// heartbeating once execution begins; this covers the gap between lock
		// acquisition and execution start. Without renewal, a short
		// ops-lock-duration (e.g. 15s in tests) would let the lease lapse before
		// the agent ever starts, allowing another AR to take over prematurely.
		leaseDuration := r.opsLockDurationOrDefault()
		leaseName := generateLeaseName(agentReq.Spec.Target.URI)
		lease := &coordinationv1.Lease{}
		if err := r.Get(ctx, types.NamespacedName{Name: leaseName, Namespace: agentReq.Namespace}, lease); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			logger.Info("OpsLock lease missing in Approved phase, requeueing", "lease", leaseName)
		} else {
			expectedHolder := fmt.Sprintf("%s/%s", agentReq.Spec.AgentIdentity, agentReq.Name)
			if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == expectedHolder {
				base := lease.DeepCopy()
				lease.Spec.RenewTime = ptr.To(metav1.NewMicroTime(r.now()))
				if err := r.Patch(ctx, lease, client.MergeFrom(base)); err != nil {
					return ctrl.Result{}, fmt.Errorf("heartbeating approved-phase lease %s: %w", leaseName, err)
				}
			}
		}
		return ctrl.Result{RequeueAfter: leaseDuration / 2}, nil
	}

	// Release the lock BEFORE patching to PhaseFailed so that if this fails
	// the object is still in PhaseApproved and the next reconcile retries.
	// releaseLock is idempotent: a missing or foreign lease is a no-op.
	if err := r.releaseLock(ctx, agentReq); err != nil {
		return ctrl.Result{}, fmt.Errorf("releasing lock after Approved timeout for %s: %w", agentReq.Name, err)
	}

	fromPhase := agentReq.Status.Phase
	base := agentReq.DeepCopy()
	meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
		Type:    governancev1alpha1.ConditionFailed,
		Status:  metav1.ConditionTrue,
		Reason:  governancev1alpha1.DenialCodeLockTimeout,
		Message: fmt.Sprintf("Timed out after %v in Approved phase without starting execution", approvedTimeout),
	})
	agentReq.Status.Phase = governancev1alpha1.PhaseFailed
	agentReq.Status.Denial = &governancev1alpha1.DenialResponse{
		Code:    governancev1alpha1.DenialCodeLockTimeout,
		Message: fmt.Sprintf("Timed out after %v in Approved phase without starting execution", approvedTimeout),
	}

	agentRequestActive.Dec()
	agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseFailed).Inc()

	if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestFailed, fromPhase, governancev1alpha1.PhaseFailed); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *AgentRequestReconciler) reconcileExecuting(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) (ctrl.Result, error) {
	// Fetch the Lease fresh from APIReader to avoid stale cache before patching.
	// APIReader is always set by SetupWithManager; fall back to cached client defensively.
	leaseName := generateLeaseName(agentReq.Spec.Target.URI)
	lease := &coordinationv1.Lease{}
	leaseReader := client.Reader(r.Client)
	if r.APIReader != nil {
		leaseReader = r.APIReader
	}
	err := leaseReader.Get(ctx, types.NamespacedName{Name: leaseName, Namespace: agentReq.Namespace}, lease)
	if err != nil {
		if errors.IsNotFound(err) {
			// Lease is missing! This shouldn't happen during execution unless deleted manually
			log.FromContext(ctx).Error(err, "Lease missing during execution", "lease", leaseName)
			// Return nil as whatever deleted it might have resolved the intent or we wait for Agent completion
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Verify ownership before doing anything with the lease.
	// releaseLock uses the same holderIdentity format.
	expectedHolder := fmt.Sprintf("%s/%s", agentReq.Spec.AgentIdentity, agentReq.Name)
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != expectedHolder {
		actualHolder := ptr.Deref(lease.Spec.HolderIdentity, "unknown")
		log.FromContext(ctx).Error(nil, "Lease holder mismatch — treating as lost",
			"lease", leaseName, "expected", expectedHolder, "actual", actualHolder)
		opsLockExpiredTotal.Inc()
		agentRequestActive.Dec()
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseFailed).Inc()
		fromPhase := agentReq.Status.Phase
		base := agentReq.DeepCopy()
		meta.SetStatusCondition(&agentReq.Status.Conditions, metav1.Condition{
			Type:    governancev1alpha1.ConditionFailed,
			Status:  metav1.ConditionTrue,
			Reason:  governancev1alpha1.DenialCodeLockTimeout,
			Message: fmt.Sprintf("OpsLock lost: lease held by %q, not this request", actualHolder),
		})
		agentReq.Status.Phase = governancev1alpha1.PhaseFailed
		if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventLockExpired, fromPhase, governancev1alpha1.PhaseFailed); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestFailed, fromPhase, governancev1alpha1.PhaseFailed); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
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

		// Patch RenewTime to heartbeat the lease
		log.FromContext(ctx).Info("Heartbeating lease", "lease", leaseName, "duration", *lease.Spec.LeaseDurationSeconds)
		base := lease.DeepCopy()
		lease.Spec.RenewTime = ptr.To(metav1.NewMicroTime(r.now()))
		if err := r.Patch(ctx, lease, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("heartbeating lease %s: %w", leaseName, err)
		}

		// Requeue at half the lease duration
		requeueAfter := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second / 2
		log.FromContext(ctx).Info("Requeueing after heartbeat", "requeueAfter", requeueAfter)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// Fallback to slow re-queue if duration missing
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// reconcileAwaitingVerdict handles AwaitingVerdict phase requests.
// When the gateway sets verdict fields (Verdict, VerdictBy, VerdictAt), the controller
// drives the phase transition to Completed and emits a verdict.submitted AuditRecord.
// Requests that remain ungraded beyond awaitingVerdictTTL are transitioned to Expired.
func (r *AgentRequestReconciler) reconcileAwaitingVerdict(ctx context.Context, agentReq *governancev1alpha1.AgentRequest) (ctrl.Result, error) {
	if agentReq.Status.Verdict != "" {
		fromPhase := agentReq.Status.Phase
		base := agentReq.DeepCopy()
		agentReq.Status.Phase = governancev1alpha1.PhaseCompleted
		agentRequestActive.Dec()
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseCompleted).Inc()
		if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		annotations := map[string]string{
			"verdict":   agentReq.Status.Verdict,
			"verdictBy": agentReq.Status.VerdictBy,
		}
		if agentReq.Status.VerdictReasonCode != "" {
			annotations["verdictReasonCode"] = agentReq.Status.VerdictReasonCode
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventVerdictSubmitted, fromPhase, governancev1alpha1.PhaseCompleted, annotations); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	now := r.now()
	// TTL is measured from creation, not from when the request entered AwaitingVerdict.
	// For SoakMode requests this is equivalent (phase is set on the first reconcile),
	// but if execution takes non-trivial time the reviewer window silently shrinks.
	// A future improvement would record an AwaitingVerdictSince timestamp in status.
	deadline := agentReq.CreationTimestamp.Add(awaitingVerdictTTL)
	if now.After(deadline) {
		fromPhase := agentReq.Status.Phase
		base := agentReq.DeepCopy()
		agentReq.Status.Phase = governancev1alpha1.PhaseExpired
		agentRequestActive.Dec()
		agentRequestTotal.WithLabelValues(governancev1alpha1.PhaseExpired).Inc()
		if err := r.Status().Patch(ctx, agentReq, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.emitAuditRecord(ctx, agentReq, governancev1alpha1.AuditEventRequestExpired, fromPhase, governancev1alpha1.PhaseExpired); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: deadline.Sub(now)}, nil
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

// emitAuditRecord creates a new AuditRecord CR. The optional extraAnnotations map
// is merged into AuditRecord.Spec.Annotations and may carry event-specific context
// (e.g. approvedBy, verdict, verdictReasonCode) without changing the call signature.
func (r *AgentRequestReconciler) emitAuditRecord(ctx context.Context, req *governancev1alpha1.AgentRequest, eventType string, fromPhase string, toPhase string, extraAnnotations ...map[string]string) error {
	auditLabels := map[string]string{
		"aip.io/agentRequestRef": req.Name,
	}
	if agentIdentity, ok := req.Labels["aip.io/agentIdentity"]; ok && agentIdentity != "" {
		auditLabels["aip.io/agentIdentity"] = agentIdentity
	}
	if corrID, ok := req.Labels["aip.io/correlationID"]; ok && corrID != "" {
		auditLabels["aip.io/correlationID"] = corrID
	}

	now := r.now()

	audit := &governancev1alpha1.AuditRecord{
		ObjectMeta: metav1.ObjectMeta{
			// Use fromPhase+toPhase (not time) so retries of the same transition
			// produce the same name, letting AlreadyExists fire correctly.
			Name:      deterministicAuditName(req.Name, eventType, fromPhase, toPhase),
			Namespace: req.Namespace,
			Labels:    auditLabels,
		},
		Spec: governancev1alpha1.AuditRecordSpec{
			Timestamp:       metav1.NewTime(now),
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

	if len(extraAnnotations) > 0 && len(extraAnnotations[0]) > 0 {
		if audit.Spec.Annotations == nil {
			audit.Spec.Annotations = make(map[string]string, len(extraAnnotations[0]))
		}
		maps.Copy(audit.Spec.Annotations, extraAnnotations[0])
	}

	// Populate Spec.Details with Status.Result on request.completed.
	if eventType == governancev1alpha1.AuditEventRequestCompleted && req.Status.Result != nil {
		resultJSON, err := json.Marshal(req.Status.Result)
		if err == nil {
			audit.Spec.Details = &apiextensionsv1.JSON{Raw: resultJSON}
		}
	}

	// Set OwnerReference for garbage collection
	if err := ctrl.SetControllerReference(req, audit, r.Scheme); err != nil {
		log.FromContext(ctx).Error(err, "Failed to set owner reference for AuditRecord")
		// Continue anyway, it's just GC
	}

	if err := r.Create(ctx, audit); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		log.FromContext(ctx).Error(err, "Failed to create AuditRecord")
		return err
	}

	return nil
}

// backfillCompletedAuditResult patches the request.completed AuditRecord's
// Spec.Details when a late PUT /result writes Status.Result after the record
// was already emitted (with nil Details). Idempotent: records that already
// have Details are left unchanged. Uses label lookup because the fromPhase
// component of the deterministic name is not known at backfill time.
func (r *AgentRequestReconciler) backfillCompletedAuditResult(ctx context.Context, req *governancev1alpha1.AgentRequest) error {
	var list governancev1alpha1.AuditRecordList
	if err := r.List(ctx, &list,
		client.InNamespace(req.Namespace),
		client.MatchingLabels{"aip.io/agentRequestRef": req.Name},
	); err != nil {
		return err
	}
	resultJSON, err := json.Marshal(req.Status.Result)
	if err != nil {
		return fmt.Errorf("marshalling result for audit backfill: %w", err)
	}
	for i := range list.Items {
		ar := &list.Items[i]
		if ar.Spec.Event != governancev1alpha1.AuditEventRequestCompleted || ar.Spec.Details != nil {
			continue
		}
		base := ar.DeepCopy()
		ar.Spec.Details = &apiextensionsv1.JSON{Raw: resultJSON}
		if err := r.Patch(ctx, ar, client.MergeFrom(base)); err != nil {
			return err
		}
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

	var (
		fetchedJSON *apiextensionsv1.JSON
		err         error
	)
	switch fetcherName {
	case "k8s-deployment":
		fetchedJSON, err = fetchers.FetchK8sDeployment(ctx, r.Client, agentReq.Spec.Target.URI)
	case "karpenter":
		fetchedJSON, err = fetchers.FetchKarpenter(ctx, r.Client, agentReq.Spec.Target.URI)
	case "github":
		if r.GitHubMCPFetcher == nil {
			return nil, fmt.Errorf("GitHubMCPFetcher not configured")
		}
		fetchedJSON, err = r.GitHubMCPFetcher.Fetch(ctx, r.Client, agentReq.Spec.Target.URI)
	default:
		return nil, fmt.Errorf("unknown fetcher: %s", fetcherName)
	}
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

// createAuditWithRetry attempts to create an AuditRecord up to 3 times, tolerating
// AlreadyExists (idempotent). Returns the last error if all attempts fail.
func (r *AgentRequestReconciler) createAuditWithRetry(ctx context.Context, audit *governancev1alpha1.AuditRecord) error {
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

// SetupWithManager sets up the controller with the Manager.
func (r *AgentRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&governancev1alpha1.AgentRequest{}).
		Named("agentrequest").
		Complete(r)
}
