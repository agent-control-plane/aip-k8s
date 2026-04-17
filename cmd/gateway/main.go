package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	addr        = flag.String("addr", ":8080", "The address to listen on for HTTP requests")
	dedupWindow = flag.Duration("dedup-window", 24*time.Hour,
		"Duration within which duplicate active requests are rejected with 409. Set to 0 to disable.")
	oidcIssuerURL = flag.String("oidc-issuer-url", "",
		"OIDC provider URL. When set, Bearer token validation is required on all non-healthz endpoints.")
	oidcAudience = flag.String("oidc-audience", "aip-gateway",
		"Expected JWT aud claim.")
	oidcIdentityClaim = flag.String("oidc-identity-claim", "sub",
		"JWT claim used as the caller identity. Default 'sub' is compatible with most OIDC providers. "+
			"Use 'azp' for Keycloak client_credentials, 'appid' for Azure AD, 'email' for Google service accounts. "+
			"Falls back to 'sub' if the configured claim is absent from the token.")
	agentSubjects = flag.String("agent-subjects", "",
		"Comma-separated identity values permitted to act as agents (matched against --oidc-identity-claim).")
	reviewerSubjects = flag.String("reviewer-subjects", "",
		"Comma-separated identity values permitted to act as reviewers (matched against --oidc-identity-claim).")
	oidcGroupsClaim = flag.String("oidc-groups-claim", "groups",
		"JWT claim that carries group memberships (array of strings). Common values: 'groups', 'roles', 'group_memberships'.")
	agentGroups = flag.String("agent-groups", "",
		"Comma-separated group names permitted to act as agents (matched against --oidc-groups-claim).")
	reviewerGroups = flag.String("reviewer-groups", "",
		"Comma-separated group names permitted to act as reviewers (matched against --oidc-groups-claim).")
	adminSubjects = flag.String("admin-subjects", "",
		"Comma-separated identity values permitted to act as admins (matched against --oidc-identity-claim).")
	adminGroups = flag.String("admin-groups", "",
		"Comma-separated group names permitted to act as admins (matched against --oidc-groups-claim).")
	requireGovernedResource = flag.Bool("require-governed-resource", false,
		"When true, reject AgentRequests even if no GovernedResource objects exist. "+
			"Default false preserves backward compatibility for deployments without a populated registry.")
	trustedProxyCIDRs = flag.String("trusted-proxy-cidrs", "",
		"Comma-separated CIDRs for proxy-header trust. Empty = any source (dev only). Ignored when --oidc-issuer-url is set.")
	waitTimeout = flag.Duration("wait-timeout", 90*time.Second,
		"Maximum time the gateway will poll for AgentRequest resolution before returning 504.")
)

type contextKey string

const callerSubKey contextKey = "callerSub"
const callerGroupsKey contextKey = "callerGroups"

func withCallerSub(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, callerSubKey, sub)
}

func callerSubFromCtx(ctx context.Context) string {
	s, _ := ctx.Value(callerSubKey).(string)
	return s
}

func withCallerGroups(ctx context.Context, groups []string) context.Context {
	return context.WithValue(ctx, callerGroupsKey, groups)
}

