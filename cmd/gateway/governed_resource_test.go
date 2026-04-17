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

// --- matchGovernedResource unit tests ---

func makeGRItem(name, pattern string) v1alpha1.GovernedResource {
	return v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.GovernedResourceSpec{URIPattern: pattern, PermittedActions: []string{"update"}},
	}
}

func TestMatchGovernedResource_NoItems(t *testing.T) {
	if matchGovernedResource(nil, "k8s://prod/nodepool/foo") != nil {
		t.Fatal("expected nil for empty list")
	}
}

func TestMatchGovernedResource_NoMatch(t *testing.T) {
	items := []v1alpha1.GovernedResource{makeGRItem("a", "k8s://prod/nodepool/*")}
	if matchGovernedResource(items, "k8s://staging/nodepool/foo") != nil {
		t.Fatal("expected nil when no pattern matches")
	}
}

func TestMatchGovernedResource_SingleMatch(t *testing.T) {
	items := []v1alpha1.GovernedResource{makeGRItem("a", "k8s://prod/nodepool/*")}
	got := matchGovernedResource(items, "k8s://prod/nodepool/team-a")
	if got == nil || got.Name != "a" {
		t.Fatalf("expected 'a', got %v", got)
	}
}

func TestMatchGovernedResource_MostSpecificWins(t *testing.T) {
	items := []v1alpha1.GovernedResource{
		makeGRItem("broad", "k8s://prod/*"),
		makeGRItem("specific", "k8s://prod/nodepool/team-a"),
	}
	got := matchGovernedResource(items, "k8s://prod/nodepool/team-a")
	if got == nil || got.Name != "specific" {
		t.Fatalf("expected 'specific', got %v", got)
	}
}

func TestMatchGovernedResource_AlphabeticalTiebreak(t *testing.T) {
	items := []v1alpha1.GovernedResource{
		makeGRItem("zzz", "k8s://prod/nodepool/*"),
		makeGRItem("aaa", "k8s://prod/nodepool/*"),
	}
	got := matchGovernedResource(items, "k8s://prod/nodepool/team-a")
	if got == nil || got.Name != "aaa" {
		t.Fatalf("expected 'aaa', got %v", got)
	}
}

func TestMatchGovernedResource_StarDoesNotCrossSlash(t *testing.T) {
	// path.Match: * does not match /
	items := []v1alpha1.GovernedResource{makeGRItem("a", "k8s://prod/nodepool/*")}
	if matchGovernedResource(items, "k8s://prod/nodepool/team-a/extra") != nil {
		t.Fatal("* should not match across a slash")
	}
}

// --- handleCreateAgentRequest GovernedResource admission tests ---

func newGR(name, pattern string, agents, actions []string) *v1alpha1.GovernedResource {
	return &v1alpha1.GovernedResource{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.GovernedResourceSpec{
			URIPattern:       pattern,
			PermittedAgents:  agents,
			PermittedActions: actions,
			ContextFetcher:   "none",
		},
	}
}

// postCreate fires POST /agent-requests. It cancels the request context after
// shortTimeout to avoid blocking in the 90s poll loop when admission passes.
// Pass 0 to use the default short timeout (200ms). For tests that expect a 4xx
// before the poll loop, the timeout is irrelevant.
func postCreate(s *Server, targetURI, action string) *httptest.ResponseRecorder {
	return postCreateCtx(s, "agent-sub", targetURI, action, 200*time.Millisecond)
}

func postCreateCtx(s *Server, callerSub, targetURI, action string, timeout time.Duration) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	body := `{"agentIdentity":"` + callerSub + `","action":"` + action + `",` +
		`"targetURI":"` + targetURI + `","reason":"test","namespace":"default"}`
	req := httptest.NewRequest(http.MethodPost, "/agent-requests", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withCallerSub(ctx, callerSub))
	w := httptest.NewRecorder()
	s.handleCreateAgentRequest(w, req)
	return w
}

func TestGR_ZeroGRs_RequireFalse_SkipsAdmission(t *testing.T) {
	g := gomega.NewWithT(t)
	// No GRs + requireGovernedResource=false → admission skipped, request proceeds past the gate.
	// Context is cancelled quickly so the test doesn't block in the 90s poll loop.
	// We assert it was NOT rejected with 403 — the handler either wrote nothing or a timeout error.
	s := newTestServer()
	s.requireGovernedResource = false

	w := postCreate(s, "k8s://prod/nodepool/team-a", "scale-up")
	g.Expect(w.Code).NotTo(gomega.Equal(http.StatusForbidden))
}

func TestGR_ZeroGRs_RequireTrue_Rejected(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServer()
	s.requireGovernedResource = true

	w := postCreate(s, "k8s://prod/nodepool/team-a", "scale-up")
	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring(v1alpha1.DenialCodeActionNotPermitted))
}

func TestGR_URINotMatched_Rejected(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServer(newGR("gr1", "k8s://prod/nodepool/*", []string{"agent-sub"}, []string{"scale-up"}))
	s.requireGovernedResource = true

	w := postCreate(s, "k8s://staging/nodepool/team-a", "scale-up")
	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring(v1alpha1.DenialCodeActionNotPermitted))
}

