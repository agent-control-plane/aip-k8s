# AIP GitHub MCP Integration — Implementation Plan

## Overview

AIP acts as a governance layer for MCP tool calls. Agents submit intent to the AIP gateway,
receive a scoped Ed25519 JWT on approval, then call the MCP proxy which validates the JWT and
forwards the tool call to the registered MCP server (e.g. github-mcp-server).

---

## Phase 1 — JWT Infrastructure (#183) ✅ DONE (PR #188)

- `internal/jwt/manager.go` — Ed25519 JWT minting and validation
- `JWTManager` with injectable clock, RWMutex for hot-reload, `prevPublic` grace period
- `--jwt-key-path` flag in gateway `main.go`
- cert-manager `Certificate` manifest for key rotation (90-day TTL)
- Unit tests in `internal/jwt/manager_test.go`

---

## Phase 2 — MCP Registry + GitHub MCP (#184) ⚠️ PARTIAL (PR #193)

**Done (PR #193):**
- ✅ `cmd/gateway/mcp_registry.go` — `GET /mcp-registry` reads `MCP_REGISTRY` env var
- ✅ `cmd/gateway/mcp_registry_test.go`
- ✅ `config/mcp/github-mcp-deployment.yaml` — github-mcp-server Deployment
- ✅ `config/mcp/github-mcp-service.yaml` — ClusterIP Service
- ✅ `config/mcp/networkpolicy.yaml` — restricts ingress to gateway pods only
- ✅ `config/gateway-config.yaml` — ConfigMap with MCP_REGISTRY JSON

**Gap — controller-side fetcher was listed but never created in PR #193.**
The plan listed `internal/evaluation/fetchers/github_mcp.go` but only the gateway-side registry
and K8s manifests shipped. The old `github_fetcher.go` continued hitting `api.github.com` directly.

---

## Phase 3 — MCP Proxy (#185) ✅ DONE (PR #193)

- `cmd/gateway/mcp_proxy.go` — `POST /mcp-proxy/{server}/{tool}`
  - Nil-guard on `jwtManager` → 503
  - Extract + validate `Authorization: Bearer <jwt>`
  - Look up server in registry, check tool allowlist
  - Read-only tools: always allowed; write tools: `tool.Name == jwt.Action`
  - `http.MaxBytesReader` on body
  - `bindToolName` — inject validated tool name into request body
  - 30s context timeout for upstream call
  - `strings.TrimSuffix(url, "/") + "/tools/call"` for forwarding
  - `emitMCPLog` — structured stdout log (not K8s AuditRecord yet — Phase 7)
- `cmd/gateway/mcp_proxy_test.go` + `mcp_proxy_integration_test.go`
- Route registered in `main.go`

**Known gaps (deferred):**
- No test for valid-JWT happy path at handler level
- OIDC middleware wraps the proxy route — external agents need a path exemption before production
- `emitMCPLog` writes to stdout only; full K8s AuditRecord emission is Phase 7

---

## Phase 4 — Demo Agent (#186) ⏸️ REDESIGNED

**Original plan** called for a Python/LangChain agent in a separate repo (`aip-demo-agent`)
with `nodepool.yaml` scaling. The revised plan below (from `github-mcp-demo.md`) is
concrete, follows existing Go demo patterns, and targets a two-scenario narrative that
mirrors the scaledown demo — showing AIP governs both K8s and GitHub surfaces.

### Scenario

An AI agent monitors `payment-api` and wants to scale it by modifying
`infra/payment-service.json` in a GitHub repo via a pull request.

AIP intercepts the intent, reads the current config from GitHub via the
in-cluster github-mcp-server, evaluates the 90% rule, and either blocks
or approves. If approved, the agent creates the PR using the MCP proxy
with a scoped JWT.

Same narrative as the scaledown demo (agent tries to scale something it
shouldn't) but enforcement at the GitHub PR layer, not inside Kubernetes.

### Config File

Repo: `agent-control-plane/aip-demo-infra` (to be created)

`infra/payment-service.json`:
```json
{
  "service": "payment-api",
  "maxReplicas": 5,
  "absoluteMax": 20
}
```

### Two-Scenario Flow

**Scenario A — Denied (agent proposes 19 replicas, 95% of absoluteMax):**
1. Agent submits intent: `targetURI: github://agent-control-plane/aip-demo-infra/files/main/infra/payment-service.json`, `action: create_pr`, `parameters.proposedMaxReplicas: 19`
2. Controller calls `FetchGitHubMCP` → reads file → gets `absoluteMax: 20`
3. SafetyPolicy evaluates: `19 / 20 = 0.95 > 0.9` → **DENIED**
4. No PR created

**Scenario B — Approved (agent proposes 17 replicas, 85% of absoluteMax):**
1. Agent submits intent: same target, `parameters.proposedMaxReplicas: 17`
2. Controller fetches file → `17 / 20 = 0.85 <= 0.9` → **APPROVED**
3. Gateway mints scoped JWT
4. Agent calls `POST /mcp-proxy/github/create_pull_request` with JWT
5. Real PR appears on GitHub

Both scenarios run in sequence in a single demo script.

### SafetyPolicy

```yaml
- name: replica-cap-guard
  type: StateEvaluation
  action: Deny
  message: "Proposed maxReplicas exceeds 90% of absoluteMax. Reduce the request."
  expression: >
    has(request.spec.parameters) &&
    has(request.spec.parameters.proposedMaxReplicas) &&
    target != null &&
    has(target.fileContent) &&
    has(target.fileContent.absoluteMax) &&
    double(request.spec.parameters.proposedMaxReplicas) / double(target.fileContent.absoluteMax) > 0.9
```

### Infrastructure

- Kind cluster (no cloud provider needed)
- AIP gateway + controller + dashboard running locally
- github-mcp-server deployed in Kind (`ghcr.io/github/github-mcp-server`, pinned tag)
- One GitHub PAT: read scope for policy evaluation, write scope for PR creation
- One K8s Secret (`aip-github-token`) — already consumed by github-mcp-server Deployment

### Prerequisites Before Building

1. **Fix YAML-to-JSON gap in `FetchGitHubMCP`** — the fetcher currently
   falls back to a plain string for non-JSON files. Use JSON for the demo
   file to sidestep this until the fetcher is fixed.

2. **`agent-control-plane/aip-demo-infra` repo** — needs to be created
   with `infra/payment-service.json` at the above content.

3. **`parameters` field on `AgentRequest`** — verify the proposed value
   can be passed via `request.spec.parameters` and is accessible in CEL.

### Files to Create

```text
demo/github/
  run.sh                        — orchestrates both scenarios
  agent/main.go                 — ReACT loop agent (Go, like scaledown)
  k8s/resource.yaml             — GovernedResource (github:// URI pattern)
  policies/replica-cap-guard.yaml — SafetyPolicy
  README.md
```

GovernedResource:
```yaml
spec:
  uriPattern: "github://agent-control-plane/aip-demo-infra/*"
  permittedActions:
    - create_pr
  contextFetcher: github
```

### e2e Test (separate, gated)

- Build tag: `-tags mcp_e2e`
- Deploys real github-mcp-server in Kind
- Expects `AIP_E2E_GITHUB_PAT` env var → creates `aip-github-token` Secret
- Test A: submit with `proposedMaxReplicas: 19` → assert phase == Denied
- Test B: submit with `proposedMaxReplicas: 17` → assert phase == Approved → assert PR created on GitHub
- Cleanup: close PR, reset file to baseline

### Narrative Thread Across Demos

| Demo       | Agent tries to...           | AIP reads...              | Enforcement surface |
|------------|-----------------------------|---------------------------|---------------------|
| scaledown  | delete a live K8s service   | live cluster state (k8s)  | Kubernetes          |
| github     | scale via a GitHub PR       | config file (github MCP)  | GitHub              |

Same agent behaviour, two surfaces — AIP governs the whole system.

### Status

- [ ] `agent-control-plane/aip-demo-infra` repo created
- [ ] `demo/github/` scaffold
- [ ] GovernedResource + SafetyPolicy manifests
- [ ] ReACT loop agent (Go)
- [ ] `run.sh` (both scenarios, idempotent)
- [ ] e2e test (gated)
- [ ] README

---

## Phase 5 — Controller-Side GitHub MCP Fetcher ✅ DONE (PR #196)

**Gap from Phase 2 was completed here.** The `github://` URI context fetcher now calls the
in-cluster GitHub MCP server instead of the GitHub REST API directly.

**Files created:**
- `internal/evaluation/fetchers/github_mcp.go` — MCP JSON-RPC HTTP client
  - Parses `github://org/repo` and `github://org/repo/files/branch/path` URIs
  - Calls `get_file_contents` MCP tool for file-path URIs
  - Calls `list_pull_requests` MCP tool for PR context (open PR count)
  - Returns JSON with `owner`, `repo`, `branch`, `filePath`, `fileContent`, `openPRCount`
- `internal/evaluation/fetchers/github_mcp_test.go` — 7 tests
  - URI parsing (basic, with file, with branch-only, invalid, missing org)
  - File contents fetch via mocked MCP server
  - Repo-only fetch via mocked MCP server
  - MCP server unreachable
  - Invalid URI error

**Files deleted:**
- `internal/evaluation/fetchers/github_fetcher.go` — replaced by MCP-based fetcher
- `internal/evaluation/fetchers/github_fetcher_test.go` — tests no longer applicable

**Wiring:**
- `internal/controller/agentrequest_controller.go` — `case "github"` now dispatches to
  `fetchers.FetchGitHubMCP` instead of `fetchers.FetchGitHub`

**Configuration:**
- The MCP server URL defaults to `http://github-mcp.aip-k8s-system.svc` (in-cluster DNS)
- The GitHub MCP server already has the `GITHUB_PERSONAL_ACCESS_TOKEN` from
  `aip-github-token` Secret (configured in `github-mcp-deployment.yaml`)
- No additional credentials needed in the controller — auth is delegated to the MCP server

---

## Phase 6 — AIP as a Native MCP Server (#203) ❌ NOT STARTED

### Context

Phases 1–5 cover AIP *consuming* MCP servers (github-mcp-server) for provider context
and tool call forwarding. This phase covers the inverse: **AIP *exposing itself* as an
MCP server** so that MCP-native clients (Claude Code, Gemini, Codex) can point directly
at the AIP gateway and get governance for free.

Today `POST /mcp-proxy/{server}/{tool}` is a custom REST shim — not MCP protocol. An
agent using Claude Code cannot point `--mcp-server-url` at the gateway and have it work.
This phase fixes that.

Both endpoints coexist permanently:

| Endpoint | Audience |
|---|---|
| `POST /mcp-proxy/{server}/{tool}` | Non-MCP clients (K8s controllers, scripts, existing integrations) |
| `POST /mcp` | MCP-native clients (Claude Code, Gemini, Codex) |

`/mcp` and `/mcp-proxy` share the same upstream forwarding logic — `/mcp` is a
protocol-compliant front-end over the same `mcp_proxy.go` internals.

---

### Transport: Streamable HTTP (MCP spec 2025-03-26)

Target clients are Claude Code, Gemini, and Codex. All three support the
**Streamable HTTP** transport (spec 2025-03-26). This is the only transport implemented.

**`GET /mcp` SSE endpoint: explicitly out of scope for v1.**
The older HTTP+SSE transport (`GET /mcp` for server→client stream, `POST /mcp/message`
for client→server) is used by some older clients. It requires session IDs, message
queues, and per-connection goroutines. Deferred. Streamable HTTP covers 95%+ of real
clients today.

Clients that require `GET /mcp` can continue using `/mcp-proxy` directly.

---

### Design Decisions

#### Q1 — Auth model for `POST /mcp` and `/mcp-proxy`

**Decision: both endpoints on the main `mux` behind `authMiddleware` + AIP JWT for writes.**

Both `/mcp-proxy/{server}/{tool}` and `POST /mcp` are registered on the main mux so that
`authMiddleware` controls OIDC enforcement via the `authRequired` flag:

- **Local dev (no `--oidc-issuer-url`)**: `authRequired=false`, `authMiddleware` is a
  no-op proxy-header middleware — both endpoints work without any OIDC token.
- **Production (`--oidc-issuer-url` set)**: `authRequired=true`, OIDC enforced on every
  request to both endpoints.

This is not a breaking change for existing callers: anyone running without OIDC configured
today continues to work exactly as before.

Auth matrix (applies to both endpoints):

```text
authRequired=false (local dev):
  all methods / tools → no OIDC required; write tools still require AIP JWT

authRequired=true (production):
  initialize / notifications/initialized → OIDC required
  tools/list                            → OIDC required
  tools/call read tools                 → OIDC required (identity for audit trail)
  tools/call write tools                → OIDC required + X-AIP-Authorization: Bearer <aip-jwt>
```

Note on `initialize`: Streamable HTTP clients (Claude Code, Gemini, Codex) configure auth
at the server URL level and send the `Authorization` header on every request including
`initialize`. There is no pre-credentials handshake in this transport. OIDC is enforced
uniformly across all methods when `authRequired=true`.

Rationale: MCP-native clients already carry OIDC identity. The AIP JWT is an additional
authorization layer for writes, not a replacement for identity. Moving `/mcp-proxy` onto
the main mux unblocks read-path identity attribution (#204) and is consistent with
the `/mcp` endpoint design.

#### Q2 — Tool name prefixing in `tools/list`

**Decision: Option A — prefixed names (`{server}/{tool}`).**

The gateway federates multiple MCP servers (github, jira, k8s, etc.). Flat tool names
collide when two servers expose a tool with the same name (e.g. both github and jira
could expose `create_issue`). Prefixing is the standard approach in multi-server MCP
gateways.

```json
{
  "tools": [
    { "name": "github/create_pull_request", "description": "...", "inputSchema": {...} },
    { "name": "github/get_file_contents",   "description": "...", "inputSchema": {...} },
    { "name": "jira/create_issue",          "description": "...", "inputSchema": {...} }
  ]
}
```

`tools/call` uses the same prefixed name. The handler splits on `/` to resolve the
server and tool name, then delegates to the existing `findMCPServer` + `findTool`
helpers in `mcp_proxy.go`.

Read-only status, JWT enforcement, and `enforceRepoClaim` all apply identically to
`/mcp-proxy` — the prefixed name is unwrapped before reaching that logic.

#### Q3 — `GET /mcp` SSE endpoint scope

**Decision: not implemented in v1.**

Streamable HTTP (`POST /mcp` only) covers Claude Code, Gemini, and Codex. The older
HTTP+SSE transport is deferred. This is explicitly documented in the EP so reviewers
understand it is a known gap, not an oversight.

If a specific client requires `GET /mcp` it can be added as a minimal stub in a
follow-up PR without touching the core `POST /mcp` logic.

#### Q4 — E2E test coverage

**Decision: add one e2e scenario to `test/e2e_mcp/` exercising `POST /mcp`.**

Unit and integration tests cover `initialize`, `tools/list`, and error paths. One e2e
test case exercises the full `tools/call` stack for a write tool (same PR creation flow
as Scenario B) via `POST /mcp` with a prefixed tool name. This proves the entire path:
OIDC → AIP JWT → JSON-RPC dispatch → upstream MCP → PR created.

Existing Scenario A and B tests (`/mcp-proxy` path) are unchanged.

---

### JSON-RPC 2.0 Method Dispatch

`POST /mcp` accepts `Content-Type: application/json` with a JSON-RPC 2.0 envelope:

```json
{ "jsonrpc": "2.0", "id": 1, "method": "<method>", "params": { ... } }
```

Supported methods:

| Method | Auth (`authRequired=true`) | Behaviour |
|---|---|---|
| `initialize` | OIDC | Return server info, protocol version, capabilities |
| `notifications/initialized` | OIDC | No-op acknowledgement, return empty result |
| `tools/list` | OIDC | Return all tools from all registered MCP servers, prefixed |
| `tools/call` | OIDC + AIP JWT (writes) | Delegate to existing proxy logic |

Unknown methods return JSON-RPC error `-32601` (Method not found).

`initialize` response:

```json
{
  "jsonrpc": "2.0", "id": 1,
  "result": {
    "protocolVersion": "2025-03-26",
    "serverInfo": { "name": "aip-gateway", "version": "v1alpha1" },
    "capabilities": { "tools": {} }
  }
}
```

---

### Files

```text
internal/mcp/protocol.go          — JSON-RPC 2.0 request/response structs + error codes
cmd/gateway/mcp_handler.go        — handleMCP: dispatches initialize/tools/list/tools/call
cmd/gateway/mcp_handler_test.go   — unit + integration tests
cmd/gateway/main.go               — move /mcp-proxy to main mux; register POST /mcp on main mux
test/e2e_mcp/mcp_e2e_test.go     — Scenario C: tools/call via POST /mcp
docs/api-reference.md             — document /mcp endpoints and auth rules
```

No changes to controller, CRDs, or existing e2e tests.

Two changes touch `/mcp-proxy` specifically:
1. `main.go` — move registration from `publicMux` to the main mux (remove the path prefix
   check that short-circuits `authMiddleware` for `/mcp-proxy/`).
2. `mcp_proxy.go` — `handleMCPProxy` currently reads the AIP JWT from `Authorization: Bearer`.
   With OIDC consuming `Authorization`, write-tool JWT validation must move to
   `X-AIP-Authorization: Bearer <aip-jwt>`. Same header convention as `handleMCP`.

### Migration: Breaking changes to `/mcp-proxy`

The move from `Authorization` to `X-AIP-Authorization` and the re-registration on the main
mux are breaking changes for existing `/mcp-proxy` clients. Follow this migration plan.

#### 1. Client migration steps

1. **Update write-tool requests**: Change the AIP JWT header from `Authorization` to
   `X-AIP-Authorization` in every `POST /mcp-proxy/{server}/{tool}` call.
2. **Keep OIDC in `Authorization`** (if OIDC is enabled). The `Authorization` header now
   carries only the OIDC Bearer token for gateway authentication. If OIDC is not configured,
   omit `Authorization` (the gateway falls back to proxy headers).
3. **Read-only tools**: No change — they never required an AIP JWT.

**Before:**
```text
Authorization: Bearer <aip-jwt>
```

**After:**
```text
Authorization: Bearer <oidc-token>       # only if OIDC is enabled
X-AIP-Authorization: Bearer <aip-jwt>    # always for write tools
```

#### 2. Transition period and dual-header acceptance

The gateway accepts **only** `X-AIP-Authorization`. There is no fallback to `Authorization`
for the AIP JWT — the `Authorization` header is exclusively used by the OIDC middleware
(on the main mux). Clients must switch before upgrading.

#### 3. Communication plan

| Audience | Message | Channel |
|---|---|---|
| Internal agent authors | "AIP JWT header moved to `X-AIP-Authorization`. Update your HTTP client code before upgrading to `v1alpha1`." | Slack #agent-dev |
| External integrators | Patch notes + migration guide in release. | GitHub release notes |
| E2e test owners | E2e tests updated to use `X-AIP-Authorization`. | PR review (#203) |

**Rollout timeline:**
- **Phase 1 (cutover)**: Gateway starts rejecting `Authorization`-based AIP JWTs.
  All `/mcp-proxy` calls must use `X-AIP-Authorization`.
- **Phase 2 (stabilize)**: Monitor for 403/401 errors from clients that haven't migrated.
  No deprecation warning emitted because both headers cannot coexist on the main mux
  (OIDC middleware would intercept `Authorization`).

#### 4. Client checklist

- [ ] Scripts / CI pipelines that call `/mcp-proxy` with AIP JWTs
- [ ] Demo apps and example code in `config/samples/`
- [ ] E2e test suite (`test/e2e_mcp/`)
- [ ] Any client registered in `MCP_REGISTRY` or `AIP_MCP_TOKEN` env (these are upward
      tokens, not affected — only the client-to-gateway header changes)

**Validation steps:**
1. Deploy the updated gateway.
2. Call a write tool with the old `Authorization` header → expect `401`.
3. Call the same tool with `X-AIP-Authorization` → expect `200`.
4. Call a read-only tool with no AIP JWT → expect `200` (no change).
5. Run `make test-e2e-mcp` (both Scenario B and Scenario C exercise the new header).

---

### Auth Flow Detail

Both `/mcp-proxy` and `POST /mcp` share the same outer auth layer:

```text
Client → POST /mcp-proxy/{server}/{tool}   OR   POST /mcp
         └── authMiddleware (main mux)
             ├── authRequired=false → pass through (local dev, no OIDC configured)
             └── authRequired=true  → validate OIDC token, extract caller identity into ctx

         /mcp-proxy path:
             ├── if tool.ReadOnly → forward, log identity from ctx
             └── if !tool.ReadOnly
                 ├── extract X-AIP-Authorization: Bearer <aip-jwt>
                 ├── validate AIP JWT (Action == tool, Repo matches args)
                 └── forward to upstream MCP server

         POST /mcp path (handleMCP dispatches on method):
             ├── initialize / notifications/initialized → return response (identity already in ctx)
             ├── tools/list → identity from ctx used for audit log, return prefixed tool list
             └── tools/call
                 ├── resolve {server}/{tool} from prefixed name
                 ├── if tool.ReadOnly → forward, log identity
                 └── if !tool.ReadOnly
                     ├── extract X-AIP-Authorization: Bearer <aip-jwt>
                     ├── validate AIP JWT (Action == tool, Repo matches args)
                     └── forward to upstream MCP server
```

---

### Testing Strategy

**Unit tests (`mcp_handler_test.go`):**
- `initialize` returns correct protocol version and capabilities
- `notifications/initialized` returns empty result, no error
- `tools/list` returns all tools from all servers with `{server}/{tool}` prefix
- `tools/call` read tool: succeeds with OIDC only, no AIP JWT needed
- `tools/call` write tool: fails without AIP JWT (403), succeeds with valid JWT
- `tools/call` unknown prefixed name: returns JSON-RPC -32602 (invalid params)
- Unknown method: returns JSON-RPC -32601 (method not found)
- Malformed JSON body: returns JSON-RPC -32700 (parse error)

**Integration tests (in-process httptest server):**
- Full `initialize` → `tools/list` → `tools/call` sequence
- Write tool with mismatched Repo claim rejected
- Read tool with no AIP JWT succeeds

**E2E test (Scenario C in `test/e2e_mcp/`):**
- `POST /mcp` with `tools/call` for `github/create_pull_request`
- Same PR creation flow as Scenario B — proves the full stack end-to-end via native MCP

---

### What This Unlocks

Once this lands, the gateway is a real MCP server. A developer can add it to Claude Code:

```json
{
  "mcpServers": {
    "aip": {
      "url": "https://aip-gateway.internal/mcp",
      "headers": { "Authorization": "Bearer <oidc-token>" }
    }
  }
}
```

Every tool call goes through AIP governance automatically. No agent code changes needed.

---

## Phase 7 — `cmd/aip-mcp` Binary Split ❌ NOT STARTED (deferred)

Extract the MCP proxy out of the gateway binary into its own `cmd/aip-mcp` binary.

**Why deferred:** Proxy in gateway is fine for demo. This is an operational concern, not a
correctness concern. Split before Phase 8 CRDs are wired in — easier to refactor the boundary
clean than retrofit it later.

**TODO comment to add in Phase 3 code:**
```go
// TODO(Phase 7): extract handleMCPProxy into cmd/aip-mcp binary
```

---

## Phase 8 — `AgentRegistration` + Scoped External Credentials 📋 EP COMPLETE

**Full design:** `ep/agent-registration.md`
(currently at `/Users/ravisantosh/ep-agent-registration.md` — move to repo when work begins)

**Start condition:** Phase 5 (PR #196) merged.

Addresses three compounding gaps:

1. **Unregistered agents act freely.** Any identity string can submit `AgentRequest`s. Even
   with OIDC enabled, the gateway validates the token but never checks whether this agent
   was provisioned by an operator.

2. **Shared downstream credentials lose agent identity.** Every agent uses the MCPServer's
   single `BearerTokenSecretRef`. GitHub audit log, Azure activity log, AWS CloudTrail all
   show the gateway's service identity, not the acting agent's. K8s audit logs have the same
   problem: every API call is `system:serviceaccount:aip-system:aip-gateway`.

3. **Agent identity config has no home.** `AgentTrustProfile` is a controller-managed
   measurement object. Its spec is write-once with one field (`agentIdentity`). Outbound
   credentials, OIDC subject bindings, and K8s impersonation config have nowhere to live.

**Design decisions (locked):**

- `AgentRegistration` is the single operator-owned source of truth for agent identity config:
  OIDC inbound validation, outbound credential bindings, and K8s impersonation config.
- `AgentTrustProfile` remains a pure controller-managed measurement object. No identity
  config lives on ATP. The gateway maintains a separate `registrationCache` watch/cache
  (same pattern as `mcpServerCache` for MCPServer CRDs) — no mirroring between objects.
- Three outbound credential types:
  - `StaticSecret` — GitHub PAT stored in a K8s Secret
  - `AzureWorkloadIdentity` — **client_credentials + Federated Identity Credential** (NOT OBO).
    Agent's Keycloak token is the `client_assertion`. Azure returns a token as the agent's
    own app registration identity, not as the gateway acting on behalf of a user.
  - `AWSWebIdentity` — `AssumeRoleWithWebIdentity`. Agent's Keycloak token is the
    `WebIdentityToken`. STS returns temp creds scoped to the agent's IAM role.
- Token cache: output token keyed on stable identity `(agentIdentity, service)`, NOT on the
  input OIDC token. Lazy refresh within buffer. Singleflight prevents thundering herd on
  cache cold start. No background refresh — input OIDC token only present during requests.
- `--unregistered-agent-policy=allow|warn|strict` flag; `allow` is the default for backward
  compatibility. `warn` annotates `AgentRequest` with `governance.aip.io/unregistered=true`,
  enabling SafetyPolicy rules to require approval. `strict` returns 403.
- K8s impersonation: when `spec.k8sImpersonation` is set, K8s API calls use
  `Impersonate-User: aip:<agentIdentity>`. Gateway SA requires `impersonate` verb scoped to
  the `aip:` prefix.
- AWS Bedrock uses SigV4, not Bearer tokens. The gateway stays credential-model-uniform via
  a thin proxy layer in front of Bedrock — gateway does not sign AWS requests.
- `AgentPolicy` CRD: **dropped**. Admission policy is handled by existing
  `GovernedResource.spec.permittedAgents` + `SafetyPolicy`. No new CRD needed.
- `MCPServer registry becomes a CRD`: ✅ **already done in Phase 3.**

**Implementation sub-phases:**

| Sub-phase | Content |
|---|---|
| 8.1 | `AgentRegistration` CRD + controller: pre-create ATP on Registration create/update |
| 8.2 | Gateway `registrationCache` + `--unregistered-agent-policy` + OIDC subject validation (fix 400→403) |
| 8.3 | `CredentialProvider` interface + `StaticSecretProvider`, `AzureWorkloadIdentityProvider`, `AWSWebIdentityProvider` + MCP credential selection |
| 8.4 | K8s impersonation: per-agent impersonating REST client; Helm/RBAC `impersonate` verb |
| 8.5 | E2e: per-agent Keycloak credentials for GitHub MCP; stub-server token exchange; cloud suite skeletons (`azure_e2e`, `aws_e2e` build tags) |

**Open questions (resolve before coding):**

1. Namespace scope of `AgentRegistration`: Namespaced (matches ATP, default) vs Cluster-scoped
   (cleaner for agents that span namespaces, but requires cluster-admin to provision).
2. Relationship to `ep/agent_identity.md` (inbound API-key auth for non-OIDC agents): merge
   into one CRD or keep separate with a cross-reference.
3. AWS SigV4 proxy API contract: follow-up EP/ADR required before sub-phase 8.3.

---

## Open Issues

| # | Title | Blocks |
|---|---|---|
| 184 | MCP registry + GitHub MCP integration | ⚠️ Partial — fetcher gap done in Phase 5 |
| 185 | MCP proxy with JWT validation | ✅ Done |
| 186 | GitHub PR governance demo agent | Phase 4 — redesigned (see demo/github/) |
| 64  | Capability-based enforcement / scoped tokens | Phase 8 |
| 118 | GovernedResource binding + enforcement plugins | Phase 8 |
| 34  | Git repository state context fetcher | Phase 8 |