func callerGroupsFromCtx(ctx context.Context) []string {
	g, _ := ctx.Value(callerGroupsKey).([]string)
	return g
}

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
	client                  client.Client
	dedupWindow             time.Duration
	waitTimeout             time.Duration // from --wait-timeout; max time to poll for AgentRequest resolution
	roles                   *roleConfig
	authRequired            bool // true when --oidc-issuer-url is set or any subject list is non-empty
	requireGovernedResource bool // from --require-governed-resource
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

	rc := newRoleConfig(*agentSubjects, *reviewerSubjects, *adminSubjects, *agentGroups, *reviewerGroups, *adminGroups)
	authRequired := *oidcIssuerURL != "" || *agentSubjects != "" || *reviewerSubjects != "" || *adminSubjects != "" ||
		*agentGroups != "" || *reviewerGroups != "" || *adminGroups != ""

	// Refuse to start in a configuration where role allowlists are set but no trust boundary is
	// defined: without --oidc-issuer-url, any client can forge X-Remote-User to claim any sub.
	if authRequired && *oidcIssuerURL == "" && *trustedProxyCIDRs == "" {
		log.Fatalf("insecure configuration: --agent-subjects or --reviewer-subjects is set but " +
			"neither --oidc-issuer-url nor --trusted-proxy-cidrs is configured — " +
			"any client can forge X-Remote-User headers; " +
			"set --oidc-issuer-url for JWT validation or --trusted-proxy-cidrs to restrict proxy-header trust")
	}

	wt := *waitTimeout
	if wt <= 0 {
		wt = 90 * time.Second
		log.Printf("--wait-timeout must be positive; using default %v", wt)
	}
	server := &Server{
		client:                  k8sClient,
		dedupWindow:             *dedupWindow,
		waitTimeout:             wt,
		roles:                   rc,
		authRequired:            authRequired,
		requireGovernedResource: *requireGovernedResource,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /whoami", server.handleWhoAmI)
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
	mux.HandleFunc("POST /governed-resources", server.handleCreateGovernedResource)
	mux.HandleFunc("GET /governed-resources", server.handleListGovernedResources)
	mux.HandleFunc("GET /governed-resources/{name}", server.handleGetGovernedResource)
	mux.HandleFunc("PUT /governed-resources/{name}", server.handleReplaceGovernedResource)
	mux.HandleFunc("DELETE /governed-resources/{name}", server.handleDeleteGovernedResource)
	mux.HandleFunc("POST /safety-policies", server.handleCreateSafetyPolicy)
	mux.HandleFunc("GET /safety-policies", server.handleListSafetyPolicies)
	mux.HandleFunc("GET /safety-policies/{name}", server.handleGetSafetyPolicy)
	mux.HandleFunc("PUT /safety-policies/{name}", server.handleReplaceSafetyPolicy)
	mux.HandleFunc("DELETE /safety-policies/{name}", server.handleDeleteSafetyPolicy)

	var authMiddleware func(http.Handler) http.Handler
	if *oidcIssuerURL != "" {
		discoverCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		mw, err := newOIDCMiddleware(discoverCtx, *oidcIssuerURL, *oidcAudience, *oidcIdentityClaim, *oidcGroupsClaim)
		if err != nil {
			log.Fatalf("OIDC setup failed: %v", err)
		}
		authMiddleware = mw
	} else {
		authMiddleware = newProxyHeaderMiddleware(*trustedProxyCIDRs)
	}

	mux.Handle("GET /metrics", metricsHandler())

	log.Printf("Starting AIP Demo Gateway on %s", *addr)
	if err := http.ListenAndServe(*addr, metricsMiddleware(loggingMiddleware(authMiddleware(mux)))); err != nil {
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
// matchGovernedResource returns the most specific GovernedResource whose URIPattern
// matches targetURI using path.Match semantics. Most specific = longest pattern;
// ties broken alphabetically by name. Returns nil if no pattern matches.
func matchGovernedResource(items []v1alpha1.GovernedResource, targetURI string) *v1alpha1.GovernedResource {
	var best *v1alpha1.GovernedResource
	for i := range items {
		gr := &items[i]
		matched, err := path.Match(gr.Spec.URIPattern, targetURI)
		if err != nil {
			log.Printf("invalid URIPattern %q in GovernedResource %s: %v", gr.Spec.URIPattern, gr.Name, err)
			continue
		}
		if !matched {
			continue
		}
		if best == nil ||
			len(gr.Spec.URIPattern) > len(best.Spec.URIPattern) ||
			(len(gr.Spec.URIPattern) == len(best.Spec.URIPattern) && gr.Name < best.Name) {
			best = gr
		}
	}
	return best
}

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
	ctx context.Context, agentIdentity, diagnosticType, correlationID, ns string,
) (*v1alpha1.AgentDiagnostic, error) {
	if s.dedupWindow == 0 {
		return nil, nil
	}
	var existing v1alpha1.AgentDiagnosticList
	if err := s.client.List(ctx, &existing,
		client.InNamespace(ns),
		client.MatchingLabels{
			"aip.io/agentIdentity": sanitizeLabelValue(agentIdentity),
			"aip.io/correlationID": sanitizeLabelValue(correlationID),
		},
	); err != nil {
		return nil, fmt.Errorf("failed to check for duplicate diagnostics: %v", err)
	}
	cutoff := time.Now().Add(-s.dedupWindow)
	for _, diag := range existing.Items {
		if !diag.CreationTimestamp.IsZero() && diag.CreationTimestamp.Time.Before(cutoff) {
			continue
		}
		if diag.Spec.AgentIdentity == agentIdentity &&
			diag.Spec.DiagnosticType == diagnosticType &&
			diag.Spec.CorrelationID == correlationID {
			return &diag, nil
		}
	}
	return nil, nil
}

// handleWhoAmI returns the caller's identity and their highest role.
// Used by the dashboard to enable role-aware rendering without a page reload.
func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if sub == "" {
		sub = "unknown"
	}
	groups := callerGroupsFromCtx(r.Context())

	role := "unknown"
	if s.roles != nil {
		switch {
		case s.roles.isAdmin(sub, groups):
			role = roleAdmin
		case s.roles.isReviewer(sub, groups):
			role = roleReviewer
		case s.roles.isAgent(sub, groups):
			role = roleAgent
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"identity": sub, "role": role})
}

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
	}
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

	// GovernedResource admission: URI → agent identity → action (per design doc order).
	var matchedGR *v1alpha1.GovernedResource
	var govResources v1alpha1.GovernedResourceList
	var agentPermitted, actionPermitted bool
	if err := s.client.List(r.Context(), &govResources); err != nil {
		// If the CRD is not yet installed, treat as an empty list.
		// This allows the system to boot gracefully even if the GovernedResource CRD
		// is not yet available (e.g., during cluster initialization in e2e tests).
		if strings.Contains(err.Error(), "no matches for kind") {
			// Treat as empty list
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list GovernedResources: %v", err))
			return
		}
	}

	// Backward compat: skip check when no GovernedResources exist and flag is false.
	if len(govResources.Items) == 0 && !s.requireGovernedResource {
		goto admissionPassed
	}

	matchedGR = matchGovernedResource(govResources.Items, body.TargetURI)
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
		writeError(w, http.StatusForbidden, v1alpha1.DenialCodeActionNotPermitted)
		return
	}

