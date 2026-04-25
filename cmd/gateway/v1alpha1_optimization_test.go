package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func TestV1alpha1ListAgentDiagnostics_FilteringAndPagination(t *testing.T) {
	g := gomega.NewWithT(t)

	objs := []client.Object{}
	for i := 1; i <= 10; i++ {
		diag := &v1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("v1a1-diag-%d", i),
				Namespace: "default",
				Labels: map[string]string{
					"aip.io/agentIdentity": "agent-1",
					"aip.io/correlationID": fmt.Sprintf("corr-%d", i),
				},
			},
			Spec: v1alpha1.AgentDiagnosticSpec{
				AgentIdentity: "agent-1",
				CorrelationID: fmt.Sprintf("corr-%d", i),
			},
		}
		objs = append(objs, diag)
	}
	s := newTestServer(objs...)

	// 1. Filter by agentIdentity+correlationID returns one item with flat DTO fields.
	req := httptest.NewRequest(http.MethodGet, "/v1alpha1/agent-diagnostics?agentIdentity=agent-1&correlationID=corr-5", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.v1alpha1ListAgentDiagnostics(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))

	var filtered struct {
		Items []map[string]any `json:"items"`
	}
	g.Expect(json.Unmarshal(w.Body.Bytes(), &filtered)).To(gomega.Succeed())
	g.Expect(filtered.Items).To(gomega.HaveLen(1))
	g.Expect(filtered.Items[0]["agentIdentity"]).To(gomega.Equal("agent-1"))
	g.Expect(filtered.Items[0]["correlationID"]).To(gomega.Equal("corr-5"))

	// 2. Without ?limit= the response is always the {items, nextPageToken} envelope.
	req = httptest.NewRequest(http.MethodGet, "/v1alpha1/agent-diagnostics", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.v1alpha1ListAgentDiagnostics(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))

	var all struct {
		Items         []map[string]any `json:"items"`
		NextPageToken *string          `json:"nextPageToken"`
	}
	g.Expect(json.Unmarshal(w.Body.Bytes(), &all)).To(gomega.Succeed())
	g.Expect(all.Items).To(gomega.HaveLen(10))
	g.Expect(all.NextPageToken).To(gomega.BeNil())

	// 3. With ?limit= the envelope is returned with nextPageToken absent (fake client ignores Limit).
	req = httptest.NewRequest(http.MethodGet, "/v1alpha1/agent-diagnostics?limit=3", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.v1alpha1ListAgentDiagnostics(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))

	var paged struct {
		Items         []map[string]any `json:"items"`
		NextPageToken *string          `json:"nextPageToken"`
	}
	g.Expect(json.Unmarshal(w.Body.Bytes(), &paged)).To(gomega.Succeed())
	g.Expect(paged.Items).NotTo(gomega.BeNil())
}

func TestV1alpha1ListAgentDiagnostics_TimeRangeFilter(t *testing.T) {
	g := gomega.NewWithT(t)

	now := time.Now().UTC()
	old := &v1alpha1.AgentDiagnostic{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "diag-old",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Hour)),
		},
		Spec: v1alpha1.AgentDiagnosticSpec{AgentIdentity: "agent-1", CorrelationID: "corr-old"},
	}
	recent := &v1alpha1.AgentDiagnostic{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "diag-recent",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
		},
		Spec: v1alpha1.AgentDiagnosticSpec{AgentIdentity: "agent-1", CorrelationID: "corr-recent"},
	}
	s := newTestServer(old, recent)

	cutoff := now.Add(-30 * time.Minute).Format(time.RFC3339)

	// 1. createdAfter returns only the recent diagnostic.
	req := httptest.NewRequest(http.MethodGet, "/v1alpha1/agent-diagnostics?createdAfter="+cutoff, nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.v1alpha1ListAgentDiagnostics(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))

	var afterResp struct {
		Items []map[string]any `json:"items"`
	}
	g.Expect(json.Unmarshal(w.Body.Bytes(), &afterResp)).To(gomega.Succeed())
	g.Expect(afterResp.Items).To(gomega.HaveLen(1))
	g.Expect(afterResp.Items[0]["name"]).To(gomega.Equal("diag-recent"))

	// 2. createdBefore returns only the old diagnostic.
	req = httptest.NewRequest(http.MethodGet, "/v1alpha1/agent-diagnostics?createdBefore="+cutoff, nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.v1alpha1ListAgentDiagnostics(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))

	var beforeResp struct {
		Items []map[string]any `json:"items"`
	}
	g.Expect(json.Unmarshal(w.Body.Bytes(), &beforeResp)).To(gomega.Succeed())
	g.Expect(beforeResp.Items).To(gomega.HaveLen(1))
	g.Expect(beforeResp.Items[0]["name"]).To(gomega.Equal("diag-old"))

	// 3. Combining createdAfter with limit is rejected with 400.
	req = httptest.NewRequest(http.MethodGet, "/v1alpha1/agent-diagnostics?createdAfter="+cutoff+"&limit=1", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.v1alpha1ListAgentDiagnostics(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusBadRequest))
}

func TestV1alpha1ListAccuracySummaries_ReturnsSummaries(t *testing.T) {
	g := gomega.NewWithT(t)

	summary := &v1alpha1.DiagnosticAccuracySummary{
		ObjectMeta: metav1.ObjectMeta{Name: "summary-agent-1", Namespace: "default"},
		Spec:       v1alpha1.DiagnosticAccuracySummarySpec{AgentIdentity: "agent-1"},
	}
	s := newTestServer(summary)

	req := httptest.NewRequest(http.MethodGet, "/v1alpha1/diagnostic-accuracy-summaries", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.v1alpha1ListAccuracySummaries(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	g.Expect(json.Unmarshal(w.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Items).To(gomega.HaveLen(1))
	g.Expect(resp.Items[0]["agentIdentity"]).To(gomega.Equal("agent-1"))
}
