# UI Gaps — Tracking Document

**Status**: Open  
**Branch**: `ep/governed-resource-design-updates`  
**Related code**: `cmd/dashboard/` (Go proxy `main.go`, `app.js`, `index.html`)

This document tracks all known gaps between what the gateway API supports and what the dashboard exposes. Items are ordered by severity. Close each gap as a standalone PR and mark it Done here.

---

## OIDC / Authentication Gaps

### G-1 — Proxy drops Authorization header [CRITICAL]

**File**: `cmd/dashboard/main.go:102–103`

`proxyToGateway` copies only `Content-Type` from the browser request to the upstream gateway call. The `Authorization: Bearer <token>` header is silently dropped. Every proxied call returns `401 Unauthorized` when the gateway has `--oidc-issuer-url` set.

**Current code:**
```go
if ct := r.Header.Get("Content-Type"); ct != "" {
    req.Header.Set("Content-Type", ct)
}
```

**Fix**: Forward `Authorization` (and `X-Remote-User` for proxy-header mode):
```go
for _, h := range []string{"Authorization", "X-Remote-User", "X-Forwarded-User"} {
    if v := r.Header.Get(h); v != "" {
        req.Header.Set(h, v)
    }
}
```

**Status**: Open

---

### G-2 — No token acquisition flow in the browser [CRITICAL]

There is no login page, token input field, or OIDC redirect in the dashboard. Even with G-1 fixed, the browser has no way to acquire a Bearer token to send.

**Fix (two-phase):**

**Phase A — Token paste field (immediate):** Add a settings panel where a user pastes a Bearer token. Store it in `sessionStorage`. Inject it as `Authorization: Bearer <token>` on every `fetch()` call in `app.js`. Show a "Not authenticated" banner when the token field is empty or when the gateway returns 401.

**Phase B — Browser OIDC (follow-up):** Register a `aip-dashboard` public client in Keycloak with Authorization Code + PKCE. On page load, if no token in `sessionStorage`, redirect to Keycloak login. Exchange the code for a token on callback. This is the production-grade path.

Phase A unblocks the other gaps immediately. Phase B is the correct long-term approach.

**Status**: Open

---

### G-3 — No role-aware rendering [SIGNIFICANT]

The gateway enforces agent/reviewer/admin roles but the UI renders identically for all users. Consequences:
- An agent sees Approve/Deny buttons → clicks them → gets `403`. No explanation shown.
- A reviewer sees no indication they cannot create GovernedResources.
- Once the admin tab exists (G-5), an agent would see it and get `403` on every action.

**Fix:** After token acquisition (G-2), decode the JWT payload client-side (`atob(token.split('.')[1])`). Map the identity claim (`azp` or `sub`) against what the gateway reports as the caller's role. Either:
- Add a `GET /whoami` endpoint to the gateway that returns `{"identity":"...","roles":["reviewer"]}`.
- Or simply decode locally and compare against a locally configured list.

Then conditionally render:
- Approve/Deny buttons: only when caller has reviewer role.
- Admin tab (G-5, G-6): only when caller has admin role.

**Status**: Open

---

### G-4 — X-Remote-User not forwarded (proxy-header mode) [MINOR]

When the gateway runs with `--trusted-proxy-cidrs` instead of `--oidc-issuer-url`, identity comes from `X-Remote-User`. The proxy doesn't forward this header either, so dev/test setups without OIDC are also broken end-to-end through the dashboard.

**Fix**: Covered by the header forwarding fix in G-1.

**Status**: Open (resolved when G-1 is fixed)

---

## Functional Gaps — Phase 2 Work Inaccessible

### G-5 — No GovernedResource management tab [CRITICAL]

The gateway has full `POST/GET/PUT/DELETE /governed-resources` (Phase 2) but the dashboard has no tab to reach them. Platform admins must use `curl` directly.

**Fix**: Add a "Governed Resources" tab in `index.html` and `app.js` with:
- Table listing all GovernedResources (name, uriPattern, contextFetcher, permittedActions, age)
- Create form: fields for name, uriPattern, permittedActions (comma-separated), permittedAgents, contextFetcher, description
- Edit (PUT): pre-populate form with existing spec on row click
- Delete: confirmation dialog, calls `DELETE /governed-resources/{name}`. Show a specific message when gateway returns 409 (active requests are blocking deletion)
- Visible only to admin role (G-3)

**Status**: Open

---

### G-6 — No SafetyPolicy management tab [CRITICAL]

Same as G-5 for `POST/GET/PUT/DELETE /safety-policies`.

