package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func TestAgentRequestLabels(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServer()
	// Use a near-zero timeout so the phase-poll exits immediately instead of
	// blocking for 90s. The object is created before polling starts, so the
	// fake store already has it by the time we assert on labels.
	s.waitTimeout = time.Millisecond

	body := `{"agentIdentity":"agent-sub","action":"restart",` +
		`"targetURI":"k8s://default/deployment/foo","reason":"test",` +
		`"namespace":"default","correlationID":"corr-123"}`
	req := httptest.NewRequest(http.MethodPost, "/agent-requests", strings.NewReader(body))
	req = req.WithContext(withCallerSub(req.Context(), "agent-sub"))
	w := httptest.NewRecorder()

	s.handleCreateAgentRequest(w, req)
	// Handler returns 504 (phase-poll timed out); the object is still written.
	g.Expect(w.Code).NotTo(gomega.Equal(http.StatusInternalServerError))

	var list v1alpha1.AgentRequestList
	g.Expect(s.client.List(context.Background(), &list)).To(gomega.Succeed())
	g.Expect(list.Items).To(gomega.HaveLen(1))

	labels := list.Items[0].Labels
	g.Expect(labels["aip.io/agentIdentity"]).To(gomega.Equal("agent-sub"))
	g.Expect(labels["aip.io/correlationID"]).To(gomega.Equal("corr-123"))
}

