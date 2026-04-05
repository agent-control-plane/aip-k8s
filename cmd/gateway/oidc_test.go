package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onsi/gomega"
)

func TestRoleConfig(t *testing.T) {
	g := gomega.NewWithT(t)

	rc := newRoleConfig("agent1, agent2", "reviewer1,reviewer2", "", "")

	g.Expect(rc.isAgent("agent1", nil)).To(gomega.BeTrue())
	g.Expect(rc.isAgent("agent3", nil)).To(gomega.BeFalse())

	g.Expect(rc.isReviewer("reviewer1", nil)).To(gomega.BeTrue())
	g.Expect(rc.isReviewer("agent1", nil)).To(gomega.BeFalse())

	// empty subject and group lists = open/dev mode: any caller is permitted
	open := newRoleConfig("", "", "", "")
	g.Expect(open.isAgent("anyone", nil)).To(gomega.BeTrue())
	g.Expect(open.isReviewer("anyone", nil)).To(gomega.BeTrue())

	// partial configuration: once either list is non-empty both roles are enforced
	partial := newRoleConfig("agent1", "", "", "")
	g.Expect(partial.isAgent("agent1", nil)).To(gomega.BeTrue())
	g.Expect(partial.isAgent("anyone", nil)).To(gomega.BeFalse())
	g.Expect(partial.isReviewer("anyone", nil)).To(gomega.BeFalse()) // no reviewer list configured → nobody is a reviewer
}

func TestRoleConfigGroups(t *testing.T) {
	g := gomega.NewWithT(t)

	rc := newRoleConfig("", "", "infra-agents,ml-agents", "sre-team,platform-eng")

	// group membership grants agent role
	g.Expect(rc.isAgent("bot-123", []string{"infra-agents"})).To(gomega.BeTrue())
	g.Expect(rc.isAgent("bot-123", []string{"ml-agents"})).To(gomega.BeTrue())
	g.Expect(rc.isAgent("bot-123", []string{"other-group"})).To(gomega.BeFalse())
	g.Expect(rc.isAgent("bot-123", nil)).To(gomega.BeFalse())

	// group membership grants reviewer role
	g.Expect(rc.isReviewer("alice", []string{"sre-team"})).To(gomega.BeTrue())
	g.Expect(rc.isReviewer("alice", []string{"platform-eng"})).To(gomega.BeTrue())
	g.Expect(rc.isReviewer("alice", []string{"dev-team"})).To(gomega.BeFalse())

	// subject match still works when groups are configured
	rcMixed := newRoleConfig("named-agent", "named-reviewer", "infra-agents", "sre-team")
	g.Expect(rcMixed.isAgent("named-agent", nil)).To(gomega.BeTrue())
	g.Expect(rcMixed.isAgent("bot-456", []string{"infra-agents"})).To(gomega.BeTrue())
	g.Expect(rcMixed.isReviewer("named-reviewer", nil)).To(gomega.BeTrue())
	g.Expect(rcMixed.isReviewer("alice", []string{"sre-team"})).To(gomega.BeTrue())

	// only groups configured — open mode is NOT active (partial config enforces both roles)
	g.Expect(rc.isAgent("anyone", nil)).To(gomega.BeFalse())
	g.Expect(rc.isReviewer("anyone", nil)).To(gomega.BeFalse())
}

func TestClaimStringSlice(t *testing.T) {
	g := gomega.NewWithT(t)

	// JSON array ([]interface{} after json.Unmarshal)
	claims := map[string]interface{}{
		"groups": []interface{}{"sre-team", "platform-eng"},
	}
	g.Expect(claimStringSlice(claims, "groups")).To(gomega.ConsistOf("sre-team", "platform-eng"))

	// native []string
	claims2 := map[string]interface{}{
		"roles": []string{"admin", "viewer"},
	}
	g.Expect(claimStringSlice(claims2, "roles")).To(gomega.ConsistOf("admin", "viewer"))

	// missing claim
	g.Expect(claimStringSlice(claims, "missing")).To(gomega.BeNil())

	// wrong type (string, not array)
	claims3 := map[string]interface{}{"groups": "single-string"}
	g.Expect(claimStringSlice(claims3, "groups")).To(gomega.BeNil())
}

func TestRequireRole(t *testing.T) {
	g := gomega.NewWithT(t)

	rc := newRoleConfig("agent", "reviewer", "", "")

	cases := []struct {
		name       string
		role       string
		sub        string
		groups     []string
		wantStatus int
		wantPass   bool
	}{
		{"valid agent by sub", "agent", "agent", nil, http.StatusOK, true},
		{"invalid agent", "agent", "intruder", nil, http.StatusForbidden, false},
		{"valid reviewer by sub", "reviewer", "reviewer", nil, http.StatusOK, true},
		{"invalid reviewer", "reviewer", "agent", nil, http.StatusForbidden, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			pass := requireRole(rc, tc.role, tc.sub, tc.groups, w)
			g.Expect(pass).To(gomega.Equal(tc.wantPass))
			if !pass {
				g.Expect(w.Code).To(gomega.Equal(tc.wantStatus))
			}
		})
	}

	// group-based requireRole
	rcGroups := newRoleConfig("", "", "infra-agents", "sre-team")
	t.Run("agent via group", func(t *testing.T) {
		w := httptest.NewRecorder()
		g.Expect(requireRole(rcGroups, "agent", "bot-1", []string{"infra-agents"}, w)).To(gomega.BeTrue())
	})
	t.Run("reviewer via group", func(t *testing.T) {
		w := httptest.NewRecorder()
		g.Expect(requireRole(rcGroups, "reviewer", "alice", []string{"sre-team"}, w)).To(gomega.BeTrue())
	})
	t.Run("wrong group denied", func(t *testing.T) {
		w := httptest.NewRecorder()
		g.Expect(requireRole(rcGroups, "agent", "bot-1", []string{"other-group"}, w)).To(gomega.BeFalse())
		g.Expect(w.Code).To(gomega.Equal(http.StatusForbidden))
	})
}

func TestProxyHeaderMiddleware(t *testing.T) {
	g := gomega.NewWithT(t)

	// test no cidrs = trust all
	mwTrustAll := newProxyHeaderMiddleware("")
	handler := mwTrustAll(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := callerSubFromCtx(r.Context())
		w.Header().Set("X-Result-Sub", sub)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Remote-User", "test-user")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	g.Expect(w.Header().Get("X-Result-Sub")).To(gomega.Equal("test-user"))

	// test with specific CIDR
	mwTrustCIDR := newProxyHeaderMiddleware("10.0.0.0/8")
	handlerCIDR := mwTrustCIDR(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := callerSubFromCtx(r.Context())
		w.Header().Set("X-Result-Sub", sub)
		w.WriteHeader(http.StatusOK)
	}))

	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set("X-Remote-User", "trusted-user")
	req2.RemoteAddr = "10.1.2.3:12345"
	w2 := httptest.NewRecorder()
	handlerCIDR.ServeHTTP(w2, req2)
	g.Expect(w2.Header().Get("X-Result-Sub")).To(gomega.Equal("trusted-user"))

	// untrusted IP
	req3 := httptest.NewRequest("GET", "/", nil)
	req3.Header.Set("X-Remote-User", "untrusted-user")
	req3.RemoteAddr = "192.168.1.1:12345"
	w3 := httptest.NewRecorder()
	handlerCIDR.ServeHTTP(w3, req3)
	// should not extract context from untrusted IP
	g.Expect(w3.Header().Get("X-Result-Sub")).To(gomega.Equal(""))
}
