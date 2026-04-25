package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	v1alpha1openapi "github.com/agent-control-plane/aip-k8s/internal/openapi/v1alpha1"
)

// diagToDTO maps a CRD AgentDiagnostic to the canonical v1alpha1 API DTO.
// This single mapping is used across GET, LIST, and POST responses (#3).
func diagToDTO(d v1alpha1.AgentDiagnostic) v1alpha1openapi.AgentDiagnostic {
	dto := v1alpha1openapi.AgentDiagnostic{
		Name:           d.Name,
		Namespace:      d.Namespace,
		CreatedAt:      d.CreationTimestamp.Time,
		AgentIdentity:  d.Spec.AgentIdentity,
		DiagnosticType: d.Spec.DiagnosticType,
		CorrelationID:  d.Spec.CorrelationID,
		Summary:        d.Spec.Summary,
	}
	if d.Spec.Details != nil {
		raw := json.RawMessage(d.Spec.Details.Raw)
		dto.Details = &raw
	}
	if d.Status.Verdict != "" || d.Status.ReviewedBy != "" || d.Status.ReviewedAt != nil {
		status := &v1alpha1openapi.AgentDiagnosticStatus{}
		if d.Status.Verdict != "" {
			v := v1alpha1openapi.AgentDiagnosticStatusVerdict(d.Status.Verdict)
			status.Verdict = &v
		}
		if d.Status.ReviewedBy != "" {
			status.ReviewedBy = &d.Status.ReviewedBy
		}
		if d.Status.ReviewedAt != nil {
			t := d.Status.ReviewedAt.Time
			status.ReviewedAt = &t
		}
		if d.Status.ReviewerNote != "" {
			status.ReviewerNote = &d.Status.ReviewerNote
		}
		dto.Status = status
	}
	return dto
}

// summaryToDTO maps a CRD DiagnosticAccuracySummary to its API DTO.
func summaryToDTO(s v1alpha1.DiagnosticAccuracySummary) v1alpha1openapi.AccuracySummary {
	dto := v1alpha1openapi.AccuracySummary{
		Name:           s.Name,
		Namespace:      s.Namespace,
		AgentIdentity:  s.Spec.AgentIdentity,
		TotalReviewed:  s.Status.TotalReviewed,
		CorrectCount:   s.Status.CorrectCount,
		PartialCount:   s.Status.PartialCount,
		IncorrectCount: s.Status.IncorrectCount,
	}
	if s.Status.DiagnosticAccuracy != nil {
		dto.DiagnosticAccuracy = s.Status.DiagnosticAccuracy
	}
	if s.Status.LastUpdatedAt != nil {
		t := s.Status.LastUpdatedAt.Time
		dto.LastUpdatedAt = &t
	}
	return dto
}

func (s *Server) checkDiagnosticDuplicate(
	ctx context.Context, agentIdentity, diagnosticType, correlationID, ns string,
) (*v1alpha1.AgentDiagnostic, error) {
	if s.dedupWindow == 0 {
		return nil, nil
	}
	var existing v1alpha1.AgentDiagnosticList
	if err := s.client.List(ctx, &existing,
		client.InNamespace(ns),
		client.MatchingLabels{
			"aip.io/agentIdentity":  sanitizeLabelValue(agentIdentity),
			"aip.io/correlationID":  sanitizeLabelValue(correlationID),
			"aip.io/diagnosticType": sanitizeLabelValue(diagnosticType),
		},
	); err != nil {
		return nil, fmt.Errorf("failed to check for duplicate diagnostics: %v", err)
	}
	for _, d := range existing.Items {
		if d.Spec.AgentIdentity == agentIdentity &&
			d.Spec.DiagnosticType == diagnosticType &&
			d.Spec.CorrelationID == correlationID {
			return &d, nil
		}
	}
	return nil, nil
}

// resolveNamespace returns the namespace query param, falling back to defaultNamespace.
func resolveNamespace(r *http.Request) string {
	if ns := r.URL.Query().Get("namespace"); ns != "" {
		return ns
	}
	return defaultNamespace
}

