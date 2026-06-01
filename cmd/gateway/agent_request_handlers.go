package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

// validPhases is the set of known AgentRequest phases for client-side filtering.
// Kept in sync with the constants in api/v1alpha1/agentrequest_types.go.
var validPhases = map[string]bool{
	v1alpha1.PhasePending:         true,
	v1alpha1.PhaseApproved:        true,
	v1alpha1.PhaseDenied:          true,
	v1alpha1.PhaseExecuting:       true,
	v1alpha1.PhaseCompleted:       true,
	v1alpha1.PhaseFailed:          true,
	v1alpha1.PhaseAwaitingVerdict: true,
	v1alpha1.PhaseExpired:         true,
	v1alpha1.PhaseObserved:        true,
}

// checkDuplicate returns a non-nil error and writes a 409 if an active request
// for the same (agentIdentity, action, targetURI) exists within the dedup window.
// Returns nil (and writes nothing) when no duplicate is found or dedup is disabled.
//
// Note: the List→Create sequence is not atomic. Concurrent requests with the
// same key can both pass this check and both be created. This is intentional:
// dedup provides best-effort protection against reconciliation-loop floods, not
// a hard mutual-exclusion guarantee. A strict guarantee would require a
// ValidatingAdmissionWebhook or a unique server-side constraint.
func (s *Server) checkDuplicate(
	ctx context.Context, agentIdentity, action, targetURI, ns string,
) (*v1alpha1.AgentRequest, error) {
	if s.dedupWindow == 0 {
		return nil, nil
	}
	var existing v1alpha1.AgentRequestList
	if err := s.client.List(ctx, &existing,
		client.InNamespace(ns),
		client.MatchingLabels{"aip.io/agentIdentity": sanitizeLabelValue(agentIdentity)},
	); err != nil {
		return nil, fmt.Errorf("failed to check for duplicate requests: %v", err)
	}
	cutoff := time.Now().Add(-s.dedupWindow)
	for _, req := range existing.Items {
		if terminalPhases[req.Status.Phase] {
			continue
		}
		if !req.CreationTimestamp.IsZero() && req.CreationTimestamp.Time.Before(cutoff) {
			continue
		}
		if req.Spec.AgentIdentity == agentIdentity &&
			req.Spec.Action == action &&
			req.Spec.Target.URI == targetURI {
			return &req, nil
		}
	}
	return nil, nil
}

