package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

var (
	ErrJSONParse             = errors.New("invalid JSON-RPC: malformed JSON")
	ErrInvalidJSONRPCVersion = errors.New("unsupported JSON-RPC version")
)

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  any           `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      map[string]any `json:"serverInfo"`
}

type MCPToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"inputSchema,omitempty"`
}

type ToolsListResult struct {
	Tools []MCPToolInfo `json:"tools"`
}

type ToolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ToolsCallResult struct {
	Content []json.RawMessage `json:"content"`
	IsError bool              `json:"isError,omitempty"`
}

const (
	JSONRPCVersion = "2.0"

	ErrCodeParse     = -32700
	ErrCodeInvalid   = -32602
	ErrCodeMethod    = -32601
	ErrCodeInternal  = -32603
	ErrCodeAuth      = -32001
	ErrCodeForbidden = -32003
)

func ParseJSONRPCRequest(body []byte) (*JSONRPCRequest, error) {
	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJSONParse, err)
	}
	if req.JSONRPC != JSONRPCVersion {
		return nil, fmt.Errorf("%w: %q", ErrInvalidJSONRPCVersion, req.JSONRPC)
	}
	return &req, nil
}

func WriteJSONRPCResponse(w http.ResponseWriter, id any, result any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(JSONRPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Result:  result,
	})
}

func WriteJSONRPCError(w http.ResponseWriter, id any, code int, message string) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(JSONRPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
		},
	})
}
