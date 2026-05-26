package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/agent-control-plane/aip-k8s/internal/jwt"
	"github.com/agent-control-plane/aip-k8s/internal/mcp"
)

var (
	ErrJWTMissing       = errors.New("missing AIP bearer token")
	ErrJWTInvalid       = errors.New("invalid AIP token")
	ErrJWTActionDenied  = errors.New("tool not allowed for this action")
	ErrJWTNotConfigured = errors.New("JWT signing not configured")
)

const mcpRequestTimeout = 30 * time.Second

func (s *Server) handleMCPProxy(w http.ResponseWriter, r *http.Request) {
	serverName := r.PathValue("server")
	toolName := r.PathValue("tool")

	mcpServer := s.findMCPServer(serverName)
	if mcpServer == nil {
		writeError(w, http.StatusNotFound, "MCP server not found: "+serverName)
		return
	}

	tool := s.findTool(mcpServer, toolName)
	if tool == nil {
		writeError(w, http.StatusNotFound, "tool not found: "+toolName)
		return
	}

	var claims *jwt.Claims
	var agent, action, requestRef string
	if !tool.ReadOnly {
		var authErr error
		claims, agent, action, requestRef, authErr = s.authorizeWriteTool(r, serverName+"/"+toolName)
		if authErr != nil {
			switch {
			case errors.Is(authErr, ErrJWTNotConfigured):
				writeError(w, http.StatusServiceUnavailable, authErr.Error())
			case errors.Is(authErr, ErrJWTActionDenied):
				writeError(w, http.StatusForbidden, authErr.Error())
			default:
				writeError(w, http.StatusUnauthorized, authErr.Error())
			}
			return
		}
	} else {
		agent = callerSubFromCtx(r.Context())
		action = serverName + "/" + toolName
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body: "+err.Error())
		return
	}

	args, err := parseProxyBody(body, toolName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	// Lazily establish the upstream session on first use.
	if !mcpServer.ensureSession(s.httpClient) {
		log.Printf("Failed to establish session with %s", mcpServer.Name)
	}
	if s.mcpCache != nil {
		s.mcpCache.commitSession(mcpServer.Name, mcpServer.SessionID, mcpServer.sessionReady, mcpServer.URL)
	}

	if !tool.ReadOnly {
		if repoErr := enforceRepoClaim(claims.Resource, args); repoErr != "" {
			writeError(w, http.StatusForbidden, repoErr)
			return
		}
	}

	result, errMsg := s.forwardToolCall(r.Context(), mcpServer, args, toolName, 1)
	if errMsg != "" {
		if !tool.ReadOnly && requestRef != "" {
			s.failAgentRequest(r.Context(), requestRef, "MCP tool execution failed: "+errMsg)
		}
		s.emitMCPLog(agent, serverName, toolName, action, http.StatusBadGateway, requestRef, mcpServer.URL)
		writeError(w, http.StatusBadGateway, errMsg)
		return
	}

	if result.IsError {
		contentStr := "MCP tool returned an error"
		if len(result.Content) > 0 {
			var textBlock struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(result.Content[0], &textBlock) == nil && textBlock.Text != "" {
				contentStr = textBlock.Text
			}
		}
		if !tool.ReadOnly && requestRef != "" {
			s.failAgentRequest(r.Context(), requestRef, "MCP tool returned an error: "+contentStr)
		}
		s.emitMCPLog(agent, serverName, toolName, action, http.StatusBadGateway, requestRef, mcpServer.URL)
		writeError(w, http.StatusBadGateway, contentStr)
		return
	}

	// JWT-authorized write tool succeeded: advance AR through Executing → Completed
	// so the controller releases the OpsLock.
	if !tool.ReadOnly && requestRef != "" {
		go func(ref string) {
			ctx, cancel := context.WithTimeout(context.Background(), mcpRequestTimeout)
			defer cancel()
			s.completeAgentRequest(ctx, ref, "")
		}(requestRef)
	}

	s.emitMCPLog(agent, serverName, toolName, action, http.StatusOK, requestRef, mcpServer.URL)
	writeJSON(w, http.StatusOK, result)
}

// mcpProxyResult is the subset of a JSON-RPC tools/call result returned to callers.
type mcpProxyResult struct {
	Content []json.RawMessage `json:"content"`
	IsError bool              `json:"isError,omitempty"`
}

// extractMCPResult parses an SSE response body from the MCP server, unwraps the
// JSON-RPC envelope, and returns the tool result. Returns ("", errMsg) on failure.
func extractMCPResult(body []byte) (mcpProxyResult, string) {
	dataLine, err := mcp.ExtractSSEDataLine(body)
	if err != nil {
		return mcpProxyResult{}, err.Error()
	}

	var rpc struct {
		Result *mcpProxyResult `json:"result,omitempty"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(dataLine), &rpc); err != nil {
		return mcpProxyResult{}, "MCP server returned invalid JSON-RPC response"
	}
	if rpc.Error != nil {
		return mcpProxyResult{}, "MCP server error: " + rpc.Error.Message
	}
	if rpc.Result == nil {
		return mcpProxyResult{}, "MCP server returned empty result"
	}
	return *rpc.Result, ""
}

// enforceRepoClaim validates that the JWT's Resource claim (a github:// URI)
// matches the owner and repo arguments in the proxy request body.
func enforceRepoClaim(claimsResource string, args map[string]any) string {
	if claimsResource == "" {
		return "token missing resource claim"
	}
	claimsResource = strings.TrimPrefix(claimsResource, "github://")
	parts := strings.SplitN(claimsResource, "/", 3)
	if len(parts) < 2 {
		return "token has invalid resource claim"
	}
	claimsOwner := parts[0]
	claimsRepoName := parts[1]

	argOwner, _ := args["owner"].(string)
	argRepo, _ := args["repo"].(string)

	if argOwner == "" || argRepo == "" {
		return "request body missing owner or repo arguments"
	}
	if argOwner != claimsOwner {
		return fmt.Sprintf("owner mismatch: token has %q, request has %q", claimsOwner, argOwner)
	}
	if argRepo != claimsRepoName {
		return fmt.Sprintf("repo mismatch: token has %q, request has %q", claimsRepoName, argRepo)
	}
	return ""
}

// enforceResourceClaim validates that the JWT's Resource claim matches the
// target URI derived from the tool arguments. Used for non-GitHub MCP tools.
func enforceResourceClaim(claimsResource string, args map[string]any) string {
	if claimsResource == "" {
		return "token missing resource claim"
	}
	expected := buildTargetURI(args)
	if claimsResource != expected {
		return fmt.Sprintf("resource mismatch: token has %q, request targets %q", claimsResource, expected)
	}
	return ""
}

// findMCPServer looks up an MCP server by name.
// The CRD-backed cache is checked first; if the server is not found there,
// we fall back to the env-var registry.  In production (main.go) env-var
// servers are pre-loaded into the cache, so the fallback is primarily for
// tests that construct Server directly without a cache.
func (s *Server) findMCPServer(name string) *MCPServer {
	if s.mcpCache != nil {
		if cached := s.mcpCache.get(name); cached != nil {
			return cached
		}
	}
	for i := range s.mcpServers {
		if s.mcpServers[i].Name == name {
			return &s.mcpServers[i]
		}
	}
	return nil
}

func (s *Server) findTool(server *MCPServer, toolName string) *MCPTool {
	for i := range server.Tools {
		if server.Tools[i].Name == toolName {
			return &server.Tools[i]
		}
	}
	return nil
}

// parseProxyBody extracts arguments from the proxy request body and validates
// the tool name matches the path parameter.
func parseProxyBody(body []byte, toolName string) (map[string]any, error) {
	var payload struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.Name != "" && payload.Name != toolName {
		return nil, fmt.Errorf("tool name mismatch: body has %q, path has %q", payload.Name, toolName)
	}
	return payload.Arguments, nil
}

// authorizeWriteTool validates the AIP JWT from X-AIP-Authorization for a write tool.
// Returns the claims and derived fields on success. Returns an error on failure.
// Read-only tools skip authorization and return without error.
func (s *Server) authorizeWriteTool(r *http.Request, toolName string) (*jwt.Claims, string, string, string, error) {
	if s.jwtManager == nil {
		return nil, "", "", "", ErrJWTNotConfigured
	}
	auth := r.Header.Get("X-AIP-Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return nil, "", "", "", ErrJWTMissing
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	claims, err := s.jwtManager.ValidateToken(token)
	if err != nil {
		log.Printf("JWT validation failed: %v", err)
		return nil, "", "", "", ErrJWTInvalid
	}
	if claims.Action != toolName {
		return nil, "", "", "", ErrJWTActionDenied
	}
	return claims, claims.Subject, claims.Action, claims.Request, nil
}

// authorizeWriteToolFromToken validates an AIP JWT passed directly (e.g. from
// _aip_authorization in tool arguments) rather than from the request header.
// Used by handleToolsCall after aip/await_approval returns a JWT.
func (s *Server) authorizeWriteToolFromToken(toolName, token string) (*jwt.Claims, string, string, string, error) {
	if s.jwtManager == nil {
		return nil, "", "", "", ErrJWTNotConfigured
	}
	claims, err := s.jwtManager.ValidateToken(token)
	if err != nil {
		log.Printf("JWT validation failed (from args): %v", err)
		return nil, "", "", "", ErrJWTInvalid
	}
	if claims.Action != toolName {
		return nil, "", "", "", ErrJWTActionDenied
	}
	return claims, claims.Subject, claims.Action, claims.Request, nil
}

type mcpProxyLog struct {
	Timestamp  string `json:"timestamp"`
	Agent      string `json:"agent"`
	Server     string `json:"server"`
	Tool       string `json:"tool"`
	Action     string `json:"action"`
	Status     int    `json:"status"`
	RequestRef string `json:"requestRef,omitempty"`
	ServerURL  string `json:"serverURL"`
}

func (s *Server) emitMCPLog(agent, server, tool, action string,
	status int, requestRef, serverURL string) {
	entry := mcpProxyLog{
		Timestamp:  time.Now().Format(time.RFC3339),
		Agent:      agent,
		Server:     server,
		Tool:       tool,
		Action:     action,
		Status:     status,
		RequestRef: requestRef,
		ServerURL:  serverURL,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		log.Printf("failed to marshal MCP proxy log: %v", err)
		return
	}
	log.Printf("MCP_PROXY %s", string(data))
}
