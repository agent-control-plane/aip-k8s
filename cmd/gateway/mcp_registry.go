package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

type MCPServer struct {
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Status      string    `json:"status"`
	Tools       []MCPTool `json:"tools"`
	BearerToken string    `json:"bearer_token,omitempty"`
	SessionID   string    `json:"-"`

	// sessionMu serializes lazy initialization; nil in test MCPServer literals
	// that are constructed directly without going through loadMCPRegistry.
	sessionMu    *sync.Mutex `json:"-"`
	sessionReady bool        `json:"-"`
}

type MCPTool struct {
	Name     string         `json:"name"`
	ReadOnly bool           `json:"read_only"`
	Schema   map[string]any `json:"schema,omitempty"`
}

type mcpServerResponse struct {
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Tools  []MCPTool `json:"tools"`
}

func loadMCPRegistry() ([]MCPServer, error) {
	registry := os.Getenv("MCP_REGISTRY")
	if registry == "" {
		return nil, nil
	}
	var servers []MCPServer
	if err := json.Unmarshal([]byte(registry), &servers); err != nil {
		return nil, fmt.Errorf("parse MCP_REGISTRY: %w", err)
	}
	for i := range servers {
		servers[i].sessionMu = &sync.Mutex{}
	}
	return servers, nil
}

// ensureSession lazily establishes an MCP 2025-03-26 streamable HTTP session
// with the upstream server and fetches tool input schemas. Safe to call
// concurrently; a no-op once ready. Returns true if the session is ready.
func (srv *MCPServer) ensureSession(httpClient *http.Client) bool {
	if srv.sessionMu == nil {
		// Test MCPServer literal: no session management needed.
		return true
	}
	srv.sessionMu.Lock()
	defer srv.sessionMu.Unlock()
	if srv.sessionReady {
		return true
	}
	sessionID, ok := initUpstreamSession(httpClient, srv)
	if !ok {
		return false
	}
	srv.SessionID = sessionID
	fetchSchemasForServer(httpClient, srv)
	srv.sessionReady = true
	return true
}

// resetSession marks the server as not ready so the next ensureSession call
// re-initializes the session. Called after upstream 401 errors.
func (srv *MCPServer) resetSession() {
	if srv.sessionMu == nil {
		return
	}
	srv.sessionMu.Lock()
	srv.sessionReady = false
	srv.sessionMu.Unlock()
}

// fetchSchemasForServer queries the upstream for its tool list and populates
// Tool.Schema fields. Must be called with sessionMu held.
func fetchSchemasForServer(httpClient *http.Client, srv *MCPServer) {
	listBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	listReq, err := http.NewRequest("POST", srv.URL, bytes.NewReader(listBody))
	if err != nil {
		log.Printf("fetchSchemasForServer: %s: build list request: %v", srv.Name, err)
		return
	}
	listReq.Header.Set("Content-Type", "application/json")
	listReq.Header.Set("Accept", "application/json, text/event-stream")
	listReq.Header.Set("Mcp-Session-Id", srv.SessionID)

	resp, err := httpClient.Do(listReq)
	if err != nil {
		log.Printf("fetchSchemasForServer: %s: tools/list: %v", srv.Name, err)
		return
	}

	body, readErr := readSSEDataLine(resp)
	if readErr != "" {
		log.Printf("fetchSchemasForServer: %s: read: %s", srv.Name, readErr)
		return
	}

	var result struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		log.Printf("fetchSchemasForServer: %s: decode: %v", srv.Name, err)
		return
	}
	if result.Error != nil {
		log.Printf("fetchSchemasForServer: %s: server error: %s", srv.Name, result.Error.Message)
		return
	}

	schemaByName := make(map[string]map[string]any, len(result.Result.Tools))
	for _, t := range result.Result.Tools {
		schemaByName[t.Name] = t.InputSchema
	}

	for j := range srv.Tools {
		bare := srv.Tools[j].Name
		if idx := strings.LastIndex(bare, "/"); idx >= 0 {
			bare = bare[idx+1:]
		}
		if s, ok := schemaByName[bare]; ok {
			srv.Tools[j].Schema = s
		}
	}
	log.Printf("fetchSchemasForServer: %s: populated schemas for %d tools", srv.Name, len(schemaByName))
}

// initUpstreamSession sends an MCP initialize request and returns the session ID.
func initUpstreamSession(httpClient *http.Client, srv *MCPServer) (string, bool) {
	initBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "aip-gateway", "version": "v1alpha1"},
		},
	})
	req, err := http.NewRequest("POST", srv.URL, bytes.NewReader(initBody))
	if err != nil {
		log.Printf("initUpstreamSession: %s: build request: %v", srv.Name, err)
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("initUpstreamSession: %s: %v", srv.Name, err)
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	sessionID := resp.Header.Get("Mcp-Session-Id")
	// Drain body so the connection can be returned to the pool.
	_, _ = io.Copy(io.Discard, resp.Body)
	if sessionID == "" {
		log.Printf("initUpstreamSession: %s: no Mcp-Session-Id returned", srv.Name)
		return "", false
	}
	return sessionID, true
}

// readSSEDataLine reads an SSE response body and returns the first data line.
func readSSEDataLine(resp *http.Response) (string, string) {
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return "", "failed to read body: " + err.Error()
	}
	for line := range strings.SplitSeq(buf.String(), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data:") {
			return strings.TrimSpace(line[5:]), ""
		}
	}
	return "", "no SSE data line in response"
}

func (s *Server) handleMCPRegistry(w http.ResponseWriter, r *http.Request) {
	servers := s.mcpServers
	if s.mcpCache != nil {
		if cached := s.mcpCache.getAll(); len(cached) > 0 {
			servers = cached
		}
	}
	resp := make([]mcpServerResponse, len(servers))
	for i, srv := range servers {
		resp[i] = mcpServerResponse{
			Name:   srv.Name,
			Status: srv.Status,
			Tools:  srv.Tools,
		}
	}
	writeJSON(w, http.StatusOK, map[string][]mcpServerResponse{"mcp_servers": resp})
}