//nolint:gocyclo // handler covers full admission pipeline: auth, dedup, GR match, SoakMode, create, poll
func (s *Server) handleCreateAgentRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	var body createAgentRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.AgentIdentity == "" || body.Action == "" || body.TargetURI == "" {
		writeError(w, http.StatusBadRequest, "agentIdentity, action, and targetURI are required")
		return
	}

	if s.authRequired && body.AgentIdentity != sub {
		writeError(w, http.StatusBadRequest, "agentIdentity must match authenticated caller")
		return
	}

	ns := body.Namespace
	if ns == "" {
		ns = defaultNamespace
	}

	var parameters *apiextensionsv1.JSON
	if len(body.Parameters) > 0 && string(body.Parameters) != "null" {
		parameters = &apiextensionsv1.JSON{Raw: body.Parameters}
	}

	reqLabels := map[string]string{
		"aip.io/agentIdentity": sanitizeLabelValue(body.AgentIdentity),
		"aip.io/profileName":   v1alpha1.ProfileNameForAgent(body.AgentIdentity),
	}
	if body.CorrelationID != "" {
		reqLabels["aip.io/correlationID"] = sanitizeLabelValue(body.CorrelationID)
	}
	if body.Classification != "" {
		reqLabels["aip.io/classification"] = sanitizeLabelValue(body.Classification)
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
			Target:         v1alpha1.Target{URI: body.TargetURI},
			Reason:         body.Reason,
			Classification: normalizeClassification(body.Classification),
			CascadeModel:   buildCascadeModel(&body),
			ReasoningTrace: buildReasoningTrace(&body),
			Parameters:     parameters,
			ExecutionMode:  body.ExecutionMode,
			ScopeBounds:    body.ScopeBounds,
		},
	}

	// GovernedResource admission: URI → agent identity → action (per design doc order).
	var matchedGR *v1alpha1.GovernedResource
	var govResources v1alpha1.GovernedResourceList
	var agentPermitted, actionPermitted bool
	if err := s.client.List(r.Context(), &govResources); err != nil {
		// If the CRD is not yet installed, treat as an empty list.
		// This allows the system to boot gracefully even if the GovernedResource CRD
		// is not yet available (e.g., during cluster initialization in e2e tests).
		if meta.IsNoMatchError(err) {
			// CRD not yet installed — treat as empty list
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list GovernedResources: %v", err))
			return
		}
	}

	// Backward compat: skip check when no GovernedResources exist and flag is false.
	if len(govResources.Items) == 0 && !s.requireGovernedResource {
		goto admissionPassed
	}

	// URI-only match here — the action check below (with its audit-trail path) is
	// the authoritative gate for the HTTP handler. Passing action="" keeps the
	// most-specific-URI-wins semantics intact across multiple GovernedResources:
	// a more-specific GR that restricts actions must not be bypassed by falling
	// back to a less-specific GR that happens to permit the action.
	matchedGR = matchGovernedResource(govResources.Items, body.TargetURI, "")
	if matchedGR == nil {
		writeError(w, http.StatusForbidden, v1alpha1.DenialCodeActionNotPermitted)
		return
	}

	// Check agent identity (step 3 in design doc).
	if len(matchedGR.Spec.PermittedAgents) > 0 {
		agentPermitted = slices.Contains(matchedGR.Spec.PermittedAgents, body.AgentIdentity)
		if !agentPermitted {
			writeError(w, http.StatusForbidden, v1alpha1.DenialCodeIdentityInvalid)
			return
		}
	}

	// Check action (step 4 in design doc).
	actionPermitted = slices.Contains(matchedGR.Spec.PermittedActions, body.Action)
	if !actionPermitted {
		// Create an AgentRequest with the gateway-denied annotation so the controller
		// emits a request.denied AuditRecord — scope escalation attempts must be visible
		// in the audit trail even though no K8s resource would otherwise be created.
		agentReq.Spec.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
			Name:       matchedGR.Name,
			Generation: matchedGR.Generation,
		}
		if agentReq.Annotations == nil {
			agentReq.Annotations = make(map[string]string)
		}
		agentReq.Annotations[v1alpha1.AnnotationGatewayDenied] = v1alpha1.DenialCodeActionNotPermitted
		createErr := s.client.Create(r.Context(), agentReq)
		if createErr == nil {
			patchBase := agentReq.DeepCopy()
			agentReq.Status.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
				Name:       matchedGR.Name,
				Generation: matchedGR.Generation,
			}
			if patchErr := s.client.Status().Patch(r.Context(), agentReq, client.MergeFrom(patchBase)); patchErr != nil {
				log.Printf("Failed to patch GovernedResourceRef to status for denied request: %v", patchErr)
			}
		} else {
			log.Printf("failed to create gateway-denied AgentRequest for audit trail: %v", createErr)
		}
		writeError(w, http.StatusForbidden, v1alpha1.DenialCodeActionNotPermitted)
		return
	}

