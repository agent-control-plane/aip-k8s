package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-control-plane/aip-k8s/internal/jwt"
	"github.com/agent-control-plane/aip-k8s/internal/mcp"
	"github.com/onsi/gomega"
)

func mcpRequest(method string, params any) []byte {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		panic("mcpRequest: " + err.Error())
	}
	return body
}

func TestMCPHandler_Initialize(t *testing.T) {
	g := gomega.NewWithT(t)
	s := &Server{httpClient: &http.Client{}}
	initParams := map[string]any{
		"protocolVersion": "2025-03-26",
		"clientInfo":      map[string]any{"name": "test"},
	}
	body := mcpRequest("initialize", initParams)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp mcp.JSONRPCResponse
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Error).To(gomega.BeNil())
	g.Expect(resp.JSONRPC).To(gomega.Equal("2.0"))
	result, ok := resp.Result.(map[string]any)
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(result["protocolVersion"]).To(gomega.Equal("2025-03-26"))
	serverInfo, ok := result["serverInfo"].(map[string]any)
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(serverInfo["name"]).To(gomega.Equal("aip-gateway"))
}

func TestMCPHandler_NotificationsInitialized(t *testing.T) {
	g := gomega.NewWithT(t)
	s := &Server{httpClient: &http.Client{}}
	body := mcpRequest("notifications/initialized", nil)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp mcp.JSONRPCResponse
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Error).To(gomega.BeNil())
	g.Expect(resp.Result).ToNot(gomega.BeNil())
}

func TestMCPHandler_NotificationsInitialized_NoID(t *testing.T) {
	g := gomega.NewWithT(t)
	s := &Server{httpClient: &http.Client{}}
	body := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(rr.Body.Len()).To(gomega.Equal(0))
}

func TestMCPHandler_ToolsList(t *testing.T) {
	g := gomega.NewWithT(t)
	s := &Server{httpClient: &http.Client{},
		mcpServers: []MCPServer{
			{Name: "github", URL: "http://example.com", Status: "available",
				Tools: []MCPTool{
					{Name: "create_pull_request", ReadOnly: false},
					{Name: "get_file_contents", ReadOnly: true},
				},
			},
			{Name: "jira", URL: "http://jira:8080", Status: "available",
				Tools: []MCPTool{
					{Name: "create_issue", ReadOnly: false},
				},
			},
		},
	}
	body := mcpRequest("tools/list", nil)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct{ Message string }
	}
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Error).To(gomega.BeNil())

	var result mcp.ToolsListResult
	g.Expect(json.Unmarshal(resp.Result, &result)).To(gomega.Succeed())
	g.Expect(result.Tools).To(gomega.HaveLen(4))

	toolNames := make(map[string]string)
	for _, t := range result.Tools {
		toolNames[t.Name] = t.Name
	}
	g.Expect(toolNames).To(gomega.HaveKey("github/create_pull_request"))
	g.Expect(toolNames).To(gomega.HaveKey("github/get_file_contents"))
	g.Expect(toolNames).To(gomega.HaveKey("jira/create_issue"))
	g.Expect(toolNames).To(gomega.HaveKey("aip/await_approval"))
}

func TestMCPHandler_ToolsList_Empty(t *testing.T) {
	g := gomega.NewWithT(t)
	s := &Server{httpClient: &http.Client{}}
	body := mcpRequest("tools/list", nil)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		Result json.RawMessage `json:"result"`
	}
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())

	var result mcp.ToolsListResult
	g.Expect(json.Unmarshal(resp.Result, &result)).To(gomega.Succeed())
	// aip/await_approval is always present as an internal governance tool.
	g.Expect(result.Tools).To(gomega.HaveLen(1))
	g.Expect(result.Tools[0].Name).To(gomega.Equal("aip/await_approval"))
}

func TestMCPHandler_ToolsCall_ReadOnly(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Safe to ignore: writing to an in-memory test ResponseRecorder never fails.
		_, _ = fmt.Fprintln(w, "event: message")
		_, _ = fmt.Fprintln(w, `data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"public data"}]}}`)
	}))
	defer upstream.Close()

	s := &Server{httpClient: &http.Client{Timeout: 5 * time.Second},
		mcpServers: []MCPServer{
			{Name: "github", URL: upstream.URL, Status: "available",
				Tools: []MCPTool{{Name: "get_file_contents", ReadOnly: true}}},
		},
		regCache: setupTestRegCache(),
	}
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name:      "github/get_file_contents",
		Arguments: map[string]any{"owner": "acme", "repo": "demo"},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("public data"))
}

