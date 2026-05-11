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

## Phase 4 — Demo Agent (#186) ❌ NOT STARTED

New repo: `agent-control-plane/aip-demo-agent` (Python)

**Scenario:** Agent monitors infra config, proposes nodepool scaling change:
1. Read `nodepool.yaml` from `agent-control-plane/aip-demo-nodepool` repo
2. Detect: `maxNodes: 5`, propose `8`
3. Submit intent to AIP gateway: `create_pr` for `github://agent-control-plane/aip-demo-nodepool`
4. Gateway evaluates SafetyPolicy (max increment, cost threshold, business hours)
5. Approved → gateway mints JWT
6. Agent calls `POST /mcp-proxy/github/create_pull_request` with JWT
7. PR created → human reviews in GitHub UI

**Files:**
- `main.py` — LangChain agent with tools: `read_logs`, `read_file`, `create_infrastructure_change`
- Two modes: `--dry-run` (no GitHub calls), `--live` (real PR, needs PAT in cluster)
- `requirements.txt` — `langchain`, `requests`
- `README.md` — step-by-step blog companion (< 30 min follow-along)

**Dependencies:** Requires #183, #184, #185 merged (all done). Requires `aip-demo-nodepool` repo created.

---

## Phase 5 — Controller-Side GitHub MCP Fetcher ✅ DONE (PR #TBD)

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

## Phase 6 — `cmd/aip-mcp` Binary Split ❌ NOT STARTED (deferred)

Extract the MCP proxy out of the gateway binary into its own `cmd/aip-mcp` binary.

**Why deferred:** Proxy in gateway is fine for demo. This is an operational concern, not a
correctness concern. Split before Phase 7 CRDs are wired in — easier to refactor the boundary
clean than retrofit it later.

**TODO comment to add in Phase 3 code:**
```go
// TODO(Phase 6): extract handleMCPProxy into cmd/aip-mcp binary
```

---

## Phase 7 — `AgentRegistration` + `AgentPolicy` CRDs ❌ NOT STARTED (design locked, deferred)

**Design decisions (locked):**
- Platform team creates `AgentRegistration` (identity) — agents cannot self-register
- Operators create `AgentPolicy`, platform team approves — two-party authorization
- Read-only MCP tool calls pass through without an `AgentRequest`
- Write tool calls require a matching approved `AgentRequest` (enforced in proxy)
- `MCPServer` registry becomes a CRD (not env var) with constraint: policy may only reference
  servers listed in the registry
- Scaling playbook for Jira/Salesforce: same pattern — register MCP server, write policy,
  reads pass through, writes governed

**Full K8s AuditRecord emission for every proxy call** (replaces stdout-only `emitMCPLog`).

---

## Open Issues

| # | Title | Blocks |
|---|---|---|
| 184 | MCP registry + GitHub MCP integration | ⚠️ Partial — fetcher gap done in Phase 5 |
| 185 | MCP proxy with JWT validation | ✅ Done |
| 186 | LangChain demo agent | Phase 4 |
| 64  | Capability-based enforcement / scoped tokens | Phase 7 |
| 118 | GovernedResource binding + enforcement plugins | Phase 7 |
| 34  | Git repository state context fetcher | Phase 7 |