**Fix**: Add a "Safety Policies" tab with:
- Table listing all SafetyPolicies (name, namespace, contextType, rule count, age)
- Namespace selector (reuse pattern from Diagnostics tab)
- Create/edit form: governedResourceSelector labels, contextType, rules (name + CEL expression + action)
- Delete with confirmation
- Visible only to admin role

**Status**: Open

---

## Functional Gaps — Request Detail View

### G-7 — governedResourceRef not shown in request detail [SIGNIFICANT]

`AgentRequestSpec.governedResourceRef` (name + generation — set at admission by the gateway) is never rendered. Reviewers cannot see which GovernedResource admitted the request they are evaluating.

**File**: `cmd/dashboard/app.js:renderDetails()`

**Fix**: In the "Agent Declared" panel, add a row:
```
Governed by: karpenter-nodepool-team-a (generation 3)
```
Render as a monospace chip. If `governedResourceRef` is nil, show "none (no GovernedResource matched)".

**Status**: Open

---

### G-8 — providerContext never rendered [SIGNIFICANT]

The detail view renders `req.status?.controlPlaneVerification` (the old hardcoded deployment verifier) but never renders `req.status?.providerContext` — the new generic JSON blob written by Phase 4 fetchers (Karpenter, GitHub, etc.). Reviewers will not see fetcher context even after Phase 4 is implemented.

**File**: `cmd/dashboard/app.js:renderDetails()` line 403

**Fix**: Add a `renderProviderContext(ctx)` function alongside `renderControlPlaneVerification`. Since `providerContext` is free-form JSON, render it as a collapsible formatted JSON block:
```
▶ Provider Context (karpenter)
  currentLimitCPU:   100
  currentNodeCount:  47
  pendingPods:       12
  ...
```
Show this panel when `providerContext` is non-null. It replaces (or appears alongside) `controlPlaneVerification` depending on which fetcher fired.

**Status**: Open

---

### G-9 — FetcherSchemaViolation condition not highlighted [SIGNIFICANT]

When the context fetcher returns data that violates `contextSchema`, the controller writes a `FetcherSchemaViolation` condition and leaves `providerContext` null. The reviewer is approving without live context. Currently this appears as a grey condition row indistinguishable from normal conditions.

**File**: `cmd/dashboard/app.js:renderDetails()` — conditions section

**Fix**: In the conditions loop, check for `condition.type === 'FetcherSchemaViolation'`. Render a distinct amber warning banner above the "Control Plane Verified" panel:
```
⚠ Context fetch failed — reviewer is operating without live resource data.
  [condition.message]
```

**Status**: Open

---

## Functional Gaps — Minor

### G-10 — GOVERNED_RESOURCE_DELETED denial not called out [MINOR]

When a request is denied with `status.denial.code === "GOVERNED_RESOURCE_DELETED"`, it gets the same visual treatment as a policy denial. Reviewers see no indication the underlying policy object disappeared.

**Fix**: In `renderDetails()`, when `req.status?.denial?.code === "GOVERNED_RESOURCE_DELETED"`, show a distinct red banner: *"The GovernedResource that admitted this request was deleted after submission."*

**Status**: Open

---

### G-11 — policyGeneration not shown in audit trail [MINOR]

`DenialResponse.policyResults[].policyGeneration` (Phase 1 addition) is never rendered. Useful for debugging whether a policy changed between submission and evaluation.

**Fix**: In the audit trail section, when rendering a denial event, expand `policyResults` and show `policyName + ruleNname + result + (gen N)`.

**Status**: Open

---

## Fix Priority Order

| # | Gap | Severity | Effort |
|---|-----|----------|--------|
| G-1 | Proxy drops Authorization header | Critical | 5 lines |
| G-2 | No token acquisition (Phase A: paste field) | Critical | ~50 lines JS |
| G-5 | No GovernedResource tab | Critical | New tab |
| G-6 | No SafetyPolicy tab | Critical | New tab |
| G-3 | No role-aware rendering | Significant | `whoami` + conditional render |
| G-7 | governedResourceRef not shown | Significant | 3 lines in renderDetails |
| G-8 | providerContext not rendered | Significant | New render function |
| G-9 | FetcherSchemaViolation not highlighted | Significant | Condition filter + banner |
| G-4 | X-Remote-User not forwarded | Minor | Covered by G-1 |
| G-10 | GOVERNED_RESOURCE_DELETED banner | Minor | One condition check |
| G-11 | policyGeneration in audit trail | Minor | One field |

G-1 and G-2 Phase A are prerequisites for validating anything else with OIDC. Fix them first.
