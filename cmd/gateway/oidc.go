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

type roleConfig struct {
	agentSubs    map[string]bool
	reviewerSubs map[string]bool
}

func newRoleConfig(agentList, reviewerList string) *roleConfig {
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
		agentSubs:    parse(agentList),
		reviewerSubs: parse(reviewerList),
	}
}

// isAgent returns true if sub is permitted to act as an agent.
// Open mode (both lists empty) permits any caller; once either list is non-empty
// both roles are enforced so that partial configuration cannot leave one role open.
func (rc *roleConfig) isAgent(sub string) bool {
	if len(rc.agentSubs) == 0 && len(rc.reviewerSubs) == 0 {
		return true
	}
	return rc.agentSubs[sub]
}

// isReviewer returns true if sub is permitted to act as a reviewer.
// Open mode (both lists empty) permits any caller; once either list is non-empty
// both roles are enforced so that partial configuration cannot leave one role open.
func (rc *roleConfig) isReviewer(sub string) bool {
	if len(rc.agentSubs) == 0 && len(rc.reviewerSubs) == 0 {
		return true
	}
	return rc.reviewerSubs[sub]
}

func requireRole(rc *roleConfig, role, sub string, w http.ResponseWriter) bool {
	switch role {
	case "agent":
		if !rc.isAgent(sub) {
			writeError(w, http.StatusForbidden, "agent role required")
			return false
		}
	case "reviewer":
		if !rc.isReviewer(sub) {
			writeError(w, http.StatusForbidden, "reviewer role required")
			return false
		}
	}
	return true
}

// newOIDCMiddleware creates JWT validation middleware.
// identityClaim is the token claim used as the caller identity (e.g. "azp",
// "sub", "appid", "email"). If the claim is absent the middleware falls back
// to "sub".
func newOIDCMiddleware(ctx context.Context, issuerURL, audience, identityClaim string) (func(http.Handler) http.Handler, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc provider: %w", err)
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: audience})
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
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
			var allClaims map[string]interface{}
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
			next.ServeHTTP(w, r.WithContext(withCallerSub(r.Context(), identity)))
		})
	}, nil
}

// claimString extracts a string value from a decoded claims map.
func claimString(claims map[string]interface{}, name string) string {
	v, _ := claims[name].(string)
	return v
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
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
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
