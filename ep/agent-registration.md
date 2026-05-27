# Design: AgentRegistration and Scoped External Credentials

Status: Draft

---

## Problem

AIP has no explicit agent registration mechanism. Agents come into existence the moment
they submit their first `AgentRequest` — the `AgentTrustProfile` bootstraps itself
reactively from that activity. This creates three compounding gaps:

**Gap 1 — Unregistered agents act freely.**
Any identity string can submit `AgentRequest`s. Even with OIDC enabled, the gateway
validates the token but never asks whether this agent has been provisioned by an operator.
An agent can appear from nowhere, pass the token check, and immediately take governed
actions at Observer trust level.

**Gap 2 — Shared downstream credentials lose agent identity.**
When the gateway forwards an approved tool call to an MCP server (GitHub, Azure DevOps,
AWS Bedrock), it uses the MCPServer's single shared `BearerTokenSecretRef` for all
agents. The downstream audit trail — GitHub audit log, Azure activity log, AWS CloudTrail
— shows the gateway's service identity, not the acting agent's identity. K8s audit logs
have the same problem: every API call appears as `system:serviceaccount:aip-system:aip-gateway`.

**Gap 3 — Agent identity config has no home.**
`AgentTrustProfile` is a controller-managed measurement object. Its spec is write-once
and contains only `agentIdentity`. Operator-declared config (outbound credentials, OIDC
subject bindings, per-service credential bindings) has nowhere to live. The gateway has
no object to read for identity policy; it can only read trust measurements.

---

## Goals

1. Introduce `AgentRegistration` as the single operator-owned source of truth for agent
   identity configuration: OIDC inbound validation, and per-service outbound credential
   bindings (static secret, Azure WIF, AWS WebIdentity, K8s OIDC passthrough).
2. Keep `AgentTrustProfile` as a pure controller-managed measurement object. No identity
   config lives on ATP.
3. Have the gateway read `AgentRegistration` directly — the same watch/cache pattern
   used for `MCPServer` — for both admission enforcement and credential selection.
4. Support the three outbound credential models encountered in practice: static secret
   (GitHub PAT), client credentials with federated identity (Azure Entra), and web
   identity federation (AWS STS).
5. Propagate agent identity into downstream audit trails: per-agent credentials for
   external MCP servers, including K8s API servers via OIDC token passthrough.
6. Configurable enforcement for unregistered agents, defaulting to backward-compatible
   `allow` mode.

## Non-Goals

- Replacing `AgentIdentity` (ep/agent_identity.md), which covers inbound authentication
  via API keys for non-OIDC agents. `AgentRegistration` covers OIDC-authenticated agents
  and their outbound credential bindings. See Open Question 4 on eventual unification.
- SCIM / bulk provisioning from an IdP.
- Per-action RBAC on `AgentRegistration`. Natural future extension; not v1.
- AWS SigV4 signing inside the gateway. The Bedrock MCP proxy pattern (see §3) keeps
  the gateway credential-model uniform.
- K8s impersonation (`Impersonate-User` header, gateway SA holding `impersonate` verbs).
  K8s is treated as any other target service: the agent's OIDC token is used directly
  (passthrough) or exchanged via RFC 8693 (exchange mode). No elevated gateway SA
  privileges needed.

---

## Object Ownership: the invariant

```text
AgentRegistration   operator-owned   identity config, outbound credentials, OIDC bindings
AgentTrustProfile   controller-owned trust measurements (level, accuracy, executions)
```

These are independent objects. `AgentRegistration` is never mirrored onto
`AgentTrustProfile`. The gateway maintains a separate watch/cache for each and reads
them for different purposes:

```text
handleCreateAgentRequest:
  reads AgentRegistration → OIDC subject validation, unregistered-agent policy

resolveAgentCredential (MCP handler):
  reads AgentRegistration → externalIdentities → CredentialProvider

evaluateTrustGate:
  reads AgentTrustProfile → trustLevel (unchanged from today)
```

---

## Current State

### How AgentTrustProfile is created today

The `AgentTrustProfileReconciler.getOrBootstrapProfile` fires when a
`DiagnosticAccuracySummary` is updated or an `AgentRequest` reaches a terminal phase.
It performs a cascading lookup:

```text
1. Get(AgentTrustProfile by name) → not found
2. Get(DiagnosticAccuracySummary by same name) → not found
3. List(AgentRequests with label aip.io/profileName=<name>)
4. Extract agentIdentity from first matching request
5. Create AgentTrustProfile{Spec: {AgentIdentity: "..."}}
```