admissionPassed:
	if matchedGR != nil {
		agentReq.Spec.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
			Name:       matchedGR.Name,
			Generation: matchedGR.Generation,
		}
		// SoakMode phase initialization is handled exclusively by the controller.
		// The gateway sets GovernedResourceRef so the controller can detect SoakMode
		// on its first reconcile and route to PhaseAwaitingVerdict.
	}

	// Trust gate: enforce trust level requirements from GovernedResource.
	if matchedGR != nil && matchedGR.Spec.TrustRequirements != nil {
		trustResult, err := s.evaluateTrustGate(r.Context(), ns, body.AgentIdentity, agentReq.Spec.Mode, matchedGR)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("trust gate error: %v", err))
			return
		}
		if trustResult.rejected {
			writeError(w, http.StatusForbidden, fmt.Sprintf("INSUFFICIENT_TRUST: %s", trustResult.message))
			return
		}
		if trustResult.annotations != nil {
			if agentReq.Annotations == nil {
				agentReq.Annotations = make(map[string]string)
			}
			maps.Copy(agentReq.Annotations, trustResult.annotations)
		}
	}

	existing, err := s.checkDuplicate(r.Context(), body.AgentIdentity, body.Action, body.TargetURI, ns)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing != nil {
		payload := map[string]any{
			"name":                     existing.Name,
			"labels":                   reqLabels,
			"phase":                    existing.Status.Phase,
			"denial":                   existing.Status.Denial,
			"conditions":               existing.Status.Conditions,
			"controlPlaneVerification": existing.Status.ControlPlaneVerification,
		}
		if acceptsSSE(r) {
			rc := http.NewResponseController(w)
			writeSSEHeaders(w)
			if err := rc.Flush(); err != nil {
				log.Printf("SSE: failed to flush for duplicate %s: %v", existing.Name, err)
				return
			}
			if err := writeSSEEvent(w, rc, sseEventResult, payload); err != nil {
				log.Printf("SSE: failed to write duplicate result for %s: %v", existing.Name, err)
			}
			return
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}

	if err := s.client.Create(r.Context(), agentReq); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(w, http.StatusConflict, fmt.Sprintf("AgentRequest already exists: %v", err))
			return
		}
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid AgentRequest: %v", err))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create AgentRequest: %v", err))
		return
	}

	// Mirror GovernedResourceRef to Status for the controller to consume.
	// This is done after creation since Status is a subresource.
	if matchedGR != nil {
		patchBase := agentReq.DeepCopy()
		agentReq.Status.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
			Name:       matchedGR.Name,
			Generation: matchedGR.Generation,
		}
		if patchErr := s.client.Status().Patch(r.Context(), agentReq, client.MergeFrom(patchBase)); patchErr != nil {
			log.Printf("Failed to patch GovernedResourceRef to status: %v", patchErr)
		}
	}

	if acceptsSSE(r) {
		s.streamAgentRequestPhase(w, r, agentReq.Name, ns, reqLabels)
	} else {
		s.pollAgentRequestPhase(w, r, agentReq.Name, ns, reqLabels)
	}
}

func (s *Server) pollAgentRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
	name, ns string,
	reqLabels map[string]string,
) {
	ctx, cancel := context.WithTimeout(r.Context(), s.waitTimeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if r.Context().Err() == nil {
				// r.Context() is still live — our waitTimeout fired; write 504.
				writeError(w, http.StatusGatewayTimeout, "timed out waiting for AgentRequest resolution")
			}
			// r.Context() is done: client disconnected — can't write a response.
			return
		case <-ticker.C:
			var current v1alpha1.AgentRequest
			if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &current); err != nil {
				continue
			}

			phase := current.Status.Phase
			if phase == v1alpha1.PhaseApproved || phase == v1alpha1.PhaseDenied ||
				phase == v1alpha1.PhaseCompleted || phase == v1alpha1.PhaseFailed ||
				phase == v1alpha1.PhaseAwaitingVerdict {
				writeJSON(w, http.StatusCreated, map[string]any{
					"name":                     current.Name,
					"labels":                   reqLabels,
					"phase":                    current.Status.Phase,
					"denial":                   current.Status.Denial,
					"conditions":               current.Status.Conditions,
					"controlPlaneVerification": current.Status.ControlPlaneVerification,
				})
				return
			}

			// Return early when human approval is required — the agent
			// should not block waiting for a human decision.
			if phase == v1alpha1.PhasePending &&
				meta.IsStatusConditionTrue(current.Status.Conditions, v1alpha1.ConditionRequiresApproval) {
				writeJSON(w, http.StatusCreated, map[string]any{
					"name":                     current.Name,
					"labels":                   reqLabels,
					"phase":                    current.Status.Phase,
					"conditions":               current.Status.Conditions,
					"controlPlaneVerification": current.Status.ControlPlaneVerification,
				})
				return
			}
		}
	}
}

func (s *Server) handleGetAgentRequest(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var current v1alpha1.AgentRequest
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &current); err != nil {
		writeError(w, http.StatusNotFound, "AgentRequest not found")
		return
	}

	var auditList v1alpha1.AuditRecordList
	if err := s.client.List(r.Context(), &auditList, client.InNamespace(ns)); err != nil {
		log.Printf("failed to list AuditRecords: %v", err)
		// continue regardless, just return empty list
	}

	auditEvents := []string{}
	for _, a := range auditList.Items {
		if a.Spec.AgentRequestRef == name {
			auditEvents = append(auditEvents, a.Spec.Event)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":                     current.Name,
		"phase":                    current.Status.Phase,
		"denial":                   current.Status.Denial,
		"conditions":               current.Status.Conditions,
		"controlPlaneVerification": current.Status.ControlPlaneVerification,
		"auditEvents":              auditEvents,
	})
}

