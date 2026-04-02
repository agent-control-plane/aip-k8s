package main

import (
	"crypto/sha256"
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
	"k8s.io/client-go/util/retry"
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

const (
	verdictCorrect   = "correct"
	verdictPartial   = "partial"
	verdictIncorrect = "incorrect"
)

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
		_, _ = fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		var list v1alpha1.AgentRequestList
		if err := k8sClient.List(r.Context(), &list, client.Limit(1)); err != nil {
			http.Error(w, "k8s api unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
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
	mux.HandleFunc("PATCH /agent-diagnostics/{name}/status", server.handlePatchAgentDiagnosticStatus)
	mux.HandleFunc("POST /agent-diagnostics/recompute-accuracy", server.handleRecomputeAccuracy)
	mux.HandleFunc("GET /diagnostic-accuracy-summaries", server.handleListAccuracySummaries)

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

// checkDiagnosticDuplicate returns a non-nil error and writes a 409 if an active
// diagnostic for the same (agentIdentity, diagnosticType, correlationID) exists
// within the dedup window.
//
// Note: the List→Create sequence is not atomic. Concurrent requests with the
// same key can both pass this check and both be created. This is intentional:
// dedup provides best-effort protection against agent retry floods, not
// a hard mutual-exclusion guarantee.
//
//nolint:dupl // structurally similar to checkDuplicate
func (s *Server) checkDiagnosticDuplicate(
	r *http.Request, agentIdentity, diagnosticType, correlationID, ns string, w http.ResponseWriter,
) error {
	if s.dedupWindow == 0 {
		return nil
	}
	var existing v1alpha1.AgentDiagnosticList
	if err := s.client.List(r.Context(), &existing, client.InNamespace(ns)); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to check for duplicate diagnostics: %v", err))
		return err
	}
	cutoff := time.Now().Add(-s.dedupWindow)
	for _, diag := range existing.Items {
		if diag.CreationTimestamp.Time.Before(cutoff) {
			continue
		}
		if diag.Spec.AgentIdentity == agentIdentity &&
			diag.Spec.DiagnosticType == diagnosticType &&
			diag.Spec.CorrelationID == correlationID {
			err := fmt.Errorf("duplicate")
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"duplicate diagnostic: an active diagnostic for the same "+
					"agent, type, and correlationID already exists (created %s ago)",
				time.Since(diag.CreationTimestamp.Time).Round(time.Second),
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

// summaryNameForAgent returns a stable, collision-resistant DNS name for a
// DiagnosticAccuracySummary CR from an arbitrary agentIdentity string.
// It combines a human-readable prefix (up to 54 chars) with an 8-char hex
// suffix derived from the SHA-256 of the full identity, giving 4B distinct
// keys. The fallback prefix "agent" is used when the identity sanitizes to empty
// (e.g., an identity consisting entirely of non-DNS characters).
func summaryNameForAgent(agentIdentity string) string {
	h := sha256.Sum256([]byte(agentIdentity))
	suffix := fmt.Sprintf("%x", h[:4]) // 8 hex chars
	prefix := sanitizeDNSSegment(agentIdentity, 54)
	if prefix == "" {
		prefix = "agent"
	}
	return prefix + "-" + suffix
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

	err := s.checkDiagnosticDuplicate(
		r, body.AgentIdentity, body.DiagnosticType, body.CorrelationID, ns, w)
	if err != nil {
		return
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
		"status":         diag.Status, // Added status so dashboard can read verdict
	})
}

func (s *Server) handlePatchAgentDiagnosticStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var body struct {
		Verdict      string `json:"verdict"`
		ReviewerNote string `json:"reviewerNote,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Verdict != verdictCorrect && body.Verdict != verdictIncorrect && body.Verdict != verdictPartial {
		writeError(w, http.StatusBadRequest, "invalid verdict")
		return
	}

	// reviewedBy is always set server-side from the upstream identity headers
	// (injected by the service mesh / authenticating proxy). If neither header
	// is present the gateway cannot establish caller identity and must refuse
	// the request to prevent unauthorised or anonymous reviews.
	caller := r.Header.Get("X-Remote-User")
	if caller == "" {
		caller = r.Header.Get("X-Forwarded-User")
	}
	if caller == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required: X-Remote-User or X-Forwarded-User header missing")
		return
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

	oldVerdict := diag.Status.Verdict
	now := metav1.Now()

	patch := client.MergeFrom(diag.DeepCopy())
	diag.Status.Verdict = body.Verdict
	diag.Status.ReviewerNote = body.ReviewerNote
	diag.Status.ReviewedBy = caller
	diag.Status.ReviewedAt = &now

	if err := s.client.Status().Patch(r.Context(), &diag, patch); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid status patch: %v", err))
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to patch status: %v", err))
		}
		return
	}

	agentId := diag.Spec.AgentIdentity
	summaryName := summaryNameForAgent(agentId)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var summary v1alpha1.DiagnosticAccuracySummary
		err := s.client.Get(r.Context(), types.NamespacedName{Name: summaryName, Namespace: ns}, &summary)
		exists := true
		if err != nil {
			if apierrors.IsNotFound(err) {
				exists = false
				summary = v1alpha1.DiagnosticAccuracySummary{
					ObjectMeta: metav1.ObjectMeta{Name: summaryName, Namespace: ns},
					Spec:       v1alpha1.DiagnosticAccuracySummarySpec{AgentIdentity: agentId},
				}
			} else {
				return err
			}
		}

		// Guard against accidental cross-agent reuse: the hash suffix makes
		// collisions essentially impossible, but verify defensively.
		if exists && summary.Spec.AgentIdentity != agentId {
			exists = false
			summary = v1alpha1.DiagnosticAccuracySummary{
				ObjectMeta: metav1.ObjectMeta{Name: summaryName, Namespace: ns},
				Spec:       v1alpha1.DiagnosticAccuracySummarySpec{AgentIdentity: agentId},
			}
		}

		if !exists {
			if err := s.client.Create(r.Context(), &summary); err != nil {
				return err
			}
			// When the summary CR is newly created its counters start at zero.
			// We must not decrement oldVerdict even if the diagnostic already
			// carries a verdict (e.g., after a manual summary deletion), or we
			// would produce negative counts and an accuracy ratio above 1.0.
			oldVerdict = ""
		}

		if oldVerdict != "" {
			switch oldVerdict {
			case verdictCorrect:
				summary.Status.CorrectCount--
			case verdictIncorrect:
				summary.Status.IncorrectCount--
			case verdictPartial:
				summary.Status.PartialCount--
			}
			summary.Status.TotalReviewed--
		}

		switch body.Verdict {
		case verdictCorrect:
			summary.Status.CorrectCount++
		case verdictIncorrect:
			summary.Status.IncorrectCount++
		case verdictPartial:
			summary.Status.PartialCount++
		}
		summary.Status.TotalReviewed++

		acc := float64(summary.Status.CorrectCount) + 0.5*float64(summary.Status.PartialCount)
		if summary.Status.TotalReviewed > 0 {
			val := acc / float64(summary.Status.TotalReviewed)
			summary.Status.DiagnosticAccuracy = &val
		} else {
			summary.Status.DiagnosticAccuracy = nil
		}
		summary.Status.LastUpdatedAt = &now
		return s.client.Status().Update(r.Context(), &summary)
	})

	if err != nil {
		log.Printf("failed to update DiagnosticAccuracySummary for agent %q: %v", agentId, err)
		writeError(w, http.StatusInternalServerError,
			"verdict saved but accuracy summary update failed — run POST /agent-diagnostics/recompute-accuracy to repair")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"message": "verdict saved"})
}

func (s *Server) handleRecomputeAccuracy(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	agentId := r.URL.Query().Get("agentIdentity")

	var list v1alpha1.AgentDiagnosticList
	if err := s.client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	stats := make(map[string]*v1alpha1.DiagnosticAccuracySummary)
	for _, item := range list.Items {
		if agentId != "" && item.Spec.AgentIdentity != agentId {
			continue
		}
		id := item.Spec.AgentIdentity
		if item.Status.Verdict == "" {
			continue
		}

		summaryName := summaryNameForAgent(id)
		summary, ok := stats[summaryName]
		if !ok {
			summary = &v1alpha1.DiagnosticAccuracySummary{
				ObjectMeta: metav1.ObjectMeta{Name: summaryName, Namespace: ns},
				Spec:       v1alpha1.DiagnosticAccuracySummarySpec{AgentIdentity: id},
			}
			stats[summaryName] = summary
		}

		switch item.Status.Verdict {
		case verdictCorrect:
			summary.Status.CorrectCount++
		case verdictIncorrect:
			summary.Status.IncorrectCount++
		case verdictPartial:
			summary.Status.PartialCount++
		}
		summary.Status.TotalReviewed++

		reviewedAt := item.Status.ReviewedAt
		if summary.Status.LastUpdatedAt == nil || (reviewedAt != nil && reviewedAt.After(summary.Status.LastUpdatedAt.Time)) {
			summary.Status.LastUpdatedAt = reviewedAt
		}
	}

	for id, summary := range stats {
		acc := float64(summary.Status.CorrectCount) + 0.5*float64(summary.Status.PartialCount)
		if summary.Status.TotalReviewed > 0 {
			val := acc / float64(summary.Status.TotalReviewed)
			summary.Status.DiagnosticAccuracy = &val
		}

		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var existing v1alpha1.DiagnosticAccuracySummary
			err := s.client.Get(r.Context(), types.NamespacedName{Name: id, Namespace: ns}, &existing)
			if err != nil {
				if apierrors.IsNotFound(err) {
					if err := s.client.Create(r.Context(), summary); err != nil {
						return err
					}
					return s.client.Status().Update(r.Context(), summary)
				}
				return err
			}
			// Verify the existing CR belongs to the same agent before overwriting.
			if existing.Spec.AgentIdentity != summary.Spec.AgentIdentity {
				return fmt.Errorf("summary %q identity mismatch: got %q, want %q",
					id, existing.Spec.AgentIdentity, summary.Spec.AgentIdentity)
			}
			existing.Status = summary.Status
			return s.client.Status().Update(r.Context(), &existing)
		})
		if err != nil {
			log.Printf("failed to upsert summary for %s: %v", id, err)
		}
	}

	// Zero out summaries for agents that no longer have any reviewed diagnostics
	// (e.g., after their diagnostics were deleted). Without this, a recompute
	// would leave stale counts behind, defeating the recovery guarantee.
	var existingSummaries v1alpha1.DiagnosticAccuracySummaryList
	if err := s.client.List(r.Context(), &existingSummaries, client.InNamespace(ns)); err != nil {
		log.Printf("failed to list existing summaries during recompute: %v", err)
	} else {
		for i := range existingSummaries.Items {
			existing := &existingSummaries.Items[i]
			if agentId != "" && existing.Spec.AgentIdentity != agentId {
				continue
			}
			if _, ok := stats[existing.Name]; ok {
				continue
			}
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var fresh v1alpha1.DiagnosticAccuracySummary
				if err := s.client.Get(r.Context(), types.NamespacedName{Name: existing.Name, Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Status = v1alpha1.DiagnosticAccuracySummaryStatus{}
				return s.client.Status().Update(r.Context(), &fresh)
			})
			if err != nil {
				log.Printf("failed to zero stale summary %s: %v", existing.Name, err)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"message": "recomputed accuracy summaries"})
}

func (s *Server) handleListAccuracySummaries(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var list v1alpha1.DiagnosticAccuracySummaryList
	if err := s.client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, list.Items)
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