func TestMCPHandler_ToolsCall_WriteTool_MissingJWT(t *testing.T) {
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
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name:      "github/create_pull_request",
		Arguments: map[string]any{"owner": "acme", "repo": "demo"},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	// With no k8s client configured, governance submission fails with ErrCodeInternal.
	// In production a client is always set and the call returns pending_approval instead.
	g.Expect(resp.Error).ToNot(gomega.BeNil())
	g.Expect(resp.Error.Code).To(gomega.Equal(mcp.ErrCodeInternal))
}

func TestMCPHandler_ToolsCall_WriteTool_ValidJWT(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Safe to ignore: writing to an in-memory test server ResponseRecorder never fails.
		_, _ = fmt.Fprintln(w, "event: message")
		_, _ = fmt.Fprintln(w, `data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"PR created"}]}}`)
	}))
	defer upstream.Close()

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	token, _, err := mgr.MintToken("agent-1", "github/create_pull_request", "acme/demo", "req-123")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	s := &Server{jwtManager: mgr, httpClient: &http.Client{Timeout: 5 * time.Second},
		mcpServers: []MCPServer{
			{Name: "github", URL: upstream.URL, Status: "available",
				Tools: []MCPTool{{Name: "create_pull_request", ReadOnly: false}}},
		},
		regCache: setupTestRegCache(),
	}
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name:      "github/create_pull_request",
		Arguments: map[string]any{"owner": "acme", "repo": "demo"},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	req.Header.Set("X-AIP-Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("PR created"))
}

func TestMCPHandler_ToolsCall_WriteTool_InvalidJWT(t *testing.T) {
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
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name:      "github/create_pull_request",
		Arguments: map[string]any{"owner": "acme", "repo": "demo"},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	req.Header.Set("X-AIP-Authorization", "Bearer invalid-token")
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Error).ToNot(gomega.BeNil())
	g.Expect(resp.Error.Code).To(gomega.Equal(mcp.ErrCodeAuth))
}

func TestMCPHandler_ToolsCall_Unknown(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
	}{
		{"unknown server", "jira/get_file_contents"},
		{"unknown tool", "github/create_pull_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			s := &Server{httpClient: &http.Client{},
				mcpServers: []MCPServer{
					{Name: "github", URL: "http://example.com", Status: "available",
						Tools: []MCPTool{{Name: "get_file_contents", ReadOnly: true}}},
				},
			}
			body := mcpRequest("tools/call", mcp.ToolsCallParams{
				Name:      tc.toolName,
				Arguments: map[string]any{},
			})
			req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
			rr := httptest.NewRecorder()

			s.handleMCP(rr, req)

			g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
			var resp struct {
				Error *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
			g.Expect(resp.Error).ToNot(gomega.BeNil())
			g.Expect(resp.Error.Code).To(gomega.Equal(mcp.ErrCodeInvalid))
		})
	}
}

func TestMCPHandler_MethodNotFound(t *testing.T) {
	g := gomega.NewWithT(t)
	s := &Server{httpClient: &http.Client{}}
	body := mcpRequest("unknown_method", nil)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Error).ToNot(gomega.BeNil())
	g.Expect(resp.Error.Code).To(gomega.Equal(mcp.ErrCodeMethod))
}

func TestMCPHandler_ParseError(t *testing.T) {
	g := gomega.NewWithT(t)
	s := &Server{httpClient: &http.Client{}}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader("not-json"))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Error).ToNot(gomega.BeNil())
	g.Expect(resp.Error.Code).To(gomega.Equal(mcp.ErrCodeParse))
}

func TestSplitPrefixedName_Valid(t *testing.T) {
	g := gomega.NewWithT(t)
	server, tool, err := splitPrefixedName("github/create_pull_request")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(server).To(gomega.Equal("github"))
	g.Expect(tool).To(gomega.Equal("create_pull_request"))
}