The spec is write-once with one field. The controller only ever touches `status`. There
is no pre-provisioning path.

### Current admission pipeline in handleCreateAgentRequest

```text
1. OIDC token validation        (only when --oidc-issuer-url configured)
2. requireRole(roleAgent)       (only when --agent-subjects configured)
3. agentIdentity != sub → 400   (only when authRequired=true; 400 not 403)
4. GovernedResource URI match   (only when any GRs exist)
5. GR.spec.permittedAgents      (only when the list is non-empty)
6. Trust gate (ATP trust level) (only when GR has trustRequirements)
7. Create AgentRequest          ← proceeds unconditionally otherwise
```

There is no step that checks for an `AgentRegistration`. In open mode (no OIDC, no
subjects configured), step 3 is skipped, meaning an agent can claim any identity in the
request body.

### Credential selection for MCP tool calls

```go
// mcp_handler.go — today
bearerToken := mcpServer.BearerToken       // shared for all agents
if bearerToken == "" {
    bearerToken = os.Getenv("AIP_MCP_TOKEN")
}
```

Every agent calling `github/create_pull_request` uses the same PAT.

---

## Proposed Design

### 1. `AgentRegistration` CRD

`AgentRegistration` is the single operator-managed object for an agent's identity
configuration. All identity config lives here and nowhere else.

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: AgentRegistration
metadata:
  name: payment-bot
  namespace: default
spec:
  # Canonical agent name — used in AgentRequest.spec.agentIdentity,
  # GovernedResource.spec.permittedAgents, and ATP labels.
  agentIdentity: payment-bot

  # oidc declares which OIDC tokens prove this agent's identity.
  # The gateway validates the incoming token's subject claim against
  # allowedSubjects before admitting the request.
  # Optional: when absent, gateway falls back to existing --agent-subjects checks.
  oidc:
    issuer: https://keycloak.company.com/realms/aip
    subjectClaim: azp         # claim to read as the agent identifier (default: sub)
    allowedSubjects:
      - payment-bot           # token where azp==payment-bot is accepted
      - payment-bot-staging   # staging instance also maps to this registration

  # externalIdentities binds per-service outbound credentials to this agent.
  # When the gateway forwards a tool call to service X, it selects the credential
  # for this agent on service X instead of the MCPServer shared token.
  externalIdentities:

    # GitHub: static PAT per agent
    - service: github
      type: StaticSecret
      staticSecret:
        name: github-pat-payment-bot
        key: token
        namespace: aip-k8s-system

    # Azure DevOps: client credentials with federated identity credential
    # payment-bot has its own app registration in Entra; its Keycloak token
    # is the client_assertion proving its identity to Azure.
    - service: azure-devops
      type: AzureWorkloadIdentity
      azureWorkloadIdentity:
        tenantID: "abc-tenant"
        clientID: "app-reg-payment-bot"   # payment-bot's own app registration
        scope: "https://app.vssps.visualstudio.com/.default"

    # AWS Bedrock: AssumeRoleWithWebIdentity
    # The Keycloak token is the WebIdentityToken for STS.
    - service: aws-bedrock
      type: AWSWebIdentity
      awsWebIdentity:
        roleARN: "arn:aws:iam::123456789:role/payment-bot-bedrock"
        roleSessionName: "payment-bot"
        region: "us-east-1"

    # K8s MCP server: OIDC token passthrough.
    # The agent's Keycloak JWT is forwarded directly to the K8s API server.
    # Requires cluster --oidc-issuer-url to match the AIP gateway issuer.
    # K8s RBAC is enforced against the agent's actual sub/groups claims.
    - service: k8s-mcp-server
      type: KubernetesOIDC
      kubernetesOIDC: {}   # passthrough mode; no exchange URL needed
```

#### Go types

```go
// +kubebuilder:validation:Enum=StaticSecret;AzureWorkloadIdentity;AWSWebIdentity;KubernetesOIDC
type ExternalIdentityType string

const (
    ExternalIdentityStaticSecret          ExternalIdentityType = "StaticSecret"
    ExternalIdentityAzureWorkloadIdentity ExternalIdentityType = "AzureWorkloadIdentity"
    ExternalIdentityAWSWebIdentity        ExternalIdentityType = "AWSWebIdentity"
    ExternalIdentityKubernetesOIDC        ExternalIdentityType = "KubernetesOIDC"
)

