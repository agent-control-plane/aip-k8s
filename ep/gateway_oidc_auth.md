# Design: Gateway OIDC/JWT Authentication and Authorization

## Problem

The gateway has no authentication. Any network-reachable client can create
`AgentRequest`s, approve them, and transition their state — meaning an agent
can approve its own requests and bypass the entire governance model. This is the
sole blocker between the current `--dry-run` posture and enabling real mutating
actions through the governance flow.

`X-Remote-User` / `X-Forwarded-User` headers are already used to establish
reviewer identity on verdict writes. This is a trust-on-header pattern that only
works correctly behind an authenticating proxy (e.g., Istio + OIDC). For
deployments without such a proxy, the gateway must validate identity itself.

## Non-Goals

- Kubernetes `TokenReview` auth. Ties the gateway to the same cluster as the
  agent; breaks cross-cluster and non-K8s agents. Explicitly out of scope.
- mTLS between agent and gateway. Valid long-term but adds certificate management
  overhead. OIDC is sufficient and simpler for v1.
- Per-resource authorization (e.g., agent A can only create requests for
  namespace X). Coarse role separation (`agent` vs `reviewer`) is the right
  first boundary. Fine-grained RBAC can be layered on top later.
- Multi-tenant namespace isolation. All namespaces in the cluster share the same
  gateway instance for now.

## Design

### Part 1: OIDC token validation middleware

Two new flags on the gateway binary:

```text
--oidc-issuer-url   string   OIDC provider URL (e.g. https://accounts.google.com).
                             When set, all non-healthz endpoints require a valid
                             Bearer token. When unset, auth is disabled (dev/test only).
--oidc-audience     string   Expected JWT `aud` claim. Defaults to "aip-gateway".
```

The middleware validates tokens using `github.com/coreos/go-oidc/v3/oidc`. This
library fetches and caches the provider's JWKS from `.well-known/openid-configuration`
automatically, handles key rotation, and validates signature, expiry, issuer, and
audience. No custom crypto code.

```go
func newOIDCMiddleware(ctx context.Context, issuerURL, audience string) (func(http.Handler) http.Handler, error) {
    provider, err := oidc.NewProvider(ctx, issuerURL)
    if err != nil {
        return nil, fmt.Errorf("oidc provider: %w", err)
    }
    verifier := provider.Verifier(&oidc.Config{ClientID: audience})
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
            var claims struct {
                Sub string `json:"sub"`
            }
            if err := idToken.Claims(&claims); err != nil || claims.Sub == "" {
                writeError(w, http.StatusUnauthorized, "token missing sub claim")
                return
            }
            next.ServeHTTP(w, r.WithContext(withCallerSub(r.Context(), claims.Sub)))
        })
    }, nil
}
```

The `sub` claim is injected into the request context. All handlers that currently
read `X-Remote-User` / `X-Forwarded-User` switch to reading from context instead.
This preserves the existing proxy-header path as a fallback when OIDC is disabled.

The middleware is applied to the entire mux except `/healthz` and `/readyz`, which
must remain unauthenticated for liveness/readiness probes.

### Part 2: Role mapping (identity → agent or reviewer)

Encoding roles in JWT claims requires every IdP to be configured to emit a custom
`role` claim. This is operationally painful and couples the gateway config to each
IdP's claim vocabulary.

Instead, roles are declared in gateway config via two flags:

```text
--agent-subjects     string   Comma-separated list of JWT `sub` values that are
                              permitted to act as agents (create requests, record
                              diagnostics, transition state).
--reviewer-subjects  string   Comma-separated list of JWT `sub` values that are
                              permitted to act as reviewers (approve/deny requests,
                              write verdicts).
```

The gateway resolves the role at request time from the validated `sub` claim:

```go
type roleConfig struct {
    agentSubs    map[string]bool
    reviewerSubs map[string]bool
}

func (rc *roleConfig) isAgent(sub string) bool    { return rc.agentSubs[sub] }
func (rc *roleConfig) isReviewer(sub string) bool { return rc.reviewerSubs[sub] }
```

This keeps IdP config minimal (no custom claims) and puts authorization logic
entirely in the control plane, where it belongs.

### Part 3: Endpoint authorization table

