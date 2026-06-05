package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func runAuthAndApprovalTests(t *testing.T, mgrClient, directClient client.Client, ctx context.Context) {
	t.Run("RequiresApproval condition - returns 201 early", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  0,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		const targetURI = "k8s://prod/default/deployment/approval-test"
		policy := createApprovalPolicy(ctx, gm, directClient, "require-approval-policy", targetURI)

		body := createAgentRequestBody{
			AgentIdentity: "agent-approval",
			Action:        "restart",
			TargetURI:     targetURI,
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).NotTo(gomega.HaveOccurred(), "body is a known serializable struct")

		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		rr := httptest.NewRecorder()

		respCh := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			s.handleCreateAgentRequest(rr, req)
			respCh <- rr
		}()

		var resp *httptest.ResponseRecorder
		gm.Eventually(respCh, eventuallyLongTimeout).Should(gomega.Receive(&resp))
		gm.Expect(resp.Code).To(gomega.Equal(http.StatusCreated))

		var respBody map[string]any
		gm.Expect(json.Unmarshal(resp.Body.Bytes(), &respBody)).To(gomega.Succeed())
		gm.Expect(respBody["phase"]).To(gomega.Equal(string(v1alpha1.PhasePending)))

		gm.Eventually(func() error {
			var list v1alpha1.AgentRequestList
			// Ignore error as this is inside Eventually and transient errors are expected
			_ = directClient.List(ctx, &list, client.InNamespace(testDefaultNS))
			for _, item := range list.Items {
				if item.Spec.AgentIdentity == "agent-approval" {
					for _, c := range item.Status.Conditions {
						if c.Type == v1alpha1.ConditionRequiresApproval && c.Status == metav1.ConditionTrue {
							return nil
						}
					}
				}
			}
			return errors.New("AgentRequest with RequiresApproval condition not found")
		}, eventuallyTimeout).Should(gomega.Succeed())

		gm.Expect(directClient.Delete(ctx, policy)).To(gomega.Succeed())
		cleanup(ctx, gm, directClient)
	})

	t.Run("Auth - missing role returns 403", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  0,
			waitTimeout:  2 * time.Second,
			roles:        newRoleConfig(testAgentSub, testReviewerSub, "", "", "", ""),
			authRequired: true,
		}

		body := createAgentRequestBody{
			AgentIdentity: "agent-auth",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/auth-fail",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).NotTo(gomega.HaveOccurred(), "body is a known serializable struct")

		ctxWithAuth := withCallerSub(ctx, "some-caller")
		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody)).WithContext(ctxWithAuth)
		rr := httptest.NewRecorder()

		s.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))
	})

	t.Run("GET /agent-requests/{name} - returns current state", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:    mgrClient,
			apiReader: mgrClient,
		}

		ar := &v1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "get-test",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRequestSpec{
				AgentIdentity: "agent-get",
				Action:        "restart",
				Target:        v1alpha1.Target{URI: "k8s://prod/default/deployment/get-test"},
				Reason:        "test",
			},
		}
		gm.Expect(directClient.Create(ctx, ar)).To(gomega.Succeed())

		gm.Eventually(func() string {
			var current v1alpha1.AgentRequest
			key := types.NamespacedName{Name: "get-test", Namespace: testDefaultNS}
			if err := directClient.Get(ctx, key, &current); err != nil {
				return ""
			}
			return current.Status.Phase
		}, eventuallyLongTimeout).Should(gomega.Equal(v1alpha1.PhaseApproved))

		req := httptest.NewRequest("GET", "/agent-requests/get-test?namespace=default", nil)
		rr := httptest.NewRecorder()

		mux := http.NewServeMux()
		mux.HandleFunc("GET /agent-requests/{name}", s.handleGetAgentRequest)
		mux.ServeHTTP(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))

		var respBody map[string]any
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &respBody)).To(gomega.Succeed())
		gm.Expect(respBody["phase"]).To(gomega.Equal(string(v1alpha1.PhaseApproved)))

		cleanup(ctx, gm, directClient)
	})

	t.Run("Auth - agentIdentity derived from token sub when authRequired: true (body.AgentIdentity ignored)", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  5 * time.Second,
			waitTimeout:  2 * time.Second,
			roles:        newRoleConfig("agent-sub-token", testReviewerSub, "", "", "", ""),
			authRequired: true,
		}

		gr := &v1alpha1.GovernedResource{
			ObjectMeta: metav1.ObjectMeta{
				Name: "auth-override-gr",
			},
			Spec: v1alpha1.GovernedResourceSpec{
				URIPattern:       "k8s://prod/default/deployment/auth-override-test",
				PermittedActions: []string{"restart"},
				PermittedAgents:  []string{"agent-sub-token"},
				ContextFetcher:   "none",
			},
		}
		gm.Expect(directClient.Create(ctx, gr)).To(gomega.Succeed())
		defer func() {
			gm.Expect(directClient.Delete(ctx, gr)).To(gomega.Succeed())
		}()

		// Wait for GR to be in mgrClient cache
		gm.Eventually(func() error {
			var checkGR v1alpha1.GovernedResource
			return mgrClient.Get(ctx, types.NamespacedName{Name: gr.Name}, &checkGR)
		}, eventuallyTimeout).Should(gomega.Succeed())

		body := createAgentRequestBody{
			AgentIdentity: "impersonated-agent-body",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/auth-override-test",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		ctxWithAuth := withCallerSub(ctx, "agent-sub-token")
		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody)).WithContext(ctxWithAuth)
		rr := httptest.NewRecorder()

		s.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Or(gomega.Equal(http.StatusCreated), gomega.Equal(http.StatusOK)))

		var list v1alpha1.AgentRequestList
		gm.Eventually(func() error {
			if err := directClient.List(ctx, &list, client.InNamespace(testDefaultNS)); err != nil {
				return err
			}
			found := false
			for _, ar := range list.Items {
				if ar.Spec.Target.URI == "k8s://prod/default/deployment/auth-override-test" {
					found = true
					break
				}
			}
			if !found {
				return errors.New("AgentRequest not created yet")
			}
			return nil
		}, eventuallyTimeout).Should(gomega.Succeed())

		var found *v1alpha1.AgentRequest
		for _, ar := range list.Items {
			if ar.Spec.Target.URI == "k8s://prod/default/deployment/auth-override-test" {
				found = &ar
				break
			}
		}
		gm.Expect(found).NotTo(gomega.BeNil())
		gm.Expect(found.Spec.AgentIdentity).To(gomega.Equal("agent-sub-token"))

		// Assert all derived keys use "agent-sub-token", not the body
		gm.Expect(found.Labels["aip.io/agentIdentity"]).To(gomega.Equal(sanitizeLabelValue("agent-sub-token")))
		gm.Expect(found.Labels["aip.io/profileName"]).To(gomega.Equal(v1alpha1.ProfileNameForAgent("agent-sub-token")))
		expectedSlug := sanitizeDNSSegment("agent-sub-token", 54)
		gm.Expect(found.Name).To(gomega.HavePrefix(expectedSlug))

		cleanup(ctx, gm, directClient)
	})

	t.Run("Auth - permittedAgents check uses callerSub, not body when authRequired: true", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  0,
			waitTimeout:  2 * time.Second,
			roles:        newRoleConfig("agent-sub-token,forbidden-agent", testReviewerSub, "", "", "", ""),
			authRequired: true,
		}

		gr := &v1alpha1.GovernedResource{
			ObjectMeta: metav1.ObjectMeta{
				Name: "auth-permitted-agents-gr",
			},
			Spec: v1alpha1.GovernedResourceSpec{
				URIPattern:       "k8s://prod/default/deployment/auth-permitted-agents-test",
				PermittedActions: []string{"restart"},
				PermittedAgents:  []string{"agent-sub-token"},
				ContextFetcher:   "none",
			},
		}
		gm.Expect(directClient.Create(ctx, gr)).To(gomega.Succeed())
		defer func() {
			gm.Expect(directClient.Delete(ctx, gr)).To(gomega.Succeed())
		}()

		// Wait for GR to be in mgrClient cache
		gm.Eventually(func() error {
			var checkGR v1alpha1.GovernedResource
			return mgrClient.Get(ctx, types.NamespacedName{Name: gr.Name}, &checkGR)
		}, eventuallyTimeout).Should(gomega.Succeed())

		// We use callerSub = "forbidden-agent" (who is not in permittedAgents)
		// but body agentIdentity = "agent-sub-token" (who is in permittedAgents).
		body := createAgentRequestBody{
			AgentIdentity: "agent-sub-token",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/auth-permitted-agents-test",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		ctxWithAuth := withCallerSub(ctx, "forbidden-agent")
		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody)).WithContext(ctxWithAuth)
		rr := httptest.NewRecorder()

		s.handleCreateAgentRequest(rr, req)
		// It must fail with StatusForbidden (403) and DenialCodeIdentityInvalid (IDENTITY_INVALID)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))
		gm.Expect(rr.Body.String()).To(gomega.ContainSubstring(v1alpha1.DenialCodeIdentityInvalid))
	})

	t.Run("Auth - agentIdentity in body is honored when authRequired: false", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  5 * time.Second,
			waitTimeout:  2 * time.Second,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		body := createAgentRequestBody{
			AgentIdentity: "agent-body-honored",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/auth-honored-test",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		rr := httptest.NewRecorder()

		s.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Or(gomega.Equal(http.StatusCreated), gomega.Equal(http.StatusOK)))

		var list v1alpha1.AgentRequestList
		gm.Eventually(func() error {
			if err := directClient.List(ctx, &list, client.InNamespace(testDefaultNS)); err != nil {
				return err
			}
			found := false
			for _, ar := range list.Items {
				if ar.Spec.Target.URI == "k8s://prod/default/deployment/auth-honored-test" {
					found = true
					break
				}
			}
			if !found {
				return errors.New("AgentRequest not created yet")
			}
			return nil
		}, eventuallyTimeout).Should(gomega.Succeed())

		var found *v1alpha1.AgentRequest
		for _, ar := range list.Items {
			if ar.Spec.Target.URI == "k8s://prod/default/deployment/auth-honored-test" {
				found = &ar
				break
			}
		}
		gm.Expect(found).NotTo(gomega.BeNil())
		gm.Expect(found.Spec.AgentIdentity).To(gomega.Equal("agent-body-honored"))

		// Assert all derived keys use "agent-body-honored"
		gm.Expect(found.Labels["aip.io/agentIdentity"]).To(gomega.Equal(sanitizeLabelValue("agent-body-honored")))
		gm.Expect(found.Labels["aip.io/profileName"]).To(gomega.Equal(v1alpha1.ProfileNameForAgent("agent-body-honored")))
		expectedSlug := sanitizeDNSSegment("agent-body-honored", 54)
		gm.Expect(found.Name).To(gomega.HavePrefix(expectedSlug))

		cleanup(ctx, gm, directClient)
	})

	t.Run("Auth - body.AgentIdentity is used even when callerSub is set and authRequired: false (proxy-header dev mode)", func(t *testing.T) {
		// Regression test: when authRequired=false the proxy-header middleware can
		// populate callerSub via X-Remote-User. The handler must still use
		// body.AgentIdentity and NOT silently substitute the proxy sub — otherwise
		// a spoofed header would override the agent's declared identity in dev mode.
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  0,
			waitTimeout:  200 * time.Millisecond,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		body := createAgentRequestBody{
			AgentIdentity: "real-body-agent",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/proxy-header-test",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		// Simulate proxy-header middleware having set a different sub.
		ctxWithSub := withCallerSub(ctx, "proxy-injected-sub")
		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody)).WithContext(ctxWithSub)
		rr := httptest.NewRecorder()

		s.handleCreateAgentRequest(rr, req)
		// Handler creates the request (times out waiting for phase — no controller).
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusGatewayTimeout))

		// The created AgentRequest must use body.AgentIdentity, not the proxy sub.
		var list v1alpha1.AgentRequestList
		gm.Eventually(func() error {
			if err := directClient.List(ctx, &list, client.InNamespace(testDefaultNS)); err != nil {
				return err
			}
			for _, ar := range list.Items {
				if ar.Spec.Target.URI == "k8s://prod/default/deployment/proxy-header-test" {
					return nil
				}
			}
			return errors.New("AgentRequest not created yet")
		}, eventuallyTimeout).Should(gomega.Succeed())

		var found *v1alpha1.AgentRequest
		for _, ar := range list.Items {
			if ar.Spec.Target.URI == "k8s://prod/default/deployment/proxy-header-test" {
				found = &ar
				break
			}
		}
		gm.Expect(found).NotTo(gomega.BeNil())
		gm.Expect(found.Spec.AgentIdentity).To(gomega.Equal("real-body-agent"),
			"proxy-injected-sub must not override body.AgentIdentity when authRequired=false")
		gm.Expect(found.Labels["aip.io/agentIdentity"]).To(gomega.Equal(sanitizeLabelValue("real-body-agent")))

		cleanup(ctx, gm, directClient)
	})

	runHumanDecisionTests(t, mgrClient, directClient, ctx)
	runAuditTests(t, mgrClient, directClient, ctx)
}