type ExternalIdentityBinding struct {
    // Service matches MCPServer.metadata.name (e.g. "github", "k8s-mcp-server").
    // +kubebuilder:validation:MinLength=1
    Service string               `json:"service"`
    Type    ExternalIdentityType `json:"type"`
    // +optional
    StaticSecret *StaticSecretCredential `json:"staticSecret,omitempty"`
    // +optional
    AzureWorkloadIdentity *AzureWorkloadIdentityCredential `json:"azureWorkloadIdentity,omitempty"`
    // +optional
    AWSWebIdentity *AWSWebIdentityCredential `json:"awsWebIdentity,omitempty"`
    // +optional
    KubernetesOIDC *KubernetesOIDCCredential `json:"kubernetesOIDC,omitempty"`
}

type StaticSecretCredential struct {
    Name      string `json:"name"`
    Key       string `json:"key"`
    Namespace string `json:"namespace"`
}

// AzureWorkloadIdentityCredential configures the client credentials flow with
// federated identity. The agent's Keycloak OIDC token serves as the
// client_assertion proving the agent's identity to Azure Entra.
//
// Pre-requisite (one-time operator setup per agent):
//   Create an app registration in Azure Entra for this agent. Under
//   "Certificates & secrets → Federated credentials", add a credential with:
//     Issuer: https://keycloak.company.com/realms/aip
//     Subject: <agentIdentity>  (must match the Keycloak token's subjectClaim)
//     Audience: api://AzureADTokenExchange
//   This allows the agent's Keycloak token to authenticate as that app registration.
type AzureWorkloadIdentityCredential struct {
    TenantID string `json:"tenantID"`
    // ClientID is the app registration client ID belonging to this specific agent,
    // not the AIP gateway's app registration.
    ClientID string `json:"clientID"`
    // Scope is the Azure resource scope, e.g.
    // "https://app.vssps.visualstudio.com/.default"
    Scope string `json:"scope"`
}

// AWSWebIdentityCredential configures AssumeRoleWithWebIdentity.
// The agent's Keycloak OIDC token is the WebIdentityToken passed to STS.
//
// Pre-requisite (one-time operator setup per agent):
//   1. Create an IAM OIDC Identity Provider pointing at Keycloak's JWKS URI.
//   2. Create an IAM role with a trust policy allowing
//      "sts:AssumeRoleWithWebIdentity" from that provider with
//      Condition: StringEquals keycloak.../sub = <agentIdentity>
type AWSWebIdentityCredential struct {
    RoleARN         string `json:"roleARN"`
    RoleSessionName string `json:"roleSessionName"`
    Region          string `json:"region"`
    // DurationSeconds is the STS session duration (default 3600, max 43200).
    // +optional
    DurationSeconds *int32 `json:"durationSeconds,omitempty"`
    // STSEndpoint overrides the default regional STS endpoint. Used in testing.
    // +optional
    STSEndpoint string `json:"stsEndpoint,omitempty"`
}

// KubernetesOIDCCredential configures OIDC token passthrough (or RFC 8693
// TokenExchange) for Kubernetes API servers acting as MCP server targets.
//
// K8s is treated as any other downstream service: the credential provider
// returns a Bearer token that the gateway uses in the Authorization header
// when proxying to the K8s MCP server. No impersonation verbs needed on the
// gateway service account.
//
// Passthrough mode (default, TokenExchangeURL empty):
//   The agent's validated OIDC JWT is forwarded directly. Requires the cluster's
//   --oidc-issuer-url to match the AIP gateway's OIDC provider. K8s RBAC is
//   enforced against the agent's actual sub/groups claims.
//
// Exchange mode (TokenExchangeURL set):
//   Calls the RFC 8693 token-exchange endpoint with the agent's JWT as
//   subject_token. Used when gateway and K8s cluster use different issuers.
type KubernetesOIDCCredential struct {
    // TokenExchangeURL is an optional RFC 8693 token exchange endpoint.
    // When empty, the agent's JWT is forwarded to K8s directly (passthrough).
    // +optional
    TokenExchangeURL string `json:"tokenExchangeURL,omitempty"`
    // Audience overrides the aud claim for the forwarded or exchanged token.
    // Defaults to the cluster's expected OIDC audience when empty.
    // +optional
    Audience string `json:"audience,omitempty"`
}

