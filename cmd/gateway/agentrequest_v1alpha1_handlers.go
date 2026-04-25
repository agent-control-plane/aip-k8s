package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	v1alpha1openapi "github.com/agent-control-plane/aip-k8s/internal/openapi/v1alpha1"
)

func agentRequestToDTO(ar v1alpha1.AgentRequest) v1alpha1openapi.AgentRequestDTO {
	dto := v1alpha1openapi.AgentRequestDTO{
		Name:          ar.Name,
		Namespace:     ar.Namespace,
		CreatedAt:     ar.CreationTimestamp.Time,
		AgentIdentity: ar.Spec.AgentIdentity,
		Action:        ar.Spec.Action,
		Target: v1alpha1openapi.Target{
			URI: ar.Spec.Target.URI,
		},
		Reason: ar.Spec.Reason,
		Phase:  v1alpha1openapi.AgentRequestDTOPhase(ar.Status.Phase),
	}

	if ar.Spec.Target.ResourceType != nil {
		dto.Target.ResourceType = ar.Spec.Target.ResourceType
	}
	if len(ar.Spec.Target.Attributes) > 0 {
		attrs := ar.Spec.Target.Attributes
		dto.Target.Attributes = &attrs
	}

	if lbl, ok := ar.Labels["aip.io/correlationID"]; ok && lbl != "" {
		dto.CorrelationID = &lbl
	}
	if ar.Spec.Classification != "" {
		dto.Classification = &ar.Spec.Classification
	}
	if ar.Spec.IntentPlanRef != nil {
		dto.IntentPlanRef = ar.Spec.IntentPlanRef
	}
	if ar.Spec.Priority != nil {
		dto.Priority = ar.Spec.Priority
	}
	if ar.Spec.Interruptibility != nil {
		dto.Interruptibility = ar.Spec.Interruptibility
	}
	if ar.Spec.ExecutionMode != nil {
		em := v1alpha1openapi.AgentRequestDTOExecutionMode(*ar.Spec.ExecutionMode)
		dto.ExecutionMode = &em
	}
	if ar.Spec.Parameters != nil {
		raw := json.RawMessage(ar.Spec.Parameters.Raw)
		dto.Parameters = &raw
	}

	if ar.Spec.CascadeModel != nil {
		cm := &v1alpha1openapi.CascadeModel{}
		if len(ar.Spec.CascadeModel.AffectedTargets) > 0 {
			targets := make([]v1alpha1openapi.AffectedTarget, len(ar.Spec.CascadeModel.AffectedTargets))
			for i, t := range ar.Spec.CascadeModel.AffectedTargets {
				targets[i] = v1alpha1openapi.AffectedTarget{
					URI:        t.URI,
					EffectType: v1alpha1openapi.AffectedTargetEffectType(t.EffectType),
				}
			}
			cm.AffectedTargets = &targets
		}
		if ar.Spec.CascadeModel.ModelSourceTrust != nil {
			trust := v1alpha1openapi.CascadeModelModelSourceTrust(*ar.Spec.CascadeModel.ModelSourceTrust)
			cm.ModelSourceTrust = &trust
		}
		if ar.Spec.CascadeModel.ModelSourceID != nil {
			cm.ModelSourceID = ar.Spec.CascadeModel.ModelSourceID
		}
		dto.CascadeModel = cm
	}

	if ar.Spec.ReasoningTrace != nil {
		rt := &v1alpha1openapi.ReasoningTrace{}
		if ar.Spec.ReasoningTrace.ConfidenceScore != nil {
			rt.ConfidenceScore = ar.Spec.ReasoningTrace.ConfidenceScore
		}
		if len(ar.Spec.ReasoningTrace.ComponentConfidence) > 0 {
			cc := ar.Spec.ReasoningTrace.ComponentConfidence
			rt.ComponentConfidence = &cc
		}
		if ar.Spec.ReasoningTrace.TraceReference != nil {
			rt.TraceReference = ar.Spec.ReasoningTrace.TraceReference
		}
		if len(ar.Spec.ReasoningTrace.Alternatives) > 0 {
			alts := ar.Spec.ReasoningTrace.Alternatives
			rt.Alternatives = &alts
		}
		dto.ReasoningTrace = rt
	}

	if ar.Spec.ScopeBounds != nil {
		dto.ScopeBounds = &v1alpha1openapi.ScopeBounds{
			PermittedActions:        ar.Spec.ScopeBounds.PermittedActions,
			PermittedTargetPatterns: ar.Spec.ScopeBounds.PermittedTargetPatterns,
			TimeBoundSeconds:        ar.Spec.ScopeBounds.TimeBoundSeconds,
		}
	}

	if ar.Spec.GovernedResourceRef != nil {
		dto.GovernedResourceRef = &v1alpha1openapi.GovernedResourceRef{
			Name:       &ar.Spec.GovernedResourceRef.Name,
			Generation: &ar.Spec.GovernedResourceRef.Generation,
		}
	}

	if len(ar.Status.Conditions) > 0 {
		conds := make([]v1alpha1openapi.Condition, len(ar.Status.Conditions))
		for i, c := range ar.Status.Conditions {
			conds[i] = v1alpha1openapi.Condition{
				Type:               c.Type,
				Status:             v1alpha1openapi.ConditionStatus(string(c.Status)),
				Reason:             c.Reason,
				Message:            c.Message,
				LastTransitionTime: c.LastTransitionTime.Time,
			}
		}
		dto.Conditions = &conds
	}

	if ar.Status.Denial != nil {
		d := &v1alpha1openapi.DenialResponse{
			Code:    &ar.Status.Denial.Code,
			Message: &ar.Status.Denial.Message,
		}
		if ar.Status.Denial.RetryAfterSeconds != nil {
			d.RetryAfterSeconds = ar.Status.Denial.RetryAfterSeconds
		}
		if len(ar.Status.Denial.PolicyResults) > 0 {
			prs := make([]v1alpha1openapi.PolicyResult, len(ar.Status.Denial.PolicyResults))
			for i, pr := range ar.Status.Denial.PolicyResults {
				prs[i] = v1alpha1openapi.PolicyResult{
					PolicyName: &pr.PolicyName,
					RuleName:   &pr.RuleName,
					Result:     &pr.Result,
				}
				if pr.PolicyGeneration != 0 {
					gen := pr.PolicyGeneration
					prs[i].PolicyGeneration = &gen
				}
			}
			d.PolicyResults = &prs
		}
		dto.Denial = d
	}

	if ar.Status.ControlPlaneVerification != nil {
		cpv := ar.Status.ControlPlaneVerification
		dtoCPV := &v1alpha1openapi.ControlPlaneVerification{
			TargetExists:       &cpv.TargetExists,
			HasActiveEndpoints: &cpv.HasActiveEndpoints,
		}
		if cpv.EvaluatedStateFingerprint != "" {
			dtoCPV.EvaluatedStateFingerprint = &cpv.EvaluatedStateFingerprint
		}
		if cpv.ActiveEndpointCount > 0 {
			dtoCPV.ActiveEndpointCount = &cpv.ActiveEndpointCount
		}
		if cpv.ReadyReplicas > 0 {
			dtoCPV.ReadyReplicas = &cpv.ReadyReplicas
		}
		if cpv.SpecReplicas > 0 {
			dtoCPV.SpecReplicas = &cpv.SpecReplicas
		}
		if len(cpv.DownstreamServices) > 0 {
			dtoCPV.DownstreamServices = &cpv.DownstreamServices
		}
		if !cpv.FetchedAt.IsZero() {
			t := cpv.FetchedAt.Time
			dtoCPV.FetchedAt = &t
		}
		dto.ControlPlaneVerification = dtoCPV
	}

	if ar.Status.Verdict != "" {
		vi := &v1alpha1openapi.VerdictInfo{}
		v := v1alpha1openapi.VerdictInfoVerdict(ar.Status.Verdict)
		vi.Verdict = &v
		if ar.Status.VerdictReasonCode != "" {
			rc := v1alpha1openapi.VerdictInfoReasonCode(ar.Status.VerdictReasonCode)
			vi.ReasonCode = &rc
		}
		if ar.Status.VerdictNote != "" {
			vi.Note = &ar.Status.VerdictNote
		}
		if ar.Status.VerdictBy != "" {
			vi.By = &ar.Status.VerdictBy
		}
		if ar.Status.VerdictAt != nil {
			t := ar.Status.VerdictAt.Time
			vi.At = &t
		}
		dto.Verdict = vi
	}

	return dto
}