func TestSplitPrefixedName_NoSeparator(t *testing.T) {
	g := gomega.NewWithT(t)
	_, _, err := splitPrefixedName("justatoolname")
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestSplitPrefixedName_EmptyParts(t *testing.T) {
	g := gomega.NewWithT(t)
	_, _, err := splitPrefixedName("/tool")
	g.Expect(err).To(gomega.HaveOccurred())
	_, _, err = splitPrefixedName("server/")
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestMCPHandler_FullSequence(t *testing.T) {
	g := gomega.NewWithT(t)

	// The upstream must handle initialize (lazy session init), tools/list (schema
	// fetch), and tools/call (actual forwarding).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Safe to ignore: r.Body in a test server is always readable.
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		switch {
		case strings.Contains(bodyStr, `"initialize"`):
			w.Header().Set("Mcp-Session-Id", "test-session-123")
			w.WriteHeader(http.StatusOK)
		case strings.Contains(bodyStr, `"tools/list"`):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Safe to ignore: writing to an in-memory test server never fails.
			_, _ = fmt.Fprintln(w, `data: {"jsonrpc":"2.0","id":2,"result":{"tools":[`+
				`{"name":"get_file_contents","inputSchema":{"type":"object","properties":{"path":{"type":"string"}}}}]}}`)
		default:
			g.Expect(bodyStr).To(gomega.ContainSubstring(`"method":"tools/call"`))
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Safe to ignore: writing to an in-memory test server never fails.
			_, _ = fmt.Fprintln(w, "event: message")
			data := `data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"integration result"}]}}`
			_, _ = fmt.Fprintln(w, data)
		}
	}))
	defer upstream.Close()

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	registry := `[{"name":"github","url":"` + upstream.URL +
		`","status":"available","tools":[{"name":"get_file_contents","read_only":true}]}]`
	if err := os.Setenv("MCP_REGISTRY", registry); err != nil {
		t.Skipf("skipping: %v", err)
	}
	defer func() {
		// Safe to ignore: Unsetenv only errors on empty name, which is a compile-time constant.
		_ = os.Unsetenv("MCP_REGISTRY")
	}()

	mcpServers, err := loadMCPRegistry()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	s := &Server{
		jwtManager: mgr,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		mcpServers: mcpServers,
		regCache:   setupTestRegCache(),
	}

	t.Run("initialize", func(t *testing.T) {
		g := gomega.NewWithT(t)
		initParams := map[string]any{
			"protocolVersion": "2025-03-26",
			"clientInfo":      map[string]any{"name": "test"},
		}
		body := mcpRequest("initialize", initParams)
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
		rr := httptest.NewRecorder()
		s.handleMCP(rr, req)
		g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
		g.Expect(rr.Body.String()).To(gomega.ContainSubstring("protocolVersion"))
	})

	t.Run("tools/list", func(t *testing.T) {
		g := gomega.NewWithT(t)
		body := mcpRequest("tools/list", nil)
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
		rr := httptest.NewRecorder()
		s.handleMCP(rr, req)
		g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
		g.Expect(rr.Body.String()).To(gomega.ContainSubstring("github/get_file_contents"))
	})

	t.Run("tools/call read-only", func(t *testing.T) {
		g := gomega.NewWithT(t)
		body := mcpRequest("tools/call", mcp.ToolsCallParams{
			Name:      "github/get_file_contents",
			Arguments: map[string]any{"owner": "acme", "repo": "demo"},
		})
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
		rr := httptest.NewRecorder()
		s.handleMCP(rr, req)
		g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
		g.Expect(rr.Body.String()).To(gomega.ContainSubstring("integration result"))
	})
}

func TestMCPHandler_ToolsCall_EnsureSessionFailureLogged(t *testing.T) {
	g := gomega.NewWithT(t)

	// Upstream returns 200 with no Mcp-Session-Id, causing ensureSession to fail.
	// It then handles tools/call normally so we can verify the handler proceeded.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"initialize"`) {
			// No Mcp-Session-Id header: initUpstreamSession returns false.
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "event: message")
		_, _ = fmt.Fprintln(w, `data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"pod data"}]}}`)
	}))
	defer upstream.Close()

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	s := &Server{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		mcpServers: []MCPServer{
			{Name: "k8s", URL: upstream.URL, Status: "available",
				Tools:     []MCPTool{{Name: "pods_list", ReadOnly: true}},
				sessionMu: &sync.Mutex{}},
		},
		regCache: setupTestRegCache(),
	}
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name:      "k8s/pods_list",
		Arguments: map[string]any{"namespace": "default"},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(logBuf.String()).To(gomega.ContainSubstring("Failed to establish session with k8s"))
	// Handler proceeds and returns the upstream result.
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("pod data"))
}

func TestMCPHandler_ToolsCall_Upstream401_ResetsSession(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	mu := &sync.Mutex{}
	s := &Server{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		mcpServers: []MCPServer{
			{Name: "k8s", URL: upstream.URL, Status: "available",
				Tools:        []MCPTool{{Name: "pods_list", ReadOnly: true}},
				sessionMu:    mu,
				sessionReady: true}, // pre-warmed session
		},
		regCache: setupTestRegCache(),
	}
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name:      "k8s/pods_list",
		Arguments: map[string]any{"namespace": "default"},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Error).ToNot(gomega.BeNil())
	g.Expect(resp.Error.Code).To(gomega.Equal(mcp.ErrCodeInternal))

	// After 401, resetSession must have cleared the ready flag.
	mu.Lock()
	ready := s.mcpServers[0].sessionReady
	mu.Unlock()
	g.Expect(ready).To(gomega.BeFalse())
}

