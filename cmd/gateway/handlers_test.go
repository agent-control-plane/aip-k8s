package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

func newTestServer(objs ...client.Object) *Server {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.AgentRequest{}, &v1alpha1.AgentDiagnostic{}).
		Build()
	return &Server{
		client:       fc,
		dedupWindow:  0,
		waitTimeout:  90 * time.Second,
		roles:        newRoleConfig("agent-sub", "reviewer-sub", "", "", "", ""),
		authRequired: true,
	}
}

func pendingAgentRequest(name, ns, agentIdentity string) *v1alpha1.AgentRequest {
	ar := &v1alpha1.AgentRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.AgentRequestSpec{
			AgentIdentity: agentIdentity,
			Action:        "restart",
			Target:        v1alpha1.Target{URI: "k8s://default/deployment/foo"},
			Reason:        "test",
		},
	}
	ar.Status.Phase = v1alpha1.PhasePending
	return ar
}

//nolint:unparam // ns is always "default" in tests but kept for symmetry with pendingAgentRequest
func approvedAgentRequest(name, ns, agentIdentity string) *v1alpha1.AgentRequest {
	ar := pendingAgentRequest(name, ns, agentIdentity)
	ar.Status.Phase = v1alpha1.PhaseApproved
	return ar
}

//nolint:unparam // ns is always "default" in tests but kept for symmetry with pendingAgentRequest
func executingAgentRequest(name, ns, agentIdentity string) *v1alpha1.AgentRequest {
	ar := pendingAgentRequest(name, ns, agentIdentity)
	ar.Status.Phase = v1alpha1.PhaseExecuting
	return ar
}

// --- Non-self-approval ---

func TestSelfApprovalRejected(t *testing.T) {
	g := gomega.NewWithT(t)

	// reviewer-sub is both a reviewer AND the creator of the request
	ar := pendingAgentRequest("req-1", "default", "reviewer-sub")
	s := newTestServer(ar)
	// prime status via fake client directly since fake doesn't go through admission
	if err := s.client.Status().Update(context.Background(), ar); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-requests/req-1/approve", nil)
	req.SetPathValue("name", "req-1")
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.handleApproveAgentRequest(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("self-approval"))
}

func TestSelfDenialRejected(t *testing.T) {
	g := gomega.NewWithT(t)

	ar := pendingAgentRequest("req-2", "default", "reviewer-sub")
	s := newTestServer(ar)
	if err := s.client.Status().Update(context.Background(), ar); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-requests/req-2/deny", nil)
	req.SetPathValue("name", "req-2")
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.handleDenyAgentRequest(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("self-approval"))
}

func TestDifferentReviewerCanApprove(t *testing.T) {
	g := gomega.NewWithT(t)

	// creator is agent-sub, reviewer is reviewer-sub — different, so allowed
	ar := pendingAgentRequest("req-3", "default", "agent-sub")
	s := newTestServer(ar)
	if err := s.client.Status().Update(context.Background(), ar); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-requests/req-3/approve", strings.NewReader(`{}`))
	req.SetPathValue("name", "req-3")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.handleApproveAgentRequest(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
}

// --- Creator-only state transitions ---

//nolint:dupl // structurally similar to TestCompletedByNonCreatorRejected
func TestExecutingByNonCreatorRejected(t *testing.T) {
	g := gomega.NewWithT(t)

	ar := approvedAgentRequest("req-4", "default", "agent-sub")
	// both "agent-sub" and "other-agent" are agents; the creator-only check
	// must fire even when the caller has a valid agent role
	fc := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(ar).
		WithStatusSubresource(&v1alpha1.AgentRequest{}).Build()
	s := &Server{
		client:       fc,
		dedupWindow:  0,
		roles:        newRoleConfig("agent-sub,other-agent", "reviewer-sub", "", "", "", ""),
		authRequired: true,
	}
	if err := s.client.Status().Update(context.Background(), ar); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-requests/req-4/executing", nil)
	req.SetPathValue("name", "req-4")
	req = req.WithContext(withCallerSub(req.Context(), "other-agent"))
	w := httptest.NewRecorder()
	s.handleExecutingAgentRequest(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("creating agent"))
}

//nolint:dupl // structurally similar to TestExecutingByNonCreatorRejected
func TestCompletedByNonCreatorRejected(t *testing.T) {
	g := gomega.NewWithT(t)

	// Use PhaseExecuting so the phase check passes and only the creator-only
	// authorization path determines the result — independent of check ordering.
	ar := executingAgentRequest("req-5", "default", "agent-sub")
	fc := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(ar).
		WithStatusSubresource(&v1alpha1.AgentRequest{}).Build()
	s := &Server{
		client:       fc,
		dedupWindow:  0,
		roles:        newRoleConfig("agent-sub,other-agent", "reviewer-sub", "", "", "", ""),
		authRequired: true,
	}
	if err := s.client.Status().Update(context.Background(), ar); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-requests/req-5/completed", nil)
	req.SetPathValue("name", "req-5")
	req = req.WithContext(withCallerSub(req.Context(), "other-agent"))
	w := httptest.NewRecorder()
	s.handleCompletedAgentRequest(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("creating agent"))
}

func TestCreatorCanTransitionToExecuting(t *testing.T) {
	g := gomega.NewWithT(t)

	ar := approvedAgentRequest("req-6", "default", "agent-sub")
	s := newTestServer(ar)
	if err := s.client.Status().Update(context.Background(), ar); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-requests/req-6/executing", nil)
	req.SetPathValue("name", "req-6")
	req = req.WithContext(withCallerSub(req.Context(), "agent-sub"))
	w := httptest.NewRecorder()
	s.handleExecutingAgentRequest(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusOK))
}