// GET /v1alpha1/agent-requests
func (s *Server) v1alpha1ListAgentRequests(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	ns := resolveNamespace(r)
	agentID := r.URL.Query().Get("filter.agentIdentity")
	correlID := r.URL.Query().Get("filter.correlationID")
	phaseFilter := r.URL.Query().Get("filter.phase")
	limitStr := r.URL.Query().Get("limit")
	continueToken := r.URL.Query().Get("nextPageToken")

	listOpts := []client.ListOption{client.InNamespace(ns)}
	matchLabels := map[string]string{}
	if agentID != "" {
		matchLabels["aip.io/agentIdentity"] = sanitizeLabelValue(agentID)
	}
	if correlID != "" {
		matchLabels["aip.io/correlationID"] = sanitizeLabelValue(correlID)
	}
	if len(matchLabels) > 0 {
		listOpts = append(listOpts, client.MatchingLabels(matchLabels))
	}
	if limitStr != "" {
		limit, err := strconv.ParseInt(limitStr, 10, 64)
		if err != nil || limit <= 0 {
			writeProblem(w, http.StatusBadRequest, "invalid limit: must be a positive integer")
			return
		}
		listOpts = append(listOpts, client.Limit(limit))
	}
	if continueToken != "" {
		listOpts = append(listOpts, client.Continue(continueToken))
	}

	var list v1alpha1.AgentRequestList
	if err := s.client.List(r.Context(), &list, listOpts...); err != nil {
		if apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := list.Items
	if phaseFilter != "" {
		filtered := make([]v1alpha1.AgentRequest, 0, len(items))
		for _, item := range items {
			if item.Status.Phase == phaseFilter {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	dtos := make([]v1alpha1openapi.AgentRequestDTO, len(items))
	for i, ar := range items {
		dtos[i] = agentRequestToDTO(ar)
	}

	resp := v1alpha1openapi.AgentRequestList{Items: dtos}
	if list.Continue != "" {
		resp.NextPageToken = &list.Continue
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /v1alpha1/agent-requests
//
//nolint:gocyclo // mirrors legacy handler's admission pipeline: auth, GR match, dedup, create
func (s *Server) v1alpha1CreateAgentRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRoleV1(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	var body v1alpha1openapi.CreateAgentRequestRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.AgentIdentity == "" || body.Action == "" || body.Target.URI == "" || body.Reason == "" {
		writeProblem(w, http.StatusBadRequest, "agentIdentity, action, target.uri, and reason are required")
		return
	}
	if s.authRequired && body.AgentIdentity != sub {
		writeProblem(w, http.StatusBadRequest, "agentIdentity must match authenticated caller")
		return
	}

	ns := resolveNamespace(r)

	var parameters *apiextensionsv1.JSON
	if body.Parameters != nil && len(*body.Parameters) > 0 && string(*body.Parameters) != jsonNull {
		parameters = &apiextensionsv1.JSON{Raw: *body.Parameters}
	}

	reqLabels := map[string]string{
		"aip.io/agentIdentity": sanitizeLabelValue(body.AgentIdentity),
	}
	if body.CorrelationID != nil && *body.CorrelationID != "" {
		reqLabels["aip.io/correlationID"] = sanitizeLabelValue(*body.CorrelationID)
	}

	var executionMode *string
	if body.ExecutionMode != nil {
		em := string(*body.ExecutionMode)
		executionMode = &em
	}

	var scopeBounds *v1alpha1.ScopeBounds
	if body.ScopeBounds != nil {
		scopeBounds = &v1alpha1.ScopeBounds{
			PermittedActions:        body.ScopeBounds.PermittedActions,
			PermittedTargetPatterns: body.ScopeBounds.PermittedTargetPatterns,
			TimeBoundSeconds:        body.ScopeBounds.TimeBoundSeconds,
		}
	}

	var cascadeModel *v1alpha1.CascadeModel
	if body.CascadeModel != nil && body.CascadeModel.AffectedTargets != nil && len(*body.CascadeModel.AffectedTargets) > 0 {
		affected := make([]v1alpha1.AffectedTarget, len(*body.CascadeModel.AffectedTargets))
		for i, t := range *body.CascadeModel.AffectedTargets {
			affected[i] = v1alpha1.AffectedTarget{URI: t.URI, EffectType: string(t.EffectType)}
		}
		cascadeModel = &v1alpha1.CascadeModel{AffectedTargets: affected}
		if body.CascadeModel.ModelSourceTrust != nil {
			s := string(*body.CascadeModel.ModelSourceTrust)
			cascadeModel.ModelSourceTrust = &s
		}
		if body.CascadeModel.ModelSourceID != nil {
			cascadeModel.ModelSourceID = body.CascadeModel.ModelSourceID
		}
	}

	var reasoningTrace *v1alpha1.ReasoningTrace
	if body.ReasoningTrace != nil {
		rt := &v1alpha1.ReasoningTrace{}
		if body.ReasoningTrace.ConfidenceScore != nil {
			rt.ConfidenceScore = body.ReasoningTrace.ConfidenceScore
		}
		if body.ReasoningTrace.ComponentConfidence != nil {
			rt.ComponentConfidence = *body.ReasoningTrace.ComponentConfidence
		}
		if body.ReasoningTrace.TraceReference != nil {
			rt.TraceReference = body.ReasoningTrace.TraceReference
		}
		if body.ReasoningTrace.Alternatives != nil {
			rt.Alternatives = *body.ReasoningTrace.Alternatives
		}
		reasoningTrace = rt
	}

	agentReq := &v1alpha1.AgentRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", sanitizeDNSSegment(body.AgentIdentity, 57)),
			Namespace:    ns,
			Labels:       reqLabels,
		},
		Spec: v1alpha1.AgentRequestSpec{
			AgentIdentity:  body.AgentIdentity,
			Action:         body.Action,
			Target:         v1alpha1.Target{URI: body.Target.URI},
			Reason:         body.Reason,
			CascadeModel:   cascadeModel,
			ReasoningTrace: reasoningTrace,
			Parameters:     parameters,
			ExecutionMode:  executionMode,
			ScopeBounds:    scopeBounds,
		},
	}

	if body.Target.ResourceType != nil {
		agentReq.Spec.Target.ResourceType = body.Target.ResourceType
	}
	if body.Target.Attributes != nil {
		agentReq.Spec.Target.Attributes = *body.Target.Attributes
	}

	// GovernedResource admission: URI → agent identity → action (per design doc order).
	var matchedGR *v1alpha1.GovernedResource
	var govResources v1alpha1.GovernedResourceList
	if err := s.client.List(r.Context(), &govResources); err != nil {
		if !strings.Contains(err.Error(), "no matches for kind") {
			writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("failed to list GovernedResources: %v", err))
			return
		}
	}

	if len(govResources.Items) == 0 && !s.requireGovernedResource {
		goto admissionPassed
	}

	matchedGR = matchGovernedResource(govResources.Items, body.Target.URI)
	if matchedGR == nil {
		writeProblem(w, http.StatusForbidden, v1alpha1.DenialCodeActionNotPermitted)
		return
	}

	if len(matchedGR.Spec.PermittedAgents) > 0 {
		if !slices.Contains(matchedGR.Spec.PermittedAgents, body.AgentIdentity) {
			writeProblem(w, http.StatusForbidden, v1alpha1.DenialCodeIdentityInvalid)
			return
		}
	}

	if !slices.Contains(matchedGR.Spec.PermittedActions, body.Action) {
		writeProblem(w, http.StatusForbidden, v1alpha1.DenialCodeActionNotPermitted)
		return
	}

admissionPassed:
	if matchedGR != nil {
		agentReq.Spec.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
			Name:       matchedGR.Name,
			Generation: matchedGR.Generation,
		}
	}

	existing, err := s.checkDuplicate(r.Context(), body.AgentIdentity, body.Action, body.Target.URI, ns)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing != nil {
		agentRequestDedupTotal.Inc()
		writeJSON(w, http.StatusOK, agentRequestToDTO(*existing))
		return
	}

	if err := s.client.Create(r.Context(), agentReq); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsAlreadyExists(err) {
			writeProblem(w, http.StatusBadRequest, fmt.Sprintf("invalid AgentRequest: %v", err))
			return
		}
		writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("failed to create AgentRequest: %v", err))
		return
	}
	agentRequestCreatedTotal.WithLabelValues(body.AgentIdentity).Inc()

	writeJSON(w, http.StatusCreated, agentRequestToDTO(*agentReq))
}

