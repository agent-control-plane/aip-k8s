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

		// 1. Allow mode (default): unregistered agent -> 201, annotation absent
		sAllow := &Server{
			client:                  directClient,
			apiReader:               directClient,
			dedupWindow:             0,
			waitTimeout:             serverWaitTimeout,
			roles:                   newRoleConfig("", "", "", "", "", ""),
			authRequired:            false,
			regCache:                newRegistrationCache(directClient),
			unregisteredAgentPolicy: "allow",
		}

		bodyAllow := createAgentRequestBody{
			AgentIdentity: "unregistered-agent-allow",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/approval-test-allow",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBodyAllow, _ := json.Marshal(bodyAllow)
		reqAllow := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBodyAllow))
		rrAllow := httptest.NewRecorder()
		sAllow.handleCreateAgentRequest(rrAllow, reqAllow)
		gm.Expect(rrAllow.Code).To(gomega.Equal(http.StatusCreated))

		// Check that the request was created and annotation is absent
		var list v1alpha1.AgentRequestList
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

		// 2. Warn mode: unregistered agent -> 201, created AgentRequest has annotation governance.aip.io/unregistered=true
		sWarn := &Server{
			client:                  directClient,
			apiReader:               directClient,
			dedupWindow:             0,
			waitTimeout:             serverWaitTimeout,
			roles:                   newRoleConfig("", "", "", "", "", ""),
			authRequired:            false,
			regCache:                newRegistrationCache(directClient),
			unregisteredAgentPolicy: "warn",
		}

		bodyWarn := createAgentRequestBody{
			AgentIdentity: "unregistered-agent-warn",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/approval-test-warn",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBodyWarn, _ := json.Marshal(bodyWarn)
		reqWarn := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBodyWarn))
		rrWarn := httptest.NewRecorder()
		sWarn.handleCreateAgentRequest(rrWarn, reqWarn)
		gm.Expect(rrWarn.Code).To(gomega.Equal(http.StatusCreated))

		// Check that the request was created and has the unregistered annotation
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

		// 3. Strict mode: unregistered agent -> 403, body contains AGENT_NOT_REGISTERED
		sStrict := &Server{
			client:                  directClient,
			apiReader:               directClient,
			dedupWindow:             0,
			waitTimeout:             serverWaitTimeout,
			roles:                   newRoleConfig("", "", "", "", "", ""),
			authRequired:            false,
			regCache:                newRegistrationCache(directClient),
			unregisteredAgentPolicy: "strict",
		}

		bodyStrict := createAgentRequestBody{
			AgentIdentity: "unregistered-agent-strict",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/approval-test-strict",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBodyStrict, _ := json.Marshal(bodyStrict)
		reqStrict := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBodyStrict))
		rrStrict := httptest.NewRecorder()
		sStrict.handleCreateAgentRequest(rrStrict, reqStrict)
		gm.Expect(rrStrict.Code).To(gomega.Equal(http.StatusForbidden))
		gm.Expect(rrStrict.Body.String()).To(gomega.ContainSubstring("AGENT_NOT_REGISTERED"))

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
			waitTimeout:             serverWaitTimeout,
			roles:                   newRoleConfig("agent-1,sub-ok-2,sub-wrong", "", "", "", "", ""),
			authRequired:            true,
			regCache:                regCache,
			unregisteredAgentPolicy: "strict",
		}

		body := createAgentRequestBody{
			Action:    "restart",
			TargetURI: "k8s://prod/default/deployment/approval-test-oidc",
			Reason:    "test",
			Namespace: testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)

		// 4. Call with correct subject -> 201
		reqOk := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		reqOkCtx := withCallerSub(reqOk.Context(), "sub-ok-2")
		reqOkCtx = withCallerGroups(reqOkCtx, []string{})
		reqOk = reqOk.WithContext(reqOkCtx)
		rrOk := httptest.NewRecorder()
		s.handleCreateAgentRequest(rrOk, reqOk)
		gm.Expect(rrOk.Code).To(gomega.Equal(http.StatusCreated))

		// 5. Call with wrong subject -> 403, body contains IDENTITY_MISMATCH
		bodyWrong := createAgentRequestBody{
			AgentIdentity: "agent-1",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/approval-test-oidc",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBodyWrong, _ := json.Marshal(bodyWrong)
		reqWrong := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBodyWrong))
		reqWrongCtx := withCallerSub(reqWrong.Context(), "sub-wrong")
		reqWrongCtx = withCallerGroups(reqWrongCtx, []string{})
		reqWrong = reqWrong.WithContext(reqWrongCtx)
		rrWrong := httptest.NewRecorder()
		s.handleCreateAgentRequest(rrWrong, reqWrong)
		gm.Expect(rrWrong.Code).To(gomega.Equal(http.StatusForbidden))
		gm.Expect(rrWrong.Body.String()).To(gomega.ContainSubstring("IDENTITY_MISMATCH"))

		// 6. Registered agent, OIDC == nil on registration -> 201 when the caller subject matches the identity.
		regNilOIDC := &v1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-agent-nil-oidc",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRegistrationSpec{
				AgentIdentity: "agent-nil-oidc",
			},
		}
		gm.Expect(directClient.Create(ctx, regNilOIDC)).To(gomega.Succeed())
		defer func() { _ = directClient.Delete(ctx, regNilOIDC) }()

		regCache.upsert(regNilOIDC)

		sNilOIDC := &Server{
			client:                  directClient,
			apiReader:               directClient,
			dedupWindow:             0,
			waitTimeout:             serverWaitTimeout,
			roles:                   newRoleConfig("agent-nil-oidc", "", "", "", "", ""),
			authRequired:            true,
			regCache:                regCache,
			unregisteredAgentPolicy: "strict",
		}

		bodyNilOIDC := createAgentRequestBody{
			AgentIdentity: "agent-nil-oidc",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/approval-test-nil-oidc",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBodyNilOIDC, _ := json.Marshal(bodyNilOIDC)
		reqNilOIDCPost := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBodyNilOIDC))
		reqNilOIDCPostCtx := withCallerSub(reqNilOIDCPost.Context(), "agent-nil-oidc")
		reqNilOIDCPostCtx = withCallerGroups(reqNilOIDCPostCtx, []string{})
		reqNilOIDCPost = reqNilOIDCPost.WithContext(reqNilOIDCPostCtx)
		rrNilOIDCPost := httptest.NewRecorder()
		sNilOIDC.handleCreateAgentRequest(rrNilOIDCPost, reqNilOIDCPost)
		gm.Expect(rrNilOIDCPost.Code).To(gomega.Equal(http.StatusCreated))

		// 7. Registered agent, OIDC.AllowedSubjects empty -> 201 (empty list = no enforcement)
		regEmptyOIDC := &v1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-agent-empty-oidc",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRegistrationSpec{
				AgentIdentity: "agent-empty-oidc",
				OIDC: &v1alpha1.AgentRegistrationOIDC{
					Issuer:          "https://oidc.example.com",
					AllowedSubjects: []string{},
				},
			},
		}
		gm.Expect(directClient.Create(ctx, regEmptyOIDC)).To(gomega.Succeed())
		defer func() { _ = directClient.Delete(ctx, regEmptyOIDC) }()

		regCache.upsert(regEmptyOIDC)

		sEmptyOIDC := &Server{
			client:                  directClient,
			apiReader:               directClient,
			dedupWindow:             0,
			waitTimeout:             serverWaitTimeout,
			roles:                   newRoleConfig("any-sub-again,agent-empty-oidc,someone-else", "", "", "", "", ""),
			authRequired:            true,
			regCache:                regCache,
			unregisteredAgentPolicy: "strict",
		}

		bodyEmptyOIDC := createAgentRequestBody{
			AgentIdentity: "agent-empty-oidc",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/approval-test-empty-oidc",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBodyEmptyOIDC, _ := json.Marshal(bodyEmptyOIDC)
		reqEmptyOIDCPost := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBodyEmptyOIDC))
		// When AllowedSubjects is empty, sub must match agentIdentity (or be empty).
		reqEmptyOIDCPostCtx := withCallerSub(reqEmptyOIDCPost.Context(), "agent-empty-oidc")
		reqEmptyOIDCPostCtx = withCallerGroups(reqEmptyOIDCPostCtx, []string{})
		reqEmptyOIDCPost = reqEmptyOIDCPost.WithContext(reqEmptyOIDCPostCtx)
		rrEmptyOIDCPost := httptest.NewRecorder()
		sEmptyOIDC.handleCreateAgentRequest(rrEmptyOIDCPost, reqEmptyOIDCPost)
		gm.Expect(rrEmptyOIDCPost.Code).To(gomega.Equal(http.StatusCreated))

		// 8. Registered agent, empty AllowedSubjects, mismatched sub -> 403 IDENTITY_MISMATCH
		reqEmptyOIDCWrong := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBodyEmptyOIDC))
		reqEmptyOIDCWrongCtx := withCallerSub(reqEmptyOIDCWrong.Context(), "someone-else")
		reqEmptyOIDCWrongCtx = withCallerGroups(reqEmptyOIDCWrongCtx, []string{})
		reqEmptyOIDCWrong = reqEmptyOIDCWrong.WithContext(reqEmptyOIDCWrongCtx)
		rrEmptyOIDCWrong := httptest.NewRecorder()
		sEmptyOIDC.handleCreateAgentRequest(rrEmptyOIDCWrong, reqEmptyOIDCWrong)
		gm.Expect(rrEmptyOIDCWrong.Code).To(gomega.Equal(http.StatusForbidden))
		gm.Expect(rrEmptyOIDCWrong.Body.String()).To(gomega.ContainSubstring("IDENTITY_MISMATCH"))

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
