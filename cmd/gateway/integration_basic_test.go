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

func runRequestLifecycleTests(t *testing.T, mgrClient, directClient client.Client, ctx context.Context) {
	t.Run("Full lifecycle - Pending to Approved", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       directClient,
			apiReader:    directClient,
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

	t.Run("Idempotent duplicate - deterministic name - returns 200 immediately", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  24 * time.Hour,
			waitTimeout:  200 * time.Millisecond, // fail fast — no controller in envtest to transition phases
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		const targetURI = "k8s://prod/default/deployment/dup-test"
		policy := createApprovalPolicy(ctx, gm, directClient, "dup-test-policy", targetURI)
		defer func() { gm.Expect(directClient.Delete(ctx, policy)).To(gomega.Succeed()) }()

		body := createAgentRequestBody{
			AgentIdentity: "agent-dup",
			Action:        "restart",
			TargetURI:     targetURI,
			Reason:        "test",
			Namespace:     testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)

		// First call: deterministic name, no controller to advance → 504
		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		rr := httptest.NewRecorder()
		s.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusGatewayTimeout))

		// Extract the name from the first request's response.
		// The 504 body from pollAgentRequestPhase does not include name, so
		// compute it deterministically to locate the created request.
		dedupKey := computeDedupKey(body.AgentIdentity, body.Action, body.TargetURI, "", "")
		firstName := deterministicRequestName(body.AgentIdentity, dedupKey, s.dedupWindow, time.Now())

		// Second call: same body, same dedupWindow → AlreadyExists → HTTP 200
		req2 := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		rr2 := httptest.NewRecorder()
		s.handleCreateAgentRequest(rr2, req2)
		gm.Expect(rr2.Code).To(gomega.Equal(http.StatusOK))
		var secondResp map[string]any
		gm.Expect(json.Unmarshal(rr2.Body.Bytes(), &secondResp)).To(gomega.Succeed())
		gm.Expect(secondResp["name"]).To(gomega.Equal(firstName))

		cleanup(ctx, gm, directClient)
	})

	t.Run("Poll loop timeout - returns 504", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
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

	t.Run("Different classification on same resource is not deduped", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  24 * time.Hour,
			waitTimeout:  200 * time.Millisecond,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		const targetURI = "k8s://prod/default/deployment/class-dedup"
		policy := createApprovalPolicy(ctx, gm, directClient, "class-dedup-policy", targetURI)
		defer func() { gm.Expect(directClient.Delete(ctx, policy)).To(gomega.Succeed()) }()

		now := time.Now()
		// Simulate what the handler does internally — compute dedupKey from normalized classification
		norm1 := normalizeClassification("nodepool/at-capacity")
		dedup1 := computeDedupKey("class-agent", "scale", targetURI, norm1, "")
		name1 := deterministicRequestName("class-agent", dedup1, s.dedupWindow, now)
		norm2 := normalizeClassification("nodepool/affinity-mismatch")
		dedup2 := computeDedupKey("class-agent", "scale", targetURI, norm2, "")
		name2 := deterministicRequestName("class-agent", dedup2, s.dedupWindow, now)

		// Different classification → different dedup keys → different names
		gm.Expect(dedup1).NotTo(gomega.Equal(dedup2))
		gm.Expect(name1).NotTo(gomega.Equal(name2))

		cleanup(ctx, gm, directClient)
	})

	t.Run("Explicit dedupKey overrides computed key", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,
			apiReader:    mgrClient,
			dedupWindow:  24 * time.Hour,
			waitTimeout:  200 * time.Millisecond,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		const targetURI = "k8s://prod/default/deployment/explicit-dedup"
		policy := createApprovalPolicy(ctx, gm, directClient, "explicit-dedup-policy", targetURI)
		defer func() { gm.Expect(directClient.Delete(ctx, policy)).To(gomega.Succeed()) }()

		now := time.Now()
		// Same explicit dedupKey, same window → same name
		dedup := computeDedupKey("dedup-agent", "update", targetURI, "anything", "my-fixed-key")
		nameA := deterministicRequestName("dedup-agent", dedup, s.dedupWindow, now)
		nameB := deterministicRequestName("dedup-agent", dedup, s.dedupWindow, now.Add(1*time.Hour))
		gm.Expect(nameA).To(gomega.Equal(nameB))

		// Different window → different name (same explicit key)
		nameC := deterministicRequestName("dedup-agent", dedup, s.dedupWindow, now.Add(48*time.Hour))
		gm.Expect(nameC).NotTo(gomega.Equal(nameA))

		cleanup(ctx, gm, directClient)
	})
}