admissionPassed:
	if matchedGR != nil {
		agentReq.Spec.GovernedResourceRef = &v1alpha1.GovernedResourceRef{
			Name:       matchedGR.Name,
			Generation: matchedGR.Generation,
		}
	}

	existing, err := s.checkDuplicate(r.Context(), body.AgentIdentity, body.Action, body.TargetURI, ns)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing != nil {
		// Idempotent: return the current state of the existing request immediately.
		// Do not poll — the caller already has an in-flight request and should
		// act on its current phase rather than wait for a terminal transition.
		writeJSON(w, http.StatusOK, map[string]any{
			"name":                     existing.Name,
			"labels":                   reqLabels,
			"phase":                    existing.Status.Phase,
			"denial":                   existing.Status.Denial,
			"conditions":               existing.Status.Conditions,
			"controlPlaneVerification": existing.Status.ControlPlaneVerification,
		})
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

	s.pollAgentRequestPhase(w, r, agentReq.Name, ns, reqLabels)
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

	if req.Spec.AgentIdentity != sub {
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

	if req.Spec.AgentIdentity != sub {
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleAgent, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	var body createAgentDiagnosticBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.AgentIdentity == "" || body.DiagnosticType == "" || body.CorrelationID == "" || body.Summary == "" {
		writeError(w, http.StatusBadRequest, "agentIdentity, diagnosticType, correlationID, and summary are required")
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

	existing, err := s.checkDiagnosticDuplicate(
		r.Context(), body.AgentIdentity, body.DiagnosticType, body.CorrelationID, ns)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing != nil {
		diagnosticDedupTotal.Inc()
		writeJSON(w, http.StatusOK, map[string]any{
			"name":           existing.Name,
			"namespace":      existing.Namespace,
			"createdAt":      existing.CreationTimestamp.Time,
			"agentIdentity":  existing.Spec.AgentIdentity,
			"diagnosticType": existing.Spec.DiagnosticType,
			"correlationID":  existing.Spec.CorrelationID,
			"summary":        existing.Spec.Summary,
			"details":        existing.Spec.Details,
			"status":         existing.Status,
		})
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
	diagnosticCreatedTotal.WithLabelValues(body.AgentIdentity).Inc()

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

	// Wrap the Get→mutate→Patch in RetryOnConflict so that concurrent verdict
	// submissions each re-read the latest resourceVersion and recompute
	// oldVerdict from the freshly fetched status — preventing stale oldVerdict
	// from causing counter drift in the DiagnosticAccuracySummary.
	var diag v1alpha1.AgentDiagnostic
	var oldVerdict string
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &diag); err != nil {
			return err
		}
		oldVerdict = diag.Status.Verdict
		now := metav1.Now()
		base := diag.DeepCopy()
		diag.Status.Verdict = body.Verdict
		diag.Status.ReviewerNote = body.ReviewerNote
		diag.Status.ReviewedBy = sub
		diag.Status.ReviewedAt = &now
		return s.client.Status().Patch(r.Context(), &diag,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "AgentDiagnostic not found")
		} else if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid status patch: %v", err))
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to patch status: %v", err))
		}
		return
	}

	diagnosticVerdictTotal.WithLabelValues(body.Verdict).Inc()
	agentId := diag.Spec.AgentIdentity
	err := s.applyVerdictToSummary(r.Context(), ns, agentId, oldVerdict, body.Verdict)
	if err != nil {
		log.Printf("DiagnosticAccuracySummary update failed for agent %q: %v — verdict saved, recomputing", agentId, err)
		go func() {
			if rerr := s.recomputeAccuracyForAgent(context.Background(), ns, agentId); rerr != nil {
				log.Printf("background recompute for agent %q in %q failed: %v", agentId, ns, rerr)
			}
		}()
		writeJSON(w, http.StatusOK, map[string]any{
			"message": "verdict saved",
			"warning": "accuracy summary update failed; recompute triggered in background",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"message": "verdict saved"})
}

// applyVerdictToSummary atomically applies a verdict change to the
// DiagnosticAccuracySummary for the given agent. It creates the summary if it
// does not yet exist and retries on conflict so concurrent calls converge.
func (s *Server) applyVerdictToSummary(ctx context.Context, ns, agentId, oldVerdict, newVerdict string) error {
	summaryName := summaryNameForAgent(agentId)
	updatedAt := metav1.Now()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var summary v1alpha1.DiagnosticAccuracySummary
		err := s.client.Get(ctx, types.NamespacedName{Name: summaryName, Namespace: ns}, &summary)
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

		// Guard against accidental cross-agent reuse.
		if exists && summary.Spec.AgentIdentity != agentId {
			exists = false
			summary = v1alpha1.DiagnosticAccuracySummary{
				ObjectMeta: metav1.ObjectMeta{Name: summaryName, Namespace: ns},
				Spec:       v1alpha1.DiagnosticAccuracySummarySpec{AgentIdentity: agentId},
			}
		}

		if !exists {
			if err := s.client.Create(ctx, &summary); err != nil {
				if apierrors.IsAlreadyExists(err) {
					// Concurrent Create won the race; synthesise a Conflict so
					// retry.RetryOnConflict re-runs the Get → mutate → Patch sequence.
					return apierrors.NewConflict(schema.GroupResource{}, summaryName, err)
				}
				return err
			}
			// Counters start at zero on a new summary; do not decrement the old
			// verdict or we produce negative counts after a manual summary deletion.
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

		switch newVerdict {
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
		summary.Status.LastUpdatedAt = &updatedAt
		return s.client.Status().Update(ctx, &summary)
	})
}

// recomputeAccuracyForAgent rebuilds DiagnosticAccuracySummary for the given
// agent (pass agentId="" to rebuild all agents) by scanning every reviewed
// AgentDiagnostic in ns. It is safe to call from a goroutine with
// context.Background() when the originating HTTP request has already returned.
//
//nolint:gocyclo // function scans and rebuilds accuracy summaries; complexity is inherent
func (s *Server) recomputeAccuracyForAgent(ctx context.Context, ns, agentId string) error {
	var list v1alpha1.AgentDiagnosticList
	if err := s.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("list diagnostics: %w", err)
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
			err := s.client.Get(ctx, types.NamespacedName{Name: id, Namespace: ns}, &existing)
			if err != nil {
				if apierrors.IsNotFound(err) {
					if err := s.client.Create(ctx, summary); err != nil {
						return err
					}
					return s.client.Status().Update(ctx, summary)
				}
				return err
			}
			// Verify the existing CR belongs to the same agent before overwriting.
			if existing.Spec.AgentIdentity != summary.Spec.AgentIdentity {
				return fmt.Errorf("summary %q identity mismatch: got %q, want %q",
					id, existing.Spec.AgentIdentity, summary.Spec.AgentIdentity)
			}
			existing.Status = summary.Status
			return s.client.Status().Update(ctx, &existing)
		})
		if err != nil {
			log.Printf("failed to upsert summary for %s: %v", id, err)
		}
	}

	// Zero out summaries for agents that no longer have any reviewed diagnostics
	// (e.g., after their diagnostics were deleted). Without this, a recompute
	// would leave stale counts behind, defeating the recovery guarantee.
	var existingSummaries v1alpha1.DiagnosticAccuracySummaryList
	if err := s.client.List(ctx, &existingSummaries, client.InNamespace(ns)); err != nil {
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
				if err := s.client.Get(ctx, types.NamespacedName{Name: existing.Name, Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Status = v1alpha1.DiagnosticAccuracySummaryStatus{}
				return s.client.Status().Update(ctx, &fresh)
			})
			if err != nil {
				log.Printf("failed to zero stale summary %s: %v", existing.Name, err)
			}
		}
	}

	return nil
}

