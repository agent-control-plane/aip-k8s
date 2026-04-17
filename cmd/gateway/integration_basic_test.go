package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func runBasicTests(t *testing.T, mgrClient, directClient client.Client, ctx context.Context) {
	t.Run("Full lifecycle - Pending to Approved", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			dedupWindow:  0,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		body := createAgentRequestBody{
			AgentIdentity: "agent-1",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/full-lifecycle",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		// jsonBody, _ := json.Marshal(body) - handled below
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
		gm.Expect(respBody["phase"]).To(gomega.Equal(string(v1alpha1.PhaseApproved)))

		cleanup(ctx, gm, directClient)
	})

	t.Run("Idempotent duplicate - returns 200 immediately", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			dedupWindow:  24 * time.Hour,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		const targetURI = "k8s://prod/default/deployment/dup-test"
		policy := createApprovalPolicy(ctx, gm, directClient, "dup-test-policy", targetURI)

		ar := &v1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dup-test",
				Namespace: testDefaultNS,
				Labels:    map[string]string{"aip.io/agentIdentity": "agent-dup"},
			},
			Spec: v1alpha1.AgentRequestSpec{
				AgentIdentity: "agent-dup",
				Action:        "restart",
				Target:        v1alpha1.Target{URI: targetURI},
				Reason:        "test",
			},
		}
		gm.Expect(directClient.Create(ctx, ar)).To(gomega.Succeed())

		gm.Eventually(func() string {
			var current v1alpha1.AgentRequest
			key := types.NamespacedName{Name: "dup-test", Namespace: testDefaultNS}
			if err := mgrClient.Get(ctx, key, &current); err != nil {
				return ""
			}
			return current.Status.Phase
		}, eventuallyTimeout).Should(gomega.Equal(v1alpha1.PhasePending))

		body := createAgentRequestBody{
			AgentIdentity: "agent-dup",
			Action:        "restart",
			TargetURI:     targetURI,
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).NotTo(gomega.HaveOccurred(), "body is a known serializable struct")

		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		rr := httptest.NewRecorder()

		s.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))

		var respBody map[string]any
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &respBody)).To(gomega.Succeed())
		gm.Expect(respBody["phase"]).To(gomega.Equal(string(v1alpha1.PhasePending)))

		gm.Expect(directClient.Delete(ctx, policy)).To(gomega.Succeed())
		cleanup(ctx, gm, directClient)
	})

	t.Run("Dedup window expired - new request allowed", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			dedupWindow:  100 * time.Millisecond,
			waitTimeout:  1 * time.Second,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		body := createAgentRequestBody{
			AgentIdentity: "agent-old",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/dedup-expired",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).NotTo(gomega.HaveOccurred(), "body is a known serializable struct")

		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		rr := httptest.NewRecorder()

		s.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusGatewayTimeout))

		time.Sleep(200 * time.Millisecond)

		rr2 := httptest.NewRecorder()
		req2 := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		s.handleCreateAgentRequest(rr2, req2)
		gm.Expect(rr2.Code).To(gomega.Equal(http.StatusGatewayTimeout))

		cleanup(ctx, gm, directClient)
	})

	t.Run("Poll loop timeout - returns 504", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			dedupWindow:  0,
			waitTimeout:  500 * time.Millisecond,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		body := createAgentRequestBody{
			AgentIdentity: "agent-timeout",
			Action:        "restart",
			TargetURI:     "k8s://prod/default/deployment/timeout",
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, err := json.Marshal(body)
		gm.Expect(err).NotTo(gomega.HaveOccurred(), "body is a known serializable struct")

		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		rr := httptest.NewRecorder()

		s.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusGatewayTimeout))

		cleanup(ctx, gm, directClient)
	})
}