// GET /v1alpha1/agent-requests/{name}
func (s *Server) v1alpha1GetAgentRequest(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	name := r.PathValue("name")
	ns := resolveNamespace(r)

	var ar v1alpha1.AgentRequest
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &ar); err != nil {
		if apierrors.IsNotFound(err) {
			writeProblem(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("failed to get AgentRequest: %v", err))
		}
		return
	}

	writeJSON(w, http.StatusOK, agentRequestToDTO(ar))
}

// POST /v1alpha1/agent-requests/{name}/phase
func (s *Server) v1alpha1TransitionAgentRequestPhase(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	name := r.PathValue("name")
	ns := resolveNamespace(r)

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	var body v1alpha1openapi.TransitionPhaseRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !body.TargetPhase.Valid() {
		writeProblem(w, http.StatusBadRequest,
			"invalid targetPhase: must be Executing, Completed, Approved, or Denied")
		return
	}

	var ar v1alpha1.AgentRequest
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &ar); err != nil {
		if apierrors.IsNotFound(err) {
			writeProblem(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeProblem(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	targetPhase := string(body.TargetPhase)
	currentPhase := ar.Status.Phase
	groups := callerGroupsFromCtx(r.Context())
	reason := ""
	if body.Reason != nil {
		reason = strings.TrimSpace(*body.Reason)
	}

	switch targetPhase {
	case v1alpha1.PhaseExecuting:
		if !requireRoleV1(s.roles, roleAgent, sub, groups, w) {
			return
		}
		if currentPhase != v1alpha1.PhaseApproved {
			writeProblem(w, http.StatusConflict,
				fmt.Sprintf("request is in phase %q — can only transition to Executing from Approved", currentPhase))
			return
		}
		if ar.Spec.AgentIdentity != sub {
			writeProblem(w, http.StatusForbidden, "forbidden: only the creating agent may transition this request")
			return
		}
		s.applyConditionTransition(w, r, &ar, v1alpha1.ConditionExecuting, "AgentStarted", "Agent is now executing action")

	case v1alpha1.PhaseCompleted:
		if !requireRoleV1(s.roles, roleAgent, sub, groups, w) {
			return
		}
		if currentPhase != v1alpha1.PhaseExecuting {
			writeProblem(w, http.StatusConflict,
				fmt.Sprintf("request is in phase %q — can only transition to Completed from Executing", currentPhase))
			return
		}
		if ar.Spec.AgentIdentity != sub {
			writeProblem(w, http.StatusForbidden, "forbidden: only the creating agent may transition this request")
			return
		}
		s.applyConditionTransition(w, r, &ar, v1alpha1.ConditionCompleted, "ActionSuccess", "Agent successfully completed the action")

	case v1alpha1.PhaseApproved:
		if !requireRoleV1(s.roles, roleReviewer, sub, groups, w) {
			return
		}
		if currentPhase != v1alpha1.PhasePending {
			writeProblem(w, http.StatusConflict,
				fmt.Sprintf("request is in phase %q — can only approve when Pending", currentPhase))
			return
		}
		if ar.Spec.AgentIdentity == sub {
			writeProblem(w, http.StatusForbidden, "forbidden: self-approval not permitted")
			return
		}
		cpv := ar.Status.ControlPlaneVerification
		if cpv != nil && cpv.HasActiveEndpoints && reason == "" {
			writeProblem(w, http.StatusBadRequest,
				"reason required: control plane verified active endpoints — explain why this override is safe")
			return
		}
		s.applyHumanDecisionV1(w, r, &ar, "approved", reason)

	case v1alpha1.PhaseDenied:
		if !requireRoleV1(s.roles, roleReviewer, sub, groups, w) {
			return
		}
		if currentPhase != v1alpha1.PhasePending {
			writeProblem(w, http.StatusConflict,
				fmt.Sprintf("request is in phase %q — can only deny when Pending", currentPhase))
			return
		}
		if ar.Spec.AgentIdentity == sub {
			writeProblem(w, http.StatusForbidden, "forbidden: self-denial not permitted")
			return
		}
		if reason == "" {
			reason = "denied via API"
		}
		s.applyHumanDecisionV1(w, r, &ar, "denied", reason)
	}
}

// applyConditionTransition patches a condition on the AgentRequest and returns the canonical DTO.
func (s *Server) applyConditionTransition(
	w http.ResponseWriter, r *http.Request,
	ar *v1alpha1.AgentRequest,
	conditionType, reason, message string,
) {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current v1alpha1.AgentRequest
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: ar.Name, Namespace: ar.Namespace}, &current); err != nil {
			return err
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		})
		if err := s.client.Status().Update(r.Context(), &current); err != nil {
			return err
		}
		*ar = current
		return nil
	}); err != nil {
		if apierrors.IsNotFound(err) {
			writeProblem(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("failed to update status: %v", err))
		}
		return
	}

	agentRequestPhaseTransitionTotal.WithLabelValues(ar.Status.Phase, conditionType).Inc()
	writeJSON(w, http.StatusOK, agentRequestToDTO(*ar))
}

