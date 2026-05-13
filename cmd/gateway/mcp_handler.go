package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/agent-control-plane/aip-k8s/internal/jwt"
	"github.com/agent-control-plane/aip-k8s/internal/mcp"
)

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		mcp.WriteJSONRPCError(w, nil, mcp.ErrCodeParse, "failed to read body: "+err.Error())
		return
	}

	req, err := mcp.ParseJSONRPCRequest(body)
	if err != nil {
		mcp.WriteJSONRPCError(w, nil, mcp.ErrCodeParse, err.Error())
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(w, req)
	case "notifications/initialized":
		mcp.WriteJSONRPCResponse(w, req.ID, map[string]any{})
	case "tools/list":
		s.handleToolsList(w, req)
	case "tools/call":
		s.handleToolsCall(w, r, req)
	default:
		mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeMethod, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, req *mcp.JSONRPCRequest) {
	mcp.WriteJSONRPCResponse(w, req.ID, mcp.InitializeResult{
		ProtocolVersion: "2025-03-26",
		ServerInfo: map[string]any{
			"name":    "aip-gateway",
			"version": "v1alpha1",
		},
		Capabilities: map[string]any{
			"tools": map[string]any{},
		},
	})
}

func (s *Server) handleToolsList(w http.ResponseWriter, req *mcp.JSONRPCRequest) {
	var tools []mcp.MCPToolInfo
	for _, srv := range s.mcpServers {
		for _, t := range srv.Tools {
			tools = append(tools, mcp.MCPToolInfo{
				Name: fmt.Sprintf("%s/%s", srv.Name, t.Name),
			})
		}
	}
	if tools == nil {
		tools = []mcp.MCPToolInfo{}
	}
	mcp.WriteJSONRPCResponse(w, req.ID, mcp.ToolsListResult{Tools: tools})
}

func (s *Server) handleToolsCall(w http.ResponseWriter, r *http.Request, req *mcp.JSONRPCRequest) {
	var params mcp.ToolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, "invalid tools/call params: "+err.Error())
		return
	}

	serverName, toolName, err := splitPrefixedName(params.Name)
	if err != nil {
		mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, err.Error())
		return
	}

	mcpServer := s.findMCPServer(serverName)
	if mcpServer == nil {
		mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, "MCP server not found: "+serverName)
		return
	}

	tool := s.findTool(mcpServer, toolName)
	if tool == nil {
		mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, "tool not found: "+toolName)
		return
	}

	var claims *jwt.Claims
	var agent, action, requestRef string
	if !tool.ReadOnly {
		if s.jwtManager == nil {
			mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInternal, "JWT signing not configured")
			return
		}
		auth := r.Header.Get("X-AIP-Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeAuth, "missing AIP bearer token")
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		var err error
		claims, err = s.jwtManager.ValidateToken(token)
		if err != nil {
			mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeAuth, "invalid token: "+err.Error())
			return
		}
		if claims.Action != toolName {
			mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeForbidden, "tool not allowed for this action")
			return
		}
		agent = claims.Subject
		action = claims.Action
		requestRef = claims.Request
	}

	if !tool.ReadOnly {
		if repoErr := enforceRepoClaim(claims.Repo, params.Arguments); repoErr != "" {
			mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeForbidden, repoErr)
			return
		}
	}

	result, errMsg := s.forwardToolCall(r.Context(), mcpServer, params.Arguments, toolName)
	if errMsg != "" {
		mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInternal, errMsg)
		return
	}

	s.emitMCPLog(agent, serverName, toolName, action, http.StatusOK, requestRef, mcpServer.URL)

	if result.IsError {
		mcp.WriteJSONRPCResponse(w, req.ID, mcp.ToolsCallResult{
			Content: result.Content,
			IsError: true,
		})
		return
	}

	mcp.WriteJSONRPCResponse(w, req.ID, result)
}

func splitPrefixedName(prefixed string) (server, tool string, err error) {
	parts := strings.SplitN(prefixed, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid tool name %q: expected format {server}/{tool}", prefixed)
	}
	return parts[0], parts[1], nil
}

func (s *Server) forwardToolCall(ctx context.Context, mcpServer *MCPServer, args map[string]any, toolName string) (mcpProxyResult, string) {
	rpcBody, err := buildJSONRPCRequestBody(args, toolName)
	if err != nil {
		return mcpProxyResult{}, "failed to build request: " + err.Error()
	}

	callCtx, cancel := context.WithTimeout(ctx, mcpRequestTimeout)
	defer cancel()

	mcpURL := strings.TrimSuffix(mcpServer.URL, "/") + "/tools/call"
	req, err := http.NewRequestWithContext(callCtx, "POST", mcpURL, bytes.NewReader(rpcBody))
	if err != nil {
		return mcpProxyResult{}, "failed to create request"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	bearerToken := mcpServer.BearerToken
	if bearerToken == "" {
		bearerToken = os.Getenv("AIP_MCP_TOKEN")
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return mcpProxyResult{}, "MCP server unavailable: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return mcpProxyResult{}, "failed to read MCP response"
	}

	if resp.StatusCode != http.StatusOK {
		return mcpProxyResult{}, fmt.Sprintf("MCP server returned status %d", resp.StatusCode)
	}

	result, rpcErr := extractMCPResult(respBody)
	if rpcErr != "" {
		return mcpProxyResult{}, rpcErr
	}
	return result, ""
}

func buildJSONRPCRequestBody(args map[string]any, toolName string) ([]byte, error) {
	rpcReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}
	return json.Marshal(rpcReq)
}
