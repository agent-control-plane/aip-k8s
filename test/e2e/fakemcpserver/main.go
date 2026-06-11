// fake-mcp-server is a minimal MCP server used only in e2e tests.
// It responds to initialize/tools/list/tools/call and exposes /_last-auth
// so the test process can verify which Authorization header was forwarded.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	var capturedAuth atomic.Value

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		id := req["id"]

		w.Header().Set("Content-Type", "text/event-stream")

		var result map[string]any
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fake-sess-1")
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"serverInfo":      map[string]any{"name": "fake-mcp-server"},
				"capabilities":    map[string]any{},
			}
		case "tools/list":
			result = map[string]any{
				"tools": []map[string]any{
					{"name": "echo", "inputSchema": map[string]any{"type": "object"}},
				},
			}
		case "tools/call":
			capturedAuth.Store(r.Header.Get("Authorization"))
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ok"}},
			}
		default:
			result = map[string]any{}
		}

		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		fmt.Fprintf(w, "data: %s\n\n", data) //nolint:errcheck
	})

	// /_last-auth lets the test process query the last Authorization header
	// received on a tools/call request without needing host-to-cluster networking.
	mux.HandleFunc("/_last-auth", func(w http.ResponseWriter, r *http.Request) {
		auth := ""
		if v := capturedAuth.Load(); v != nil {
			auth = v.(string)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"auth": auth})
	})

	if err := http.ListenAndServe(":"+port, mux); err != nil { //nolint:gosec
		fmt.Fprintf(os.Stderr, "fake-mcp-server: %v\n", err)
		os.Exit(1)
	}
}
