package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-control-plane/aip-k8s/internal/jwt"
	"github.com/onsi/gomega"
)

func generateTestKeyFile(t *testing.T) (string, func()) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp("", "jwt-test-*.key")
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name(), func() {
		if err := os.Remove(f.Name()); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}
}

func TestMCPProxy_FullFlow_ValidJWT(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		g.Expect(string(body)).To(gomega.ContainSubstring(`"jsonrpc":"2.0"`))
		g.Expect(string(body)).To(gomega.ContainSubstring(`"method":"tools/call"`))
		g.Expect(string(body)).To(gomega.ContainSubstring(`"params"`))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "event: message")
		_, _ = fmt.Fprintln(w, `data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"file contents"}]}}`)
	}))
	defer upstream.Close()

	registry := `[{"name":"github","url":"` + upstream.URL +
		`","status":"available","tools":[{"name":"get_file_contents","read_only":true}]}]`
	if err := os.Setenv("MCP_REGISTRY", registry); err != nil {
		t.Skipf("skipping: %v", err)
	}
	defer func() { _ = os.Unsetenv("MCP_REGISTRY") }()

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	token, _, err := mgr.MintToken("agent-1", "github/get_file_contents", "acme/demo", "req-123")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	mcpServers, err := loadMCPRegistry()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	s := &Server{jwtManager: mgr, httpClient: &http.Client{Timeout: 5 * time.Second}, mcpServers: mcpServers}
	req := httptest.NewRequest("POST", "/mcp-proxy/github/get_file_contents",
		strings.NewReader(`{"name":"get_file_contents","arguments":{"owner":"acme","repo":"demo"}}`))
	req.SetPathValue("server", "github")
	req.SetPathValue("tool", "get_file_contents")
	req.Header.Set("X-AIP-Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	s.handleMCPProxy(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("file contents"))
}

func TestMCPProxy_ReadOnlyNoAuth(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "event: message")
		_, _ = fmt.Fprintln(w, `data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"public data"}]}}`)
	}))
	defer upstream.Close()

	s := &Server{httpClient: &http.Client{Timeout: 5 * time.Second},
		mcpServers: []MCPServer{
			{Name: "github", URL: upstream.URL, Status: "available",
				Tools: []MCPTool{{Name: "get_file_contents", ReadOnly: true}}},
		},
	}
	req := httptest.NewRequest("POST", "/mcp-proxy/github/get_file_contents",
		strings.NewReader(`{"name":"get_file_contents","arguments":{"owner":"acme","repo":"demo"}}`))
	req.SetPathValue("server", "github")
	req.SetPathValue("tool", "get_file_contents")
	rr := httptest.NewRecorder()

	s.handleMCPProxy(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("public data"))
}

func TestMCPProxy_InvalidJWT_WriteTool(t *testing.T) {
	g := gomega.NewWithT(t)

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	s := &Server{jwtManager: mgr, httpClient: &http.Client{},
		mcpServers: []MCPServer{
			{Name: "github", URL: "http://example.com", Status: "available",
				Tools: []MCPTool{{Name: "create_pull_request", ReadOnly: false}}},
		},
	}
	req := httptest.NewRequest("POST", "/mcp-proxy/github/create_pull_request",
		strings.NewReader("{}"))
	req.SetPathValue("server", "github")
	req.SetPathValue("tool", "create_pull_request")
	req.Header.Set("X-AIP-Authorization", "Bearer invalid-token")
	rr := httptest.NewRecorder()

	s.handleMCPProxy(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusUnauthorized))
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("invalid AIP token"))
}

func TestMCPProxy_UnknownServer(t *testing.T) {
	g := gomega.NewWithT(t)

	if err := os.Setenv("MCP_REGISTRY",
		`[{"name":"github","url":"http://example.com","status":"available","tools":[]}]`); err != nil {
		t.Skipf("skipping: %v", err)
	}
	defer func() { _ = os.Unsetenv("MCP_REGISTRY") }()

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// The "jira" server does not exist in MCP_REGISTRY, so the handler returns 404
	// before any JWT validation. The action claim is irrelevant to this test.
	token, _, err := mgr.MintToken("agent-1", "whatever/whatever", "acme/demo", "req-123")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	mcpServers, err := loadMCPRegistry()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	s := &Server{jwtManager: mgr, httpClient: &http.Client{}, mcpServers: mcpServers}
	req := httptest.NewRequest("POST", "/mcp-proxy/jira/get_file_contents",
		strings.NewReader("{}"))
	req.SetPathValue("server", "jira")
	req.SetPathValue("tool", "get_file_contents")
	req.Header.Set("X-AIP-Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	s.handleMCPProxy(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusNotFound))
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("MCP server not found"))
}

func TestMCPProxy_UnknownTool(t *testing.T) {
	g := gomega.NewWithT(t)

	if err := os.Setenv("MCP_REGISTRY",
		`[{"name":"github","url":"http://example.com","status":"available",`+
			`"tools":[{"name":"get_file_contents","read_only":true}]}]`); err != nil {
		t.Skipf("skipping: %v", err)
	}
	defer func() { _ = os.Unsetenv("MCP_REGISTRY") }()

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	token, _, err := mgr.MintToken("agent-1", "github/get_file_contents", "acme/demo", "req-123")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	mcpServers, err := loadMCPRegistry()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	s := &Server{jwtManager: mgr, httpClient: &http.Client{}, mcpServers: mcpServers}
	req := httptest.NewRequest("POST", "/mcp-proxy/github/create_pull_request",
		strings.NewReader("{}"))
	req.SetPathValue("server", "github")
	req.SetPathValue("tool", "create_pull_request")
	req.Header.Set("X-AIP-Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	s.handleMCPProxy(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusNotFound))
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("tool not found"))
}

func TestMCPProxy_WriteTool_ActionMismatch(t *testing.T) {
	g := gomega.NewWithT(t)

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	token, _, err := mgr.MintToken("agent-1", "github/list_pull_requests", "acme/demo", "req-123")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	s := &Server{jwtManager: mgr, httpClient: &http.Client{},
		mcpServers: []MCPServer{
			{Name: "github", URL: "http://example.com", Status: "available",
				Tools: []MCPTool{{Name: "create_pull_request", ReadOnly: false}}},
		},
	}
	req := httptest.NewRequest("POST", "/mcp-proxy/github/create_pull_request",
		strings.NewReader("{}"))
	req.SetPathValue("server", "github")
	req.SetPathValue("tool", "create_pull_request")
	req.Header.Set("X-AIP-Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	s.handleMCPProxy(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("tool not allowed"))
}