type AgentRegistrationOIDC struct {
    Issuer string `json:"issuer"`
    // SubjectClaim is the token claim used as the agent identifier.
    // Defaults to "sub". Use "azp" for Keycloak client_credentials, "appid"
    // for Azure AD, "email" for Google service accounts.
    // +optional
    SubjectClaim string `json:"subjectClaim,omitempty"`
    // AllowedSubjects lists token subject values that may act as this agent.
    // Supports multiple values for staging/prod variants of the same agent.
    // +optional
    AllowedSubjects []string `json:"allowedSubjects,omitempty"`
}

type AgentRegistrationSpec struct {
    // +kubebuilder:validation:MinLength=1
    AgentIdentity string `json:"agentIdentity"`
    // +optional
    OIDC *AgentRegistrationOIDC `json:"oidc,omitempty"`
    // +optional
    ExternalIdentities []ExternalIdentityBinding `json:"externalIdentities,omitempty"`
}
```

### 2. `CredentialProvider` interface

`golang.org/x/oauth2 v0.35.0` is already an indirect dependency. The `TokenSource`
interface is the right abstraction — all providers implement it.

```go
// internal/credential/provider.go

// Provider resolves a bearer token for calling an external service on behalf
// of a specific agent. Implementations own their own caching and refresh.
//
// agentOIDCToken is the validated token from the gateway's OIDC middleware.
// For exchange-based providers (Azure, AWS) it is the input to the exchange.
// For StaticSecret it is ignored.
//
// Caching contract (all implementations):
//   - Cache the OUTPUT token keyed on stable identity, NOT on the input token.
//   - Refresh lazily when the output token is within refreshBuffer of expiry.
//   - Background refresh is not possible: the input OIDC token is only present
//     during an active agent request, not between requests.
type Provider interface {
    Token(ctx context.Context, agentOIDCToken string) (string, error)

    // Invalidate drops the cached credential for this provider.
    // Called by the gateway's AgentRegistration watch handler when the
    // registration changes (credential rotated, binding updated).
    Invalidate()
}
```

#### `StaticSecretProvider`

Reads the K8s Secret value at construction time. `agentOIDCToken` is ignored.
`Invalidate()` clears the cached value; the gateway's Registration watch handler
calls `Invalidate()` and re-constructs the provider from the updated registration,
which triggers a fresh Secret read on the next call.

#### `AzureWorkloadIdentityProvider`

Uses the **Client Credentials flow with Federated Identity Credential** — the correct
pattern for machine-to-machine Azure authentication. This is distinct from On-Behalf-Of
(OBO), which requires delegated user tokens and is not appropriate for agents.

```http
POST https://login.microsoftonline.com/{tenantID}/oauth2/v2.0/token
  grant_type=client_credentials
  client_id={payment-bot app registration clientID}
  client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer
  client_assertion={agentOIDCToken}   ← Keycloak token as federated proof
  scope={scope}
```

Azure validates `client_assertion` against the federated identity credential registered
on `payment-bot`'s app registration (issuer=Keycloak, subject=payment-bot). On success,
Azure returns an access token **as `payment-bot`'s own app identity**, not as AIP acting
on behalf of someone. This is what shows up in Azure's audit log.

Cache key: `(agentIdentity, tenantID, clientID, scope)`
Output TTL: typically 1 hour. Refresh within 5-minute buffer using current request's token.
No Azure SDK required: standard HTTP POST with `golang.org/x/oauth2`.

#### `AWSWebIdentityProvider`

Calls `STS.AssumeRoleWithWebIdentity` with the agent's Keycloak token as the
`WebIdentityToken`. STS validates the token against the registered IAM OIDC Identity
Provider and issues temporary credentials scoped to the role's permission policy.

```http
POST https://sts.{region}.amazonaws.com/
  Action=AssumeRoleWithWebIdentity
  RoleArn={roleARN}
  WebIdentityToken={agentOIDCToken}
  RoleSessionName={roleSessionName}
  DurationSeconds={durationSeconds, default 3600}