func runHumanDecisionTests(t *testing.T, mgrClient, directClient client.Client, ctx context.Context) {
	t.Run("POST /approve transitions to Approved", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  0,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig(testAgentSub, testReviewerSub, "", "", "", ""),
			authRequired: false,
		}

		const targetURI = "k8s://prod/default/deployment/approve-test"
		policy := createApprovalPolicy(ctx, gm, directClient, "approve-policy", targetURI)

		arName := createRequestAndWaitForPending(ctx, gm, s, targetURI)

		// Approve as reviewer
		body := `{"decision":"approved","reason":"looks good"}`
		path := fmt.Sprintf("/agent-requests/%s/approve?namespace=default", arName)
		approveReq := httptest.NewRequest("POST", path, bytes.NewBufferString(body))
		approveRR := httptest.NewRecorder()

		mux := http.NewServeMux()
		mux.HandleFunc("POST /agent-requests/{name}/approve", s.handleApproveAgentRequest)
		mux.ServeHTTP(approveRR, approveReq.WithContext(withCallerSub(ctx, testReviewerSub)))
		gm.Expect(approveRR.Code).To(gomega.Equal(http.StatusOK))

		gm.Eventually(func() string {
			var current v1alpha1.AgentRequest
			if err := directClient.Get(ctx, types.NamespacedName{Name: arName, Namespace: testDefaultNS}, &current); err != nil {
				return ""
			}
			return current.Status.Phase
		}, eventuallyLongTimeout).Should(gomega.Equal(v1alpha1.PhaseApproved))

		gm.Expect(directClient.Delete(ctx, policy)).To(gomega.Succeed())
		cleanup(ctx, gm, directClient)
	})

	t.Run("POST /deny transitions to Denied", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  0,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig(testAgentSub, testReviewerSub, "", "", "", ""),
			authRequired: false,
		}

		const targetURI = "k8s://prod/default/deployment/deny-test"
		policy := createApprovalPolicy(ctx, gm, directClient, "deny-test-policy", targetURI)

		arName := createRequestAndWaitForPending(ctx, gm, s, targetURI)

		// Deny as reviewer
		path := fmt.Sprintf("/agent-requests/%s/deny?namespace=default", arName)
		denyReq := httptest.NewRequest("POST", path, bytes.NewBufferString(`{"reason":"not allowed"}`))
		denyRR := httptest.NewRecorder()

		mux := http.NewServeMux()
		mux.HandleFunc("POST /agent-requests/{name}/deny", s.handleDenyAgentRequest)
		mux.ServeHTTP(denyRR, denyReq.WithContext(withCallerSub(ctx, testReviewerSub)))
		gm.Expect(denyRR.Code).To(gomega.Equal(http.StatusOK))

		gm.Eventually(func() string {
			var current v1alpha1.AgentRequest
			if err := directClient.Get(ctx, types.NamespacedName{Name: arName, Namespace: testDefaultNS}, &current); err != nil {
				return ""
			}
			return current.Status.Phase
		}, eventuallyLongTimeout).Should(gomega.Equal(v1alpha1.PhaseDenied))

		gm.Expect(directClient.Delete(ctx, policy)).To(gomega.Succeed())
		cleanup(ctx, gm, directClient)
	})
}