func TestGR_AgentNotPermitted_Rejected(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServer(newGR("gr1", "k8s://prod/nodepool/*", []string{"other-agent"}, []string{"scale-up"}))
	s.requireGovernedResource = true

	w := postCreate(s, "k8s://prod/nodepool/team-a", "scale-up")
	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring(v1alpha1.DenialCodeIdentityInvalid))
}

func TestGR_ActionNotPermitted_Rejected(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServer(newGR("gr1", "k8s://prod/nodepool/*", []string{"agent-sub"}, []string{"scale-up"}))
	s.requireGovernedResource = true

	w := postCreate(s, "k8s://prod/nodepool/team-a", "delete")
	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring(v1alpha1.DenialCodeActionNotPermitted))
}

func TestGR_EmptyPermittedAgents_AnyAgentAllowed(t *testing.T) {
	g := gomega.NewWithT(t)
	// PermittedAgents nil/empty = any authenticated agent is allowed.
	// Admission passes — no 403 for identity. Context cancelled quickly to avoid poll loop.
	s := newTestServer(newGR("gr1", "k8s://prod/nodepool/*", nil, []string{"scale-up"}))
	s.requireGovernedResource = true

	w := postCreate(s, "k8s://prod/nodepool/team-a", "scale-up")
	g.Expect(w.Code).NotTo(gomega.Equal(http.StatusForbidden))
}

func TestGR_MostSpecificMatchUsed(t *testing.T) {
	g := gomega.NewWithT(t)
	// broad: any agent allowed. specific (longer pattern): only "other-agent".
	// Specific should win — "agent-sub" gets IDENTITY_INVALID, not pass-through.
	s := newTestServer(
		newGR("broad", "k8s://prod/nodepool/*", nil, []string{"scale-up"}),
		newGR("specific", "k8s://prod/nodepool/team-a", []string{"other-agent"}, []string{"scale-up"}),
	)
	s.requireGovernedResource = true

	w := postCreate(s, "k8s://prod/nodepool/team-a", "scale-up")
	g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	g.Expect(w.Body.String()).To(gomega.ContainSubstring(v1alpha1.DenialCodeIdentityInvalid))
}

func TestGR_AdmissionSetsGovernedResourceRef(t *testing.T) {
	g := gomega.NewWithT(t)
	gr := newGR("gr1", "k8s://prod/nodepool/*", []string{"agent-sub"}, []string{"scale-up"})
	gr.Generation = 42
	s := newTestServer(gr)
	s.requireGovernedResource = true

	// Use a longer timeout just in case, though 4xx/2xx usually happen fast
	w := postCreateCtx(s, "agent-sub", "k8s://prod/nodepool/team-a", "scale-up", 500*time.Millisecond)
	// If it passes admission, it might return 201 or block on the poll loop (returning 201 eventually or timing out)
	// In fake client, Create is instant.
	g.Expect(w.Code).To(gomega.Or(gomega.Equal(http.StatusCreated), gomega.Equal(http.StatusOK)))

	var list v1alpha1.AgentRequestList
	g.Expect(s.client.List(context.Background(), &list)).To(gomega.Succeed())
	g.Expect(list.Items).To(gomega.HaveLen(1))
	req := list.Items[0]
	g.Expect(req.Spec.GovernedResourceRef).NotTo(gomega.BeNil())
	g.Expect(req.Spec.GovernedResourceRef.Name).To(gomega.Equal("gr1"))
	g.Expect(req.Spec.GovernedResourceRef.Generation).To(gomega.Equal(int64(42)))
}

func TestGR_NoGRs_RequireFalse_RefIsNil(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServer()
	s.requireGovernedResource = false

	w := postCreate(s, "k8s://prod/nodepool/team-a", "scale-up")
	g.Expect(w.Code).To(gomega.Or(gomega.Equal(http.StatusCreated), gomega.Equal(http.StatusOK)))

	var list v1alpha1.AgentRequestList
	g.Expect(s.client.List(context.Background(), &list)).To(gomega.Succeed())
	g.Expect(list.Items).To(gomega.HaveLen(1))
	req := list.Items[0]
	g.Expect(req.Spec.GovernedResourceRef).To(gomega.BeNil())
}

func TestCreateAgentRequest_Idempotent(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newTestServer()
	s.dedupWindow = 5 * time.Minute

	// First creation — context is cancelled after 200ms so the poll loop returns
	// without writing; recorder default is 200. We only care that exactly one
	// AgentRequest was created.
	postCreate(s, "k8s://prod/nodepool/team-a", "scale-up")

	var list v1alpha1.AgentRequestList
	g.Expect(s.client.List(context.Background(), &list)).To(gomega.Succeed())
	g.Expect(list.Items).To(gomega.HaveLen(1))
	existingName := list.Items[0].Name

	// Duplicate creation — no polling, returns 200 immediately with current state.
	w2 := postCreate(s, "k8s://prod/nodepool/team-a", "scale-up")
	g.Expect(w2.Code).To(gomega.Equal(http.StatusOK))

	var resp map[string]any
	g.Expect(json.NewDecoder(w2.Body).Decode(&resp)).To(gomega.Succeed())
	g.Expect(resp["name"]).To(gomega.Equal(existingName))

	// Exactly one resource in the cluster — no duplicate was created.
	g.Expect(s.client.List(context.Background(), &list)).To(gomega.Succeed())
	g.Expect(list.Items).To(gomega.HaveLen(1))
}
