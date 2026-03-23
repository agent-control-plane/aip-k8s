package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	governancev1alpha1 "github.com/ravisantoshgudimetla/aip-k8s/api/v1alpha1"
)

var (
	scheme = runtime.NewScheme()
)

const defaultNamespace = "default"

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = governancev1alpha1.AddToScheme(scheme)
}

type DashboardServer struct {
	client client.Client
	port   int
	dir    string
}

func main() {
	port := flag.Int("port", 8082, "Port to run the dashboard on")
	flag.Parse()

	cfg, err := config.GetConfig()
	if err != nil {
		log.Fatalf("Failed to get kubeconfig: %v", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	// Aggressive static directory detection
	cwd, _ := os.Getwd()
	staticDir := ""

	// Search order:
	// 1. Current directory (if index.html is here)
	// 2. demo/dashboard subdirectory
	// 3. ../ (if we are in a child of dashboard)
	if _, err := os.Stat("index.html"); err == nil {
		staticDir = cwd
	} else if _, err := os.Stat(filepath.Join(cwd, "demo", "dashboard", "index.html")); err == nil {
		staticDir = filepath.Join(cwd, "demo", "dashboard")
	} else if _, err := os.Stat(filepath.Join(cwd, "dashboard", "index.html")); err == nil {
		staticDir = filepath.Join(cwd, "dashboard")
	}

	if staticDir == "" {
		log.Printf("CRITICAL: index.html not found! Check directory structure. CWD: %s", cwd)
		// Fallback to best guess
		staticDir = filepath.Join(cwd, "demo", "dashboard")
	}

	absStaticDir, _ := filepath.Abs(staticDir)

	server := &DashboardServer{
		client: c,
		port:   *port,
		dir:    absStaticDir,
	}

	mux := http.NewServeMux()

	// Logger middleware
	logMiddleware := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			h.ServeHTTP(rw, r)
			log.Printf("%s %s %s %d", r.RemoteAddr, r.Method, r.URL.Path, rw.status)
		})
	}

	// API Handlers
	mux.HandleFunc("/api/agent-requests", server.handleListAgentRequests)
	mux.HandleFunc("/api/agent-requests/", server.handleAgentRequestAction)
	mux.HandleFunc("/api/audit-records", server.handleListAuditRecords)

	// Explicitly serve index.html for the root
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, filepath.Join(absStaticDir, "index.html"))
			return
		}
		// Serve other static files
		http.FileServer(http.Dir(absStaticDir)).ServeHTTP(w, r)
	})

	fmt.Printf("AIP Visual Audit Dashboard starting on http://localhost:%d\n", *port)
	fmt.Printf("Serving static files from: %s\n", absStaticDir)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), logMiddleware(mux)))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (s *DashboardServer) handleListAgentRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = defaultNamespace
	}

	var list governancev1alpha1.AgentRequestList
	if err := s.client.List(context.Background(), &list, client.InNamespace(namespace)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list.Items)
}

func (s *DashboardServer) handleListAuditRecords(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = defaultNamespace
	}
	reqName := r.URL.Query().Get("agentRequest")

	var list governancev1alpha1.AuditRecordList
	if err := s.client.List(context.Background(), &list, client.InNamespace(namespace)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Filter by agentRequest if provided
	results := []governancev1alpha1.AuditRecord{}
	for _, item := range list.Items {
		if reqName == "" || item.Spec.AgentRequestRef == reqName {
			results = append(results, item)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(results)
}

func (s *DashboardServer) handleAgentRequestAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Path: /api/agent-requests/{name}/{approve|deny}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/agent-requests/"), "/")
	if len(parts) < 2 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	name := parts[0]
	action := parts[1]
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = defaultNamespace
	}

	var decision string
	switch action {
	case "approve":
		decision = "approved"
	case "deny":
		decision = "denied"
	default:
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	nn := types.NamespacedName{Name: name, Namespace: namespace}

	var agentReq governancev1alpha1.AgentRequest
	if err := s.client.Get(ctx, nn, &agentReq); err != nil {
		log.Printf("ERROR get %s/%s: %v", namespace, name, err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Write the human decision to spec via merge patch — avoids 409 conflicts
	// with the controller's concurrent status updates (they share resourceVersion).
	// The controller owns status and will drive the state machine when it sees
	// spec.humanApproval.decision set.
	specPatch := client.MergeFrom(agentReq.DeepCopy())
	agentReq.Spec.HumanApproval = &governancev1alpha1.HumanApproval{
		Decision: decision,
		Reason:   "Decision made via Visual Audit Dashboard",
	}
	log.Printf("PATCH spec.humanApproval=%s on %s/%s (RV=%s)", decision, namespace, name, agentReq.ResourceVersion)
	if err := s.client.Patch(ctx, &agentReq, specPatch); err != nil {
		log.Printf("ERROR patch %s/%s: %v", namespace, name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("OK spec.humanApproval=%s patched on %s/%s (new RV=%s)",
		decision, namespace, name, agentReq.ResourceVersion)

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "Action %s applied to %s", action, name)
}