func (s *Server) handleRecomputeAccuracy(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}
	if !requireRole(s.roles, roleReviewer, sub, callerGroupsFromCtx(r.Context()), w) {
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}
	agentId := r.URL.Query().Get("agentIdentity")

	if err := s.recomputeAccuracyForAgent(r.Context(), ns, agentId); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"message": "recomputed accuracy summaries"})
}

func (s *Server) handleListAccuracySummaries(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}

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
	limitStr := r.URL.Query().Get("limit")
	continueToken := r.URL.Query().Get("continue")

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

	if limitStr != "" || continueToken != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"items":    items,
			"continue": list.Continue,
		})
	} else {
		writeJSON(w, http.StatusOK, items)
	}
}

//nolint:dupl // similar to handleListAgentDiagnostics
func (s *Server) handleListAuditRecords(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	if s.authRequired && sub == "" {
		writeError(w, http.StatusUnauthorized, "caller identity required")
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}
	agentReq := r.URL.Query().Get("agentRequest")
	limitStr := r.URL.Query().Get("limit")
	continueToken := r.URL.Query().Get("continue")

	listOpts := []client.ListOption{client.InNamespace(ns)}
	if agentReq != "" {
		listOpts = append(listOpts, client.MatchingLabels{"aip.io/agentRequestRef": agentReq})
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

	var list v1alpha1.AuditRecordList
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
		items = []v1alpha1.AuditRecord{}
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

func (s *Server) handleListAgentDiagnostics(w http.ResponseWriter, r *http.Request) {
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
	limitStr := r.URL.Query().Get("limit")
	continueToken := r.URL.Query().Get("continue")

	caStr := r.URL.Query().Get("createdAfter")
	cbStr := r.URL.Query().Get("createdBefore")
	hasTimeFilter := caStr != "" || cbStr != ""

	// Pagination and time-range filtering are mutually exclusive: fetching a page
	// from etcd and then dropping items in-memory breaks continuation semantics.
	if hasTimeFilter && (limitStr != "" || continueToken != "") {
		writeError(w, http.StatusBadRequest, "pagination (limit/continue) cannot be combined with createdAfter/createdBefore")
		return
	}

	ca, cb, parseErr := parseTimeRange(caStr, cbStr)
	if parseErr != nil {
		writeError(w, http.StatusBadRequest, parseErr.Error())
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
			writeError(w, http.StatusBadRequest, "invalid limit: must be a positive integer")
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
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if hasTimeFilter {
		writeJSON(w, http.StatusOK, filterAndSortDiagnostics(list.Items, ca, cb))
		return
	}

	items := list.Items
	if items == nil {
		items = []v1alpha1.AgentDiagnostic{}
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

// parseTimeRange parses RFC3339 after/before strings into time.Time values.
// Returns an error describing which field was invalid.
func parseTimeRange(afterStr, beforeStr string) (after, before time.Time, err error) {
	if afterStr != "" {
		if after, err = time.Parse(time.RFC3339, afterStr); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid createdAfter")
		}
	}
	if beforeStr != "" {
		if before, err = time.Parse(time.RFC3339, beforeStr); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid createdBefore")
		}
	}
	return after, before, nil
}

// filterAndSortDiagnostics applies in-memory time-range filtering and returns
// results sorted newest-first. Only called when pagination is not in use.
func filterAndSortDiagnostics(items []v1alpha1.AgentDiagnostic, after, before time.Time) []v1alpha1.AgentDiagnostic {
	results := make([]v1alpha1.AgentDiagnostic, 0, len(items))
	for _, item := range items {
		if !after.IsZero() && !item.CreationTimestamp.After(after) {
			continue
		}
		if !before.IsZero() && !item.CreationTimestamp.Time.Before(before) {
			continue
		}
		results = append(results, item)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].CreationTimestamp.After(results[j].CreationTimestamp.Time)
	})
	return results
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

func (s *Server) handleCreateGovernedResource(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	var gr v1alpha1.GovernedResource
	if err := json.NewDecoder(r.Body).Decode(&gr); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if gr.Spec.ContextSchema != nil {
		if err := validateContextSchema(gr.Spec.ContextSchema.Raw); err != nil {
			writeError(w, 422, fmt.Sprintf("invalid contextSchema: %v", err))
			return
		}
	}

	if err := s.checkContextSchemaConsistency(r.Context(), &gr); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	if err := s.client.Create(r.Context(), &gr); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, gr)
}

func (s *Server) handleListGovernedResources(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	var list v1alpha1.GovernedResourceList
	if err := s.client.List(r.Context(), &list); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetGovernedResource(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	name := r.PathValue("name")
	var gr v1alpha1.GovernedResource
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name}, &gr); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gr)
}