// GET /v1alpha1/agent-diagnostics
func (s *Server) v1alpha1ListAgentDiagnostics(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	ns := resolveNamespace(r)
	agentID := r.URL.Query().Get("agentIdentity")
	correlID := r.URL.Query().Get("correlationID")
	limitStr := r.URL.Query().Get("limit")
	continueToken := r.URL.Query().Get("nextPageToken")
	caStr := r.URL.Query().Get("createdAfter")
	cbStr := r.URL.Query().Get("createdBefore")
	hasTimeFilter := caStr != "" || cbStr != ""

	if hasTimeFilter && (limitStr != "" || continueToken != "") {
		writeProblem(w, http.StatusBadRequest,
			"pagination (limit/nextPageToken) cannot be combined with createdAfter/createdBefore")
		return
	}

	ca, cb, parseErr := parseTimeRange(caStr, cbStr)
	if parseErr != nil {
		writeProblem(w, http.StatusBadRequest, parseErr.Error())
		return
	}

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

	var list v1alpha1.AgentDiagnosticList
	if err := s.client.List(r.Context(), &list, listOpts...); err != nil {
		if apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}

	var items []v1alpha1.AgentDiagnostic
	if hasTimeFilter {
		items = filterAndSortDiagnostics(list.Items, ca, cb)
	} else {
		items = list.Items
	}

	dtos := make([]v1alpha1openapi.AgentDiagnostic, len(items))
	for i, d := range items {
		dtos[i] = diagToDTO(d)
	}

	resp := v1alpha1openapi.AgentDiagnosticList{Items: dtos}
	if list.Continue != "" {
		resp.NextPageToken = &list.Continue
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /v1alpha1/agent-diagnostics
func (s *Server) v1alpha1CreateAgentDiagnostic(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRoleV1(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	var body v1alpha1openapi.CreateAgentDiagnosticRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.AgentIdentity == "" || body.DiagnosticType == "" || body.CorrelationID == "" || body.Summary == "" {
		writeProblem(w, http.StatusBadRequest, "agentIdentity, diagnosticType, correlationID, and summary are required")
		return
	}
	if s.authRequired && body.AgentIdentity != sub {
		writeProblem(w, http.StatusBadRequest, "agentIdentity must match authenticated caller")
		return
	}

	ns := resolveNamespace(r)

	existing, err := s.checkDiagnosticDuplicate(
		r.Context(), body.AgentIdentity, body.DiagnosticType, body.CorrelationID, ns)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing != nil {
		diagnosticDedupTotal.Inc()
		writeJSON(w, http.StatusOK, diagToDTO(*existing))
		return
	}

	var details *apiextensionsv1.JSON
	if body.Details != nil && len(*body.Details) > 0 && string(*body.Details) != jsonNull {
		details = &apiextensionsv1.JSON{Raw: *body.Details}
	}

	safeIdentityForName := sanitizeDNSSegment(body.AgentIdentity, 57)
	labelAgentIdentity := sanitizeLabelValue(body.AgentIdentity)
	labelCorrelationID := sanitizeLabelValue(body.CorrelationID)
	labelDiagnosticType := sanitizeLabelValue(body.DiagnosticType)

	diag := &v1alpha1.AgentDiagnostic{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("diag-%s-", safeIdentityForName),
			Namespace:    ns,
			Labels: map[string]string{
				"aip.io/correlationID":  labelCorrelationID,
				"aip.io/agentIdentity":  labelAgentIdentity,
				"aip.io/diagnosticType": labelDiagnosticType,
			},
		},
		Spec: v1alpha1.AgentDiagnosticSpec{
			AgentIdentity:  body.AgentIdentity,
			DiagnosticType: body.DiagnosticType,
			CorrelationID:  body.CorrelationID,
			Summary:        body.Summary,
			Details:        details,
		},
	}

	if err := s.client.Create(r.Context(), diag); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsAlreadyExists(err) {
			writeProblem(w, http.StatusBadRequest, fmt.Sprintf("invalid AgentDiagnostic: %v", err))
			return
		}
		writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("failed to create AgentDiagnostic: %v", err))
		return
	}
	diagnosticCreatedTotal.WithLabelValues(body.AgentIdentity).Inc()

	writeJSON(w, http.StatusCreated, diagToDTO(*diag))
}

