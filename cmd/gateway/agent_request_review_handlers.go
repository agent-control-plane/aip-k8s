package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func (s *Server) handleVerdictAgentRequest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleReviewer, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	var body struct {
		Verdict    string `json:"verdict"`
		ReasonCode string `json:"reasonCode,omitempty"`
		Note       string `json:"note,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Verdict != verdictCorrect && body.Verdict != verdictIncorrect && body.Verdict != verdictPartial {
		writeError(w, http.StatusBadRequest, "invalid verdict")
		return
	}

	if body.Verdict != verdictCorrect && body.ReasonCode == "" {
		writeError(w, http.StatusBadRequest, "reasonCode is required when verdict is not 'correct'")
		return
	}

	validReasonCodes := []string{"wrong_diagnosis", "bad_timing", "scope_too_broad", "precautionary", "policy_block"}
	if body.ReasonCode != "" && !slices.Contains(validReasonCodes, body.ReasonCode) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid reasonCode %q; must be one of: %s", body.ReasonCode, strings.Join(validReasonCodes, ", ")))
		return
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var agentReq v1alpha1.AgentRequest
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &agentReq); err != nil {
			return err
		}

		if agentReq.Status.Phase != v1alpha1.PhaseAwaitingVerdict {
			return fmt.Errorf("request is in phase %q: %w", agentReq.Status.Phase, errVerdictWrongPhase)
		}
		if agentReq.Status.Verdict != "" {
			return fmt.Errorf("request already graded as %q: %w", agentReq.Status.Verdict, errVerdictWrongPhase)
		}

		now := metav1.Now()
		base := agentReq.DeepCopy()
		agentReq.Status.Verdict = body.Verdict
		agentReq.Status.VerdictReasonCode = body.ReasonCode
		agentReq.Status.VerdictNote = body.Note
		agentReq.Status.VerdictBy = sub
		agentReq.Status.VerdictAt = &now
		// Phase transition to Completed is driven by the controller after it
		// detects Verdict != "" and emits the verdict.submitted AuditRecord.

		return s.client.Status().Patch(r.Context(), &agentReq, client.MergeFromWithOptions(base,
			client.MergeFromWithOptimisticLock{}))
	}); err != nil {
		log.Printf("ERROR: handleVerdictAgentRequest failed for %s: %v", name, err)
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentRequest not found")
		} else if errors.Is(err, errVerdictWrongPhase) {
			writeError(w, http.StatusConflict, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to submit verdict: %v", err))
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"message": "verdict submitted"})
}

func (s *Server) handleApproveAgentRequest(w http.ResponseWriter, r *http.Request) {
	s.handleHumanDecision(w, r, "approved")
}

func (s *Server) handleDenyAgentRequest(w http.ResponseWriter, r *http.Request) {
	s.handleHumanDecision(w, r, "denied")
}

func (s *Server) handleHumanDecision(w http.ResponseWriter, r *http.Request, decision string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleReviewer, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var body struct {
		Reason string `json:"reason"`
	}
	if r.Header.Get("Content-Type") == "application/json" {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	var agentReq v1alpha1.AgentRequest
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &agentReq); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if agentReq.Spec.AgentIdentity == sub {
		writeError(w, http.StatusForbidden, "forbidden: self-approval not permitted")
		return
	}

	phase := agentReq.Status.Phase
	if phase != v1alpha1.PhasePending {
		msg := fmt.Sprintf("request is in phase %q — can only approve/deny when Pending", phase)
		writeError(w, http.StatusConflict, msg)
		return
	}

	if decision == "approved" {
		cpv := agentReq.Status.ControlPlaneVerification
		if cpv != nil && cpv.HasActiveEndpoints && strings.TrimSpace(body.Reason) == "" {
			msg := "reason required: control plane verified active endpoints " +
				"— explain why this override is safe"
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}

	humanReason := strings.TrimSpace(body.Reason)
	if humanReason == "" && decision == "denied" {
		humanReason = "denied via dashboard"
	}

	response := map[string]any{
		"name": agentReq.Name,
	}

	// Mint token *before* persisting approval so a mint failure does not leave
	// the request approved without a token.
	if decision == "approved" && s.jwtManager != nil {
		token, expiresAt, err := s.jwtManager.MintToken(
			agentReq.Spec.AgentIdentity,
			agentReq.Spec.Action,
			agentReq.Spec.Target.URI,
			agentReq.Name,
		)
		if err != nil {
			log.Printf("Failed to mint JWT token: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "approval failed: could not mint token",
			})
			return
		}
		response["token"] = token
		response["token_expires_at"] = expiresAt.Format(time.RFC3339)
	}

	reviewer := sub
	if reviewer == "" {
		reviewer = "unknown (admin)"
	}
	patch := client.MergeFrom(agentReq.DeepCopy())
	agentReq.Spec.HumanApproval = &v1alpha1.HumanApproval{
		Decision:      decision,
		Reason:        humanReason,
		ForGeneration: agentReq.Status.EvaluationGeneration,
		ApprovedBy:    reviewer,
	}

	if err := s.client.Patch(r.Context(), &agentReq, patch); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	response["phase"] = agentReq.Status.Phase
	writeJSON(w, http.StatusOK, response)
}