```

Returns: `AccessKeyId`, `SecretAccessKey`, `SessionToken` (valid for DurationSeconds).

AWS Bedrock uses SigV4, not Bearer tokens. The gateway stays token-model-uniform via
a thin proxy layer in front of Bedrock that accepts the credential bundle as Bearer and
handles SigV4 internally — the gateway does not sign AWS requests.

Cache key: `(agentIdentity, roleARN)`
Output TTL: DurationSeconds. Refresh window: min(10% of duration, 10 min), matching the
AWS SDK `WebIdentityRoleProvider` default.

#### `KubernetesOIDCProvider`

K8s is just another target service that accepts Bearer tokens. The provider operates in
two modes based on whether `tokenExchangeURL` is set.

**Passthrough mode** (`tokenExchangeURL` empty):

The agent's validated OIDC JWT is returned as-is. The gateway uses it as the Bearer
token when proxying to the K8s MCP server:

```go
// In mcp_handler.go: per-agent K8s client uses agent token, not gateway SA.
agentCfg := rest.CopyConfig(s.baseK8sCfg)
agentCfg.BearerToken = agentOIDCToken
agentCfg.BearerTokenFile = ""
agentK8sClient, _ := client.New(agentCfg, client.Options{Scheme: scheme})
```

Requires the K8s cluster's `--oidc-issuer-url` to match the AIP gateway's OIDC
provider. K8s RBAC is enforced against the agent's actual `sub` and `groups` claims.
The gateway service account requires no additional privileges.

**Exchange mode** (`tokenExchangeURL` set):

Posts an RFC 8693 token exchange request:
```http
POST {tokenExchangeURL}
  grant_type=urn:ietf:params:oauth:grant-type:token-exchange
  subject_token={agentOIDCToken}
  subject_token_type=urn:ietf:params:oauth:token-type:jwt
  audience={audience}
```

Returns a K8s-valid token scoped to the target cluster. Used when gateway and cluster
use different OIDC issuers.

Cache key: `(agentIdentity, service)` — the JWT itself is not the cache key since it
rotates with each request. In passthrough mode caching is a no-op (the token is
caller-supplied). In exchange mode the output token is cached until expiry.

No impersonation verbs needed on the gateway service account in either mode.

#### Token cache (shared infrastructure)

```go
// internal/credential/cache.go

const refreshBuffer = 5 * time.Minute

type TokenCache struct {
    mu    sync.RWMutex
    store map[string]*cachedEntry
    group singleflight.Group  // golang.org/x/sync/singleflight
}

// GetOrFetch returns the cached token if fresh, otherwise calls fetch exactly
// once even under concurrent requests for the same key (singleflight).
func (c *TokenCache) GetOrFetch(
    ctx context.Context,
    key string,
    fetch func(context.Context) (token string, expiry time.Time, err error),
) (string, error)
```

The `singleflight.Group` deduplicates concurrent exchanges: if 10 requests arrive
simultaneously for the same agent when the cache is cold, only one STS/Azure call is
made. The other 9 wait on the in-flight result.

### 3. Gateway: AgentRegistration watch and credential resolution

The gateway adds an `AgentRegistration` informer/cache. This is the same watch pattern
used for `MCPServer` CRDs today (`cmd/gateway/mcp_watch.go`).

```go
// cmd/gateway/registration_watch.go

// registrationCache is a read-through cache of AgentRegistration objects,
// keyed by agentIdentity. Analogous to mcpServerCache for MCPServer CRDs.
type registrationCache struct {
    mu          sync.RWMutex
    byAgent     map[string]*v1alpha1.AgentRegistration  // agentIdentity → Registration
    providers   map[string]map[string]credential.Provider // agentIdentity → service → Provider
    credCache   *credential.TokenCache
    k8sClient   client.Client
}

// get returns the Registration for agentIdentity, or nil if not found.
func (c *registrationCache) get(agentIdentity string) *v1alpha1.AgentRegistration

// providerFor returns the CredentialProvider for (agentIdentity, service).
// Returns nil if no binding exists (caller falls back to shared MCPServer token).
func (c *registrationCache) providerFor(agentIdentity, service string) credential.Provider

// upsert is called by the watch handler on Registration add/update events.
// Re-builds CredentialProviders for the registration and calls Invalidate()
// on providers whose binding changed.
func (c *registrationCache) upsert(reg *v1alpha1.AgentRegistration)

