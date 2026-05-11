package fetchers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var GitHubMCPURL = "http://github-mcp.aip-k8s-system.svc"

type mcpToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type mcpToolResponse struct {
	Content []mcpContentBlock `json:"content"`
	IsError bool              `json:"isError"`
}

type mcpContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

var mcpHTTPClient = &http.Client{Timeout: 15 * time.Second}

func parseGitHubURI(targetURI string) (org, repo, branch, filePath string, err error) {
	u, perr := url.Parse(targetURI)
	if perr != nil || u.Scheme != "github" {
		return "", "", "", "", fmt.Errorf("invalid GitHub URI: %s", targetURI)
	}

	org = u.Host
	rest := strings.TrimPrefix(u.Path, "/")
	if org == "" || rest == "" {
		return "", "", "", "", fmt.Errorf("invalid GitHub URI: %s (missing org or repo)", targetURI)
	}

	if strings.Contains(rest, "/files/") {
		parts := strings.SplitN(rest, "/files/", 2)
		repo = parts[0]
		rest = parts[1]
		branch, filePath, _ = strings.Cut(rest, "/")
	} else {
		repo = rest
	}

	return org, repo, branch, filePath, nil
}

func callMCPTool(ctx context.Context, toolName string, args map[string]any) (*mcpToolResponse, error) {
	body := mcpToolCall{Name: toolName, Arguments: args}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to encode MCP tool call: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", GitHubMCPURL+"/tools/call", bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("failed to build MCP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := mcpHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MCP server unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read MCP response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var mcpErr mcpToolError
		if json.Unmarshal(raw, &mcpErr) == nil && mcpErr.Message != "" {
			return nil, fmt.Errorf("MCP tool error: %s", mcpErr.Message)
		}
		return nil, fmt.Errorf("MCP server returned status %d", resp.StatusCode)
	}

	var result mcpToolResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("failed to decode MCP response: %w", err)
	}
	if result.IsError {
		msg := "MCP tool returned an error"
		if len(result.Content) > 0 {
			msg = result.Content[0].Text
		}
		return nil, fmt.Errorf("MCP tool error: %s", msg)
	}

	return &result, nil
}

func extractTextContent(resp *mcpToolResponse) string {
	for _, block := range resp.Content {
		if block.Type == "text" {
			return block.Text
		}
	}
	return ""
}

func FetchGitHubMCP(ctx context.Context, _ client.Client, targetURI string) (*apiextensionsv1.JSON, error) {
	org, repoName, branch, filePath, err := parseGitHubURI(targetURI)
	if err != nil {
		return nil, err
	}

	result := map[string]any{}

	if filePath != "" {
		args := map[string]any{
			"owner": org,
			"repo":  repoName,
			"path":  filePath,
		}
		if branch != "" {
			args["branch"] = branch
		}

		resp, err := callMCPTool(ctx, "get_file_contents", args)
		if err != nil {
			return nil, fmt.Errorf("MCP get_file_contents failed: %w", err)
		}

		text := extractTextContent(resp)
		var content any
		if json.Unmarshal([]byte(text), &content) == nil {
			result["fileContent"] = content
		} else {
			result["fileContent"] = text
		}
		result["owner"] = org
		result["repo"] = repoName
		if branch != "" {
			result["branch"] = branch
		}
		result["filePath"] = filePath
	} else {
		result["owner"] = org
		result["repo"] = repoName
		result["ciStatus"] = "unknown"
	}

	prResp, err := callMCPTool(ctx, "list_pull_requests", map[string]any{
		"owner":    org,
		"repo":     repoName,
		"state":    "open",
		"per_page": 100,
	})
	if err == nil {
		text := extractTextContent(prResp)
		var prs []any
		if json.Unmarshal([]byte(text), &prs) == nil {
			result["openPRCount"] = len(prs)
		}
	}

	raw, _ := json.Marshal(result)
	return &apiextensionsv1.JSON{Raw: raw}, nil
}
