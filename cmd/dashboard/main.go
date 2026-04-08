package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

type DashboardServer struct {
	gatewayURL string
	port       int
	staticDir  string
	httpClient *http.Client
}

func main() {
	var port int
	var staticDir string
	var gatewayURL string
	flag.IntVar(&port, "port", 8082, "Port to run the dashboard on")
	flag.StringVar(&staticDir, "static-dir", "cmd/dashboard",
		"Directory containing static frontend files (index.html, app.js, styles.css)")
	flag.StringVar(&gatewayURL, "gateway-url", "http://localhost:8080", "The base URL of the AIP gateway")
	flag.Parse()

	absStaticDir, err := filepath.Abs(staticDir)
	if err != nil {
		log.Fatalf("Invalid static-dir %q: %v", staticDir, err)
	}

	server := &DashboardServer{
		gatewayURL: strings.TrimRight(gatewayURL, "/"),
		port:       port,
		staticDir:  absStaticDir,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}

	mux := http.NewServeMux()

	logMiddleware := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			h.ServeHTTP(rw, r)
			log.Printf("%s %s %s %d", r.RemoteAddr, r.Method, r.URL.Path, rw.status)
		})
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
	})

	// All /api/* requests are proxied to the gateway (strip /api prefix).
	mux.HandleFunc("/api/", server.proxyToGateway)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		if r.URL.Path == "/" {
			http.ServeFile(w, r, filepath.Join(absStaticDir, "index.html"))
			return
		}
		http.FileServer(http.Dir(absStaticDir)).ServeHTTP(w, r)
	})

	fmt.Printf("AIP Visual Audit Dashboard starting on http://localhost:%d\n", port)
	fmt.Printf("Proxying API calls to gateway: %s\n", server.gatewayURL)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), logMiddleware(mux)))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// proxyToGateway forwards /api/* requests to the gateway by stripping the
// /api prefix. Example: /api/agent-requests?namespace=X → {gatewayURL}/agent-requests?namespace=X
func (s *DashboardServer) proxyToGateway(w http.ResponseWriter, r *http.Request) {
	targetPath := strings.TrimPrefix(r.URL.Path, "/api")
	targetURL := s.gatewayURL + targetPath
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Only forward safe, non-identity headers. X-Remote-User / X-Forwarded-User
	// are intentionally excluded: forwarding client-supplied identity headers
	// would allow browser users to impersonate arbitrary identities if the
	// gateway trusts the dashboard pod IP via --trusted-proxy-cidrs.
	for _, h := range []string{"Authorization", "Content-Type"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