// remove is called on Registration delete events.
func (c *registrationCache) remove(agentIdentity string)
```

**Credential resolution in `mcp_handler.go`:**

```go
// Per-agent binding takes priority over the shared MCPServer token.
// rawOIDCToken is the validated token already in request context.
bearerToken := mcpServer.BearerToken  // fallback: shared token
if provider := s.regCache.providerFor(agentIdentity, mcpServerName); provider != nil {
    tok, err := provider.Token(r.Context(), rawOIDCToken)
    if err != nil {
        log.Printf("credential resolution %s/%s: %v — using shared token",
            agentIdentity, mcpServerName, err)
    } else {
        bearerToken = tok
    }
}
```

**Admission enforcement in `handleCreateAgentRequest`:**

```go
// After existing role check, before any K8s object is created.
reg := s.regCache.get(body.AgentIdentity)
if reg == nil {
    switch s.unregisteredAgentPolicy {
    case "strict":
        writeError(w, http.StatusForbidden, "AGENT_NOT_REGISTERED")
        return
    case "warn":
        log.Printf("warn: unregistered agent %q", body.AgentIdentity)
        setAnnotation(agentReq, "governance.aip.io/unregistered", "true")
    }
    // "allow": proceed silently (backward-compatible default)
} else {
    // Validate OIDC token subject against registration's allowedSubjects.
    // Replaces the existing loose `agentIdentity != sub` equality check.
    if err := validateOIDCSubject(reg, sub); err != nil {
        writeError(w, http.StatusForbidden, "IDENTITY_MISMATCH: "+err.Error())
        return
    }
}
```

**Identity validation fix — 400 → 403, equality → allowedSubjects:**

The existing check (`agent_request_handlers.go:88`) fires only when `authRequired=true`
and uses exact equality (`body.AgentIdentity != sub`). This misses the case where an
agent legitimately uses a different OIDC subject name than its registered identity (e.g.,
`azp=payment-bot-prod` maps to `agentIdentity=payment-bot`), and uses status 400 instead
of 403.

The replacement:

```go
// validateOIDCSubject checks sub against reg.Spec.OIDC.AllowedSubjects.
// Returns nil when the registration has no OIDC config (fallback to role checks).
func validateOIDCSubject(reg *v1alpha1.AgentRegistration, sub string) error {
    if reg.Spec.OIDC == nil || len(reg.Spec.OIDC.AllowedSubjects) == 0 {
        return nil
    }
    if slices.Contains(reg.Spec.OIDC.AllowedSubjects, sub) {
        return nil
    }
    return fmt.Errorf("token subject %q not in allowedSubjects for agent %q",
        sub, reg.Spec.AgentIdentity)
}
```

### 4. Controller changes: ATP bootstrap from AgentRegistration

The `AgentTrustProfileReconciler` gains a watch on `AgentRegistration`.

**Purpose**: pre-create the ATP when a Registration is provisioned, so the agent starts
at Observer trust level before its first request rather than requiring a request to exist.

**What the controller does** on Registration create/update:
1. Look up or create the `AgentTrustProfile` for `spec.agentIdentity`.
2. Set `atp.spec.agentIdentity` if not already set (existing bootstrap behaviour).
3. Nothing else. No credential data is written to the ATP.

The ATP spec remains a 1-field object. Identity config stays in `AgentRegistration`.

**Fallback `getOrBootstrapProfile` (unchanged):**
If no `AgentRegistration` exists, the existing reactive bootstrap from `AgentRequest`
activity continues to work. Backward-compatible.

### 5. K8s audit trail via OIDC federation

When an agent's `externalIdentities` includes a `KubernetesOIDC` binding for the K8s
MCP server, the agent's own OIDC token is used as the Bearer credential for K8s API
calls. K8s RBAC is enforced under the agent's actual identity, and the audit log records
the agent directly:

```text
user: payment-bot          ← agent's actual OIDC sub claim
verb: patch
resource: deployments/scale
```

No impersonation header. No elevated gateway SA permissions. The gateway service account
is used only for its own management operations (reading AgentRequests, GovernedResources,
etc.) — not for agent tool calls that target the K8s MCP server.

### 6. `--unregistered-agent-policy` flag

```text
--unregistered-agent-policy string
    allow   Current behaviour; any agent identity is accepted. (default)
    warn    Request proceeds; AgentRequest annotated with
            governance.aip.io/unregistered=true; warning logged.
    strict  Request rejected: 403 AGENT_NOT_REGISTERED.
```

The `governance.aip.io/unregistered=true` annotation enables SafetyPolicy rules:

```yaml
rules:
  - name: require-approval-unregistered
    type: StateEvaluation
    action: RequireApproval
    expression: >
      has(request.metadata.annotations) &&
      request.metadata.annotations["governance.aip.io/unregistered"] == "true"
    message: "Agent is not registered. Human approval required."
