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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8swatch "k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/mcp"
	"github.com/onsi/gomega"
)

// parseToolResult unmarshals the first text content block of a ToolsCallResult
// into a map for assertion.
func parseToolResult(t *testing.T, result *mcp.ToolsCallResult) map[string]any {
	t.Helper()
	g := gomega.NewWithT(t)
	g.Expect(result).NotTo(gomega.BeNil())
	g.Expect(result.Content).NotTo(gomega.BeEmpty())
	var block struct {
		Text string `json:"text"`
	}
	g.Expect(json.Unmarshal(result.Content[0], &block)).To(gomega.Succeed())
	var m map[string]any
	g.Expect(json.Unmarshal([]byte(block.Text), &m)).To(gomega.Succeed())
	return m
}

func ar(phase string) *v1alpha1.AgentRequest {
	return &v1alpha1.AgentRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-test"},
		Spec: v1alpha1.AgentRequestSpec{
			AgentIdentity: "agent-1",
			Action:        "k8s/scale_deployment",
		},
		Status: v1alpha1.AgentRequestStatus{Phase: phase},
	}
}

func TestAIPApprovalResult_Pending(t *testing.T) {
	s := &Server{}
	result := s.aipApprovalResult(ar(v1alpha1.PhasePending))
	gomega.NewWithT(t).Expect(result).To(gomega.BeNil())
}

func TestAIPApprovalResult_Approved_NoJWT(t *testing.T) {
	s := &Server{} // jwtManager nil → no JWT minted
	a := ar(v1alpha1.PhaseApproved)
	a.Spec.HumanApproval = &v1alpha1.HumanApproval{ApprovedBy: "alice"}
	result := s.aipApprovalResult(a)
	m := parseToolResult(t, result)
	g := gomega.NewWithT(t)
	g.Expect(m["status"]).To(gomega.Equal("approved"))
	g.Expect(m["approvedBy"]).To(gomega.Equal("alice"))
	g.Expect(m["requestId"]).To(gomega.Equal("mcp-test"))
	g.Expect(m["jwt"]).To(gomega.Equal("")) // no jwtManager
}

func TestAIPApprovalResult_Denied(t *testing.T) {
	g := gomega.NewWithT(t)
	s := &Server{}
	a := ar(v1alpha1.PhaseDenied)
	a.Status.Denial = &v1alpha1.DenialResponse{Message: "policy: replicas exceeds cap"}
	result := s.aipApprovalResult(a)
	m := parseToolResult(t, result)
	g.Expect(m["status"]).To(gomega.Equal("denied"))
	g.Expect(m["reason"]).To(gomega.Equal("policy: replicas exceeds cap"))
	g.Expect(m["requestId"]).To(gomega.Equal("mcp-test"))
}

func TestAIPApprovalResult_Denied_NoMessage(t *testing.T) {
	s := &Server{}
	result := s.aipApprovalResult(ar(v1alpha1.PhaseDenied))
	m := parseToolResult(t, result)
	gomega.NewWithT(t).Expect(m["reason"]).To(gomega.Equal("denied by policy"))
}

func TestAIPApprovalResult_Expired(t *testing.T) {
	s := &Server{}
	result := s.aipApprovalResult(ar(v1alpha1.PhaseExpired))
	m := parseToolResult(t, result)
	gomega.NewWithT(t).Expect(m["status"]).To(gomega.Equal("expired"))
}

func TestAIPApprovalResult_Failed(t *testing.T) {
	s := &Server{}
	result := s.aipApprovalResult(ar(v1alpha1.PhaseFailed))
	m := parseToolResult(t, result)
	gomega.NewWithT(t).Expect(m["status"]).To(gomega.Equal("failed"))
}

func TestAIPApprovalResult_Completed_WithVerdict(t *testing.T) {
	g := gomega.NewWithT(t)
	s := &Server{}
	a := ar(v1alpha1.PhaseCompleted)
	a.Status.Verdict = "correct"
	a.Status.VerdictReasonCode = "ok"
	a.Status.VerdictNote = "looked good"
	result := s.aipApprovalResult(a)
	m := parseToolResult(t, result)
	g.Expect(m["status"]).To(gomega.Equal("graded"))
	g.Expect(m["verdict"]).To(gomega.Equal("correct"))
	g.Expect(m["reasonCode"]).To(gomega.Equal("ok"))
	g.Expect(m["note"]).To(gomega.Equal("looked good"))
}