func runAuditTests(t *testing.T, mgrClient, directClient client.Client, ctx context.Context) {
	t.Run("AuditRecord emitted on request.submitted", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  0,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		body := createAgentRequestBody{
			AgentIdentity: "agent-audit",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/audit-record",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).NotTo(gomega.HaveOccurred(), "body is a known serializable struct")

		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		rr := httptest.NewRecorder()

		s.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

		gm.Eventually(func() bool {
			var auditList v1alpha1.AuditRecordList
			// Ignore error as this is inside Eventually and transient errors are expected
			_ = directClient.List(ctx, &auditList, client.InNamespace(testDefaultNS))
			for _, item := range auditList.Items {
				if item.Spec.Event == "request.submitted" && item.Spec.AgentIdentity == "agent-audit" {
					return true
				}
			}
			return false
		}, eventuallyTimeout).Should(gomega.BeTrue())

		cleanup(ctx, gm, directClient)
	})
}

func createApprovalPolicy(
	ctx context.Context, gm *gomega.WithT, c client.Client, name, targetURI string,
) *v1alpha1.SafetyPolicy {
	policy := &v1alpha1.SafetyPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testDefaultNS,
		},
		Spec: v1alpha1.SafetyPolicySpec{
			Rules: []v1alpha1.Rule{
				{
					Name:       "require-approval-rule",
					Type:       "StateEvaluation",
					Action:     "RequireApproval",
					Expression: fmt.Sprintf(`request.spec.target.uri == %q`, targetURI),
				},
			},
		},
	}
	gm.Expect(c.Create(ctx, policy)).To(gomega.Succeed())
	return policy
}

