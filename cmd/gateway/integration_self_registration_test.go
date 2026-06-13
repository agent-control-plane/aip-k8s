package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func runSelfRegistrationTests(t *testing.T, directClient client.Client, ctx context.Context) {
	t.Run("self-register happy path", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		sub := "self-agent"
		issuer := "https://issuer.example.com"
		ss := &Server{
			client:       directClient,
			apiReader:    directClient,
			roles:        newRoleConfig(sub, "", "", "", "", ""),
			authRequired: true,
		}
		body := selfRegisterRequest{
			RequestedServices: []string{"svc1"},
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).To(gomega.Succeed())
		req := httptest.NewRequest("POST", "/agent-registrations/self", bytes.NewBuffer(jsonBody))
		reqCtx := withCallerSub(context.Background(), sub)
		reqCtx = withCallerGroups(reqCtx, []string{})
		reqCtx = withCallerIssuer(reqCtx, issuer)
		req = req.WithContext(reqCtx)
		rr := httptest.NewRecorder()
		ss.handleSelfRegisterAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

		var created v1alpha1.AgentRegistration
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &created)).To(gomega.Succeed())
		gm.Expect(created.Name).To(gomega.Equal(v1alpha1.RegistrationObjectName(sub)))
		gm.Expect(created.Spec.AgentIdentity).To(gomega.Equal(sub))
		gm.Expect(created.Spec.OIDC).ToNot(gomega.BeNil())
		gm.Expect(created.Spec.OIDC.Issuer).To(gomega.Equal(issuer))
		gm.Expect(created.Spec.Mode).To(gomega.Equal(v1alpha1.AgentRegistrationModeStanding))
		gm.Expect(created.Spec.RequestedServices).To(gomega.ConsistOf("svc1"))

		gm.Expect(directClient.Delete(ctx, &created)).To(gomega.Succeed())
	})

	t.Run("self-register duplicate -> 409", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		sub := "dup-agent"
		issuer := "https://issuer.example.com"
		ss := &Server{
			client:       directClient,
			apiReader:    directClient,
			roles:        newRoleConfig(sub, "", "", "", "", ""),
			authRequired: true,
		}
		body := selfRegisterRequest{}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).To(gomega.Succeed())
		req := httptest.NewRequest("POST", "/agent-registrations/self", bytes.NewBuffer(jsonBody))
		reqCtx := withCallerSub(context.Background(), sub)
		reqCtx = withCallerGroups(reqCtx, []string{})
		reqCtx = withCallerIssuer(reqCtx, issuer)
		req = req.WithContext(reqCtx)
		rr := httptest.NewRecorder()
		ss.handleSelfRegisterAgentRegistration(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

		var created v1alpha1.AgentRegistration
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &created)).To(gomega.Succeed())
		defer func() {
			if delErr := directClient.Delete(ctx, &created); delErr != nil {
				t.Logf("failed to delete test registration: %v", delErr)
			}
		}()

		// Second registration with same sub hits AlreadyExists -> 409
		req2 := httptest.NewRequest("POST", "/agent-registrations/self", bytes.NewBuffer(jsonBody))
		reqCtx2 := withCallerSub(context.Background(), sub)
		reqCtx2 = withCallerGroups(reqCtx2, []string{})
		reqCtx2 = withCallerIssuer(reqCtx2, issuer)
		req2 = req2.WithContext(reqCtx2)
		rr2 := httptest.NewRecorder()
		ss.handleSelfRegisterAgentRegistration(rr2, req2)
		gm.Expect(rr2.Code).To(gomega.Equal(http.StatusConflict))
	})

	t.Run("issuer mismatch on AgentRequest -> 403", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		reg := &v1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "issuer-test-reg",
				Namespace: testDefaultNS,
			},
			Spec: v1alpha1.AgentRegistrationSpec{
				AgentIdentity: "check-agent",
				OIDC: &v1alpha1.AgentRegistrationOIDC{
					Issuer:          "https://real-issuer.example.com",
					AllowedSubjects: []string{"check-agent"},
				},
			},
		}
		gm.Expect(directClient.Create(ctx, reg)).To(gomega.Succeed())
		defer func() {
			if delErr := directClient.Delete(ctx, reg); delErr != nil {
				t.Logf("failed to delete test registration: %v", delErr)
			}
		}()

		regCache2 := newRegistrationCache(directClient)
		regCache2.upsert(reg)
		ss := &Server{
			client:                  directClient,
			apiReader:               directClient,
			dedupWindow:             0,
			waitTimeout:             serverWaitTimeout,
			roles:                   newRoleConfig("check-agent", "", "", "", "", ""),
			authRequired:            true,
			regCache:                regCache2,
			unregisteredAgentPolicy: "strict",
		}

		body := createAgentRequestBody{
			AgentIdentity: "check-agent",
			Action:        "test",
			TargetURI:     "k8s://test/ns/deploy/test",
			Reason:        "testing issuer validation",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).To(gomega.Succeed())
		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		reqCtx := withCallerSub(context.Background(), "check-agent")
		reqCtx = withCallerGroups(reqCtx, []string{})
		reqCtx = withCallerIssuer(reqCtx, "https://evil-issuer.example.com")
		req = req.WithContext(reqCtx)
		rr := httptest.NewRecorder()
		ss.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusForbidden))
		gm.Expect(rr.Body.String()).To(gomega.ContainSubstring("IDENTITY_MISMATCH"))
	})
}