func TestMCPHandler_ToolsCall_WriteTool_RepoMismatch(t *testing.T) {
	g := gomega.NewWithT(t)

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	token, _, err := mgr.MintToken("agent-1", "github/create_pull_request", "acme/demo", "req-123")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	s := &Server{jwtManager: mgr, httpClient: &http.Client{},
		mcpServers: []MCPServer{
			{Name: "github", URL: "http://example.com", Status: "available",
				Tools: []MCPTool{{Name: "create_pull_request", ReadOnly: false}}},
		},
	}
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name:      "github/create_pull_request",
		Arguments: map[string]any{"owner": "evil", "repo": "demo"},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	req.Header.Set("X-AIP-Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Error).ToNot(gomega.BeNil())
	g.Expect(resp.Error.Code).To(gomega.Equal(mcp.ErrCodeForbidden))
}

func TestMCPHandler_ToolsCall_WriteTool_K8sResourceMismatch(t *testing.T) {
	g := gomega.NewWithT(t)

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// JWT authorizes "other-app"; call targets "payment-api".
	token, _, err := mgr.MintToken("agent-1", "k8s/resources_scale",
		"k8s://default/deployment/other-app", "req-456")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	s := &Server{jwtManager: mgr, httpClient: &http.Client{},
		mcpServers: []MCPServer{
			{Name: "k8s", URL: "http://example.com", Status: "available",
				Tools: []MCPTool{{Name: "resources_scale", ReadOnly: false}}},
		},
	}
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name: "k8s/resources_scale",
		Arguments: map[string]any{
			"_aip_authorization": token,
			"namespace":          "default",
			"name":               "payment-api",
			"kind":               "Deployment",
		},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Error).ToNot(gomega.BeNil())
	g.Expect(resp.Error.Code).To(gomega.Equal(mcp.ErrCodeForbidden))
}

// TestMCPHandler_ToolsCall_NilRegCache_Returns403 verifies the fail-closed
// credential binding: when Server.regCache is nil, forwardToolCall must return
// HTTP 403 rather than silently falling back to an unauthenticated request.
func TestMCPHandler_ToolsCall_NilRegCache_Returns403(t *testing.T) {
	g := gomega.NewWithT(t)

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	token, _, err := mgr.MintToken("agent-1", "github/create_pull_request", "acme/demo", "req-123")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// regCache is intentionally nil — simulates a gateway with no AgentRegistration watch.
	s := &Server{
		jwtManager: mgr,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		mcpServers: []MCPServer{
			{Name: "github", URL: "http://example.com", Status: "available",
				Tools: []MCPTool{{Name: "create_pull_request", ReadOnly: false}}},
		},
		regCache: nil,
	}
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name:      "github/create_pull_request",
		Arguments: map[string]any{"owner": "acme", "repo": "demo"},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	req.Header.Set("X-AIP-Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("no credential binding"))
}

// TestMCPHandler_ToolsCall_MissingBinding_Returns403 verifies the fail-closed
// credential binding: when the regCache exists but has no provider for the
// (agentIdentity, serverName) pair, forwardToolCall must return HTTP 403.
func TestMCPHandler_ToolsCall_MissingBinding_Returns403(t *testing.T) {
	g := gomega.NewWithT(t)

	keyPath, cleanup := generateTestKeyFile(t)
	defer cleanup()
	mgr, err := jwt.NewManager(keyPath, time.Now)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// enforceResourceClaim (for non-GitHub servers) compares the JWT resource
	// claim against buildTargetURI(args) = "k8s://<ns>/<kind>/<name>".
	// Use matching values so the resource check passes and we reach forwardToolCall.
	token, _, err := mgr.MintToken("agent-1", "unknown-server/create_resource",
		"k8s://default/deployment/my-app", "req-456")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	s := &Server{
		jwtManager: mgr,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		mcpServers: []MCPServer{
			{Name: "unknown-server", URL: "http://example.com", Status: "available",
				Tools: []MCPTool{{Name: "create_resource", ReadOnly: false}}},
		},
		// setupTestRegCache has providers for agent-1 on github/k8s/jira/github-mcp,
		// but NOT on "unknown-server" — providerFor returns nil → 403.
		regCache: setupTestRegCache(),
	}
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name: "unknown-server/create_resource",
		Arguments: map[string]any{
			"namespace": "default",
			"name":      "my-app",
			"kind":      "Deployment",
		},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	req.Header.Set("X-AIP-Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req)

	g.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(rr.Body.String()).To(gomega.ContainSubstring("no credential binding"))
}
