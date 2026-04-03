package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onsi/gomega"
)

func TestRoleConfig(t *testing.T) {
	g := gomega.NewWithT(t)

	rc := newRoleConfig("agent1, agent2", "reviewer1,reviewer2")

	g.Expect(rc.isAgent("agent1")).To(gomega.BeTrue())
	g.Expect(rc.isAgent("agent3")).To(gomega.BeFalse())

	g.Expect(rc.isReviewer("reviewer1")).To(gomega.BeTrue())
	g.Expect(rc.isReviewer("agent1")).To(gomega.BeFalse())

	// empty subject lists = open/dev mode: any caller is permitted
	open := newRoleConfig("", "")
	g.Expect(open.isAgent("anyone")).To(gomega.BeTrue())
	g.Expect(open.isReviewer("anyone")).To(gomega.BeTrue())
}

func TestRequireRole(t *testing.T) {
	g := gomega.NewWithT(t)

	rc := newRoleConfig("agent", "reviewer")

	cases := []struct {
		name       string
		role       string
		sub        string
		wantStatus int
		wantPass   bool
	}{
		{"valid agent", "agent", "agent", http.StatusOK, true},
		{"invalid agent", "agent", "intruder", http.StatusForbidden, false},
		{"valid reviewer", "reviewer", "reviewer", http.StatusOK, true},
		{"invalid reviewer", "reviewer", "agent", http.StatusForbidden, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			pass := requireRole(rc, tc.role, tc.sub, w)
			g.Expect(pass).To(gomega.Equal(tc.wantPass))
			if !pass {
				g.Expect(w.Code).To(gomega.Equal(tc.wantStatus))
			}
		})
	}
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
