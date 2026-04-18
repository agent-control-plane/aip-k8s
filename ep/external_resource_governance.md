# Design: External Resource Governance

**Status**: Draft

## 1. Problem Statement

The existing `GovernedResource` design governs K8s resources that exist at evaluation time. Extending it to external systems (GitHub, Terraform, Jira) raises three concerns:

1. **The Ordering Problem**: Agents that open GitHub PRs or Terraform plans — the artifact (PR, plan) does not exist at approval time (T1).
2. **The Enforcement Gap**: External systems have no native AIP admission hooks. There is no equivalent of a K8s webhook for a GitHub merge or a Terraform apply.
3. **The Revocation Gap**: TTL-based tokens (proposed in Issue #64) cannot be revoked. An admin denial after token issuance has no effect until expiry.

**Resolution for Problem 1**: The ordering problem is a false problem for the common case. The agent's target is not the PR — it is the **file the PR will modify**. The file already exists at approval time. A single-mode `AgentRequest` with a `github://` URI governs the file, not the delivery artifact. `StateFingerprint` is the file's blob SHA.

**Resolution for Problems 2 and 3**: AIP registers a webhook with the external platform (GitHub, Terraform Cloud) and receives events when mutations occur. At event time, AIP verifies that the actual changed resources match what was declared in the `AgentRequest` and that the base state has not drifted since T1. Because verification is live, revocation is instant — a denied or expired `AgentRequest` blocks the platform action immediately.

**External agent registration**: agents running outside the cluster (AWS Lambda, SaaS bots, scripts integrating with Salesforce or Snowflake) authenticate via API key or OIDC federation and are registered as `AgentIdentity` objects. Authorization to act on external resources is granted by listing the `AgentIdentity` name in `GovernedResource.spec.permittedAgents`. See `ep/agent_identity.md` for the full design.

---

## 2. Design Goals

1. **Govern the resource, not the delivery mechanism**: An `AgentRequest` targets the file or resource that will be mutated, not the PR or plan artifact.
2. **AIP is the active verifier**: AIP receives platform events via webhooks and sets enforcement status directly. The agent is not in the verification trust chain.
3. **Verify what actually changed**: At webhook time, AIP fetches the PR's changed files and verifies they match the declared `AgentRequest` target. Intent drift (agent approved for file A, opened PR for file B) is caught immediately.
4. **Plugin SDK for community ecosystem**: Each external system is a plugin implementing a stable interface. Adding a new platform requires no changes to core aip-k8s code.
5. **Single `GovernedResource` match per request**: One request → one `GovernedResource`. Multi-GR overlap is a Phase 2 problem.
6. **Graded enforcement model**: Not all platforms support native gates. The enforcement model is graded by platform capability — hard enforcement is preferred but not universally achievable:

   | Tier | Mechanism | How | Example |
   |---|---|---|---|
   | Hard | Webhook + status | Platform pushes events to AIP; AIP sets required status | GitHub, Terraform Cloud |
   | Cooperative | `/verify` SDK call | Agent calls AIP before acting; spec.md §4.4 | Jira SDK, any custom agent |
   | Audit | Proxy intercept | AIP proxy intercepts and records; blocks if NetworkPolicy enforced | Slack, PagerDuty |

   `spec.md §4.4 /verify` remains the cooperative path for platforms and agents that cannot participate in the webhook model. Webhooks are the hard enforcement path. Both are first-class.

---

## 3. Core Design: Single-Mode for Known Resources

### 3.1 The Enforcement Flow

An agent that wants to update `nodepools/team-a/config.yaml`:

```
T1 — Intent Declaration
  Agent  →  AIP Gateway:  AgentRequest {
                            target.uri: github://myorg/infra/files/main/nodepools/team-a/config.yaml
                            action:     open-pr
                          }
  AIP Controller:  matches GovernedResource pattern
                   fetches blob SHA from GitHub → stores in status.evaluatedStateFingerprint
                   evaluates SafetyPolicies
                   phase → Approved
                   mints scoped GitHub installation token (repo:myorg/infra, contents:write, expires: timeBoundSeconds)
  AIP Gateway  →  Agent:  Grant { token: <installation token> }

T2 — Execution
  Agent  →  GitHub:  OpenPR(branch, commit: nodepools/team-a/config.yaml, title: "[aip:req-xyz] scale team-a")

T3 — Webhook Verification (triggered by GitHub, not the agent)
  GitHub  →  AIP:  POST /hooks/github  (pull_request event: opened/synchronized)
  AIP:         fetch PR changed files via GitHub Contents API
               check: changed files ⊆ AgentRequest target URI     ← catches intent drift
               check: base blob SHA == status.evaluatedStateFingerprint  ← catches state drift
               check: AgentRequest phase == Approved               ← catches revocation
  AIP  →  GitHub:  set commit status "AIP Governance" = success | failure + reason

T4 — Merge Gate
  Engineer clicks merge → GitHub checks required status "AIP Governance"
                        → blocked if AIP status is failure
```

The agent's scoped token expires after `timeBoundSeconds`. After that, the agent has zero GitHub write access. The PR may still exist but the AIP status will reflect the expired/revoked state if someone tries to re-run the check.

### 3.2 What AIP Verifies at Webhook Time

The webhook handler runs the same checks regardless of platform:

1. **Phase check**: Is the `AgentRequest` in `Approved` or `Executing` phase?
2. **Intent drift check**: Are all changed resources covered by `spec.target.uri`? If the agent opened a PR touching `team-b/config.yaml` when approved for `team-a/config.yaml`, this fails.
3. **State drift check**: Does the base file's current blob SHA match `status.controlPlaneVerification.evaluatedStateFingerprint`? Catches out-of-band changes between T1 and T3.
4. **Generation check**: Does the `AgentRequest` generation match what was approved? Detects spec drift.

These checks run on every push to the PR branch (not just on open), so a new commit adding unauthorized files triggers a re-check.

**GitHub-specific**: changed files and their SHAs are retrieved via the PR Files API in a single paginated call:
```
GET /repos/{owner}/{repo}/pulls/{pull_number}/files
```
Each entry returns `filename` and `sha` (the blob SHA of the file in the PR's head commit). This is one API call regardless of how many files changed — no per-file Contents API calls. The `sha` from this response is compared against `status.controlPlaneVerification.evaluatedStateFingerprint` for the state drift check.

### 3.3 Scoped Credential Delivery

When an `AgentRequest` transitions to `Approved`, the agent retrieves a scoped, short-lived credential via a dedicated subresource endpoint on the gateway — **never written to etcd**:

```
POST /agent-requests/{name}/token
```

This mirrors the K8s `TokenRequest` pattern (`POST /api/v1/namespaces/ns/serviceaccounts/sa/token`). The gateway calls the `CredentialMinter` plugin for the target URI scheme and returns the credential in the response body. Callers must be authenticated as the agent identity that created the request.

```
# Agent waits for Approved phase, then:
POST /agent-requests/req-xyz/token
Authorization: Bearer <agent-token>

Response:
{
  "token": "<scoped-github-installation-token>",
  "expiresAt": "2026-04-17T14:00:00Z",
  "scheme": "github"
}
```

Platform-specific credential scoping:
- **GitHub**: App installation token scoped to `org/repo`, `contents:write + pull_requests:write`, maximum TTL 1 hour (GitHub App limit).
- **Terraform Cloud**: workspace-scoped API token, expires at `timeBoundSeconds`.
- **K8s**: `TokenRequest` subresource on the agent's `ServiceAccount`, bound to `AgentRequest` name, expires at `timeBoundSeconds`.

The `/token` endpoint is callable while the `AgentRequest` is in `Approved` or `Executing` phase. Each call mints a fresh credential. If the `AgentRequest` is revoked between calls, subsequent calls return `409 Conflict`. Previously minted credentials remain valid until their own expiry — the blast radius is bounded by the platform TTL (maximum 1 hour for GitHub App installation tokens).

The agent's baseline identity has no write access to any external system. All write access is mediated through AIP-issued scoped credentials.

```go
result, err := client.GovernedAction(ctx, aip.GovernedActionOpts{
    Action:           "open-pr",
    Target:           "github://myorg/infra/files/main/nodepools/team-a/config.yaml",
    Reason:           "team-a batch job requires 2x node capacity",
    TimeBoundSeconds: 3600,
}, func(grant *aip.Grant) error {
    // SDK calls POST /agent-requests/{name}/token internally.
    // grant.GitHubToken: scoped installation token, expires ≤ 1 hour.
    // After this function returns, the agent has no GitHub write access.
    gh := github.NewClient(oauth2.NewClient(ctx,
        oauth2.StaticTokenSource(&oauth2.Token{AccessToken: grant.GitHubToken})))
    return openPR(ctx, gh, grant.Name, newNodePoolConfig)
})
```

---

## 4. Plugin Architecture

Each external system is a plugin. Plugins are registered in a central registry keyed by URI scheme. The controller and gateway dispatch through the registry — no platform-specific logic in core code.

### 4.1 Plugin SDK Package

Community plugin authors import one package only:

```
github.com/agent-control-plane/aip-k8s/plugin
```

This package contains the interface definitions and shared types. It has no dependency on controller-runtime, K8s client, or gateway internals.

### 4.2 Plugin Interfaces

```go
// Plugin is the minimum required interface. Every plugin must implement this.
type Plugin interface {
    Scheme() string   // "github", "terraform", "jira"
    FetchContext(ctx context.Context, uri string) (*Context, error)
}

// CredentialMinter is implemented by plugins that issue scoped credentials at approval time.
// Optional. If not implemented, the agent must supply its own credentials (audit-grade only).
type CredentialMinter interface {
    MintCredential(ctx context.Context, req *CredentialRequest) (*Credential, error)
}

// WebhookPlugin is implemented by plugins for platforms that push events to AIP.
// AIP registers a webhook with the platform; the platform calls back when mutations occur.
// AIP verifies the event and sets a platform status check (pass/fail).
// Optional. Provides hard enforcement. Preferred over ProxyPlugin when the platform supports it.
type WebhookPlugin interface {
    ValidateWebhook(r *http.Request) error
    ExtractEvent(r *http.Request) (*EnforcementEvent, error)
    WriteResult(w http.ResponseWriter, result *VerifyResult)
}

// ProxyPlugin is implemented by plugins for platforms with no native event push.
// The AIP egress proxy intercepts outgoing HTTP calls and verifies before forwarding.
// Optional. Fallback enforcement for systems that cannot push events (Slack, some Jira configs).
type ProxyPlugin interface {
    IsMutation(req *http.Request) bool
    ExtractURI(req *http.Request) (string, error)
    ExtractAction(req *http.Request) (string, error)
}
```

A platform implements the interfaces it supports. Capability is discovered at runtime via type assertion:

```go
// Gateway webhook endpoint — platform-agnostic dispatch
if wp, ok := registry.WebhookPlugin(scheme); ok {
    // platform pushes events: GitHub, Terraform Cloud, Jira (with webhook registration)
    handleWebhook(w, r, wp)
} else if pp, ok := registry.ProxyPlugin(scheme); ok {
    // proxy intercepts outgoing calls: Slack, PagerDuty
    handleProxyRequest(w, r, pp)
}
```

### 4.3 Shared Types

```go
// Context is returned by FetchContext.
type Context struct {
    StateFingerprint string                 // blob SHA, state serial, ETag — opaque string
    ResourceExists   bool
    Raw              map[string]interface{} // stored in AgentRequestStatus.ProviderContext
}

// EnforcementEvent is the normalized event from a platform webhook.
type EnforcementEvent struct {
    AgentRequestName string            // extracted from PR title, run message, etc.
    ChangedResources []string          // URIs of resources being mutated
    BaseFingerprints map[string]string // current fingerprint per changed resource
    Action           string
}

type CredentialRequest struct {
    URI              string
    Action           string
    TTL              time.Duration
    AgentRequestName string
}

type Credential struct {
    Token     string
    ExpiresAt time.Time
}
```

### 4.4 Registration

Plugins self-register via `init()`, identical to the `database/sql` driver pattern:

```go
// In the plugin package:
func init() {
    aipplugin.Register(New(configFromEnv()))
}

// In cmd/controller/main.go — blank imports activate plugins:
import (
    _ "github.com/agent-control-plane/aip-k8s-plugin-github"
    _ "github.com/agent-control-plane/aip-k8s-plugin-terraform"
    _ "github.com/acme-corp/aip-k8s-plugin-servicenow"  // community plugin
)
```

### 4.5 Webhook Endpoint

`POST /hooks/{scheme}` is a single generic endpoint. Each plugin handles its own signature validation and payload parsing:

```go
// Gateway: POST /hooks/{scheme}
func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    scheme := chi.URLParam(r, "scheme")
    wp, ok := registry.WebhookPlugin(scheme)
    if !ok {
        http.NotFound(w, r)
        return
    }
    if err := wp.ValidateWebhook(r); err != nil {
        http.Error(w, "invalid signature", http.StatusUnauthorized)
        return
    }
    event, err := wp.ExtractEvent(r)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    result := h.verifyEvent(r.Context(), event)  // platform-agnostic verification
    wp.WriteResult(w, result)
}
```

The `verifyEvent` logic is platform-agnostic and shared across all webhook plugins:

```go
func (g *Gateway) verifyEvent(ctx context.Context, event *plugin.EnforcementEvent) *VerifyResult {
    ar, err := g.getAgentRequest(ctx, event.AgentRequestName)
    if err != nil {
        return denied("request not found")
    }
    if ar.Status.Phase != PhaseApproved && ar.Status.Phase != PhaseExecuting {
        return denied("request not in Approved phase — may have been revoked")
    }
    for _, changedURI := range event.ChangedResources {
        if !uriMatchesTarget(changedURI, ar.Spec.Target.URI) {
            return denied(fmt.Sprintf("resource %s not covered by this request (intent drift)", changedURI))
        }
        if fp, ok := event.BaseFingerprints[changedURI]; ok {
            if fp != ar.Status.ControlPlaneVerification.EvaluatedStateFingerprint {
                return denied("resource state changed since approval (state drift)")
            }
        }
    }
    return verified()
}
```

---

## 5. Platform Tiers

| Platform | ContextFetcher | CredentialMinter | WebhookPlugin | ProxyPlugin | Enforcement |
|---|---|---|---|---|---|
| K8s | resourceVersion | `TokenRequest` | — (admission webhook) | — | Hard |
| GitHub | blob SHA | App installation token | `pull_request` events | optional | Hard |
| Terraform Cloud | state serial | workspace token | run task callback | — | Hard |
| Jira | issue state | OAuth 2.0 token | issue transition webhook | fallback | Hard / Audit |
| ArgoCD | sync status | project token | sync webhook | — | Hard |
| Slack | — | bot token | — | channel proxy | Audit |
| PagerDuty | incident state | scoped token | — | API proxy | Audit |

Hard enforcement requires either a `WebhookPlugin` (platform pushes events to AIP) or a `ProxyPlugin` with NetworkPolicy restricting direct egress. Audit-grade applies when neither is feasible.

---

## 6. Three Concrete Examples

### 6.1 Karpenter GitOps (GitHub)

**Setup** (done once by platform engineer):
- Register webhook `https://aip.internal/hooks/github` on `myorg/infra` repo
- Mark `AIP Governance` as a required status check on `main`

**Per-operation flow**:
- Agent submits `AgentRequest` for `github://myorg/infra/files/main/nodepools/team-a/config.yaml`, `action: open-pr`
- Controller fetches blob SHA → stores in `status.evaluatedStateFingerprint` → `Approved`
- Agent receives `Grant.GitHubToken` (scoped to `myorg/infra`, `contents:write`, 1h)
- Agent creates branch, commits `nodepools/team-a/config.yaml`, opens PR titled `[aip:req-xyz] scale team-a`
- GitHub fires `pull_request` webhook to AIP
- AIP fetches PR files, checks `nodepools/team-a/config.yaml` matches target URI, checks blob SHA
- AIP sets `AIP Governance = success` on the PR head commit
- Engineer reviews, clicks merge — GitHub enforces required status check
- Any subsequent push to the PR branch re-triggers the webhook and re-checks

**If the agent opens a PR for `team-b/config.yaml` instead**: AIP sets `AIP Governance = failure`, reason `intent drift`. Merge is blocked.

### 6.2 Terraform Cloud

**Setup** (done once by platform engineer):
- Register AIP as a Run Task on the Terraform Cloud organization: `https://aip.internal/hooks/terraform`
- Set enforcement level: `mandatory` on target workspaces

**Per-operation flow**:
- Agent submits `AgentRequest` for `terraform://myorg/workspaces/prod-nodegroup-team-a`, `action: apply`
- Controller fetches state serial → `Approved`
- Agent receives `Grant.TerraformToken` (scoped to `prod-nodegroup-team-a`, 30m)
- Agent queues a run: `[aip:req-xyz] governed apply`
- Terraform Cloud calls AIP Run Task callback before apply: `POST /hooks/terraform`
- AIP checks workspace URI, state serial, phase
- AIP returns `{"status": "passed"}` or `{"status": "failed", "message": "..."}`
- Terraform Cloud proceeds or cancels the run accordingly

### 6.3 Jira

**Setup** (done once by platform engineer):
- Register Jira webhook for project INFRA: `https://aip.internal/hooks/jira`, events: `issue_updated`
- Or: deploy AIP egress proxy + NetworkPolicy if webhook registration is not permitted

**Per-operation flow (webhook model)**:
- Agent submits `AgentRequest` for `jira://myorg/projects/INFRA/issues/INFRA-123`, `action: transition`
- Controller fetches issue state → `Approved`
- Agent receives `Grant.JiraToken` (OAuth 2.0, project INFRA, 15m)
- Agent transitions the issue
- Jira fires `issue_updated` webhook to AIP
- AIP checks issue key matches AgentRequest target URI, issue state matches fingerprint
- AIP records `AuditRecord` — pass or fail

**Note**: Jira has no mechanism to block a transition after the fact (webhook fires post-transition). The enforcement model here is **detect and record**, not block. For blocking enforcement, deploy the proxy model with NetworkPolicy.

---

## 7. What This Does Not Solve (Phase 2)

1. **Scoped-mode requests** (`executionMode: scoped`): agents that do not know the specific resource upfront. Requires pattern containment checks and OpsLock acquisition per matching resource.

2. **Multi-`GovernedResource` overlap**: a single request spanning URIs governed by multiple `GovernedResource` objects. Requires union-of-policies evaluation.

3. **Dynamic webhook registration**: When a platform engineer creates a `GovernedResource` for a GitHub repo, AIP should automatically register the webhook on that repo. Currently requires manual setup. Controller can manage this via the GitHub App API — deferred.

4. **Fine-grained action filtering in WebhookPlugin**: Phase 1 verifies that the changed resources match the target URI. It does not verify the nature of the change (e.g., only `maxNodes` field changed, not the node type). Schema-level diff validation is a Phase 2 concern.

---

## 8. Implementation Phases

### Phase 1 — Plugin SDK + GitHub + Webhook Enforcement
- Define plugin interfaces (`Plugin`, `CredentialMinter`, `WebhookPlugin`, `ProxyPlugin`) in `plugin/` package
- Define `PluginRegistry` with capability-based dispatch
- Implement `github` plugin: `FetchContext` (blob SHA via PR Files API), `MintCredential` (App installation token), `WebhookPlugin` (`pull_request` events → commit status)
- Replace direct `KubernetesTargetContextFetcher` dispatch in controller with registry lookup
- Add `POST /hooks/{scheme}` webhook endpoint in gateway with shared `verifyEvent` logic
- Add `POST /agent-requests/{name}/token` subresource endpoint in gateway; calls `CredentialMinter` plugin, never persists to etcd
- Document one-time webhook setup and required status check configuration

### Phase 2 — Scoped Mode, Multi-GR, Dynamic Registration
- `executionMode: scoped` pattern containment check at admission
- OpsLock acquisition per matching resource pattern
- Union-of-policies evaluation for multi-GR overlap
- Controller manages webhook lifecycle: auto-register/deregister webhooks when `GovernedResource` is created/deleted
- Terraform Cloud plugin
- Jira plugin (detect-and-record model; proxy model for blocking enforcement)
