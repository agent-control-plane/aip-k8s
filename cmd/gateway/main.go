package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ravisantoshgudimetla/aip-k8s/api/v1alpha1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	addr        = flag.String("addr", ":8080", "The address to listen on for HTTP requests")
	dedupWindow = flag.Duration("dedup-window", 24*time.Hour,
		"Duration within which duplicate active requests are rejected with 409. Set to 0 to disable.")
)

const defaultNamespace = "default"

// terminalPhases are AgentRequest phases that represent a resolved request.
// Requests in these phases do not block a new attempt for the same intent.
var terminalPhases = map[string]bool{
	v1alpha1.PhaseDenied:    true,
	v1alpha1.PhaseCompleted: true,
	v1alpha1.PhaseFailed:    true,
}

type Server struct {
	client      client.Client
	dedupWindow time.Duration
}

type affectedTargetBody struct {
	URI        string `json:"uri"`
	EffectType string `json:"effectType"`
}

type cascadeModelBody struct {
	AffectedTargets  []affectedTargetBody `json:"affectedTargets,omitempty"`
	ModelSourceTrust string               `json:"modelSourceTrust,omitempty"`
	ModelSourceID    string               `json:"modelSourceID,omitempty"`
}

type reasoningTraceBody struct {
	ConfidenceScore     float64            `json:"confidenceScore,omitempty"`
	ComponentConfidence map[string]float64 `json:"componentConfidence,omitempty"`
	TraceReference      string             `json:"traceReference,omitempty"`
	Alternatives        []string           `json:"alternatives,omitempty"`
}

type createAgentDiagnosticBody struct {
	AgentIdentity  string          `json:"agentIdentity"`
	DiagnosticType string          `json:"diagnosticType"`
	CorrelationID  string          `json:"correlationID"`
	Summary        string          `json:"summary"`
	Namespace      string          `json:"namespace,omitempty"`
	Details        json.RawMessage `json:"details,omitempty"`
}