func TestListAgentRequests_FilteringAndPagination(t *testing.T) {
	g := gomega.NewWithT(t)

	// Setup: 5 requests for agent-1, 3 for agent-2
	objs := []client.Object{}
	for i := 1; i <= 5; i++ {
		ar := pendingAgentRequest(fmt.Sprintf("req-agent-1-%d", i), "default", "agent-1")
		ar.Labels = map[string]string{
			"aip.io/agentIdentity": "agent-1",
			"aip.io/correlationID": fmt.Sprintf("corr-%d", i),
		}
		objs = append(objs, ar)
	}
	for i := 1; i <= 3; i++ {
		ar := pendingAgentRequest(fmt.Sprintf("req-agent-2-%d", i), "default", "agent-2")
		ar.Labels = map[string]string{"aip.io/agentIdentity": "agent-2"}
		objs = append(objs, ar)
	}

	s := newTestServer(objs...)

	// 1. Filter by agentIdentity
	req := httptest.NewRequest(http.MethodGet, "/agent-requests?agentIdentity=agent-1", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.handleListAgentRequests(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))

	var items []v1alpha1.AgentRequest
	g.Expect(json.Unmarshal(w.Body.Bytes(), &items)).To(gomega.Succeed())
	g.Expect(items).To(gomega.HaveLen(5))

	// 2. Filter by correlationID
	req = httptest.NewRequest(http.MethodGet, "/agent-requests?correlationID=corr-3", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.handleListAgentRequests(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(json.Unmarshal(w.Body.Bytes(), &items)).To(gomega.Succeed())
	g.Expect(items).To(gomega.HaveLen(1))

	// 2b. Combined filter: both agentIdentity and correlationID — both labels must
	// be applied in a single MatchingLabels call or the second overwrites the first.
	req = httptest.NewRequest(http.MethodGet, "/agent-requests?agentIdentity=agent-1&correlationID=corr-3", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.handleListAgentRequests(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(json.Unmarshal(w.Body.Bytes(), &items)).To(gomega.Succeed())
	g.Expect(items).To(gomega.HaveLen(1))
	g.Expect(items[0].Name).To(gomega.Equal("req-agent-1-3"))

	// 3. Pagination: verify the response switches to the paged envelope format when
	// ?limit= is present. The fake client does not enforce Limit, so item count is
	// not asserted — only that the envelope is used and items is non-nil.
	req = httptest.NewRequest(http.MethodGet, "/agent-requests?limit=2", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.handleListAgentRequests(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))

	var paginated struct {
		Items    []v1alpha1.AgentRequest `json:"items"`
		Continue string                  `json:"continue"`
	}
	g.Expect(json.Unmarshal(w.Body.Bytes(), &paginated)).To(gomega.Succeed())
	g.Expect(paginated.Items).NotTo(gomega.BeNil())

	// 4. Without ?limit= the response is a flat array, not an envelope.
	req = httptest.NewRequest(http.MethodGet, "/agent-requests", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.handleListAgentRequests(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	var flat []v1alpha1.AgentRequest
	g.Expect(json.Unmarshal(w.Body.Bytes(), &flat)).To(gomega.Succeed())
	g.Expect(flat).To(gomega.HaveLen(8))
}

func TestListAuditRecords_LabelFilteringAndPagination(t *testing.T) {
	g := gomega.NewWithT(t)

	// Setup: 4 audit records for req-a, 2 for req-b
	objs := []client.Object{}
	for i := 1; i <= 4; i++ {
		ar := &v1alpha1.AuditRecord{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("req-a-audit-%d", i),
				Namespace: "default",
				Labels:    map[string]string{"aip.io/agentRequestRef": "req-a"},
			},
			Spec: v1alpha1.AuditRecordSpec{AgentRequestRef: "req-a"},
		}
		objs = append(objs, ar)
	}
	for i := 1; i <= 2; i++ {
		ar := &v1alpha1.AuditRecord{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("req-b-audit-%d", i),
				Namespace: "default",
				Labels:    map[string]string{"aip.io/agentRequestRef": "req-b"},
			},
			Spec: v1alpha1.AuditRecordSpec{AgentRequestRef: "req-b"},
		}
		objs = append(objs, ar)
	}

	s := newTestServer(objs...)

	// 1. Filter by agentRequest uses the label selector server-side.
	req := httptest.NewRequest(http.MethodGet, "/audit-records?agentRequest=req-a", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.handleListAuditRecords(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))

	var items []v1alpha1.AuditRecord
	g.Expect(json.Unmarshal(w.Body.Bytes(), &items)).To(gomega.Succeed())
	g.Expect(items).To(gomega.HaveLen(4))
	for _, item := range items {
		g.Expect(item.Spec.AgentRequestRef).To(gomega.Equal("req-a"))
	}

	// 2. No filter returns all records.
	req = httptest.NewRequest(http.MethodGet, "/audit-records", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.handleListAuditRecords(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(json.Unmarshal(w.Body.Bytes(), &items)).To(gomega.Succeed())
	g.Expect(items).To(gomega.HaveLen(6))

	// 3. Pagination: ?limit= switches to envelope format.
	req = httptest.NewRequest(http.MethodGet, "/audit-records?limit=2", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.handleListAuditRecords(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))

	var paginated struct {
		Items    []v1alpha1.AuditRecord `json:"items"`
		Continue string                 `json:"continue"`
	}
	g.Expect(json.Unmarshal(w.Body.Bytes(), &paginated)).To(gomega.Succeed())
	g.Expect(paginated.Items).NotTo(gomega.BeNil())
}

func TestListAgentRequests_PhaseFilter(t *testing.T) {
	g := gomega.NewWithT(t)

	objs := []client.Object{}
	for _, o := range []struct {
		name, agentID string
		phase         string
		mod           func(*v1alpha1.AgentRequest)
	}{
		{"req-1-pend", "agent-1", v1alpha1.PhasePending, nil},
		{"req-2-appr", "agent-1", v1alpha1.PhaseApproved, nil},
		{"req-3-deni", "agent-2", v1alpha1.PhaseDenied, nil},
		{"req-4-comp", "agent-2", v1alpha1.PhaseCompleted, nil},
	} {
		ar := pendingAgentRequest(o.name, "default", o.agentID)
		ar.Labels = map[string]string{"aip.io/agentIdentity": o.agentID}
		ar.Status.Phase = o.phase
		objs = append(objs, ar)
	}
	s := newTestServer(objs...)

	// 1. Single phase filter.
	req := httptest.NewRequest(http.MethodGet, "/agent-requests?phase=Pending", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.handleListAgentRequests(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	var items []v1alpha1.AgentRequest
	g.Expect(json.Unmarshal(w.Body.Bytes(), &items)).To(gomega.Succeed())
	g.Expect(items).To(gomega.HaveLen(1))
	g.Expect(items[0].Status.Phase).To(gomega.Equal(v1alpha1.PhasePending))

	// 2. Multi-phase filter.
	req = httptest.NewRequest(http.MethodGet, "/agent-requests?phase=Approved,Pending", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.handleListAgentRequests(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(json.Unmarshal(w.Body.Bytes(), &items)).To(gomega.Succeed())
	g.Expect(items).To(gomega.HaveLen(2))

	// 3. No phase filter — all items returned.
	req = httptest.NewRequest(http.MethodGet, "/agent-requests", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.handleListAgentRequests(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(json.Unmarshal(w.Body.Bytes(), &items)).To(gomega.Succeed())
	g.Expect(items).To(gomega.HaveLen(4))

	// 4. Invalid phase returns 400.
	req = httptest.NewRequest(http.MethodGet, "/agent-requests?phase=InvalidPhase", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.handleListAgentRequests(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusBadRequest))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("invalid phase"))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("Approved"))

	// 5. Combined with agentIdentity.
	req = httptest.NewRequest(http.MethodGet, "/agent-requests?agentIdentity=agent-1&phase=Pending", nil)
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w = httptest.NewRecorder()
	s.handleListAgentRequests(w, req)
	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
	g.Expect(json.Unmarshal(w.Body.Bytes(), &items)).To(gomega.Succeed())
	g.Expect(items).To(gomega.HaveLen(1))
}
