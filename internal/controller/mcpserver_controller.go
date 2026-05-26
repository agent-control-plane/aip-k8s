/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

const (
	mcpRequestTimeout    = 30 * time.Second
	mcpRequeueInterval   = 5 * time.Minute
	mcpDefaultTokenKey   = "token"
	conditionTypeSynced  = "Synced"
	conditionReasonOK    = "DiscoverySucceeded"
	conditionReasonError = "DiscoveryFailed"
	maxMessageLen        = 256
)

// MCPServerReconciler reconciles MCPServer objects by discovering tools from upstream MCP servers.
type MCPServerReconciler struct {
	client.Client
	APIReader  client.Reader
	Scheme     *runtime.Scheme
	HTTPClient *http.Client
	Clock      func() time.Time
}

func (r *MCPServerReconciler) clock() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// sanitizeURL strips userinfo, query, and fragment from a URL for safe logging.
func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid url]"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// +kubebuilder:rbac:groups=governance.aip.io,resources=mcpservers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=governance.aip.io,resources=mcpservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=governance.aip.io,resources=mcpservers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *MCPServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var server governancev1alpha1.MCPServer
	if err := r.Get(ctx, req.NamespacedName, &server); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling MCPServer", "name", server.Name, "url", sanitizeURL(server.Spec.URL))
	base := server.DeepCopy()

	tools, syncErr := r.discoverTools(ctx, &server)

	now := metav1.NewTime(r.clock())
	server.Status.LastSyncTime = &now
	server.Status.ObservedGeneration = server.Generation

	if syncErr != nil {
		logger.Error(syncErr, "Failed to discover tools from upstream", "name", server.Name)
		// Preserve existing tools on transient failure — don't clobber a prior successful
		// discovery. Clearing tools would remove them from the gateway watch cache until
		// the upstream recovers and the controller succeeds on the next requeue.
		// server.Status.Tools and server.Status.DiscoveredToolCount remain unchanged
		// from the base copy; the merge patch will include no diff for those fields.
		meta.SetStatusCondition(&server.Status.Conditions, metav1.Condition{
			Type:               conditionTypeSynced,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: server.Generation,
			LastTransitionTime: now,
			Reason:             conditionReasonError,
			Message:            truncateMessage(syncErr.Error()),
		})
	} else {
		server.Status.Tools = tools
		server.Status.DiscoveredToolCount = len(tools)
		meta.SetStatusCondition(&server.Status.Conditions, metav1.Condition{
			Type:               conditionTypeSynced,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: server.Generation,
			LastTransitionTime: now,
			Reason:             conditionReasonOK,
			Message:            fmt.Sprintf("Discovered %d tools from upstream", len(tools)),
		})
	}

	if err := r.Status().Patch(ctx, &server, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: mcpRequeueInterval}, nil
}

