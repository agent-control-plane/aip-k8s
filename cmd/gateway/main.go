package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	addr = flag.String("addr", ":8080", "The address to listen on for HTTP requests")
)

const defaultNamespace = "default"

type Server struct {
	client client.Client
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

type createAgentRequestBody struct {
	AgentIdentity  string                `json:"agentIdentity"`
	Action         string                `json:"action"`
	TargetURI      string                `json:"targetURI"`
	Reason         string                `json:"reason"`
	Namespace      string                `json:"namespace"`
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

	server := &Server{client: k8sClient}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /agent-requests", server.handleCreateAgentRequest)
	mux.HandleFunc("GET /agent-requests/{name}", server.handleGetAgentRequest)
	mux.HandleFunc("POST /agent-requests/{name}/executing", server.handleExecutingAgentRequest)
	mux.HandleFunc("POST /agent-requests/{name}/completed", server.handleCompletedAgentRequest)

	log.Printf("Starting AIP Demo Gateway on %s", *addr)
	if err := http.ListenAndServe(*addr, loggingMiddleware(mux)); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
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

	var cascadeModel *v1alpha1.CascadeModel
	if body.CascadeModel != nil && len(body.CascadeModel.AffectedTargets) > 0 {
		affected := make([]v1alpha1.AffectedTarget, len(body.CascadeModel.AffectedTargets))
		for i, t := range body.CascadeModel.AffectedTargets {
			affected[i] = v1alpha1.AffectedTarget{
				URI:        t.URI,
				EffectType: t.EffectType,
			}
		}
		cascadeModel = &v1alpha1.CascadeModel{
			AffectedTargets: affected,
		}
		if body.CascadeModel.ModelSourceTrust != "" {
			cascadeModel.ModelSourceTrust = &body.CascadeModel.ModelSourceTrust
		}
		if body.CascadeModel.ModelSourceID != "" {
			cascadeModel.ModelSourceID = &body.CascadeModel.ModelSourceID
		}
	}

	var reasoningTrace *v1alpha1.ReasoningTrace
	if body.ReasoningTrace != nil {
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
		reasoningTrace = rt
	}

	var parameters *apiextensionsv1.JSON
	if len(body.Parameters) > 0 && string(body.Parameters) != "null" {
		parameters = &apiextensionsv1.JSON{Raw: body.Parameters}
	}

	agentReq := &v1alpha1.AgentRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", body.AgentIdentity),
			Namespace:    ns,
		},
		Spec: v1alpha1.AgentRequestSpec{
			AgentIdentity:  body.AgentIdentity,
			Action:         body.Action,
			Target:         v1alpha1.Target{URI: body.TargetURI},
			Reason:         body.Reason,
			CascadeModel:   cascadeModel,
			ReasoningTrace: reasoningTrace,
			Parameters:     parameters,
			ExecutionMode:  body.ExecutionMode,
			ScopeBounds:    body.ScopeBounds,
		},
	}

	if err := s.client.Create(r.Context(), agentReq); err != nil {
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