func runSoakModeAndVerdictTests(t *testing.T, mgrClient, directClient client.Client, ctx context.Context) {
	t.Run("SoakMode routes to AwaitingVerdict", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       directClient,
			apiReader:    directClient,
			dedupWindow:  0,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		gr := &v1alpha1.GovernedResource{
			ObjectMeta: metav1.ObjectMeta{Name: "soak-gr"},
			Spec: v1alpha1.GovernedResourceSpec{
				URIPattern:       "k8s://soak/*",
				PermittedActions: []string{"test"},
				ContextFetcher:   "none",
				SoakMode:         true,
			},
		}
		gm.Expect(directClient.Create(ctx, gr)).To(gomega.Succeed())

		// Wait for GR to be in mgrClient cache so reconciler sees SoakMode
		gm.Eventually(func() error {
			var checkGR v1alpha1.GovernedResource
			return mgrClient.Get(ctx, types.NamespacedName{Name: "soak-gr"}, &checkGR)
		}, eventuallyTimeout).Should(gomega.Succeed())

		body := createAgentRequestBody{
			AgentIdentity: "agent-soak",
			Action:        "test",
			TargetURI:     "k8s://soak/resource",
			Reason:        "soak test",
			Namespace:     testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		rr := httptest.NewRecorder()

		s.handleCreateAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusCreated))

		var respBody map[string]any
		gm.Expect(json.Unmarshal(rr.Body.Bytes(), &respBody)).To(gomega.Succeed())
		gm.Expect(respBody["phase"]).To(gomega.Equal(string(v1alpha1.PhaseAwaitingVerdict)))

		gm.Expect(directClient.Delete(ctx, gr)).To(gomega.Succeed())
		cleanup(ctx, gm, directClient)
	})

	t.Run("Verdict endpoint succeeds for AwaitingVerdict", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       directClient,
			apiReader:    directClient,
			roles:        newRoleConfig("", testReviewerSub, "", "", "", ""),
			authRequired: false,
		}

		// Create a SoakMode GovernedResource so the reconciler routes this AR to AwaitingVerdict.
		gr := &v1alpha1.GovernedResource{
			ObjectMeta: metav1.ObjectMeta{Name: "verdict-gr"},
			Spec: v1alpha1.GovernedResourceSpec{
				URIPattern:       "k8s://verdict/*",
				PermittedActions: []string{"test"},
				ContextFetcher:   "none",
				SoakMode:         true,
			},
		}
		gm.Expect(directClient.Create(ctx, gr)).To(gomega.Succeed())

		// Wait for GR in manager cache so the reconciler can look it up.
		gm.Eventually(func() error {
			var check v1alpha1.GovernedResource
			return mgrClient.Get(ctx, types.NamespacedName{Name: "verdict-gr"}, &check)
		}, eventuallyTimeout).Should(gomega.Succeed())

		ar := &v1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "verdict-test", Namespace: testDefaultNS},
			Spec: v1alpha1.AgentRequestSpec{
				AgentIdentity: "agent-1",
				Action:        "test",
				Target:        v1alpha1.Target{URI: "k8s://verdict/resource"},
				Reason:        "test",
				GovernedResourceRef: &v1alpha1.GovernedResourceRef{
					Name:       gr.Name,
					Generation: gr.Generation,
				},
			},
		}
		gm.Expect(directClient.Create(ctx, ar)).To(gomega.Succeed())

		// Wait for reconciler to set AwaitingVerdict via SoakMode — no manual status patch needed.
		gm.Eventually(func() string {
			var check v1alpha1.AgentRequest
			_ = directClient.Get(ctx, types.NamespacedName{Name: "verdict-test", Namespace: testDefaultNS}, &check)
			return check.Status.Phase
		}, eventuallyTimeout).Should(gomega.Equal(v1alpha1.PhaseAwaitingVerdict))

		verdictBody := map[string]string{
			"verdict":    "correct",
			"reasonCode": "",
			"note":       "good job",
		}
		jsonBody, _ := json.Marshal(verdictBody)
		req := httptest.NewRequest("PATCH", "/agent-requests/verdict-test/verdict", bytes.NewBuffer(jsonBody))
		req.SetPathValue("name", "verdict-test")
		req = req.WithContext(context.WithValue(req.Context(), callerSubKey, testReviewerSub))
		rr := httptest.NewRecorder()

		s.handleVerdictAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusOK))

		var updated v1alpha1.AgentRequest
		gm.Eventually(func() string {
			_ = directClient.Get(ctx, types.NamespacedName{Name: "verdict-test", Namespace: testDefaultNS}, &updated)
			return updated.Status.Phase
		}, eventuallyTimeout).Should(gomega.Equal(v1alpha1.PhaseCompleted))
		gm.Expect(updated.Status.Verdict).To(gomega.Equal("correct"))
		gm.Expect(updated.Status.VerdictBy).To(gomega.Equal(testReviewerSub))

		gm.Expect(directClient.Delete(ctx, gr)).To(gomega.Succeed())
		cleanup(ctx, gm, directClient)
	})

	t.Run("Verdict endpoint fails for wrong phase", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       directClient,
			apiReader:    directClient,
			roles:        newRoleConfig("", testReviewerSub, "", "", "", ""),
			authRequired: false,
		}

		name := "verdict-fail-phase"
		ar := &v1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testDefaultNS},
			Spec: v1alpha1.AgentRequestSpec{
				AgentIdentity: "agent-1",
				Action:        "test",
				Target:        v1alpha1.Target{URI: "k8s://test"},
				Reason:        "test",
			},
		}
		gm.Expect(directClient.Create(ctx, ar)).To(gomega.Succeed())

		// Wait for reconciler to set any non-AwaitingVerdict phase (no SoakMode GovernedResource,
		// so Pending, Approved, or beyond — all invalid for verdict submission).
		gm.Eventually(func() string {
			var check v1alpha1.AgentRequest
			_ = directClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testDefaultNS}, &check)
			return check.Status.Phase
		}, eventuallyTimeout).Should(gomega.And(
			gomega.Not(gomega.BeEmpty()),
			gomega.Not(gomega.Equal(v1alpha1.PhaseAwaitingVerdict)),
		))

		verdictBody := map[string]string{"verdict": "correct"}
		jsonBody, _ := json.Marshal(verdictBody)
		req := httptest.NewRequest("PATCH", "/agent-requests/"+name+"/verdict", bytes.NewBuffer(jsonBody))
		req.SetPathValue("name", name)
		req = req.WithContext(context.WithValue(req.Context(), callerSubKey, testReviewerSub))

		rr := httptest.NewRecorder()

		s.handleVerdictAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusConflict))

		cleanup(ctx, gm, directClient)
	})

	t.Run("Verdict endpoint requires reasonCode for incorrect", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       directClient,
			apiReader:    directClient,
			roles:        newRoleConfig("", testReviewerSub, "", "", "", ""),
			authRequired: false,
		}

		name := "any"
		verdictBody := map[string]string{"verdict": "incorrect"}
		jsonBody, _ := json.Marshal(verdictBody)
		req := httptest.NewRequest("PATCH", "/agent-requests/"+name+"/verdict", bytes.NewBuffer(jsonBody))
		req.SetPathValue("name", name)
		req = req.WithContext(context.WithValue(req.Context(), callerSubKey, testReviewerSub))

		rr := httptest.NewRecorder()

		s.handleVerdictAgentRequest(rr, req)
		gm.Expect(rr.Code).To(gomega.Equal(http.StatusBadRequest))
	})

}
