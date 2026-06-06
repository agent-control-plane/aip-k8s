package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/agent-control-plane/aip-k8s/internal/credential"
	"github.com/agent-control-plane/aip-k8s/internal/jwt"
	"github.com/agent-control-plane/aip-k8s/internal/mcp"
)

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if wErr := mcp.WriteJSONRPCError(w, nil, mcp.ErrCodeParse, "failed to read body: "+err.Error()); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}

	req, err := mcp.ParseJSONRPCRequest(body)
	if err != nil {
		if wErr := mcp.WriteJSONRPCError(w, nil, mcp.ErrCodeParse, err.Error()); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(w, req)
	case "notifications/initialized":
		if req.ID != nil {
			// Be lenient: respond to requests with an id (misparked notification).
			if wErr := mcp.WriteJSONRPCResponse(w, req.ID, map[string]any{}); wErr != nil {
				log.Printf("WriteJSONRPCResponse failed: %v", wErr)
			}
		}
	case "tools/list":
		s.handleToolsList(w, req)
	case "tools/call":
		s.handleToolsCall(w, r, req)
	default:
		msg := fmt.Sprintf("Method not found: %s", req.Method)
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeMethod, msg); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, req *mcp.JSONRPCRequest) {
	if wErr := mcp.WriteJSONRPCResponse(w, req.ID, mcp.InitializeResult{
		ProtocolVersion: "2025-03-26",
		ServerInfo: map[string]any{
			"name":    "aip-gateway",
			"version": "v1alpha1",
		},
		Capabilities: map[string]any{
			"tools": map[string]any{},
		},
	}); wErr != nil {
		log.Printf("WriteJSONRPCResponse failed: %v", wErr)
	}
}

func (s *Server) handleToolsList(w http.ResponseWriter, req *mcp.JSONRPCRequest) {
	var tools []mcp.MCPToolInfo
	servers := s.mcpServers
	if s.mcpCache != nil {
		if cached := s.mcpCache.getAll(); len(cached) > 0 {
			servers = cached
		}
	}
	for _, srv := range servers {
		for _, t := range srv.Tools {
			schema := t.Schema
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			tools = append(tools, mcp.MCPToolInfo{
				Name:        fmt.Sprintf("%s/%s", srv.Name, t.Name),
				InputSchema: schema,
			})
		}
	}
	// AIP governance tools are always available alongside external server tools.
	tools = append(tools, aipToolDefs...)
	if tools == nil {
		tools = []mcp.MCPToolInfo{}
	}
	if wErr := mcp.WriteJSONRPCResponse(w, req.ID, mcp.ToolsListResult{Tools: tools}); wErr != nil {
		log.Printf("WriteJSONRPCResponse failed: %v", wErr)
	}
}