func (s *Server) handleReplaceGovernedResource(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	name := r.PathValue("name")
	var newGR v1alpha1.GovernedResource
	if err := json.NewDecoder(r.Body).Decode(&newGR); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	newGR.Name = name

	if newGR.Spec.ContextSchema != nil {
		if err := validateContextSchema(newGR.Spec.ContextSchema.Raw); err != nil {
			writeError(w, 422, fmt.Sprintf("invalid contextSchema: %v", err))
			return
		}
	}

	if err := s.checkContextSchemaConsistency(r.Context(), &newGR); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var existing v1alpha1.GovernedResource
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: name}, &existing); err != nil {
			return err
		}

		if err := checkContextSchemaAppendOnly(existing.Spec.ContextSchema, newGR.Spec.ContextSchema); err != nil {
			return fmt.Errorf("INVALID_EVOLUTION: %w", err)
		}

		existing.Spec = newGR.Spec
		existing.Labels = newGR.Labels
		existing.Annotations = newGR.Annotations
		return s.client.Update(r.Context(), &existing)
	})

	if err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if strings.Contains(err.Error(), "INVALID_EVOLUTION") {
			writeError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), "INVALID_EVOLUTION: "))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var updated v1alpha1.GovernedResource
	_ = s.client.Get(r.Context(), types.NamespacedName{Name: name}, &updated)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteGovernedResource(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	name := r.PathValue("name")
	gr := &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	if err := s.client.Delete(r.Context(), gr); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if apierrors.IsConflict(err) {
			writeError(w, http.StatusConflict, "active requests are blocking deletion (finalizer present)")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Kubernetes sets deletionTimestamp and returns 202 when finalizers are present;
	// the actual removal happens after the controller clears them. Check whether
	// the object is still terminating so callers get the correct status code.
	var check v1alpha1.GovernedResource
	if getErr := s.client.Get(r.Context(), types.NamespacedName{Name: name}, &check); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, getErr.Error())
		return
	}
	// Object still exists — finalizers are blocking final removal.
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCreateSafetyPolicy(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var sp v1alpha1.SafetyPolicy
	if err := json.NewDecoder(r.Body).Decode(&sp); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sp.Namespace = ns

	var grList v1alpha1.GovernedResourceList
	if err := s.client.List(r.Context(), &grList); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := validateSafetypolicyCEL(r.Context(), s.client, &sp, grList.Items); err != nil {
		writeError(w, 422, err.Error())
		return
	}

	if err := s.client.Create(r.Context(), &sp); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sp)
}

