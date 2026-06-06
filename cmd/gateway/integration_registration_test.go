package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/mcp"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func runRegistrationIntegrationTests(t *testing.T, directClient client.Client, ctx context.Context) {
	t.Run("AgentRegistration - Admission Policies (strict/warn/allow)", func(t *testing.T) {
		gm := gomega.NewWithT(t)

		// Create a strict server
		sStrict := &Server{
			client:                  directClient,
			apiReader:               directClient,
			dedupWindow:             0,
			waitTimeout:             200 * time.Millisecond,
			roles:                   newRoleConfig("", "", "", "", "", ""),
			authRequired:            false,
			regCache:                newRegistrationCache(directClient),
			unregisteredAgentPolicy: "strict",
		}

		// Try to create unregistered agent request on strict server -> Forbidden
		body := createAgentRequestBody{
			AgentIdentity: "unregistered-agent-strict",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/approval-test",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		rr := httptest.NewRecorder()
		sStrict.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))
		gm.Expect(rr.Body.String()).To(gomega.ContainSubstring("AGENT_NOT_REGISTERED"))

		// Create a warn server
		sWarn := &Server{
			client:                  directClient,
			apiReader:               directClient,
			dedupWindow:             0,
			waitTimeout:             100 * time.Millisecond,
			roles:                   newRoleConfig("", "", "", "", "", ""),
			authRequired:            false,
			regCache:                newRegistrationCache(directClient),
			unregisteredAgentPolicy: "warn",
		}

		bodyWarn := createAgentRequestBody{
			AgentIdentity: "unregistered-agent-warn",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/approval-test",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBodyWarn, _ := json.Marshal(bodyWarn)
		reqWarn := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBodyWarn))
		rrWarn := httptest.NewRecorder()
		sWarn.handleCreateAgentRequest(rrWarn, reqWarn)
		gm.Expect(rrWarn.Code).To(gomega.Equal(http.StatusGatewayTimeout))

		// Check that the request was created and has the unregistered annotation
		var list v1alpha1.AgentRequestList
		gm.Expect(directClient.List(ctx, &list)).To(gomega.Succeed())
		var foundWarnReq *v1alpha1.AgentRequest
		for _, item := range list.Items {
			if item.Spec.AgentIdentity == "unregistered-agent-warn" {
				foundWarnReq = &item
				break
			}
		}
		gm.Expect(foundWarnReq).NotTo(gomega.BeNil())
		gm.Expect(foundWarnReq.Annotations["governance.aip.io/unregistered"]).To(gomega.Equal("true"))

		// Create an allow server
		sAllow := &Server{
			client:                  directClient,
			apiReader:               directClient,
			dedupWindow:             0,
			waitTimeout:             100 * time.Millisecond,
			roles:                   newRoleConfig("", "", "", "", "", ""),
			authRequired:            false,
			regCache:                newRegistrationCache(directClient),
			unregisteredAgentPolicy: "allow",
		}

		bodyAllow := createAgentRequestBody{
			AgentIdentity: "unregistered-agent-allow",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/approval-test",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBodyAllow, _ := json.Marshal(bodyAllow)
		reqAllow := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBodyAllow))
		rrAllow := httptest.NewRecorder()
		sAllow.handleCreateAgentRequest(rrAllow, reqAllow)
		gm.Expect(rrAllow.Code).To(gomega.Equal(http.StatusGatewayTimeout))

		// Check that the request was created without the unregistered annotation
		gm.Expect(directClient.List(ctx, &list)).To(gomega.Succeed())
		var foundAllowReq *v1alpha1.AgentRequest
		for _, item := range list.Items {
			if item.Spec.AgentIdentity == "unregistered-agent-allow" {
				foundAllowReq = &item
				break
			}
		}
		gm.Expect(foundAllowReq).NotTo(gomega.BeNil())
		gm.Expect(foundAllowReq.Annotations["governance.aip.io/unregistered"]).To(gomega.Equal(""))

		cleanup(ctx, gm, directClient)
	})

	t.Run("AgentRegistration - OIDC AllowedSubjects validation", func(t *testing.T) {
		gm := gomega.NewWithT(t)

		// Create an agent registration with OIDC allowed subjects list
		reg := &v1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-agent-reg",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRegistrationSpec{
				AgentIdentity: "agent-1",
				OIDC: &v1alpha1.AgentRegistrationOIDC{
					Issuer:          "https://oidc.example.com",
					AllowedSubjects: []string{"sub-ok-1", "sub-ok-2"},
				},
			},
		}
		gm.Expect(directClient.Create(ctx, reg)).To(gomega.Succeed())
		defer func() { _ = directClient.Delete(ctx, reg) }()

		regCache := newRegistrationCache(directClient)
		regCache.upsert(reg)

		s := &Server{
			client:                  directClient,
			apiReader:               directClient,
			dedupWindow:             0,
			waitTimeout:             100 * time.Millisecond,
			roles:                   newRoleConfig("agent-1", "", "", "", "", ""),
			authRequired:            true,
			regCache:                regCache,
			unregisteredAgentPolicy: "strict",
		}

		body := createAgentRequestBody{
			AgentIdentity: "agent-1",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/approval-test",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)

		// 1. Call with wrong subject -> Forbidden
		reqWrong := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		reqWrongCtx := withCallerSub(reqWrong.Context(), "sub-wrong")
		reqWrongCtx = withCallerGroups(reqWrongCtx, []string{})
		reqWrong = reqWrong.WithContext(reqWrongCtx)
		rrWrong := httptest.NewRecorder()
		s.handleCreateAgentRequest(rrWrong, reqWrong)
		gm.Expect(rrWrong.Code).To(gomega.Equal(http.StatusForbidden))
		gm.Expect(rrWrong.Body.String()).To(gomega.ContainSubstring("IDENTITY_MISMATCH"))

		// 2. Call with correct subject -> proceed to GatewayTimeout (not Forbidden)
		reqOk := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		reqOkCtx := withCallerSub(reqOk.Context(), "sub-ok-2")
		reqOkCtx = withCallerGroups(reqOkCtx, []string{})
		reqOk = reqOk.WithContext(reqOkCtx)
		rrOk := httptest.NewRecorder()
		s.handleCreateAgentRequest(rrOk, reqOk)
		gm.Expect(rrOk.Code).To(gomega.Equal(http.StatusGatewayTimeout))

		cleanup(ctx, gm, directClient)
	})

	t.Run("AgentRegistration - Outbound Credential Propagation (StaticSecret)", func(t *testing.T) {
		gm := gomega.NewWithT(t)

		// 1. Create a secret with the agent PAT
		patVal := "agent-pat-token-value-xyz"
		patSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "agent-pat-secret",
				Namespace: testDefaultNS,
			},
			Data: map[string][]byte{
				"token": []byte(patVal),
			},
		}
		gm.Expect(directClient.Create(ctx, patSecret)).To(gomega.Succeed())
		defer func() { _ = directClient.Delete(ctx, patSecret) }()

		// 2. Create the AgentRegistration binding the MCP service 'github' to this StaticSecret
		reg := &v1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "agent-github-reg",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRegistrationSpec{
				AgentIdentity: "github-agent",
				ExternalIdentities: []v1alpha1.ExternalIdentityBinding{
					{
						Service: "github",
						Type:    v1alpha1.ExternalIdentityStaticSecret,
						StaticSecret: &v1alpha1.StaticSecretCredential{
							Name:      "agent-pat-secret",
							Namespace: testDefaultNS,
							Key:       "token",
						},
					},
				},
			},
		}
		gm.Expect(directClient.Create(ctx, reg)).To(gomega.Succeed())
		defer func() { _ = directClient.Delete(ctx, reg) }()

		regCache := newRegistrationCache(directClient)
		regCache.upsert(reg)

		// 3. Spin up local stub MCP server that records the Authorization header
		var receivedAuth string
		var mu sync.Mutex
		stubMCPServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			receivedAuth = r.Header.Get("Authorization")
			mu.Unlock()

			bodyBytes, _ := io.ReadAll(r.Body)
			if strings.Contains(string(bodyBytes), `"tools/list"`) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, "event: message")
			_, _ = fmt.Fprintln(w, `data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"pr created"}]}}`)
		}))
		defer stubMCPServer.Close()

		// 4. Create server instance wired with registrationCache and stub MCPServer definition
		s := &Server{
			client:                  directClient,
			apiReader:               directClient,
			httpClient:              &http.Client{Timeout: 5 * time.Second},
			regCache:                regCache,
			unregisteredAgentPolicy: "allow",
			mcpServers: []MCPServer{
				{
					Name:        "github",
					URL:         stubMCPServer.URL,
					Status:      "available",
					BearerToken: "global-mcp-shared-token",
					Tools: []MCPTool{
						{Name: "create_pull_request", ReadOnly: true},
					},
					sessionMu: &sync.Mutex{},
				},
			},
		}

		body := mcpRequest("tools/call", mcp.ToolsCallParams{
			Name:      "github/create_pull_request",
			Arguments: map[string]any{},
		})

		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
		rctx := withCallerSub(req.Context(), "github-agent")
		req = req.WithContext(rctx)
		rr := httptest.NewRecorder()

		s.handleMCP(rr, req)

		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))

		mu.Lock()
		authHeader := receivedAuth
		mu.Unlock()

		gm.Expect(authHeader).To(gomega.Equal("Bearer " + patVal))

		cleanup(ctx, gm, directClient)
	})
}
