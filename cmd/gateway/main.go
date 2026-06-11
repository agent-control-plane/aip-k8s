package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/jwt"
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
	requireGovernedResourceFlag = flag.Bool("require-governed-resource", false,
		"When true, reject AgentRequests even if no GovernedResource objects exist. "+
			"Default false preserves backward compatibility for deployments without a populated registry.")
	trustedProxyCIDRs = flag.String("trusted-proxy-cidrs", "",
		"Comma-separated CIDRs for proxy-header trust. Empty = any source (dev only). Ignored when --oidc-issuer-url is set.")
	waitTimeout = flag.Duration("wait-timeout", 90*time.Second,
		"Maximum time the gateway will poll for AgentRequest resolution before returning 504.")
	jwtKeyPath = flag.String("jwt-key-path", "",
		"Path to Ed25519 private key PEM file for JWT signing")
	jwtKey = flag.String("jwt-key", "",
		"PEM-encoded Ed25519 private key for JWT signing (alternative to --jwt-key-path). "+
			"Typically sourced from a K8s Secret via environment variable.")
	unregisteredAgentPolicy = flag.String("unregistered-agent-policy", "allow",
		"Policy for unregistered agents: 'allow', 'warn', or 'strict'")
)

const (
	defaultKeyWatchInterval = 5 * time.Minute
	shutdownTimeout         = 5 * time.Second
)