| Endpoint | Required role | Rationale |
|---|---|---|
| `GET /healthz`, `GET /readyz` | None | Probe endpoints must be unauthenticated |
| `GET /agent-requests` | Any authenticated | Read-only |
| `GET /agent-requests/{name}` | Any authenticated | Read-only |
| `POST /agent-requests` | `agent` | Declaring intent |
| `POST /agent-requests/{name}/executing` | `agent` (creator only) | Own state transition |
| `POST /agent-requests/{name}/completed` | `agent` (creator only) | Own state transition |
| `POST /agent-requests/{name}/approve` | `reviewer` | **Must not be self-approving** |
| `POST /agent-requests/{name}/deny` | `reviewer` | Same |
| `GET /agent-diagnostics` | Any authenticated | Read-only |
| `GET /agent-diagnostics/{name}` | Any authenticated | Read-only |
| `POST /agent-diagnostics` | `agent` | Recording observations |
| `PATCH /agent-diagnostics/{name}/status` | `reviewer` | Verdict write |
| `POST /agent-diagnostics/recompute-accuracy` | `reviewer` | Admin operation |
| `GET /diagnostic-accuracy-summaries` | Any authenticated | Read-only |
| `GET /audit-records` | Any authenticated | Read-only |

The "creator only" constraint on `executing` and `completed` transitions: the
gateway reads the `AgentRequest`'s `spec.agentIdentity` and compares it to the
validated `sub`. If they don't match, 403. This prevents one agent from
transitioning another agent's request.

The non-self-approval constraint on `approve` and `deny`: even a valid reviewer
MUST NOT approve or deny their own agent request. The gateway reads the
`AgentRequest`'s `spec.agentIdentity` and compares it to the validated `sub`.
If they match, return 403 with `"self-approval not permitted"` before any state
transition or side effect. This check runs after the `reviewer` role check.

Authorization enforcement is a thin wrapper per handler, not middleware, because
each endpoint has different role requirements. A helper covers the common cases:

```go
func requireRole(rc *roleConfig, role string, sub string, w http.ResponseWriter) bool {
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
```

### Part 4: Backward compatibility — proxy-header fallback

Deployments behind an authenticating proxy (Istio + OIDC, oauth2-proxy) that
inject `X-Remote-User` / `X-Forwarded-User` continue to work when `--oidc-issuer-url`
is unset. When OIDC is enabled, proxy headers are ignored — the validated `sub`
from the JWT is authoritative.

When OIDC is disabled, proxy headers are accepted only from trusted source IPs.
Accepting these headers from arbitrary clients is equivalent to having no auth
at all — any client can forge `X-Remote-User: sre@example.com`.

One new flag:

```text
--trusted-proxy-cidrs   string   Comma-separated list of CIDR ranges whose
                                 requests may supply X-Remote-User /
                                 X-Forwarded-User headers (e.g. "10.0.0.0/8,
                                 192.168.0.0/16"). When unset, proxy headers
                                 are accepted from any source (dev/test only).
                                 Ignored when --oidc-issuer-url is set.
```

Identity resolution when OIDC is disabled:

1. Parse `r.RemoteAddr` to extract the source IP.
2. If `--trusted-proxy-cidrs` is set and the source IP does not fall within any
   listed CIDR, reject proxy headers and treat the request as unauthenticated
   (401 if the endpoint requires identity, passthrough for read-only endpoints).
3. If the source IP is trusted (or `--trusted-proxy-cidrs` is unset), read
   `X-Remote-User` then `X-Forwarded-User` as before.

This preserves zero breaking change for existing proxy-based deployments (the
proxy runs in a known CIDR) while closing the header-spoofing hole.

### Part 5: Body size limit

Add `http.MaxBytesReader` on all POST/PATCH handlers:

```go
r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
```

This prevents OOM from large request bodies. A single line per mutating handler.

### Part 6: Pod security context in Helm chart

Add to `deployment.yaml` for all three components (controller, gateway, dashboard):

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  seccompProfile:
    type: RuntimeDefault
  capabilities:
    drop: ["ALL"]
```

Standard hardening. Required for any CIS benchmark or security review.

## Deployment flow

1. Configure your OIDC provider (Keycloak, Okta, Auth0, Google, etc.) to issue
   tokens with `aud: aip-gateway`.
2. Add `--oidc-issuer-url` and `--oidc-audience` to the gateway Helm values.
3. Populate `--agent-subjects` with the agent's service identity (`sub` claim).
4. Populate `--reviewer-subjects` with SRE team identities.
5. Remove `--dry-run` flag from the agent deployment.

## What this unblocks

Real mutating actions through the governance flow. An agent can no longer
self-approve. SRE identity on verdict writes is cryptographically verified, not
header-trusted.