```

---

## E2E Test Plan

### Phase 8b — per-agent Keycloak credentials for GitHub MCP

Extends `test/e2e/gateway_keycloak_test.go` (existing Phase 8) with a new Context.
No new suite file. Requires `AIP_E2E_GITHUB_PAT_AGENT1` and `AIP_E2E_GITHUB_PAT_AGENT2`.

```text
BeforeAll:
  - Register aip-agent-2 in Keycloak (new client, reuses kcSetup helpers)
  - Create per-agent PAT Secrets (agent1, agent2)
  - kubectl apply AgentRegistration for each with externalIdentities[github] set
  - Wait for ATP to exist (controller bootstrap from Registration)
  - Gateway subprocess already running with --oidc-issuer-url=keycloak (Phase 8 setup)

It "agent-1 Keycloak token → GitHub MCP call uses agent-1 PAT":
  - Fetch Keycloak token for aip-agent-1
  - Submit + approve AgentRequest via gateway
  - Call POST /mcp github/create_pull_request with AIP JWT
  - Verify PR created; verify GitHub PR creator == agent-1's PAT owner

It "agent-2 Keycloak token → GitHub MCP call uses agent-2 PAT":
  - Same shape; different token, PAT, expected GitHub login

It "unregistered agent falls back to shared MCPServer token (warn mode)":
  - Unknown agentIdentity; no AgentRegistration
  - MCP call succeeds using shared MCPServer PAT
  - AgentRequest.metadata.annotations["governance.aip.io/unregistered"] == "true"

It "--unregistered-agent-policy=strict rejects unknown agent":
  - New gateway subprocess with --unregistered-agent-policy=strict
  - POST /agent-requests with unregistered identity → 403 AGENT_NOT_REGISTERED
```

### Phase 8c — token exchange mechanics (stub servers, no cloud accounts)

Tests `AzureWorkloadIdentityProvider` and `AWSWebIdentityProvider` without real Azure or
AWS accounts. Uses `httptest.NewServer` stubs. Lives in `gateway_keycloak_test.go`.

```text
It "AzureWorkloadIdentity uses client_credentials + federated identity, not OBO":
  - Stub Azure token endpoint: validates grant_type=client_credentials,
    client_assertion == agent OIDC token, returns synthetic Azure AD token
  - Stub upstream MCP server: captures Authorization header
  - AgentRegistration with AzureWorkloadIdentity pointing at stub endpoint
  - Submit + approve + execute MCP call
  - Assert stub MCP received the exchanged token (not the raw OIDC token)
  - Assert stub token endpoint received client_credentials grant (not jwt-bearer OBO)

It "AWSWebIdentity calls STS and uses session token for upstream MCP call":
  - Stub STS endpoint: validates Action=AssumeRoleWithWebIdentity,
    WebIdentityToken == agent OIDC token; returns synthetic temp credentials
  - Stub upstream MCP server: captures credential bundle in Authorization header
  - Assert STS stub was called once; second request within TTL hits the cache

It "token cache: second request for same agent within TTL skips re-exchange":
  - Two concurrent requests for same agent (singleflight test)
  - Assert stub exchange endpoint called exactly once
```

### Cloud e2e suites (env-var gated, separate suite files)

```text
test/e2e_azure/gateway_azure_entra_test.go
  Build tag: azure_e2e
  Skips unless: AZURE_TENANT_ID, AZURE_CLIENT_ID, AZURE_FEDERATED_KEYCLOAK_ISSUER
  Verifies: Keycloak OIDC → client_credentials + federated identity → Azure DevOps MCP
  Azure audit log shows payment-bot's app registration identity

test/e2e_aws/gateway_aws_bedrock_test.go
  Build tag: aws_e2e
  Skips unless: AWS_ROLE_ARN, AWS_REGION, AWS_BEDROCK_MCP_PROXY_URL
  Verifies: Keycloak OIDC → AssumeRoleWithWebIdentity → Bedrock MCP proxy call
  AWS CloudTrail shows per-agent IAM role session identity
