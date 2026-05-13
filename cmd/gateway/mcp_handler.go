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
	"os"
	"strings"

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

	mcpServer := s.findMCPServer(serverName)
	if mcpServer == nil {
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, "MCP server not found: "+serverName); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}

	tool := s.findTool(mcpServer, toolName)
	if tool == nil {
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInvalid, "tool not found: "+toolName); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
	}

	var claims *jwt.Claims
	var agent, action, requestRef string
	if !tool.ReadOnly {
		var authErr error
		claims, agent, action, requestRef, authErr = s.authorizeWriteTool(r, toolName)
		if authErr != nil {
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
			return
		}

		if repoErr := enforceRepoClaim(claims.Repo, params.Arguments); repoErr != "" {
			if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeForbidden, repoErr); wErr != nil {
				log.Printf("WriteJSONRPCError failed: %v", wErr)
			}
			return
		}
	}

	result, errMsg := s.forwardToolCall(r.Context(), mcpServer, params.Arguments, toolName, req.ID)
	if errMsg != "" {
		if wErr := mcp.WriteJSONRPCError(w, req.ID, mcp.ErrCodeInternal, errMsg); wErr != nil {
			log.Printf("WriteJSONRPCError failed: %v", wErr)
		}
		return
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

func (s *Server) forwardToolCall(
	ctx context.Context, mcpServer *MCPServer, args map[string]any,
	toolName string, id any,
) (mcpProxyResult, string) {
	rpcBody, err := buildJSONRPCRequestBody(args, toolName, id)
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
		log.Printf("MCP forward error: %v", err)
		return mcpProxyResult{}, "MCP server unavailable"
	}
	defer func() { _ = resp.Body.Close() }()

	const maxMCPResponseSize = 10 << 20
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponseSize+1))
	if err != nil {
		return mcpProxyResult{}, "failed to read MCP response"
	}
	if len(respBody) > maxMCPResponseSize {
		return mcpProxyResult{}, "MCP response too large"
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
