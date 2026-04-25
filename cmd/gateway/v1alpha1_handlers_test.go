package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func TestV1alpha1CreateAgentDiagnostic_Idempotent(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServer()
	s.dedupWindow = 5 * time.Minute

	body := `{"agentIdentity":"agent-sub","diagnosticType":"observation","correlationID":"corr-v1a1","summary":"v1alpha1 test"}`

	req1 := httptest.NewRequest(http.MethodPost, "/v1alpha1/agent-diagnostics", strings.NewReader(body))
	req1 = req1.WithContext(withCallerSub(req1.Context(), "agent-sub"))
	w1 := httptest.NewRecorder()
	s.v1alpha1CreateAgentDiagnostic(w1, req1)
	g.Expect(w1.Code).To(gomega.Equal(http.StatusCreated))

	var dto map[string]any
	g.Expect(json.Unmarshal(w1.Body.Bytes(), &dto)).To(gomega.Succeed())
	g.Expect(dto["agentIdentity"]).To(gomega.Equal("agent-sub"))
	g.Expect(dto["diagnosticType"]).To(gomega.Equal("observation"))
	g.Expect(dto["correlationID"]).To(gomega.Equal("corr-v1a1"))

	req2 := httptest.NewRequest(http.MethodPost, "/v1alpha1/agent-diagnostics", strings.NewReader(body))
	req2 = req2.WithContext(withCallerSub(req2.Context(), "agent-sub"))
	w2 := httptest.NewRecorder()
	s.v1alpha1CreateAgentDiagnostic(w2, req2)
	g.Expect(w2.Code).To(gomega.Equal(http.StatusOK))

	var list v1alpha1.AgentDiagnosticList
	g.Expect(s.client.List(context.Background(), &list)).To(gomega.Succeed())
	g.Expect(list.Items).To(gomega.HaveLen(1))
}

func TestV1alpha1GetAgentDiagnostic_ReturnsDTO(t *testing.T) {
	g := gomega.NewWithT(t)

	diag := &v1alpha1.AgentDiagnostic{
		ObjectMeta: metav1.ObjectMeta{Name: "diag-get-test", Namespace: "default"},
		Spec: v1alpha1.AgentDiagnosticSpec{
			AgentIdentity:  "agent-sub",
			DiagnosticType: "signal",
			CorrelationID:  "corr-get-001",
			Summary:        "get test summary",
		},
	}
	s := newTestServer(diag)

	req := httptest.NewRequest(http.MethodGet, "/v1alpha1/agent-diagnostics/diag-get-test", nil)
	req.SetPathValue("name", "diag-get-test")
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.v1alpha1GetAgentDiagnostic(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	var dto map[string]any
	g.Expect(json.Unmarshal(w.Body.Bytes(), &dto)).To(gomega.Succeed())
	g.Expect(dto["agentIdentity"]).To(gomega.Equal("agent-sub"))
	g.Expect(dto["diagnosticType"]).To(gomega.Equal("signal"))
	g.Expect(dto["correlationID"]).To(gomega.Equal("corr-get-001"))
	g.Expect(dto["name"]).To(gomega.Equal("diag-get-test"))
}

func TestV1alpha1SetAgentDiagnosticVerdict_RoleAndVerdict(t *testing.T) {
	g := gomega.NewWithT(t)

	diag := &v1alpha1.AgentDiagnostic{
		ObjectMeta: metav1.ObjectMeta{Name: "diag-verdict-test", Namespace: "default"},
		Spec: v1alpha1.AgentDiagnosticSpec{
			AgentIdentity:  "agent-sub",
			DiagnosticType: "observation",
			CorrelationID:  "corr-verdict",
			Summary:        "verdict test",
		},
	}
	s := newTestServer(diag)

	// Non-reviewer gets 403 with problem+json content type.
	req := httptest.NewRequest(http.MethodPatch, "/v1alpha1/agent-diagnostics/diag-verdict-test/status",
		strings.NewReader(`{"verdict":"correct"}`))
	req.SetPathValue("name", "diag-verdict-test")
	req = req.WithContext(withCallerSub(req.Context(), "agent-sub"))
	w := httptest.NewRecorder()
	s.v1alpha1SetAgentDiagnosticVerdict(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Header().Get("Content-Type")).To(gomega.ContainSubstring("problem+json"))

	// Invalid verdict string returns 400.
	req = httptest.NewRequest(http.MethodPatch, "/v1alpha1/agent-diagnostics/diag-verdict-test/status",
		strings.NewReader(`{"verdict":"bogus"}`))
	req.SetPathValue("name", "diag-verdict-test")
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.v1alpha1SetAgentDiagnosticVerdict(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusBadRequest))

	// Reviewer with valid verdict returns 200 and "verdict saved".
	req = httptest.NewRequest(http.MethodPatch, "/v1alpha1/agent-diagnostics/diag-verdict-test/status",
		strings.NewReader(`{"verdict":"correct"}`))
	req.SetPathValue("name", "diag-verdict-test")
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.v1alpha1SetAgentDiagnosticVerdict(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	var resp map[string]any
	g.Expect(json.Unmarshal(w.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp["message"]).To(gomega.Equal("verdict saved"))
}

func TestV1alpha1RecomputeAccuracy_ReturnsOK(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServer()

	req := httptest.NewRequest(http.MethodPost, "/v1alpha1/agent-diagnostics/recompute-accuracy", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.v1alpha1RecomputeAccuracy(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	var resp map[string]any
	g.Expect(json.Unmarshal(w.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp["message"]).To(gomega.Equal("recomputed accuracy summaries"))
}