```

---

## Migration Path

**Stage 1 — CRD present, not enforced (default `--unregistered-agent-policy=allow`)**
- Install `AgentRegistration` CRD.
- Operators create registrations and verify credential selection works.
- Unregistered agents proceed unchanged.
- Zero breaking changes.

**Stage 2 — Warning mode (`--unregistered-agent-policy=warn`)**
- Unregistered agents annotated; operators discover gaps via annotation.
- SafetyPolicy rules can require approval for unregistered agents.
- Operators create registrations for all active agents.

**Stage 3 — Strict mode (`--unregistered-agent-policy=strict`)**
- No registration = 403. Recommended production posture.
- Opt-in by explicit flag; never the default.

---

## Implementation Phases

### Phase 1 — CRD and controller bootstrap

Files:
- `api/v1alpha1/agentregistration_types.go`
- `api/v1alpha1/zz_generated.deepcopy.go` — `make generate`
- `config/crd/bases/` — `make manifests`
- `internal/controller/agenttrustprofile_controller.go` — Registration watch,
  pre-create ATP on Registration create/update (spec.agentIdentity only)
- `internal/controller/agenttrustprofile_controller_test.go`
- `charts/aip-k8s/` — RBAC for AgentRegistration get/list/watch

### Phase 2 — Gateway registration watch + admission enforcement

Files:
- `cmd/gateway/registration_watch.go` — `registrationCache`, watch loop
- `cmd/gateway/main.go` — `--unregistered-agent-policy` flag, wire regCache
- `cmd/gateway/agent_request_handlers.go` — registration lookup, OIDC subject
  validation (fixes 400→403 and equality→allowedSubjects)
- `cmd/gateway/integration_test.go` — strict/warn/allow mode tests

### Phase 3 — Credential providers and MCP credential selection

Files:
- `internal/credential/provider.go` — `Provider` interface, `TokenCache`, singleflight
- `internal/credential/static.go` — `StaticSecretProvider`
- `internal/credential/azure.go` — `AzureWorkloadIdentityProvider` (client_credentials + WIF)
- `internal/credential/aws.go` — `AWSWebIdentityProvider` (AssumeRoleWithWebIdentity)
- `internal/credential/k8s.go` — `KubernetesOIDCProvider` (passthrough + RFC 8693 exchange)
- `internal/credential/provider_test.go` — unit tests with stub HTTP servers
- `cmd/gateway/registration_watch.go` — `providerFor`, `upsert` provider construction
- `cmd/gateway/mcp_handler.go` — `resolveAgentCredential` call; K8s MCP path builds
  per-agent client from `KubernetesOIDCProvider` token instead of gateway SA credentials

### Phase 4 — E2e tests

Files:
- `test/e2e/gateway_keycloak_test.go` — Phase 8b + 8c contexts
- `test/e2e_azure/` — suite skeleton (build tag `azure_e2e`, skipped by default)
- `test/e2e_aws/` — suite skeleton (build tag `aws_e2e`, skipped by default)
- `.github/workflows/e2e.yml` — document env var requirements

---

## Alternatives Considered

### Copy `externalIdentities` to AgentTrustProfile spec

Rejected. It creates dual-ownership of the ATP object — the controller would write
identity config it doesn't understand, and operators would need to know that ATP spec
is a derived mirror and should not be edited directly. The gateway already maintains
separate informer caches for different resource types (MCPServer, AgentRequest, ATP).
Adding a Registration cache is the same pattern, not additional complexity.

### `AgentCredentialBinding` as a separate CRD (like ClusterRoleBinding)

Not unreasonable, but a separate CRD for outbound credentials without a registration
object means operators have two places to configure one agent. `AgentRegistration` is
already the "this agent is provisioned" object — credentials are part of provisioning.

### Store per-agent credentials on `MCPServer.spec`

Makes credential config a property of the server, not the agent. An operator managing
the GitHub MCP server should not need to enumerate credentials for every agent that
uses it. Registration-side ownership is the correct boundary.

---

## Open Questions

1. **Namespace scope of `AgentRegistration`**: **Resolved — Namespaced**, matching ATP.
   Operators manage registrations in the same namespace as other agent governance objects.
   Agents that span namespaces can be registered once per namespace or use a shared
   namespace with cross-namespace GovernedResource references.

2. **Relationship to `ep/agent_identity.md`**: `AgentIdentity` covers inbound API-key
   authentication for non-OIDC agents. `AgentRegistration` covers OIDC-authenticated
   agents and outbound credential selection. A future EP should define whether these
   merge into one CRD (with an `auth` section for inbound and `externalIdentities` for
   outbound) or remain separate with a reference.

3. **AWS SigV4 proxy specification**: The Bedrock MCP proxy that accepts the credential
   bundle and handles SigV4 is described but not specified. **Deferred to Phase 3b
   (AWS track).** A follow-up EP or ADR should define its API contract before
   `AWSWebIdentityProvider` is wired to the MCP handler.