func (s *Server) handleListSafetyPolicies(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}

	var list v1alpha1.SafetyPolicyList
	if err := s.client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetSafetyPolicy(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}
	name := r.PathValue("name")

	var sp v1alpha1.SafetyPolicy
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &sp); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (s *Server) handleReplaceSafetyPolicy(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}
	name := r.PathValue("name")

	var newSP v1alpha1.SafetyPolicy
	if err := json.NewDecoder(r.Body).Decode(&newSP); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var grList v1alpha1.GovernedResourceList
	if err := s.client.List(r.Context(), &grList); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := validateSafetypolicyCEL(r.Context(), s.client, &newSP, grList.Items); err != nil {
		writeError(w, 422, err.Error())
		return
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var existing v1alpha1.SafetyPolicy
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &existing); err != nil {
			return err
		}
		existing.Spec = newSP.Spec
		existing.Labels = newSP.Labels
		existing.Annotations = newSP.Annotations
		return s.client.Update(r.Context(), &existing)
	})

	if err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var updated v1alpha1.SafetyPolicy
	_ = s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, &updated)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteSafetyPolicy(w http.ResponseWriter, r *http.Request) {
	sub := callerSubFromCtx(r.Context())
	groups := callerGroupsFromCtx(r.Context())
	if !requireRole(s.roles, roleAdmin, sub, groups, w) {
		return
	}

	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultNamespace
	}
	name := r.PathValue("name")

	sp := &v1alpha1.SafetyPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	if err := s.client.Delete(r.Context(), sp); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) checkContextSchemaConsistency(ctx context.Context, newGR *v1alpha1.GovernedResource) error {
	if newGR.Spec.ContextFetcher == "none" || newGR.Spec.ContextSchema == nil {
		return nil
	}

	var list v1alpha1.GovernedResourceList
	if err := s.client.List(ctx, &list); err != nil {
		return err
	}

	for _, gr := range list.Items {
		if gr.Name == newGR.Name {
			continue // Skip self on update
		}
		if gr.Spec.ContextFetcher == newGR.Spec.ContextFetcher && gr.Spec.ContextSchema != nil {
			// New schema must be append-only compatible with each peer's schema so
			// that existing CEL expressions continue to compile after rollout.
			if err := checkContextSchemaAppendOnly(gr.Spec.ContextSchema, newGR.Spec.ContextSchema); err != nil {
				return fmt.Errorf("contextSchema evolution incompatible with GovernedResource %q: %w", gr.Name, err)
			}
			// Don't return early — validate all peers.
		}
	}
	return nil
}

func checkContextSchemaAppendOnly(oldSchema, newSchema *apiextensionsv1.JSON) error {
	if oldSchema == nil {
		return nil
	}
	if newSchema == nil {
		var oldM map[string]any
		_ = json.Unmarshal(oldSchema.Raw, &oldM)
		if props, ok := oldM["properties"].(map[string]any); ok && len(props) > 0 {
			return fmt.Errorf("contextSchema is append-only: field %q was removed", "any")
		}
		return nil
	}

	var oldM, newM map[string]any
	_ = json.Unmarshal(oldSchema.Raw, &oldM)
	_ = json.Unmarshal(newSchema.Raw, &newM)

	oldProps, _ := oldM["properties"].(map[string]any)
	newProps, _ := newM["properties"].(map[string]any)

	for k, v := range oldProps {
		oldField, _ := v.(map[string]any)
		newFieldRaw, exists := newProps[k]
		if !exists {
			return fmt.Errorf("contextSchema is append-only: field %q was removed", k)
		}
		newField, _ := newFieldRaw.(map[string]any)

		if oldField["type"] != newField["type"] {
			return fmt.Errorf("contextSchema is append-only: field %q was removed or changed type", k)
		}
	}
	return nil
}
