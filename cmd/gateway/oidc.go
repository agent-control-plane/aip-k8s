package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	roleAgent    = "agent"
	roleReviewer = "reviewer"
	roleAdmin    = "admin"
)

type roleConfig struct {
	agentSubs      map[string]bool
	reviewerSubs   map[string]bool
	adminSubs      map[string]bool
	agentGroups    map[string]bool
	reviewerGroups map[string]bool
	adminGroups    map[string]bool
}

func newRoleConfig(
	agentList, reviewerList, adminList,
	agentGroupList, reviewerGroupList, adminGroupList string,
) *roleConfig {
	parse := func(s string) map[string]bool {
		m := map[string]bool{}
		for v := range strings.SplitSeq(s, ",") {
			if t := strings.TrimSpace(v); t != "" {
				m[t] = true
			}
		}
		return m
	}
	return &roleConfig{
		agentSubs:      parse(agentList),
		reviewerSubs:   parse(reviewerList),
		adminSubs:      parse(adminList),
		agentGroups:    parse(agentGroupList),
		reviewerGroups: parse(reviewerGroupList),
		adminGroups:    parse(adminGroupList),
	}
}

// openMode returns true when no subjects or groups are configured — any caller is permitted.
// Once any list is non-empty all roles are enforced so that partial configuration
// cannot leave one role open.
func (rc *roleConfig) openMode() bool {
	return len(rc.agentSubs) == 0 && len(rc.reviewerSubs) == 0 && len(rc.adminSubs) == 0 &&
		len(rc.agentGroups) == 0 && len(rc.reviewerGroups) == 0 && len(rc.adminGroups) == 0
}

// isAgent returns true if sub or any of groups is permitted to act as an agent.
func (rc *roleConfig) isAgent(sub string, groups []string) bool {
	if rc.openMode() {
		return true
	}
	if rc.agentSubs[sub] {
		return true
	}
	for _, g := range groups {
		if rc.agentGroups[g] {
			return true
		}
	}
	return false
}

// isReviewer returns true if sub or any of groups is permitted to act as a reviewer.
func (rc *roleConfig) isReviewer(sub string, groups []string) bool {
	if rc.openMode() {
		return true
	}
	if rc.reviewerSubs[sub] {
		return true
	}
	for _, g := range groups {
		if rc.reviewerGroups[g] {
			return true
		}
	}
	return false
}

// isAdmin returns true if sub or any of groups is permitted to act as an admin.
func (rc *roleConfig) isAdmin(sub string, groups []string) bool {
	if rc.openMode() {
		return true
	}
	if rc.adminSubs[sub] {
		return true
	}
	for _, g := range groups {
		if rc.adminGroups[g] {
			return true
		}
	}
	return false
}

func requireRole(rc *roleConfig, role, sub string, groups []string, w http.ResponseWriter) bool {
	switch role {
	case roleAgent:
		if !rc.isAgent(sub, groups) {
			writeError(w, http.StatusForbidden, "agent role required")
			return false
		}
	case roleReviewer:
		if !rc.isReviewer(sub, groups) {
			writeError(w, http.StatusForbidden, "reviewer role required")
			return false
		}
	case roleAdmin:
		if !rc.isAdmin(sub, groups) {
			writeError(w, http.StatusForbidden, "admin role required")
			return false
		}
	default:
		writeError(w, http.StatusForbidden, "unknown role")
		return false
	}
	return true
}

// newOIDCMiddleware creates JWT validation middleware.
// identityClaim is the token claim used as the caller identity (e.g. "azp",
// "sub", "appid", "email"). If the claim is absent the middleware falls back
// to "sub". groupsClaim is the token claim that carries group memberships
// (typically "groups"); its value is stored in context for role checks.
func newOIDCMiddleware(
	ctx context.Context, issuerURL, audience, identityClaim, groupsClaim string,
) (func(http.Handler) http.Handler, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc provider: %w", err)
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: audience})
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}
			raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if raw == "" {
				writeError(w, http.StatusUnauthorized, "missing Bearer token")
				return
			}
			idToken, err := verifier.Verify(r.Context(), raw)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "invalid token")
				return
			}
			var allClaims map[string]any
			if err := idToken.Claims(&allClaims); err != nil {
				writeError(w, http.StatusUnauthorized, "token missing claims")
				return
			}
			identity := claimString(allClaims, identityClaim)
			if identity == "" {
				identity = claimString(allClaims, "sub") // fallback
			}
			if identity == "" {
				writeError(w, http.StatusUnauthorized, "token missing identity claim")
				return
			}
			groups := claimStringSlice(allClaims, groupsClaim)
			rctx := withCallerSub(r.Context(), identity)
			rctx = withCallerGroups(rctx, groups)
			next.ServeHTTP(w, r.WithContext(rctx))
		})
	}, nil
}

// claimString extracts a string value from a decoded claims map.
func claimString(claims map[string]any, name string) string {
	v, _ := claims[name].(string)
	return v
}

// claimStringSlice extracts a []string from a decoded claims map.
// The claim value may be []any (JSON array) or []string.
func claimStringSlice(claims map[string]any, name string) []string {
	raw, ok := claims[name]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func newProxyHeaderMiddleware(trustedCIDRs string) func(http.Handler) http.Handler {
	parts := strings.Split(trustedCIDRs, ",")
	nets := make([]*net.IPNet, 0, len(parts))
	for _, cidr := range parts {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			log.Fatalf("invalid --trusted-proxy-cidrs entry %q: %v", cidr, err)
		}
		nets = append(nets, ipNet)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}
			caller := ""
			if len(nets) == 0 {
				// dev/test: accept from any source
				caller = r.Header.Get("X-Remote-User")
				if caller == "" {
					caller = r.Header.Get("X-Forwarded-User")
				}
			} else {
				host, _, err := net.SplitHostPort(r.RemoteAddr)
				if err != nil {
					host = r.RemoteAddr
				}
				srcIP := net.ParseIP(host)
				trusted := false
				for _, n := range nets {
					if srcIP != nil && n.Contains(srcIP) {
						trusted = true
						break
					}
				}
				if trusted {
					caller = r.Header.Get("X-Remote-User")
					if caller == "" {
						caller = r.Header.Get("X-Forwarded-User")
					}
				}
			}
			if caller != "" {
				r = r.WithContext(withCallerSub(r.Context(), caller))
			}
			next.ServeHTTP(w, r)
		})
	}
}