//nolint:dupl // structurally similar to handleCompletedAgentRequest
func (s *Server) handleExecutingAgentRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var req v1alpha1.AgentRequest
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &req); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	if s.authRequired && req.Spec.AgentIdentity != sub {
		writeError(w, http.StatusForbidden, "forbidden: only the creating agent may transition this request")
		return
	}

	if req.Status.Phase != v1alpha1.PhaseApproved {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("request is in phase %q — can only transition to Executing from Approved", req.Status.Phase))
		return
	}

	s.patchAgentRequestCondition(w, r, v1alpha1.ConditionExecuting, "AgentStarted", "Agent is now executing action")
}

//nolint:dupl // structurally similar to handleExecutingAgentRequest
func (s *Server) handleCompletedAgentRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var req v1alpha1.AgentRequest
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &req); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	if s.authRequired && req.Spec.AgentIdentity != sub {
		writeError(w, http.StatusForbidden, "forbidden: only the creating agent may transition this request")
		return
	}

	if req.Status.Phase != v1alpha1.PhaseExecuting {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("request is in phase %q — can only transition to Completed from Executing", req.Status.Phase))
		return
	}

	s.patchAgentRequestCondition(w, r, v1alpha1.ConditionCompleted,
		"ActionSuccess", "Agent successfully completed the action")
}

func (s *Server) patchAgentRequestCondition(
	w http.ResponseWriter, r *http.Request, conditionType, reason, message string,
) {
	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current v1alpha1.AgentRequest
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &current); err != nil {
			return err
		}

		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		})

		return s.client.Status().Update(r.Context(), &current)
	}); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update status: %v", err))
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message": fmt.Sprintf("successfully patched condition %s", conditionType),
	})
}

func (s *Server) handleListAgentRequests(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	agentID := r.URL.Query().Get("agentIdentity")
	correlID := r.URL.Query().Get("correlationID")
	classification := r.URL.Query().Get("classification")
	phaseParam := r.URL.Query().Get("phase")
	limitStr := r.URL.Query().Get("limit")
	continueToken := r.URL.Query().Get("continue")

	var requestedPhases []string
	if phaseParam != "" {
		for p := range strings.SplitSeq(phaseParam, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !validPhases[p] {
				valid := make([]string, 0, len(validPhases))
				for ph := range validPhases {
					valid = append(valid, ph)
				}
				slices.Sort(valid)
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("invalid phase %q: must be one of %s", p, strings.Join(valid, ", ")))
				return
			}
			requestedPhases = append(requestedPhases, p)
		}
	}

	listOpts := []client.ListOption{client.InNamespace(ns)}
	matchLabels := map[string]string{}
	if agentID != "" {
		matchLabels["aip.io/agentIdentity"] = sanitizeLabelValue(agentID)
	}
	if correlID != "" {
		matchLabels["aip.io/correlationID"] = sanitizeLabelValue(correlID)
	}
	if classification != "" {
		matchLabels["aip.io/classification"] = sanitizeLabelValue(classification)
	}
	if len(matchLabels) > 0 {
		listOpts = append(listOpts, client.MatchingLabels(matchLabels))
	}
	if limitStr != "" {
		limit, err := strconv.ParseInt(limitStr, 10, 64)
		if err != nil || limit <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit: must be a positive integer")
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
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := list.Items
	if items == nil {
		items = []v1alpha1.AgentRequest{}
	}

	if len(requestedPhases) > 0 {
		filtered := make([]v1alpha1.AgentRequest, 0, len(items))
		for _, item := range items {
			if slices.Contains(requestedPhases, item.Status.Phase) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	if limitStr != "" || continueToken != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"items":    items,
			"continue": list.Continue,
		})
	} else {
		writeJSON(w, http.StatusOK, items)
	}
}