// discoverTools contacts the upstream MCP server's tools/list endpoint and returns
// the discovered tool list merged with the readOnlyTools from the spec.
func (r *MCPServerReconciler) discoverTools(ctx context.Context, server *governancev1alpha1.MCPServer) ([]governancev1alpha1.MCPServerTool, error) {
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	bearerToken, err := r.resolveBearerToken(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}

	readOnlySet := make(map[string]bool, len(server.Spec.ReadOnlyTools))
	for _, t := range server.Spec.ReadOnlyTools {
		readOnlySet[t] = true
	}

	callCtx, cancel := context.WithTimeout(ctx, mcpRequestTimeout)
	defer cancel()

	sessionID, err := r.initSession(callCtx, httpClient, server.Spec.URL, bearerToken)
	if err != nil {
		return nil, fmt.Errorf("initialize session: %w", err)
	}

	listBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal tools/list request body: %w", err)
	}
	listReq, err := http.NewRequestWithContext(callCtx, "POST", server.Spec.URL, bytes.NewReader(listBody))
	if err != nil {
		return nil, fmt.Errorf("build list request: %w", err)
	}
	listReq.Header.Set("Content-Type", "application/json")
	listReq.Header.Set("Accept", "application/json, text/event-stream")
	listReq.Header.Set("Mcp-Session-Id", sessionID)
	if bearerToken != "" {
		listReq.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := httpClient.Do(listReq)
	if err != nil {
		return nil, fmt.Errorf("tools/list request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("tools/list returned HTTP %d", resp.StatusCode)
	}

	dataLine, err := readSSEDataLine(resp)
	if err != nil {
		return nil, fmt.Errorf("read SSE data: %w", err)
	}

	var result struct {
		Result *struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(dataLine), &result); err != nil {
		return nil, fmt.Errorf("decode tools/list response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("upstream error: %s", result.Error.Message)
	}
	if result.Result == nil {
		return nil, fmt.Errorf("empty tools/list result")
	}

	tools := make([]governancev1alpha1.MCPServerTool, 0, len(result.Result.Tools))
	for _, t := range result.Result.Tools {
		tools = append(tools, governancev1alpha1.MCPServerTool{
			Name:     t.Name,
			ReadOnly: readOnlySet[t.Name],
		})
	}
	return tools, nil
}

// initSession sends an MCP initialize request and returns the session ID.
func (r *MCPServerReconciler) initSession(ctx context.Context, httpClient *http.Client, rawURL, bearerToken string) (string, error) {
	initBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "aip-controller", "version": "v1alpha1"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal initialize request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, bytes.NewReader(initBody))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}

	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		return "", fmt.Errorf("upstream returned empty Mcp-Session-Id")
	}

	return sessionID, nil
}

// resolveBearerToken reads the bearer token from the Secret referenced by spec.bearerTokenSecretRef.
func (r *MCPServerReconciler) resolveBearerToken(ctx context.Context, server *governancev1alpha1.MCPServer) (string, error) {
	if server.Spec.BearerTokenSecretRef == nil {
		return "", nil
	}

	ref := server.Spec.BearerTokenSecretRef
	key := ref.Key
	if key == "" {
		key = mcpDefaultTokenKey
	}

	secretNS := server.Spec.SecretNamespace
	if secretNS == "" {
		return "", fmt.Errorf("secretNamespace is required for cluster-scoped MCPServer %s", server.Name)
	}

	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: secretNS}, &secret); err != nil {
		return "", fmt.Errorf("get secret %s/%s: %w", secretNS, ref.Name, err)
	}

	tokenBytes, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %q", secretNS, ref.Name, key)
	}

	return string(tokenBytes), nil
}

// readSSEDataLine reads an SSE response body and returns the first data line.
// The caller is responsible for closing resp.Body.
func readSSEDataLine(resp *http.Response) (string, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return "", fmt.Errorf("failed to read body: %w", err)
	}
	for line := range bytes.SplitSeq(buf.Bytes(), []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("data:")) {
			return string(bytes.TrimSpace(line[5:])), nil
		}
	}
	return "", fmt.Errorf("no SSE data line in response")
}

// truncateMessage truncates a string to maxMessageLen bytes,
// ensuring the result is valid UTF-8 (no partial multi-byte runes).
func truncateMessage(msg string) string {
	if len(msg) <= maxMessageLen {
		return msg
	}
	s := msg[:maxMessageLen]
	// Drop any incomplete rune at the end.
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// SetupWithManager sets up the controller with the Manager.
func (r *MCPServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&governancev1alpha1.MCPServer{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToMCPServer),
		).
		Named("mcpserver").
		Complete(r)
}

// mapSecretToMCPServer maps a Secret change to the MCPServer(s) that reference it.
func (r *MCPServerReconciler) mapSecretToMCPServer(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	var list governancev1alpha1.MCPServerList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list MCPServers for Secret watch")
		return nil
	}

	var reqs []reconcile.Request
	for _, srv := range list.Items {
		if srv.Spec.BearerTokenSecretRef == nil {
			continue
		}
		secretNS := srv.Spec.SecretNamespace
		if secretNS == "" {
			continue
		}
		if srv.Spec.BearerTokenSecretRef.Name == secret.Name &&
			secretNS == secret.Namespace {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      srv.Name,
					Namespace: srv.Namespace,
				},
			})
		}
	}
	return reqs
}
