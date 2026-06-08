package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func runAgentRegistrationCRUDTests(t *testing.T, directClient client.Client, ctx context.Context) {
	t.Run("AgentRegistration CRUD and Token Retrieval", func(t *testing.T) {
		gm := gomega.NewWithT(t)

		roles := newRoleConfig("agent-sub,unregistered-agent-sub", "reviewer-sub", "admin-sub", "", "", "")
		regCache := newRegistrationCache(directClient)
		s := &Server{
			client:       directClient,
			apiReader:    directClient,
			roles:        roles,
			authRequired: true,
			regCache:     regCache,
		}

		// 1. Admin creates AgentRegistration → 201, spec.agentIdentity present
		reg := v1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-reg-1",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRegistrationSpec{
				AgentIdentity: "agent-sub",
			},
		}
		jsonBody, _ := json.Marshal(reg)
		req := httptest.NewRequest("POST", "/agent-registrations", bytes.NewBuffer(jsonBody))
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "admin-sub"))
		rr := httptest.NewRecorder()
		s.handleCreateAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

		var created v1alpha1.AgentRegistration
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &created)).To(gomega.Succeed())
		gm.Expect(created.Spec.AgentIdentity).To(gomega.Equal("agent-sub"))

		// 2. Agent role tries to create → 403
		req = httptest.NewRequest("POST", "/agent-registrations", bytes.NewBuffer(jsonBody))
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "agent-sub"))
		rr = httptest.NewRecorder()
		s.handleCreateAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))

		// 3. Admin lists → 200, contains the created registration
		req = httptest.NewRequest("GET", "/agent-registrations", nil)
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "admin-sub"))
		rr = httptest.NewRecorder()
		s.handleListAgentRegistrations(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))

		var list v1alpha1.AgentRegistrationList
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &list)).To(gomega.Succeed())
		found := false
		for _, item := range list.Items {
			if item.Name == "test-reg-1" {
				found = true
				break
			}
		}
		gm.Expect(found).To(gomega.BeTrue())

		// 4. Reviewer role lists → 200 (read access)
		req = httptest.NewRequest("GET", "/agent-registrations", nil)
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "reviewer-sub"))
		rr = httptest.NewRecorder()
		s.handleListAgentRegistrations(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))

		// 5. Agent role lists → 403
		req = httptest.NewRequest("GET", "/agent-registrations", nil)
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "agent-sub"))
		rr = httptest.NewRecorder()
		s.handleListAgentRegistrations(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))

		// 6. Admin gets by name → 200
		req = httptest.NewRequest("GET", "/agent-registrations/test-reg-1", nil)
		req.SetPathValue("name", "test-reg-1")
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "admin-sub"))
		rr = httptest.NewRecorder()
		s.handleGetAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))

		// 7. Reviewer gets by name → 200
		req = httptest.NewRequest("GET", "/agent-registrations/test-reg-1", nil)
		req.SetPathValue("name", "test-reg-1")
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "reviewer-sub"))
		rr = httptest.NewRecorder()
		s.handleGetAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))

		// 8. Admin replaces (PUT) → 200, spec updated
		created.Spec.AgentIdentity = "updated-agent-identity"
		jsonBodyPut, _ := json.Marshal(created)
		req = httptest.NewRequest("PUT", "/agent-registrations/test-reg-1", bytes.NewBuffer(jsonBodyPut))
		req.SetPathValue("name", "test-reg-1")
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "admin-sub"))
		rr = httptest.NewRecorder()
		s.handleReplaceAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))

		var updated v1alpha1.AgentRegistration
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &updated)).To(gomega.Succeed())
		gm.Expect(updated.Spec.AgentIdentity).To(gomega.Equal("updated-agent-identity"))

		// 8b. Admin replaces (PUT) with name mismatch in body vs path → 400
		createdMismatch := created
		createdMismatch.Name = "different-name"
		jsonBodyMismatch, _ := json.Marshal(createdMismatch)
		req = httptest.NewRequest("PUT", "/agent-registrations/test-reg-1", bytes.NewBuffer(jsonBodyMismatch))
		req.SetPathValue("name", "test-reg-1")
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "admin-sub"))
		rr = httptest.NewRecorder()
		s.handleReplaceAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusBadRequest))

		// 9. Agent role tries PUT → 403
		req = httptest.NewRequest("PUT", "/agent-registrations/test-reg-1", bytes.NewBuffer(jsonBodyPut))
		req.SetPathValue("name", "test-reg-1")
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "agent-sub"))
		rr = httptest.NewRecorder()
		s.handleReplaceAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))

		// 10. Admin deletes → 204 (or 202 if finalizers)
		req = httptest.NewRequest("DELETE", "/agent-registrations/test-reg-1", nil)
		req.SetPathValue("name", "test-reg-1")
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "admin-sub"))
		rr = httptest.NewRecorder()
		s.handleDeleteAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Or(gomega.Equal(http.StatusNoContent), gomega.Equal(http.StatusAccepted)))

		// 11. Get after delete → 404
		req = httptest.NewRequest("GET", "/agent-registrations/test-reg-1", nil)
		req.SetPathValue("name", "test-reg-1")
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "admin-sub"))
		rr = httptest.NewRecorder()
		s.handleGetAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusNotFound))

		// 12. POST with missing agentIdentity → 400
		badReg := v1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bad-reg",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRegistrationSpec{
				AgentIdentity: "",
			},
		}
		jsonBad, _ := json.Marshal(badReg)
		req = httptest.NewRequest("POST", "/agent-registrations", bytes.NewBuffer(jsonBad))
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "admin-sub"))
		rr = httptest.NewRecorder()
		s.handleCreateAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusBadRequest))

		// 13. POST duplicate name → 409
		dupReg := v1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-reg-dup",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRegistrationSpec{
				AgentIdentity: "agent-sub",
			},
		}
		jsonDup, _ := json.Marshal(dupReg)
		req = httptest.NewRequest("POST", "/agent-registrations", bytes.NewBuffer(jsonDup))
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "admin-sub"))
		rr = httptest.NewRecorder()
		s.handleCreateAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

		req = httptest.NewRequest("POST", "/agent-registrations", bytes.NewBuffer(jsonDup))
		req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "admin-sub"))
		rr = httptest.NewRecorder()
		s.handleCreateAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusConflict))

		// Cleanup duplicate
		_ = directClient.Delete(ctx, &dupReg)

		// --- Token tests ---

		// Create a Secret
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-token-secret",
				Namespace: testDefaultNS,
			},
			Data: map[string][]byte{
				"token": []byte("test-brokered-token-value"),
			},
		}
		gm.Expect(directClient.Create(ctx, secret)).To(gomega.Succeed())
		defer func() { _ = directClient.Delete(ctx, secret) }()

		// Create AgentRegistration
		regToken := &v1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-agent-token-reg",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRegistrationSpec{
				AgentIdentity: "agent-sub",
				ExternalIdentities: []v1alpha1.ExternalIdentityBinding{
					{
						Service: "github-mcp",
						Type:    v1alpha1.ExternalIdentityStaticSecret,
						StaticSecret: &v1alpha1.StaticSecretCredential{
							Name:      "test-token-secret",
							Namespace: testDefaultNS,
							Key:       "token",
						},
					},
				},
			},
		}
		gm.Expect(directClient.Create(ctx, regToken)).To(gomega.Succeed())
		defer func() { _ = directClient.Delete(ctx, regToken) }()

		// Create approved AgentRequest for agent-sub
		reqApproved := &v1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-req-approved",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRequestSpec{
				AgentIdentity: "agent-sub",
				Action:        "test",
				Reason:        "testing",
				Target:        v1alpha1.Target{URI: "k8s://prod/default/deployment/test"},
			},
		}
		gm.Expect(directClient.Create(ctx, reqApproved)).To(gomega.Succeed())
		defer func() { _ = directClient.Delete(ctx, reqApproved) }()

		// Wait for the controller to naturally transition reqApproved to PhaseApproved
		gm.Eventually(func() string {
			var current v1alpha1.AgentRequest
			key := types.NamespacedName{Name: "test-req-approved", Namespace: testDefaultNS}
			if err := directClient.Get(ctx, key, &current); err == nil {
				return current.Status.Phase
			}
			return ""
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseApproved))

		// Create a SafetyPolicy that requires approval for reqPending
		policyPending := createApprovalPolicy(
			ctx, gm, directClient, "pending-policy", "k8s://prod/default/deployment/pending-target")
		defer func() { _ = directClient.Delete(ctx, policyPending) }()

		// Create pending AgentRequest for agent-sub
		reqPending := &v1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-req-pending",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRequestSpec{
				AgentIdentity: "agent-sub",
				Action:        "test",
				Reason:        "testing",
				Target:        v1alpha1.Target{URI: "k8s://prod/default/deployment/pending-target"},
			},
		}
		gm.Expect(directClient.Create(ctx, reqPending)).To(gomega.Succeed())
		defer func() { _ = directClient.Delete(ctx, reqPending) }()

		// Wait for the controller to transition reqPending to PhasePending with RequiresApproval condition
		gm.Eventually(func() bool {
			var current v1alpha1.AgentRequest
			key := types.NamespacedName{Name: "test-req-pending", Namespace: testDefaultNS}
			if err := directClient.Get(ctx, key, &current); err == nil {
				return current.Status.Phase == v1alpha1.PhasePending &&
					meta.IsStatusConditionTrue(current.Status.Conditions, v1alpha1.ConditionRequiresApproval)
			}
			return false
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.BeTrue())

		// Simulate the watch loop populating the cache (tests bypass watchAgentRegistrations).
		regCache.upsert(regToken)

		// 1. Agent owns approved AgentRequest + has AgentRegistration with StaticSecret binding
		//    → POST /agent-requests/{name}/token with service=<bound-service>
		//    → 200, token == the StaticSecret value
		{
			body := struct {
				Service string `json:"service"`
			}{
				Service: "github-mcp",
			}
			jsonBodyToken, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/agent-requests/test-req-approved/token", bytes.NewBuffer(jsonBodyToken))
			req.SetPathValue("name", "test-req-approved")
			req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "agent-sub"))
			rr := httptest.NewRecorder()
			s.handleGetAgentRequestToken(rr, req)
			gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))

			var resp tokenResponse
			gm.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
			gm.Expect(resp.Token).To(gomega.Equal("test-brokered-token-value"))
			gm.Expect(resp.Service).To(gomega.Equal("github-mcp"))
		}

		// 2. Agent requests token but AgentRequest is in Pending state → 409
		{
			body := struct {
				Service string `json:"service"`
			}{
				Service: "github-mcp",
			}
			jsonBodyToken, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/agent-requests/test-req-pending/token", bytes.NewBuffer(jsonBodyToken))
			req.SetPathValue("name", "test-req-pending")
			req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "agent-sub"))
			rr := httptest.NewRecorder()
			s.handleGetAgentRequestToken(rr, req)
			gm.Expect(rr.Code).To(gomega.Equal(http.StatusConflict))
		}

		// 3. Agent-A tries to get token for Agent-B's AgentRequest → 403
		{
			body := struct {
				Service string `json:"service"`
			}{
				Service: "github-mcp",
			}
			jsonBodyToken, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/agent-requests/test-req-approved/token", bytes.NewBuffer(jsonBodyToken))
			req.SetPathValue("name", "test-req-approved")
			req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "agent-another"))
			rr := httptest.NewRecorder()
			s.handleGetAgentRequestToken(rr, req)
			gm.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))
		}

		// 4. Valid approved request but no AgentRegistration exists → 404
		{
			// Create approved AgentRequest for unregistered-agent-sub
			reqUnreg := &v1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-req-unreg",
					Namespace: testDefaultNS,
				},
				Spec: v1alpha1.AgentRequestSpec{
					AgentIdentity: "unregistered-agent-sub",
					Action:        "test",
					Reason:        "testing",
					Target:        v1alpha1.Target{URI: "k8s://prod/default/deployment/unreg"},
				},
			}
			gm.Expect(directClient.Create(ctx, reqUnreg)).To(gomega.Succeed())
			defer func() { _ = directClient.Delete(ctx, reqUnreg) }()

			// Wait for the controller to transition reqUnreg to PhaseApproved
			gm.Eventually(func() string {
				var current v1alpha1.AgentRequest
				key := types.NamespacedName{Name: "test-req-unreg", Namespace: testDefaultNS}
				if err := directClient.Get(ctx, key, &current); err == nil {
					return current.Status.Phase
				}
				return ""
			}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseApproved))

			body := struct {
				Service string `json:"service"`
			}{
				Service: "github-mcp",
			}
			jsonBodyToken, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/agent-requests/test-req-unreg/token", bytes.NewBuffer(jsonBodyToken))
			req.SetPathValue("name", "test-req-unreg")
			req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "unregistered-agent-sub"))
			rr := httptest.NewRecorder()
			s.handleGetAgentRequestToken(rr, req)
			gm.Expect(rr.Code).To(gomega.Equal(http.StatusNotFound))
		}

		// 5. Valid approved request + registration exists but no binding for that service → 404
		{
			body := struct {
				Service string `json:"service"`
			}{
				Service: "non-existent-service",
			}
			jsonBodyToken, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/agent-requests/test-req-approved/token", bytes.NewBuffer(jsonBodyToken))
			req.SetPathValue("name", "test-req-approved")
			req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "agent-sub"))
			rr := httptest.NewRecorder()
			s.handleGetAgentRequestToken(rr, req)
			gm.Expect(rr.Code).To(gomega.Equal(http.StatusNotFound))
		}

		// 6. Non-agent role (reviewer) calls token endpoint → 403
		{
			body := struct {
				Service string `json:"service"`
			}{
				Service: "github-mcp",
			}
			jsonBodyToken, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/agent-requests/test-req-approved/token", bytes.NewBuffer(jsonBodyToken))
			req.SetPathValue("name", "test-req-approved")
			req = req.WithContext(withCallerSub(withCallerGroups(req.Context(), []string{}), "reviewer-sub"))
			rr := httptest.NewRecorder()
			s.handleGetAgentRequestToken(rr, req)
			gm.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))
		}
	})
}