func main() { //nolint:gocyclo  // setup-heavy, acceptable for main
	flag.Parse()

	// Load custom CA certs from SSL_CERT_FILE programmatically if set (primarily for macOS)
	if sslCertFile := os.Getenv("SSL_CERT_FILE"); sslCertFile != "" {
		pemCerts, err := os.ReadFile(sslCertFile)
		if err == nil {
			sysPool, err := x509.SystemCertPool()
			if err != nil || sysPool == nil {
				sysPool = x509.NewCertPool()
			}
			if sysPool.AppendCertsFromPEM(pemCerts) {
				if transport, ok := http.DefaultTransport.(*http.Transport); ok {
					if transport.TLSClientConfig == nil {
						transport.TLSClientConfig = &tls.Config{}
					}
					transport.TLSClientConfig.RootCAs = sysPool
				}
			}
		}
	}

	if *unregisteredAgentPolicy != "allow" && *unregisteredAgentPolicy != "warn" && *unregisteredAgentPolicy != "strict" {
		log.Fatalf("invalid --unregistered-agent-policy %q: must be allow, warn, or strict", *unregisteredAgentPolicy)
	}

	// Load KubeConfig — use the standard loading rules which handle
	// colon-separated KUBECONFIG, ~/.kube/config, and in-cluster config.
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		// Fallback to in-cluster config
		cfg, err = rest.InClusterConfig()
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Create Controller-Runtime Client (direct watch-capable client).
	k8sClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("Failed to create watch client: %v", err)
	}

	// Create manager for cached reads + field indexers.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         false,
		HealthProbeBindAddress: "",
		// "0" disables the metrics listener; "" would default to :8080, conflicting with the gateway.
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		log.Fatalf("Failed to create manager: %v", err)
	}

	// Register status.phase field indexer for server-side phase filtering.
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1alpha1.AgentRequest{}, agentRequestPhaseIndexKey,
		agentRequestPhaseIndexFunc); err != nil {
		log.Fatalf("Failed to register status.phase field indexer: %v", err)
	}

	// Start manager cache in background.
	go func() {
		if err := mgr.Start(ctx); err != nil {
			log.Fatalf("Manager failed: %v", err)
		}
	}()

	// Wait for cache to sync before serving requests.
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		log.Fatalf("Manager cache failed to sync")
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

	var jwtMgr *jwt.Manager
	switch {
	case *jwtKey != "":
		var err error
		jwtMgr, err = jwt.NewManagerFromPEM([]byte(*jwtKey), time.Now)
		if err != nil {
			log.Fatalf("Failed to parse JWT key from --jwt-key: %v", err)
		}
		log.Printf("JWT manager initialized from --jwt-key")
	case *jwtKeyPath != "":
		var err error
		jwtMgr, err = jwt.NewManager(*jwtKeyPath, time.Now)
		if err != nil {
			log.Fatalf("Failed to load JWT key: %v", err)
		}
		log.Printf("JWT manager initialized with key: %s", *jwtKeyPath)
		jwtMgr.StartKeyWatcher(ctx, *jwtKeyPath, defaultKeyWatchInterval, log.Printf)
	}

	mcpServers, err := loadMCPRegistry()
	if err != nil {
		log.Fatalf("Failed to load MCP registry: %v", err)
	}
	// Upstream sessions are initialized lazily on first tools/call.

	mcpCache := newMCPServerCache()
	for i := range mcpServers {
		srv := &mcpServers[i]
		// seed sets Pinned=true so the watch eviction loop never removes
		// env-var-configured servers even when no matching MCPServer CRD exists.
		mcpCache.seed(srv.Name, srv.URL, srv.BearerToken, srv.Tools)
	}

	regCache := newRegistrationCache(k8sClient)

	server := &Server{
		client:                  mgr.GetClient(),    // cached — field indexers work here
		apiReader:               mgr.GetAPIReader(), // direct — bypass cache for consistency-sensitive reads
		watchClient:             k8sClient,          // raw watch — keep using direct client
		dedupWindow:             *dedupWindow,
		waitTimeout:             wt,
		roles:                   rc,
		authRequired:            authRequired,
		requireGovernedResource: *requireGovernedResourceFlag,
		jwtManager:              jwtMgr,
		httpClient:              &http.Client{Timeout: 30 * time.Second},
		mcpServers:              mcpServers,
		mcpCache:                mcpCache,
		regCache:                regCache,
		unregisteredAgentPolicy: *unregisteredAgentPolicy,
	}

	go watchMCPServers(ctx, k8sClient, mcpCache)
	go watchAgentRegistrations(ctx, k8sClient, regCache)
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
	mux.HandleFunc("GET /agent-requests/{name}/watch", server.handleWatchAgentRequest)
	mux.HandleFunc("POST /agent-requests/{name}/executing", server.handleExecutingAgentRequest)
	mux.HandleFunc("POST /agent-requests/{name}/completed", server.handleCompletedAgentRequest)
	mux.HandleFunc("PUT /agent-requests/{name}/result", server.handlePutAgentRequestResult)
	mux.HandleFunc("POST /agent-requests/{name}/approve", server.handleApproveAgentRequest)
	mux.HandleFunc("POST /agent-requests/{name}/deny", server.handleDenyAgentRequest)
	mux.HandleFunc("PATCH /agent-requests/{name}/verdict", server.handleVerdictAgentRequest)
	mux.HandleFunc("POST /agent-requests/{name}/token", server.handleGetAgentRequestToken)
	mux.HandleFunc("GET /audit-records", server.handleListAuditRecords)
	mux.HandleFunc("POST /agent-requests/recompute-accuracy", server.handleRecomputeAccuracy)
	mux.HandleFunc("GET /diagnostic-accuracy-summaries", server.handleListAccuracySummaries)
	mux.HandleFunc("GET /agent-trust-profiles", server.handleListAgentTrustProfiles)
	mux.HandleFunc("GET /agent-trust-profiles/{name}", server.handleGetAgentTrustProfile)
	mux.HandleFunc("GET /mcp-registry", server.handleMCPRegistry)
	mux.HandleFunc("POST /mcp-proxy/{server}/{tool}", server.handleMCPProxy)
	mux.HandleFunc("POST /mcp", server.handleMCP)

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
	mux.HandleFunc("POST /agent-registrations", server.handleCreateAgentRegistration)
	mux.HandleFunc("GET /agent-registrations", server.handleListAgentRegistrations)
	mux.HandleFunc("GET /agent-registrations/{name}", server.handleGetAgentRegistration)
	mux.HandleFunc("PUT /agent-registrations/{name}", server.handleReplaceAgentRegistration)
	mux.HandleFunc("DELETE /agent-registrations/{name}", server.handleDeleteAgentRegistration)
	mux.HandleFunc("POST /agent-graduation-policies", server.handleCreateAgentGraduationPolicy)
	mux.HandleFunc("GET /agent-graduation-policies", server.handleListAgentGraduationPolicies)
	mux.HandleFunc("GET /agent-graduation-policies/{name}", server.handleGetAgentGraduationPolicy)
	mux.HandleFunc("PUT /agent-graduation-policies/{name}", server.handleReplaceAgentGraduationPolicy)
	mux.HandleFunc("DELETE /agent-graduation-policies/{name}", server.handleDeleteAgentGraduationPolicy)

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
	handler := authMiddleware(mux)
	srv := &http.Server{
		Addr:    *addr,
		Handler: metricsMiddleware(loggingMiddleware(handler)),
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server failed: %v", err)
	}
}

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