func (s *Server) handleToolsCall(w http.ResponseWriter, r *http.Request, req *mcp.JSONRPCRequest) {
	params, serverName, toolName, err := parseToolCallParams(req)
	if err != nil {
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, err.Error()); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}

	// AIP governance tools (aip/*) are handled internally before any registry lookup.
	if serverName == "aip" {
		s.handleAIPTool(w, r, req, toolName, params.Arguments)
		return
	}

	mcpServer := s.findMCPServer(serverName)
	if mcpServer == nil {
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, "MCP server not found: "+serverName); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}

	// Lazily establish the upstream session and populate tool schemas on first use.
	if !mcpServer.ensureSession(s.httpClient) {
		log.Printf("Failed to establish session with %s", mcpServer.Name)
	}
	if s.mcpCache != nil {
		s.mcpCache.commitSession(mcpServer.Name, mcpServer.SessionID, mcpServer.sessionReady, mcpServer.URL)
	}

	tool := s.findTool(mcpServer, toolName)
	if tool == nil {
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, "tool not found: "+toolName); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}

	// Extract AIP-specific arguments before forwarding to the upstream server.
	// These are stripped from the args map so they are not sent upstream.
	aipJWT, _ := params.Arguments["_aip_authorization"].(string)
	delete(params.Arguments, "_aip_authorization")
	aipTargetURI, _ := params.Arguments["_aip_target_uri"].(string)
	delete(params.Arguments, "_aip_target_uri")
	aipReason, _ := params.Arguments["_aip_reason"].(string)
	delete(params.Arguments, "_aip_reason")

	var claims *jwt.Claims
	var agent, action, requestRef string
	prefixedAction := serverName + "/" + toolName
	if !tool.ReadOnly {
		var authErr error
		if aipJWT != "" {
			// JWT supplied via tool arguments (after aip/await_approval approval).
			claims, agent, action, requestRef, authErr = s.authorizeWriteToolFromToken(prefixedAction, aipJWT)
		} else {
			// JWT from X-AIP-Authorization header (direct /mcp-proxy callers).
			claims, agent, action, requestRef, authErr = s.authorizeWriteTool(r, prefixedAction)
		}

		if authErr != nil {
			if errors.Is(authErr, ErrJWTMissing) {
				s.governanceSubmissionPath(w, r, req, params, aipTargetURI, aipReason)
				return
			}
			s.writeAuthError(w, req, authErr)
			return
		}

		// Enforce resource claim: GitHub tools parse the github:// URI against
		// owner/repo args; all other tools compare the claim against the target
		// URI rebuilt from arguments.
		if err := s.enforceResourceClaimForTool(serverName, claims.Resource, params.Arguments); err != "" {
			if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeForbidden, err); wErr != nil {
				log.Printf("WriteJSONRPCError failed: %v", wErr)
			}
			return
		}
	} else {
		agent = callerSubFromCtx(r.Context())
		action = prefixedAction
	}

	result, errMsg, done := s.forwardToolCall(r.Context(), w, mcpServer, params.Arguments, toolName, req.ID, agent)
	if done {
		return
	}
	if errMsg != "" {
		// JWT-authorized write tool failed: advance AR to Failed to release the lock.
		if requestRef != "" {
			s.failAgentRequest(r.Context(), requestRef, "MCP tool execution failed: "+errMsg)
		}
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInternal, errMsg); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}

	// JWT-authorized write tool succeeded: advance AR through Executing → Completed
	// so the controller releases the OpsLock. Fire-and-forget in background so the
	// MCP response is not delayed by the K8s patch.
	if requestRef != "" {
		go func(ref string) {
			ctx, cancel := context.WithTimeout(context.Background(), mcpRequestTimeout)
			defer cancel()
			s.completeAgentRequest(ctx, ref, "")
		}(requestRef)
	}

	s.emitMCPLog(agent, serverName, toolName, action, http.StatusOK, requestRef, mcpServer.URL)

	if result.IsError {
		if wErr := mcp.WriteJSONRPCResponse(w, req.ID, mcp.ToolsCallResult{
			Content: result.Content,
			IsError: true,
		}); wErr != nil {
			log.Printf("WriteJSONRPCResponse failed: %v", wErr)
		}
		return
	}

	if wErr := mcp.WriteJSONRPCResponse(w, req.ID, result); wErr != nil {
		log.Printf("WriteJSONRPCResponse failed: %v", wErr)
	}
}

func splitPrefixedName(prefixed string) (server, tool string, err error) {
	parts := strings.SplitN(prefixed, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid tool name %q: expected format {server}/{tool}", prefixed)
	}
	return parts[0], parts[1], nil
}

// parseToolCallParams unmarshals and validates the params of a tools/call request.
func parseToolCallParams(req *mcp.JSONRPCRequest) (*mcp.ToolsCallParams, string, string, error) {
	var params mcp.ToolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, "", "", fmt.Errorf("invalid tools/call params: %w", err)
	}
	server, tool, err := splitPrefixedName(params.Name)
	if err != nil {
		return nil, "", "", err
	}
	return &params, server, tool, nil
}

// governanceSubmissionPath submits an AgentRequest for governance and writes a
// pending_approval JSON-RPC response. Returns after writing; caller must return.
func (s *Server) governanceSubmissionPath(
	w http.ResponseWriter, r *http.Request, req *mcp.JSONRPCRequest,
	params *mcp.ToolsCallParams, aipTargetURI, aipReason string,
) {
	agentID := callerSubFromCtx(r.Context())
	if agentID == "" {
		if s.authRequired {
			if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeAuth,
				"caller identity required for write tools"); wErr != nil {
				log.Printf("WriteJSONRPCError failed: %v", wErr)
			}
			return
		}
		agentID = "unauthenticated"
	}
	ar, submitErr := s.submitAgentRequestForTool(
		r.Context(), agentID, params.Name, aipTargetURI, aipReason, params.Arguments,
	)
	if submitErr != nil {
		code := mcp.ErrCodeInternal
		if errors.Is(submitErr, ErrAgentNotRegistered) || errors.Is(submitErr, ErrIdentityMismatch) {
			code = mcp.ErrCodeForbidden
		}
		if wErr := mcp.WriteJSONRPCError(w, req.ID, code,
			"failed to submit governance request: "+submitErr.Error()); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}
	if wErr := mcp.WriteJSONRPCResponse(w, req.ID, mcp.ToolsCallResult{
		Content: []json.RawMessage{pendingApprovalContent(ar.Name)},
	}); wErr != nil {
		log.Printf("WriteJSONRPCResponse failed: %v", wErr)
	}
}

