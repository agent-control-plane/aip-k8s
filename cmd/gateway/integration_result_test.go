package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func runResultTests(t *testing.T, mgrClient, directClient client.Client, ctx context.Context) {
	t.Run("PUT /result -> POST /completed -> status.result survives on completed object", func(t *testing.T) {
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
			AgentIdentity: "result-agent",
			Action:        "test",
			TargetURI:     "k8s://prod/default/deployment/result-test",
			Reason:        "test result lifecycle",
			Namespace:     testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)
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

		var createResp map[string]any
		gm.Expect(json.Unmarshal(resp.Body.Bytes(), &createResp)).To(gomega.Succeed())
		reqName := createResp["name"].(string)

		// Wait for PhaseApproved before advancing to Executing.
		key := types.NamespacedName{Name: reqName, Namespace: testDefaultNS}
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseApproved))

		// Advance to Executing via POST /executing
		execReq := httptest.NewRequest("POST", "/agent-requests/"+reqName+"/executing", nil)
		execReq.SetPathValue("name", reqName)
		execReq = execReq.WithContext(withCallerSub(execReq.Context(), "result-agent"))
		execRR := httptest.NewRecorder()
		s.handleExecutingAgentRequest(execRR, execReq)
		gm.Expect(execRR.Code).To(gomega.Equal(http.StatusOK))

		// Wait for PhaseExecuting before PUT /result.
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, types.NamespacedName{Name: reqName, Namespace: testDefaultNS}, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseExecuting))

		// PUT /result
		resultBody := `{"url":"https://github.com/org/repo/pull/1","summary":"PR #1"}`
		putReq := httptest.NewRequest("PUT", "/agent-requests/"+reqName+"/result", bytes.NewBufferString(resultBody))
		putReq.SetPathValue("name", reqName)
		putReq = putReq.WithContext(withCallerSub(putReq.Context(), "result-agent"))
		putRR := httptest.NewRecorder()
		s.handlePutAgentRequestResult(putRR, putReq)
		gm.Expect(putRR.Code).To(gomega.Equal(http.StatusOK))

		// POST /completed
		compReq := httptest.NewRequest("POST", "/agent-requests/"+reqName+"/completed", nil)
		compReq.SetPathValue("name", reqName)
		compReq = compReq.WithContext(withCallerSub(compReq.Context(), "result-agent"))
		compRR := httptest.NewRecorder()
		s.handleCompletedAgentRequest(compRR, compReq)
		gm.Expect(compRR.Code).To(gomega.Equal(http.StatusOK))

		// Eventually verify PhaseCompleted and Status.Result preserved.
		key = types.NamespacedName{Name: reqName, Namespace: testDefaultNS}
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseCompleted))

		var finalAR v1alpha1.AgentRequest
		gm.Expect(directClient.Get(ctx, key, &finalAR)).To(gomega.Succeed())
		gm.Expect(finalAR.Status.Result).NotTo(gomega.BeNil())
		gm.Expect(finalAR.Status.Result.URL).To(gomega.Equal("https://github.com/org/repo/pull/1"))
		gm.Expect(finalAR.Status.Result.Summary).To(gomega.Equal("PR #1"))

		// GET endpoint includes result
		getReq := httptest.NewRequest("GET", "/agent-requests/"+reqName, nil)
		getReq.SetPathValue("name", reqName)
		getRR := httptest.NewRecorder()
		s.handleGetAgentRequest(getRR, getReq)
		gm.Expect(getRR.Code).To(gomega.Equal(http.StatusOK))
		var getResp map[string]any
		gm.Expect(json.Unmarshal(getRR.Body.Bytes(), &getResp)).To(gomega.Succeed())
		gm.Expect(getResp).To(gomega.HaveKey("result"))
		res, _ := getResp["result"].(map[string]any)
		gm.Expect(res).NotTo(gomega.BeNil())
		gm.Expect(res["url"]).To(gomega.Equal("https://github.com/org/repo/pull/1"))
		gm.Expect(res["summary"]).To(gomega.Equal("PR #1"))

		// AuditRecord has details with result URL
		gm.Eventually(func() bool {
			var list v1alpha1.AuditRecordList
			if err := directClient.List(ctx, &list, client.InNamespace(testDefaultNS)); err != nil {
				return false
			}
			for _, a := range list.Items {
				if a.Spec.Event == v1alpha1.AuditEventRequestCompleted && a.Spec.Details != nil {
					var details map[string]any
					if err := json.Unmarshal(a.Spec.Details.Raw, &details); err != nil {
						continue
					}
					if u, _ := details["url"].(string); u == "https://github.com/org/repo/pull/1" {
						return true
					}
				}
			}
			return false
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.BeTrue())

		cleanup(ctx, gm, directClient)
	})

	t.Run("PUT /result by non-owner agent returns 403", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       directClient,
			apiReader:    directClient,
			dedupWindow:  0,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig(testAgentSub, testReviewerSub, "", "", "", ""),
			authRequired: true,
		}

		body := createAgentRequestBody{
			AgentIdentity: testAgentSub,
			Action:        "test",
			TargetURI:     "k8s://prod/default/deployment/result-owner",
			Reason:        "ownership test",
			Namespace:     testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/agent-requests", bytes.NewBuffer(jsonBody))
		req = req.WithContext(withCallerSub(req.Context(), testAgentSub))
		rr := httptest.NewRecorder()
		respCh := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			s.handleCreateAgentRequest(rr, req)
			respCh <- rr
		}()
		var resp *httptest.ResponseRecorder
		gm.Eventually(respCh, eventuallyLongTimeout).Should(gomega.Receive(&resp))
		gm.Expect(resp.Code).To(gomega.Equal(http.StatusCreated))

		var createResp map[string]any
		gm.Expect(json.Unmarshal(resp.Body.Bytes(), &createResp)).To(gomega.Succeed())
		reqName := createResp["name"].(string)

		// Wait for phase Approved, then advance to Executing.
		key := types.NamespacedName{Name: reqName, Namespace: testDefaultNS}
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseApproved))

		execReq := httptest.NewRequest("POST", "/agent-requests/"+reqName+"/executing", nil)
		execReq.SetPathValue("name", reqName)
		execReq = execReq.WithContext(withCallerSub(execReq.Context(), testAgentSub))
		execRR := httptest.NewRecorder()
		s.handleExecutingAgentRequest(execRR, execReq)
		gm.Expect(execRR.Code).To(gomega.Equal(http.StatusOK))

		// PUT /result as rogue agent
		rogueBody := `{"url":"https://github.com/rogue/pr/1","summary":"rogue"}`
		rogueReq := httptest.NewRequest("PUT", "/agent-requests/"+reqName+"/result", bytes.NewBufferString(rogueBody))
		rogueReq.SetPathValue("name", reqName)
		rogueReq = rogueReq.WithContext(withCallerSub(rogueReq.Context(), "rogue-agent"))
		rogueRR := httptest.NewRecorder()
		s.handlePutAgentRequestResult(rogueRR, rogueReq)
		gm.Expect(rogueRR.Code).To(gomega.Equal(http.StatusForbidden))

		// Status.Result must still be nil.
		var ar v1alpha1.AgentRequest
		gm.Expect(directClient.Get(ctx, key, &ar)).To(gomega.Succeed())
		gm.Expect(ar.Status.Result).To(gomega.BeNil())

		cleanup(ctx, gm, directClient)
	})

	t.Run("PUT /result on non-Executing phase returns 409", func(t *testing.T) {
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
			AgentIdentity: "phase-guard-agent",
			Action:        "test",
			TargetURI:     "k8s://prod/default/deployment/result-phase",
			Reason:        "phase guard test",
			Namespace:     testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)
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

		var createResp map[string]any
		gm.Expect(json.Unmarshal(resp.Body.Bytes(), &createResp)).To(gomega.Succeed())
		reqName := createResp["name"].(string)

		// Wait for PhaseApproved, do NOT advance to Executing.
		key := types.NamespacedName{Name: reqName, Namespace: testDefaultNS}
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseApproved))

		// PUT /result on Approved phase should fail.
		resultBody := `{"url":"https://github.com/org/repo/pull/1","summary":"should fail"}`
		putReq := httptest.NewRequest("PUT", "/agent-requests/"+reqName+"/result", bytes.NewBufferString(resultBody))
		putReq.SetPathValue("name", reqName)
		putReq = putReq.WithContext(withCallerSub(putReq.Context(), "phase-guard-agent"))
		putRR := httptest.NewRecorder()
		s.handlePutAgentRequestResult(putRR, putReq)
		gm.Expect(putRR.Code).To(gomega.Equal(http.StatusConflict))

		var ar v1alpha1.AgentRequest
		gm.Expect(directClient.Get(ctx, key, &ar)).To(gomega.Succeed())
		gm.Expect(ar.Status.Result).To(gomega.BeNil())

		cleanup(ctx, gm, directClient)
	})

	t.Run("PUT /result is idempotent -- last write wins", func(t *testing.T) {
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
			AgentIdentity: "idempotent-agent",
			Action:        "test",
			TargetURI:     "k8s://prod/default/deployment/result-idempotent",
			Reason:        "idempotency test",
			Namespace:     testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)
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

		var createResp map[string]any
		gm.Expect(json.Unmarshal(resp.Body.Bytes(), &createResp)).To(gomega.Succeed())
		reqName := createResp["name"].(string)

		key := types.NamespacedName{Name: reqName, Namespace: testDefaultNS}
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseApproved))

		execReq := httptest.NewRequest("POST", "/agent-requests/"+reqName+"/executing", nil)
		execReq.SetPathValue("name", reqName)
		execReq = execReq.WithContext(withCallerSub(execReq.Context(), "idempotent-agent"))
		execRR := httptest.NewRecorder()
		s.handleExecutingAgentRequest(execRR, execReq)
		gm.Expect(execRR.Code).To(gomega.Equal(http.StatusOK))

		// Wait for PhaseExecuting before PUT /result.
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseExecuting))

		// First PUT
		put1 := `{"url":"https://github.com/org/repo/pull/1","summary":"PR #1"}`
		putReq1 := httptest.NewRequest("PUT", "/agent-requests/"+reqName+"/result", bytes.NewBufferString(put1))
		putReq1.SetPathValue("name", reqName)
		putReq1 = putReq1.WithContext(withCallerSub(putReq1.Context(), "idempotent-agent"))
		putRR1 := httptest.NewRecorder()
		s.handlePutAgentRequestResult(putRR1, putReq1)
		gm.Expect(putRR1.Code).To(gomega.Equal(http.StatusOK))

		// Second PUT overwrites
		put2 := `{"url":"https://github.com/org/repo/pull/2","summary":"PR #2"}`
		putReq2 := httptest.NewRequest("PUT", "/agent-requests/"+reqName+"/result", bytes.NewBufferString(put2))
		putReq2.SetPathValue("name", reqName)
		putReq2 = putReq2.WithContext(withCallerSub(putReq2.Context(), "idempotent-agent"))
		putRR2 := httptest.NewRecorder()
		s.handlePutAgentRequestResult(putRR2, putReq2)
		gm.Expect(putRR2.Code).To(gomega.Equal(http.StatusOK))

		var ar v1alpha1.AgentRequest
		gm.Expect(directClient.Get(ctx, key, &ar)).To(gomega.Succeed())
		gm.Expect(ar.Status.Result).NotTo(gomega.BeNil())
		gm.Expect(ar.Status.Result.URL).To(gomega.Equal("https://github.com/org/repo/pull/2"))
		gm.Expect(ar.Status.Result.Summary).To(gomega.Equal("PR #2"))

		cleanup(ctx, gm, directClient)
	})

	t.Run("PUT /result on a controller-completed request returns 409 (stale cache guard)", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		s := &Server{
			client:       mgrClient,    // cached (informer)
			apiReader:    directClient,  // direct (bypasses cache)
			dedupWindow:  0,
			waitTimeout:  serverWaitTimeout,
			roles:        newRoleConfig("", "", "", "", "", ""),
			authRequired: false,
		}

		body := createAgentRequestBody{
			AgentIdentity: "cache-guard-agent",
			Action:        "test",
			TargetURI:     "k8s://prod/default/deployment/result-cache-guard",
			Reason:        "cache guard test",
			Namespace:     testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)
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

		var createResp map[string]any
		gm.Expect(json.Unmarshal(resp.Body.Bytes(), &createResp)).To(gomega.Succeed())
		reqName := createResp["name"].(string)

		key := types.NamespacedName{Name: reqName, Namespace: testDefaultNS}
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseApproved))

		// Advance to Executing
		execReq := httptest.NewRequest("POST", "/agent-requests/"+reqName+"/executing", nil)
		execReq.SetPathValue("name", reqName)
		execReq = execReq.WithContext(withCallerSub(execReq.Context(), "cache-guard-agent"))
		execRR := httptest.NewRecorder()
		s.handleExecutingAgentRequest(execRR, execReq)
		gm.Expect(execRR.Code).To(gomega.Equal(http.StatusOK))

		// Wait for PhaseExecuting before POST /completed.
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseExecuting))

		// POST /completed -- controller transitions to PhaseCompleted
		compReq := httptest.NewRequest("POST", "/agent-requests/"+reqName+"/completed", nil)
		compReq.SetPathValue("name", reqName)
		compReq = compReq.WithContext(withCallerSub(compReq.Context(), "cache-guard-agent"))
		compRR := httptest.NewRecorder()
		s.handleCompletedAgentRequest(compRR, compReq)
		gm.Expect(compRR.Code).To(gomega.Equal(http.StatusOK))

		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseCompleted))

		// PUT /result should be rejected -- direct reader sees PhaseCompleted
		resultBody := `{"url":"https://github.com/org/repo/pull/1","summary":"too late"}`
		putReq := httptest.NewRequest("PUT", "/agent-requests/"+reqName+"/result", bytes.NewBufferString(resultBody))
		putReq.SetPathValue("name", reqName)
		putReq = putReq.WithContext(withCallerSub(putReq.Context(), "cache-guard-agent"))
		putRR := httptest.NewRecorder()
		s.handlePutAgentRequestResult(putRR, putReq)
		gm.Expect(putRR.Code).To(gomega.Equal(http.StatusConflict))

		var ar v1alpha1.AgentRequest
		gm.Expect(directClient.Get(ctx, key, &ar)).To(gomega.Succeed())
		gm.Expect(ar.Status.Result).To(gomega.BeNil())

		cleanup(ctx, gm, directClient)
	})

	t.Run("PUT /result with non-https URL returns 400", func(t *testing.T) {
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
			AgentIdentity: "url-validation-agent",
			Action:        "test",
			TargetURI:     "k8s://prod/default/deployment/result-url",
			Reason:        "url validation test",
			Namespace:     testDefaultNS,
		}
		jsonBody, _ := json.Marshal(body)
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

		var createResp map[string]any
		gm.Expect(json.Unmarshal(resp.Body.Bytes(), &createResp)).To(gomega.Succeed())
		reqName := createResp["name"].(string)

		key := types.NamespacedName{Name: reqName, Namespace: testDefaultNS}
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseApproved))

		execReq := httptest.NewRequest("POST", "/agent-requests/"+reqName+"/executing", nil)
		execReq.SetPathValue("name", reqName)
		execReq = execReq.WithContext(withCallerSub(execReq.Context(), "url-validation-agent"))
		execRR := httptest.NewRecorder()
		s.handleExecutingAgentRequest(execRR, execReq)
		gm.Expect(execRR.Code).To(gomega.Equal(http.StatusOK))

		// Wait for PhaseExecuting before PUT /result.
		gm.Eventually(func() string {
			var ar v1alpha1.AgentRequest
			if err := directClient.Get(ctx, key, &ar); err != nil {
				return ""
			}
			return ar.Status.Phase
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.Equal(v1alpha1.PhaseExecuting))

		for _, u := range []string{
			"http://not-https.example.com",
			"not-a-url-at-all",
			"https://",
		} {
			resultBody := `{"url":"` + u + `","summary":"bad"}`
			putReq := httptest.NewRequest("PUT", "/agent-requests/"+reqName+"/result", bytes.NewBufferString(resultBody))
			putReq.SetPathValue("name", reqName)
			putReq = putReq.WithContext(withCallerSub(putReq.Context(), "url-validation-agent"))
			putRR := httptest.NewRecorder()
			s.handlePutAgentRequestResult(putRR, putReq)
			gm.Expect(putRR.Code).To(gomega.Equal(http.StatusBadRequest))
		}

		var ar v1alpha1.AgentRequest
		gm.Expect(directClient.Get(ctx, key, &ar)).To(gomega.Succeed())
		gm.Expect(ar.Status.Result).To(gomega.BeNil())

		cleanup(ctx, gm, directClient)
	})
}