// applyHumanDecisionV1 patches HumanApproval on the spec and returns the canonical DTO.
func (s *Server) applyHumanDecisionV1(
	w http.ResponseWriter, r *http.Request,
	ar *v1alpha1.AgentRequest,
	decision, reason string,
) {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current v1alpha1.AgentRequest
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: ar.Name, Namespace: ar.Namespace}, &current); err != nil {
			return err
		}
		patch := client.MergeFrom(current.DeepCopy())
		current.Spec.HumanApproval = &v1alpha1.HumanApproval{
			Decision:      decision,
			Reason:        reason,
			ForGeneration: current.Status.EvaluationGeneration,
		}
		if err := s.client.Patch(r.Context(), &current, patch); err != nil {
			return err
		}
		*ar = current
		return nil
	}); err != nil {
		if apierrors.IsNotFound(err) {
			writeProblem(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("failed to patch approval: %v", err))
		}
		return
	}

	targetPhase := v1alpha1.PhaseApproved
	if decision == "denied" {
		targetPhase = v1alpha1.PhaseDenied
	}
	agentRequestPhaseTransitionTotal.WithLabelValues(v1alpha1.PhasePending, targetPhase).Inc()
	writeJSON(w, http.StatusOK, agentRequestToDTO(*ar))
}

// PATCH /v1alpha1/agent-requests/{name}/verdict
func (s *Server) v1alpha1SubmitAgentRequestVerdict(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	name := r.PathValue("name")
	ns := resolveNamespace(r)

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRoleV1(s.roles, roleReviewer, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	var body v1alpha1openapi.SubmitVerdictRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !body.Verdict.Valid() {
		writeProblem(w, http.StatusBadRequest, "invalid verdict: must be correct, incorrect, or partial")
		return
	}

	verdict := string(body.Verdict)
	reasonCode := ""
	if body.ReasonCode != nil {
		reasonCode = string(*body.ReasonCode)
	}

	if verdict != verdictCorrect && reasonCode == "" {
		writeProblem(w, http.StatusBadRequest, "reasonCode is required when verdict is not 'correct'")
		return
	}

	validReasonCodes := []string{"wrong_diagnosis", "bad_timing", "scope_too_broad", "precautionary", "policy_block"}
	if reasonCode != "" && !slices.Contains(validReasonCodes, reasonCode) {
		writeProblem(w, http.StatusBadRequest,
			fmt.Sprintf("invalid reasonCode %q; must be one of: %s", reasonCode, strings.Join(validReasonCodes, ", ")))
		return
	}

	note := ""
	if body.Note != nil {
		note = *body.Note
	}

	var ar v1alpha1.AgentRequest
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &ar); err != nil {
			return err
		}
		if ar.Status.Phase != v1alpha1.PhaseAwaitingVerdict {
			return fmt.Errorf("request is in phase %q: %w", ar.Status.Phase, errVerdictWrongPhase)
		}

		now := metav1.Now()
		base := ar.DeepCopy()
		ar.Status.Verdict = verdict
		ar.Status.VerdictReasonCode = reasonCode
		ar.Status.VerdictNote = note
		ar.Status.VerdictBy = sub
		ar.Status.VerdictAt = &now
		ar.Status.Phase = v1alpha1.PhaseCompleted

		return s.client.Status().Patch(r.Context(), &ar, client.MergeFrom(base))
	}); err != nil {
		log.Printf("ERROR: v1alpha1SubmitAgentRequestVerdict failed for %s: %v", name, err)
		if apierrors.IsNotFound(err) {
			writeProblem(w, http.StatusNotFound, "AgentRequest not found")
		} else if errors.Is(err, errVerdictWrongPhase) {
			writeProblem(w, http.StatusConflict, err.Error())
		} else {
			writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("failed to submit verdict: %v", err))
		}
		return
	}

	agentRequestVerdictTotal.WithLabelValues(verdict).Inc()
	writeJSON(w, http.StatusOK, agentRequestToDTO(ar))
}