func TestAIPApprovalResult_Completed_NoVerdict(t *testing.T) {
	s := &Server{}
	// Completed without a verdict (e.g., execution completed but no grading yet)
	// should return nil so the caller keeps waiting.
	result := s.aipApprovalResult(ar(v1alpha1.PhaseCompleted))
	gomega.NewWithT(t).Expect(result).To(gomega.BeNil())
}

func TestBuildTargetURI_NameAndKind(t *testing.T) {
	g := gomega.NewWithT(t)
	uri := buildTargetURI(map[string]any{"namespace": "prod", "name": "payment-api", "kind": "Deployment"})
	g.Expect(uri).To(gomega.Equal("k8s://prod/deployment/payment-api"))
}

func TestBuildTargetURI_DeploymentFallback(t *testing.T) {
	uri := buildTargetURI(map[string]any{"deployment": "worker", "namespace": "staging"})
	gomega.NewWithT(t).Expect(uri).To(gomega.Equal("k8s://staging/deployment/worker"))
}

func TestBuildTargetURI_NamespaceOnly(t *testing.T) {
	uri := buildTargetURI(map[string]any{"namespace": "monitoring"})
	gomega.NewWithT(t).Expect(uri).To(gomega.Equal("k8s://monitoring"))
}

func TestBuildTargetURI_Empty(t *testing.T) {
	uri := buildTargetURI(map[string]any{})
	gomega.NewWithT(t).Expect(uri).To(gomega.Equal("k8s://default"))
}

func TestMCPRequestName_Format(t *testing.T) {
	g := gomega.NewWithT(t)
	name := mcpRequestName()
	g.Expect(name).To(gomega.HavePrefix("mcp-"))
	g.Expect(name).To(gomega.HaveLen(12)) // "mcp-" + 8 hex chars
	g.Expect(strings.TrimPrefix(name, "mcp-")).To(gomega.MatchRegexp(`^[0-9a-f]{8}$`))
}

func TestMCPRequestName_Unique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for range 100 {
		n := mcpRequestName()
		if _, dup := seen[n]; dup {
			t.Fatalf("duplicate name generated: %s", n)
		}
		seen[n] = struct{}{}
	}
}

// stubGetClient is a minimal client.Client for testing: returns the given AR on
// the first Get, then an error on all subsequent calls so failAgentRequest bails.
type stubGetClient struct {
	client.Client // nil embedded; other methods panic if called
	ar            *v1alpha1.AgentRequest
	getCount      int
}

func (m *stubGetClient) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	m.getCount++
	if m.getCount == 1 {
		if target, ok := obj.(*v1alpha1.AgentRequest); ok {
			*target = *m.ar
		}
		return nil
	}
	return fmt.Errorf("not found")
}

// stubWatchClient is a minimal client.WithWatch that returns a non-draining channel.
type stubWatchClient struct {
	client.WithWatch // nil embedded; other methods panic if called
	ch               chan k8swatch.Event
}

func (s *stubWatchClient) Watch(
	_ context.Context, _ client.ObjectList, _ ...client.ListOption,
) (k8swatch.Interface, error) {
	return &blockingWatcher{ch: s.ch}, nil
}

type blockingWatcher struct{ ch chan k8swatch.Event }

func (w *blockingWatcher) Stop()                             {}
func (w *blockingWatcher) ResultChan() <-chan k8swatch.Event { return w.ch }

func TestAIPAwaitApproval_Timeout(t *testing.T) {
	g := gomega.NewWithT(t)

	pendingAR := ar(v1alpha1.PhasePending)

	s := &Server{
		client:      &stubGetClient{ar: pendingAR},
		apiReader:   &stubGetClient{ar: pendingAR}, // direct-read path used by handleAIPAwaitApproval
		watchClient: &stubWatchClient{ch: make(chan k8swatch.Event)}, // never sends
		waitTimeout: 50 * time.Millisecond,
	}
	body := mcpRequest("tools/call", mcp.ToolsCallParams{
		Name:      "aip/await_approval",
		Arguments: map[string]any{"requestId": pendingAR.Name},
	})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	s.handleMCP(rr, req) // blocks ~50ms until context times out

	g.Expect(rr.Code).To(gomega.Equal(http.StatusOK))
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	g.Expect(json.Unmarshal(rr.Body.Bytes(), &resp)).To(gomega.Succeed())
	g.Expect(resp.Error).ToNot(gomega.BeNil())
	g.Expect(resp.Error.Code).To(gomega.Equal(mcp.ErrCodeInternal))
	g.Expect(resp.Error.Message).To(gomega.ContainSubstring("timed out"))
}