func TestExecutingFromWrongPhaseRejected(t *testing.T) {
	g := gomega.NewWithT(t)

	// request is still Pending — agent cannot call /executing before approval
	ar := pendingAgentRequest("req-8", "default", "agent-sub")
	s := newTestServer(ar)
	if err := s.client.Status().Update(context.Background(), ar); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-requests/req-8/executing", nil)
	req.SetPathValue("name", "req-8")
	req = req.WithContext(withCallerSub(req.Context(), "agent-sub"))
	w := httptest.NewRecorder()
	s.handleExecutingAgentRequest(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusConflict))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("Approved"))
}

func TestCompletedFromWrongPhaseRejected(t *testing.T) {
	g := gomega.NewWithT(t)

	// request is Approved but not yet Executing — agent cannot skip to /completed
	ar := approvedAgentRequest("req-9", "default", "agent-sub")
	s := newTestServer(ar)
	if err := s.client.Status().Update(context.Background(), ar); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-requests/req-9/completed", nil)
	req.SetPathValue("name", "req-9")
	req = req.WithContext(withCallerSub(req.Context(), "agent-sub"))
	w := httptest.NewRecorder()
	s.handleCompletedAgentRequest(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusConflict))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("Executing"))
}

// --- Body size limit ---

func TestOversizedBodyRejected(t *testing.T) {
	g := gomega.NewWithT(t)

	s := newTestServer()
	// Build a valid JSON body that exceeds 1 MiB so MaxBytesReader is the limiting factor,
	// not a JSON syntax error from the very first byte.
	inner := bytes.Repeat([]byte("x"), (1<<20)+1)
	bigBody := make([]byte, 0, len(inner)+20)
	bigBody = append(bigBody, []byte(`{"agentIdentity":"`)...)
	bigBody = append(bigBody, inner...)
	bigBody = append(bigBody, '"', '}')

	req := httptest.NewRequest(http.MethodPost, "/agent-requests", bytes.NewReader(bigBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withCallerSub(req.Context(), "agent-sub"))
	w := httptest.NewRecorder()
	s.handleCreateAgentRequest(w, req)

	// MaxBytesReader causes Decode to fail — handler returns 400
	g.Expect(w.Code).To(gomega.Equal(http.StatusBadRequest))
}

// --- Role enforcement on handler level ---

func TestAgentCannotApprove(t *testing.T) {
	g := gomega.NewWithT(t)

	ar := pendingAgentRequest("req-7", "default", "other-creator")
	s := newTestServer(ar)
	if err := s.client.Status().Update(context.Background(), ar); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agent-requests/req-7/approve", nil)
	req.SetPathValue("name", "req-7")
	// agent-sub is an agent, not a reviewer
	req = req.WithContext(withCallerSub(req.Context(), "agent-sub"))
	w := httptest.NewRecorder()
	s.handleApproveAgentRequest(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("reviewer role required"))
}

func TestReviewerCannotCreateRequest(t *testing.T) {
	g := gomega.NewWithT(t)

	s := newTestServer()
	body := `{"agentIdentity":"reviewer-sub","action":"restart",` +
		`"targetURI":"k8s://default/deployment/foo","reason":"test","namespace":"default"}`

	req := httptest.NewRequest(http.MethodPost, "/agent-requests", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// reviewer-sub is a reviewer, not an agent
	req = req.WithContext(withCallerSub(req.Context(), "reviewer-sub"))
	w := httptest.NewRecorder()
	s.handleCreateAgentRequest(w, req)

	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring("agent role required"))
}

func TestCreateAgentDiagnostic_Idempotent(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServer()
	s.dedupWindow = 5 * time.Minute

	body := `{"agentIdentity":"agent-sub","diagnosticType":"test-type","correlationID":"corr-1","summary":"test summary"}`

	// First creation
	req1 := httptest.NewRequest(http.MethodPost, "/agent-diagnostics", strings.NewReader(body))
	req1 = req1.WithContext(withCallerSub(req1.Context(), "agent-sub"))
	w1 := httptest.NewRecorder()
	s.handleCreateAgentDiagnostic(w1, req1)
	g.Expect(w1.Code).To(gomega.Equal(http.StatusCreated))

	// Duplicate creation
	req2 := httptest.NewRequest(http.MethodPost, "/agent-diagnostics", strings.NewReader(body))
	req2 = req2.WithContext(withCallerSub(req2.Context(), "agent-sub"))
	w2 := httptest.NewRecorder()
	s.handleCreateAgentDiagnostic(w2, req2)
	g.Expect(w2.Code).To(gomega.Equal(http.StatusOK))

	var list v1alpha1.AgentDiagnosticList
	g.Expect(s.client.List(context.Background(), &list)).To(gomega.Succeed())
	g.Expect(list.Items).To(gomega.HaveLen(1))
}
