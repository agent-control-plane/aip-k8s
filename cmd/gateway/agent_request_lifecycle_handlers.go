package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

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
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
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
		"result":                   current.Status.Result,
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

//nolint:dupl // structurally similar to handleCompletedAgentRequest
func (s *Server) handlePutAgentRequestResult(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	// Validate input before any K8s API call so malformed requests are rejected cheaply.
	var body struct {
		URL     string `json:"url"`
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.URL == "" || !strings.HasPrefix(body.URL, "https://") || len(body.URL) < 9 {
		writeError(w, http.StatusBadRequest, "url must be a valid https URL")
		return
	}
	if utf8.RuneCountInString(body.Summary) > 512 {
		writeError(w, http.StatusBadRequest, "summary must be at most 512 characters")
		return
	}

	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var req v1alpha1.AgentRequest
	if err := s.apiReader.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &req); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// Only the creating agent can record a result.
	if s.authRequired && req.Spec.AgentIdentity != sub {
		writeError(w, http.StatusForbidden, "forbidden: only the creating agent may record a result")
		return
	}

	var wrongPhase string
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current v1alpha1.AgentRequest
		if err := s.apiReader.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &current); err != nil {
			return err
		}

		// Allow recording a result on Executing OR Completed. Accepting Completed
		// closes the ordering race: if the agent calls /completed before /result
		// (accidentally or under a crash-restart), the result is not silently lost.
		// All other phases are terminal-without-result or pre-execution — reject them.
		if current.Status.Phase != v1alpha1.PhaseExecuting &&
			current.Status.Phase != v1alpha1.PhaseCompleted {
			wrongPhase = current.Status.Phase
			return fmt.Errorf("wrong phase") // not a 409 conflict; RetryOnConflict will not retry this
		}

		base := current.DeepCopy()
		current.Status.Result = &v1alpha1.AgentRequestResult{
			URL:     body.URL,
			Summary: body.Summary,
		}
		return s.client.Status().Patch(r.Context(), &current, client.MergeFrom(base))
	}); err != nil {
		if wrongPhase != "" {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("request is in phase %q — result can only be recorded in Executing or Completed", wrongPhase))
			return
		}
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else {
			writeError(w, http.StatusInternalServerError,
				fmt.Sprintf("failed to record result: %v", err))
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "result recorded"})
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

		base := current.DeepCopy()
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		})

		return s.client.Status().Patch(r.Context(), &current, client.MergeFrom(base))
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
	// Single-phase: use field indexer — server-side, pagination-correct.
	if len(requestedPhases) == 1 {
		listOpts = append(listOpts, client.MatchingFields{agentRequestPhaseIndexKey: requestedPhases[0]})
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

	// Multi-phase (>1) fallback: client-side filter after the K8s List.
	// Single-phase uses the field indexer (server-side) above.
	if len(requestedPhases) > 1 {
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

// validateOIDCSubject checks whether sub appears in reg.Spec.OIDC.AllowedSubjects.
// Returns nil when the registration has no OIDC config (no subject enforcement).
// Replaces the old equality check that used 400 and required exact agentIdentity==sub.
func validateOIDCSubject(reg *v1alpha1.AgentRegistration, sub string) error {
	if reg.Spec.OIDC == nil {
		return nil
	}
	if len(reg.Spec.OIDC.AllowedSubjects) > 0 {
		if slices.Contains(reg.Spec.OIDC.AllowedSubjects, sub) {
			return nil
		}
		return fmt.Errorf("token subject %q not in allowedSubjects for agent %q",
			sub, reg.Spec.AgentIdentity)
	}
	// Fallback when AllowedSubjects is empty: sub must match the agent identity (or sub is empty)
	if sub == "" || sub == reg.Spec.AgentIdentity {
		return nil
	}
	return fmt.Errorf("token subject %q does not match agentIdentity %q",
		sub, reg.Spec.AgentIdentity)
}