// GET /v1alpha1/agent-diagnostics/{name}
func (s *Server) v1alpha1GetAgentDiagnostic(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	name := r.PathValue("name")
	ns := resolveNamespace(r)

	var diag v1alpha1.AgentDiagnostic
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &diag); err != nil {
		if apierrors.IsNotFound(err) {
			writeProblem(w, http.StatusNotFound, "AgentDiagnostic not found")
		} else {
			writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("failed to get AgentDiagnostic: %v", err))
		}
		return
	}

	writeJSON(w, http.StatusOK, diagToDTO(diag))
}

// PATCH /v1alpha1/agent-diagnostics/{name}/status
func (s *Server) v1alpha1SetAgentDiagnosticVerdict(w http.ResponseWriter, r *http.Request) {
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

	var body v1alpha1openapi.SetVerdictRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !body.Verdict.Valid() {
		writeProblem(w, http.StatusBadRequest, "invalid verdict: must be correct, incorrect, or partial")
		return
	}
	verdict := string(body.Verdict)

	var diag v1alpha1.AgentDiagnostic
	var oldVerdict string
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &diag); err != nil {
			return err
		}
		oldVerdict = diag.Status.Verdict
		now := metav1.Now()
		base := diag.DeepCopy()
		diag.Status.Verdict = verdict
		if body.ReviewerNote != nil {
			diag.Status.ReviewerNote = *body.ReviewerNote
		}
		diag.Status.ReviewedBy = sub
		diag.Status.ReviewedAt = &now
		return s.client.Status().Patch(r.Context(), &diag,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}); err != nil {
		if apierrors.IsNotFound(err) {
			writeProblem(w, http.StatusNotFound, "AgentDiagnostic not found")
		} else if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			writeProblem(w, http.StatusBadRequest, fmt.Sprintf("invalid status patch: %v", err))
		} else {
			writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("failed to patch status: %v", err))
		}
		return
	}

	diagnosticVerdictTotal.WithLabelValues(verdict).Inc()
	agentId := diag.Spec.AgentIdentity
	if err := s.applyVerdictToSummary(r.Context(), ns, agentId, oldVerdict, verdict); err != nil {
		log.Printf("DiagnosticAccuracySummary update failed for agent %q: %v — verdict saved, recomputing", agentId, err)
		go func() {
			if rerr := s.recomputeAccuracyForAgent(context.Background(), ns, agentId); rerr != nil {
				log.Printf("background recompute for agent %q in %q failed: %v", agentId, ns, rerr)
			}
		}()
		warning := "accuracy summary update failed; recompute triggered in background"
		writeJSON(w, http.StatusOK, v1alpha1openapi.SetVerdictResponse{
			Message: "verdict saved",
			Warning: &warning,
		})
		return
	}

	writeJSON(w, http.StatusOK, v1alpha1openapi.SetVerdictResponse{Message: "verdict saved"})
}

// POST /v1alpha1/agent-diagnostics/recompute-accuracy
func (s *Server) v1alpha1RecomputeAccuracy(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRoleV1(s.roles, roleReviewer, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	ns := resolveNamespace(r)
	agentId := r.URL.Query().Get("agentIdentity")

	if err := s.recomputeAccuracyForAgent(r.Context(), ns, agentId); err != nil {
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, v1alpha1openapi.MessageResponse{Message: "recomputed accuracy summaries"})
}

// GET /v1alpha1/agent-diagnostics/{name}/watch
// Not yet implemented — returns 501 so the route is registered and the spec is honoured.
func (s *Server) v1alpha1WatchAgentDiagnostic(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	http.Error(w, "watch not yet implemented", http.StatusNotImplemented)
}

// GET /v1alpha1/diagnostic-accuracy-summaries
func (s *Server) v1alpha1ListAccuracySummaries(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeProblem(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	ns := resolveNamespace(r)

	var list v1alpha1.DiagnosticAccuracySummaryList
	if err := s.client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}

	dtos := make([]v1alpha1openapi.AccuracySummary, len(list.Items))
	for i, s := range list.Items {
		dtos[i] = summaryToDTO(s)
	}

	writeJSON(w, http.StatusOK, v1alpha1openapi.AccuracySummaryList{Items: dtos})
}