func createRequestAndWaitForPending(ctx context.Context, gm *gomega.WithT, s *Server, targetURI string) string {
	body := createAgentRequestBody{
		AgentIdentity: testAgentSub,
		Action:        "restart",
		TargetURI:     targetURI,
		Reason:        "test",
		Namespace:     testDefaultNS,
	}
	jsonBody, err := json.Marshal(body)
	gm.Expect(err).NotTo(gomega.HaveOccurred(), "body is a known serializable struct")

	req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
	req = req.WithContext(withCallerSub(ctx, testAgentSub))
	rr := httptest.NewRecorder()

	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		s.handleCreateAgentRequest(rr, req)
		respCh <- rr
	}()

	var resp *httptest.ResponseRecorder
	gm.Eventually(respCh, eventuallyLongTimeout).Should(gomega.Receive(&resp))
	gm.Expect(resp.Code).To(gomega.Equal(http.StatusCreated))

	var list v1alpha1.AgentRequestList
	gm.Eventually(func() int {
		// Ignore error as this is inside Eventually and transient errors are expected
		_ = s.client.List(ctx, &list, client.InNamespace(testDefaultNS))
		count := 0
		for _, item := range list.Items {
			if item.Spec.AgentIdentity == testAgentSub && item.Status.Phase == v1alpha1.PhasePending {
				count++
			}
		}
		return count
	}, eventuallyTimeout).Should(gomega.BeNumerically(">=", 1))

	for _, item := range list.Items {
		if item.Spec.AgentIdentity == testAgentSub && item.Status.Phase == v1alpha1.PhasePending {
			return item.Name
		}
	}
	return ""
}