// writeAuthError writes an appropriate JSON-RPC error response for a write tool
// auth failure. Returns after writing; caller must return.
func (s *Server) writeAuthError(w http.ResponseWriter, req *mcp.JSONRPCRequest, authErr error) {
	var wErr error
	switch {
	case errors.Is(authErr, ErrJWTNotConfigured):
		wErr = mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInternal, authErr.Error())
	case errors.Is(authErr, ErrJWTActionDenied):
		wErr = mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeForbidden, authErr.Error())
	default:
		wErr = mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeAuth, authErr.Error())
	}
	if wErr != nil {
		log.Printf("WriteJSONRPCError failed: %v", wErr)
	}
}

// enforceResourceClaimForTool enforces the JWT resource claim against the tool
// arguments. Returns an error string (non-empty means mismatch) or "" on match.
func (s *Server) enforceResourceClaimForTool(serverName, resource string, args map[string]any) string {
	if serverName == "github" {
		return enforceRepoClaim(resource, args)
	}
	return enforceResourceClaim(resource, args)
}

func (s *Server) forwardToolCall(
	ctx context.Context, w http.ResponseWriter, mcpServer *MCPServer, args map[string]any,
	toolName string, id any, agentIdentity string,
) (mcpProxyResult, string, bool) {
	rpcBody, err := buildJSONRPCRequestBody(args, toolName, id)
	if err != nil {
		return mcpProxyResult{}, "failed to build request: " + err.Error(), false
	}

	callCtx, cancel := context.WithTimeout(ctx, mcpRequestTimeout)
	defer cancel()

	// Streamable HTTP transport (MCP 2025-03-26): POST JSON-RPC to the base URL,
	// not to a /tools/call sub-path.
	req, err := http.NewRequestWithContext(callCtx, "POST", mcpServer.URL, bytes.NewReader(rpcBody))
	if err != nil {
		return mcpProxyResult{}, "failed to create request", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if mcpServer.SessionID != "" {
		req.Header.Set("Mcp-Session-Id", mcpServer.SessionID)
	}
	rawOIDCToken := rawOIDCTokenFromCtx(ctx)

	var p credential.Provider
	if s.regCache != nil {
		p = s.regCache.providerFor(agentIdentity, mcpServer.Name)
	}
	if p == nil {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("no credential binding for agent %q on server %q: "+
				"register an AgentRegistration with an ExternalIdentityBinding",
				agentIdentity, mcpServer.Name))
		return mcpProxyResult{}, "", true
	}
	bearerToken, err := p.Token(ctx, rawOIDCToken)
	if err != nil {
		// Log server-side so operators can grep without relying on client-visible error text.
		log.Printf("credential resolution failed: agent=%q server=%q err=%v", agentIdentity, mcpServer.Name, err)
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to resolve credential for agent %q on server %q",
				agentIdentity, mcpServer.Name))
		return mcpProxyResult{}, "", true
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		log.Printf("MCP forward error: %v", err)
		return mcpProxyResult{}, "MCP server unavailable", false
	}
	defer func() { _ = resp.Body.Close() }()

	const maxMCPResponseSize = 10 << 20
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponseSize+1))
	if err != nil {
		return mcpProxyResult{}, "failed to read MCP response", false
	}
	if len(respBody) > maxMCPResponseSize {
		return mcpProxyResult{}, "MCP response too large", false
	}

	if resp.StatusCode != http.StatusOK {
		// A 401 from upstream means the session expired. Reset both the in-hand
		// pointer and the canonical cache entry so the next call re-initializes.
		// resetSession on the pointer alone is insufficient: commitSession already
		// replaced the map entry with a new snapshot, so the pointer the handler
		// holds is detached from the canonical entry.
		if resp.StatusCode == http.StatusUnauthorized {
			mcpServer.resetSession()
			if s.mcpCache != nil {
				s.mcpCache.resetSession(mcpServer.Name, mcpServer.URL)
			}
		}
		return mcpProxyResult{}, fmt.Sprintf("MCP server returned status %d", resp.StatusCode), false
	}

	result, rpcErr := extractMCPResult(respBody)
	if rpcErr != "" {
		return mcpProxyResult{}, rpcErr, false
	}
	return result, "", false
}

func buildJSONRPCRequestBody(args map[string]any, toolName string, id any) ([]byte, error) {
	rpcReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}
	return json.Marshal(rpcReq)
}