type createAgentRequestBody struct {
	AgentIdentity  string                `json:"agentIdentity"`
	Action         string                `json:"action"`
	TargetURI      string                `json:"targetURI"`
	Reason         string                `json:"reason"`
	Namespace      string                `json:"namespace"`
	CorrelationID  string                `json:"correlationID,omitempty"`
	CascadeModel   *cascadeModelBody     `json:"cascadeModel,omitempty"`
	ReasoningTrace *reasoningTraceBody   `json:"reasoningTrace,omitempty"`
	Parameters     json.RawMessage       `json:"parameters,omitempty"`
	ExecutionMode  *string               `json:"executionMode,omitempty"`
	ScopeBounds    *v1alpha1.ScopeBounds `json:"scopeBounds,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// loggingMiddleware logs the request method, path, and outcome
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Wrap ResponseWriter to capture status code
		rw := &responseWriter{w, http.StatusOK}
		next.ServeHTTP(rw, r)
		log.Printf("%s %s %d %v", r.Method, r.URL.Path, rw.status, time.Since(start))
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func main() {
	flag.Parse()

	// Load KubeConfig
	var cfg *rest.Config
	var err error
	if kEnv := os.Getenv("KUBECONFIG"); kEnv != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kEnv)
	} else {
		homeDir, _ := os.UserHomeDir()
		kubeconfig := filepath.Join(homeDir, ".kube", "config")
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			// Fallback to in-cluster config
			cfg, err = rest.InClusterConfig()
		}
	}
	if err != nil {
		log.Fatalf("Failed to load kubeconfig: %v", err)
	}

	// Register Scheme
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Fatalf("Failed to add client-go to scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		log.Fatalf("Failed to add v1alpha1 to scheme: %v", err)
	}

	// Create Controller-Runtime Client
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	server := &Server{client: k8sClient, dedupWindow: *dedupWindow}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /agent-requests", server.handleListAgentRequests)
	mux.HandleFunc("POST /agent-requests", server.handleCreateAgentRequest)
	mux.HandleFunc("GET /agent-requests/{name}", server.handleGetAgentRequest)
	mux.HandleFunc("POST /agent-requests/{name}/executing", server.handleExecutingAgentRequest)
	mux.HandleFunc("POST /agent-requests/{name}/completed", server.handleCompletedAgentRequest)
	mux.HandleFunc("POST /agent-requests/{name}/approve", server.handleApproveAgentRequest)
	mux.HandleFunc("POST /agent-requests/{name}/deny", server.handleDenyAgentRequest)
	mux.HandleFunc("GET /audit-records", server.handleListAuditRecords)
	mux.HandleFunc("GET /agent-diagnostics", server.handleListAgentDiagnostics)
	mux.HandleFunc("POST /agent-diagnostics", server.handleCreateAgentDiagnostic)
	mux.HandleFunc("GET /agent-diagnostics/{name}", server.handleGetAgentDiagnostic)

	log.Printf("Starting AIP Demo Gateway on %s", *addr)
	if err := http.ListenAndServe(*addr, loggingMiddleware(mux)); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func buildCascadeModel(body *createAgentRequestBody) *v1alpha1.CascadeModel {
	if body.CascadeModel == nil || len(body.CascadeModel.AffectedTargets) == 0 {
		return nil
	}
	affected := make([]v1alpha1.AffectedTarget, len(body.CascadeModel.AffectedTargets))
	for i, t := range body.CascadeModel.AffectedTargets {
		affected[i] = v1alpha1.AffectedTarget{URI: t.URI, EffectType: t.EffectType}
	}
	cm := &v1alpha1.CascadeModel{AffectedTargets: affected}
	if body.CascadeModel.ModelSourceTrust != "" {
		cm.ModelSourceTrust = &body.CascadeModel.ModelSourceTrust
	}
	if body.CascadeModel.ModelSourceID != "" {
		cm.ModelSourceID = &body.CascadeModel.ModelSourceID
	}
	return cm
}

func buildReasoningTrace(body *createAgentRequestBody) *v1alpha1.ReasoningTrace {
	if body.ReasoningTrace == nil {
		return nil
	}
	rt := &v1alpha1.ReasoningTrace{}
	if body.ReasoningTrace.ConfidenceScore > 0 {
		rt.ConfidenceScore = &body.ReasoningTrace.ConfidenceScore
	}
	if len(body.ReasoningTrace.ComponentConfidence) > 0 {
		rt.ComponentConfidence = body.ReasoningTrace.ComponentConfidence
	}
	if body.ReasoningTrace.TraceReference != "" {
		rt.TraceReference = &body.ReasoningTrace.TraceReference
	}
	if len(body.ReasoningTrace.Alternatives) > 0 {
		rt.Alternatives = body.ReasoningTrace.Alternatives
	}
	return rt
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
	r *http.Request, agentIdentity, action, targetURI, ns string, w http.ResponseWriter,
) error {
	if s.dedupWindow == 0 {
		return nil
	}
	var existing v1alpha1.AgentRequestList
	if err := s.client.List(r.Context(), &existing, client.InNamespace(ns)); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to check for duplicate requests: %v", err))
		return err
	}
	cutoff := time.Now().Add(-s.dedupWindow)
	for _, req := range existing.Items {
		if terminalPhases[req.Status.Phase] || req.CreationTimestamp.Time.Before(cutoff) {
			continue
		}
		if req.Spec.AgentIdentity == agentIdentity &&
			req.Spec.Action == action &&
			req.Spec.Target.URI == targetURI {
			err := fmt.Errorf("duplicate")
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"duplicate request: an active request for the same agent, action, and target already exists (created %s ago)",
				time.Since(req.CreationTimestamp.Time).Round(time.Second),
			))
			return err
		}
	}
	return nil
}

func (s *Server) handleCreateAgentRequest(w http.ResponseWriter, r *http.Request) {
	var body createAgentRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.AgentIdentity == "" || body.Action == "" || body.TargetURI == "" {
		writeError(w, http.StatusBadRequest, "agentIdentity, action, and targetURI are required")
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

	reqLabels := map[string]string{}
	if body.CorrelationID != "" {
		reqLabels["aip.io/correlationID"] = sanitizeLabelValue(body.CorrelationID)
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
			CascadeModel:   buildCascadeModel(&body),
			ReasoningTrace: buildReasoningTrace(&body),
			Parameters:     parameters,
			ExecutionMode:  body.ExecutionMode,
			ScopeBounds:    body.ScopeBounds,
		},
	}

	if err := s.checkDuplicate(r, body.AgentIdentity, body.Action, body.TargetURI, ns, w); err != nil {
		return
	}

	if err := s.client.Create(r.Context(), agentReq); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsAlreadyExists(err) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid AgentRequest: %v", err))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create AgentRequest: %v", err))
		return
	}

	name := agentReq.Name

	// Poll phase logic
	timeout := time.After(90 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-timeout:
			writeError(w, http.StatusGatewayTimeout, "timed out waiting for AgentRequest resolution")
			return
		case <-ticker.C:
			var current v1alpha1.AgentRequest
			if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &current); err != nil {
				continue
			}

			phase := current.Status.Phase
			if phase == v1alpha1.PhaseApproved || phase == v1alpha1.PhaseDenied ||
				phase == v1alpha1.PhaseCompleted || phase == v1alpha1.PhaseFailed {
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

func (s *Server) handleExecutingAgentRequest(w http.ResponseWriter, r *http.Request) {
	s.patchAgentRequestCondition(w, r, v1alpha1.ConditionExecuting, "AgentStarted", "Agent is now executing action")
}

func (s *Server) handleCompletedAgentRequest(w http.ResponseWriter, r *http.Request) {
	s.patchAgentRequestCondition(w, r, v1alpha1.ConditionCompleted,
		"ActionSuccess", "Agent successfully completed the action")
}

// sanitizeDNSSegment converts an arbitrary string into a valid DNS label
// segment suitable for use in GenerateName prefixes. maxLen should be 57 to
// leave room for the API-server-generated suffix.
var invalidDNSChars = regexp.MustCompile(`[^a-z0-9-]`)

func sanitizeDNSSegment(s string, maxLen int) string {
	s = strings.ToLower(s)
	s = invalidDNSChars.ReplaceAllString(s, "-")
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	s = strings.Trim(s, "-")
	return s
}

// sanitizeLabelValue converts an arbitrary string into a valid Kubernetes label
// value: allows [A-Za-z0-9], [-_.], max 63 chars, must begin/end alphanumeric.
var invalidLabelChars = regexp.MustCompile(`[^A-Za-z0-9\-_.]`)

func sanitizeLabelValue(s string) string {
	s = invalidLabelChars.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.Trim(s, "-_.")
	return s
}

func (s *Server) handleCreateAgentDiagnostic(w http.ResponseWriter, r *http.Request) {
	var body createAgentDiagnosticBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.AgentIdentity == "" || body.DiagnosticType == "" || body.CorrelationID == "" || body.Summary == "" {
		writeError(w, http.StatusBadRequest, "agentIdentity, diagnosticType, correlationID, and summary are required")
		return
	}

	ns := body.Namespace
	if ns == "" {
		ns = defaultNamespace
	}

	var details *apiextensionsv1.JSON
	if len(body.Details) > 0 && string(body.Details) != "null" {
		details = &apiextensionsv1.JSON{Raw: body.Details}
	}

	// GenerateName prefix must be a valid DNS segment.
	// Label values use the looser Kubernetes label-value charset (allows _, .).
	// Normalize independently so callers can see exactly which labels were stored.
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
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid AgentDiagnostic: %v", err))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create AgentDiagnostic: %v", err))
		return
	}

	// Return normalized label values so callers can use them in label-selector
	// queries without having to guess what normalization was applied.
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":      diag.Name,
		"namespace": diag.Namespace,
		"createdAt": diag.CreationTimestamp.Time,
		"labels": map[string]string{
			"aip.io/correlationID":  labelCorrelationID,
			"aip.io/agentIdentity":  labelAgentIdentity,
			"aip.io/diagnosticType": labelDiagnosticType,
		},
	})
}

func (s *Server) handleGetAgentDiagnostic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var diag v1alpha1.AgentDiagnostic
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &diag); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentDiagnostic not found")
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get AgentDiagnostic: %v", err))
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":           diag.Name,
		"namespace":      diag.Namespace,
		"createdAt":      diag.CreationTimestamp.Time,
		"agentIdentity":  diag.Spec.AgentIdentity,
		"diagnosticType": diag.Spec.DiagnosticType,
		"correlationID":  diag.Spec.CorrelationID,
		"summary":        diag.Spec.Summary,
		"details":        diag.Spec.Details,
	})
}

func (s *Server) patchAgentRequestCondition(
	w http.ResponseWriter, r *http.Request, conditionType, reason, message string,
) {
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

	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})

	if err := s.client.Status().Update(r.Context(), &current); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update status: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message": fmt.Sprintf("successfully patched condition %s", conditionType),
	})
}

func (s *Server) handleListAgentRequests(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var list v1alpha1.AgentRequestList
	if err := s.client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := list.Items
	if items == nil {
		items = []v1alpha1.AgentRequest{}
	}
	writeJSON(w, http.StatusOK, items)
}

//nolint:dupl // similar to handleListAgentDiagnostics
func (s *Server) handleListAuditRecords(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}
	agentReq := r.URL.Query().Get("agentRequest")

	var list v1alpha1.AuditRecordList
	if err := s.client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var results []v1alpha1.AuditRecord
	for _, item := range list.Items {
		if agentReq == "" || item.Spec.AgentRequestRef == agentReq {
			results = append(results, item)
		}
	}
	if results == nil {
		results = []v1alpha1.AuditRecord{}
	}

	writeJSON(w, http.StatusOK, results)
}

//nolint:dupl // similar to handleListAuditRecords
func (s *Server) handleListAgentDiagnostics(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}
	agentID := r.URL.Query().Get("agentIdentity")
	correlID := r.URL.Query().Get("correlationID")

	var err error
	var ca, cb time.Time
	if caStr := r.URL.Query().Get("createdAfter"); caStr != "" {
		if ca, err = time.Parse(time.RFC3339, caStr); err != nil {
			writeError(w, http.StatusBadRequest, "invalid createdAfter")
			return
		}
	}
	if cbStr := r.URL.Query().Get("createdBefore"); cbStr != "" {
		if cb, err = time.Parse(time.RFC3339, cbStr); err != nil {
			writeError(w, http.StatusBadRequest, "invalid createdBefore")
			return
		}
	}

	var list v1alpha1.AgentDiagnosticList
	if err := s.client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	results := make([]v1alpha1.AgentDiagnostic, 0)
	for _, item := range list.Items {
		if agentID != "" && item.Spec.AgentIdentity != agentID {
			continue
		}
		if correlID != "" && item.Labels["aip.io/correlationID"] != correlID {
			continue
		}
		if !ca.IsZero() && !item.CreationTimestamp.After(ca) {
			continue
		}
		if !cb.IsZero() && !item.CreationTimestamp.Time.Before(cb) {
			continue
		}
		results = append(results, item)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].CreationTimestamp.After(results[j].CreationTimestamp.Time)
	})

	writeJSON(w, http.StatusOK, results)
}

func (s *Server) handleApproveAgentRequest(w http.ResponseWriter, r *http.Request) {
	s.handleHumanDecision(w, r, "approved")
}

func (s *Server) handleDenyAgentRequest(w http.ResponseWriter, r *http.Request) {
	s.handleHumanDecision(w, r, "denied")
}

func (s *Server) handleHumanDecision(w http.ResponseWriter, r *http.Request, decision string) {
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

	patch := client.MergeFrom(agentReq.DeepCopy())
	agentReq.Spec.HumanApproval = &v1alpha1.HumanApproval{
		Decision:      decision,
		Reason:        humanReason,
		ForGeneration: agentReq.Status.EvaluationGeneration,
	}

	if err := s.client.Patch(r.Context(), &agentReq, patch); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
}
