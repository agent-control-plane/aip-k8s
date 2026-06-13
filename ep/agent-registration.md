# Design: AgentRegistration and Scoped External Credentials

Status: In Progress — Phases 1–5 + 8b + 9 merged (PRs #245, #250, #252); Phases 10–12 (self-service registration, `aipctl`, client library) drafted

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

    # K8s MCP server: RFC 8693 token exchange.
    # The agent's Keycloak JWT (aud=aip-gateway) is exchanged at Keycloak's
    # token exchange endpoint for a new JWT scoped to the kubernetes audience.
    # The K8s MCP server forwards this token to the K8s API server.
    # Requires: cluster --oidc-issuer-url=keycloak, --oidc-client-id=kubernetes.
    # K8s RBAC is enforced against the agent's actual sub claim.
    - service: k8s-mcp-server
      type: KubernetesOIDC
      kubernetesOIDC:
        tokenExchangeURL: https://keycloak.company.com/realms/aip/protocol/openid-connect/token
        audience: kubernetes
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

// KubernetesOIDCCredential configures RFC 8693 token exchange for Kubernetes
// API servers acting as MCP server targets.
//
// K8s is treated as any other downstream service: the credential provider
// returns a Bearer token that the gateway injects in the Authorization header
// when proxying to the K8s MCP server. No impersonation verbs needed on the
// gateway service account.
//
// Passthrough mode is intentionally NOT supported. The inbound OIDC token is
// scoped to the gateway audience (aud=aip-gateway). Forwarding it to upstream
// servers would allow a compromised MCP server to replay the token against the
// gateway. TokenExchangeURL is therefore required.
//
// Exchange mode (TokenExchangeURL required):
//   Posts an RFC 8693 token exchange to tokenExchangeURL with the agent's JWT
//   as subject_token. The response token (scoped to `audience`) is injected
//   into the upstream MCP server call. Keycloak natively supports RFC 8693
//   when token exchange is enabled in the realm.
//
//   K8s cluster must be configured with:
//     --oidc-issuer-url=<keycloak-realm-url>
//     --oidc-client-id=<audience>       (e.g. "kubernetes")
//     --oidc-username-claim=sub
type KubernetesOIDCCredential struct {
    // TokenExchangeURL is the RFC 8693 token exchange endpoint.
    // Required — passthrough is not supported for security reasons.
    // For Keycloak: https://<host>/realms/<realm>/protocol/openid-connect/token
    TokenExchangeURL string `json:"tokenExchangeURL"`
    // Audience is the target audience for the exchanged token.
    // Must match the K8s API server's --oidc-client-id value (e.g. "kubernetes").
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
// Token-gated mint invariant (binding on every implementation and caller):
//   The gateway mints a downstream credential ONLY in the context of a live,
//   validated agent identity token presented at mint time. No provider may be
//   invoked by a background job, a controller, or any path without a live
//   token in request context. An approved AgentRegistration alone is policy
//   (the ceiling), never capability (the key): mint-from-stored-approval would
//   be replayable ambient authority — the same shape that disqualified K8s
//   impersonation. Exchange-based providers satisfy this structurally (the
//   live token IS the exchange input). Key-based providers (GitHubApp) must
//   enforce it explicitly: Token() returns an error when agentOIDCToken is
//   empty. The MCP handler rejects with 403 before calling any provider if
//   the request context carries no validated token while auth is enabled.
//   (StaticSecret is exempt: it performs no mint — it returns a
//   pre-provisioned secret, and access to it is gated by the same admission
//   path as everything else.)
//
// Caching contract (all implementations):
//   - Cache the OUTPUT token keyed on stable identity, NOT on the input token.
//   - Refresh lazily when the output token is within refreshBuffer of expiry.
//   - Background refresh is not possible: the input OIDC token is only present
//     during an active agent request, not between requests. This is the mint
//     invariant showing up as a caching constraint, not an inconvenience.
type Provider interface {
    Token(ctx context.Context, agentOIDCToken string) (string, error)

    // Invalidate drops the cached credential for this provider.
    // Called by the gateway's AgentRegistration watch handler when the
    // registration changes (credential rotated, binding updated, ceiling
    // narrowed, phase changed). Cached output tokens are dropped immediately;
    // the next mint re-evaluates against the new registration state.
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

K8s is just another target service that accepts Bearer tokens. The provider always
operates in exchange mode — passthrough is intentionally not supported because the
inbound token is scoped to `aud=aip-gateway` and forwarding it would allow a
compromised upstream server to replay it against the gateway.

**Exchange mode** (`tokenExchangeURL` required):

Posts an RFC 8693 token exchange request to the configured endpoint (e.g. Keycloak):

```http
POST {tokenExchangeURL}
  grant_type=urn:ietf:params:oauth:grant-type:token-exchange
  subject_token={agentOIDCToken}        ← agent's Keycloak JWT (aud=aip-gateway)
  subject_token_type=urn:ietf:params:oauth:token-type:jwt
  audience={audience}                   ← e.g. "kubernetes"
```

Keycloak returns a new JWT scoped to `aud=kubernetes` with the same `sub` as the
original token. This token is injected as the Bearer credential when proxying to
the K8s MCP server. The K8s MCP server passes it to the K8s API server, which
validates it via Keycloak's OIDC discovery endpoint.

Cache key: `sha256(rawOIDCToken)` — each distinct agent token gets its own
`TokenCache` entry. Cached entries are evicted amortized on each `Token()` call
when the exchanged token has expired.

No impersonation verbs needed on the gateway service account.

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

### 3. Gateway: AgentRegistration watch, naming, and credential resolution

#### Identity model and storage

`agentIdentity` is **flat and globally unique within an AIP deployment's trust domain**.
It never carries a namespace — the protocol is K8s-independent and must survive a
move to any other storage backend unchanged. K8s namespace is a storage detail:
the gateway stores all registrations in its own deployment namespace, read at startup
from the `POD_NAMESPACE` env var (downward API). Same convention controllers already
use for leader election leases. No flag, no config, never exposed in any API shape,
error message, or client tool — exactly as Kubernetes never exposes which namespace
is `kube-system` as a configuration option.

Uniqueness is anchored in the IdP: the bootstrap carve-out enforces
`agentIdentity == token subject` at self-registration time, and an IdP cannot issue
two clients with the same subject. Cross-issuer collisions (cluster SA issuer vs
enterprise IdP both happen to have a `payment-bot`) are rare in practice (SA subjects
are prefixed `system:serviceaccount:…`) and handled by first-wins + admin arbitration
— the same contract as any flat registry.

#### Issuer binding: names are flat, authentication is issuer-qualified

With repeatable `--oidc-issuer-url` (Phase 10), a flat name alone is not enough to
*authenticate* — two trusted issuers can each emit `sub=cleanup-bot`. The design
splits the two concerns deliberately:

- **Identity (the name)** stays flat and globally unique. The deterministic object
  name hashes `agentIdentity` alone — **intentionally excluding the issuer** — so
  that K8s `Create` atomically enforces "one registration per name." Folding the
  issuer into the hash would *permit* two registrations with the same flat
  `agentIdentity` (differing only by issuer), and every downstream flat-string
  consumer — `GovernedResource.permittedAgents`, ATP names, K8s audit usernames,
  CEL policy expressions, AgentRequest dedup keys — would silently match both.
  That is the cross-tenant fusion failure, relocated from the registration object
  to everywhere else. Name-level first-wins (409) is the protection, not the bug.

- **Authentication (who may use the name)** is issuer-qualified. Every registration
  binds `(spec.oidc.issuer, spec.oidc.allowedSubjects)`. At self-registration the
  gateway stamps `spec.oidc.issuer` from the validated token's `iss` claim — the
  caller cannot choose it. Admin-created registrations must set it explicitly.
  Every admission and credential-resolution path validates **both** the token's
  issuer and its subject against the binding (`validateOIDCIdentity`, below). A
  token from issuer B with `sub=cleanup-bot` cannot act as issuer A's registered
  `cleanup-bot` — it fails the issuer check with 403, regardless of subject match.

- **Collision resolution** for the losing party: if issuer B's bot finds its
  subject's name already claimed, it registers under a different `agentIdentity`
  (admin-created, since the self-service carve-out requires name == subject) with
  `allowedSubjects` containing its actual subject. `allowedSubjects` exists
  precisely to decouple the flat name from the IdP subject.

**Ordering constraint (hard):** repeatable `--oidc-issuer-url` must not merge
before issuer-binding validation does. Today's single-issuer deployments are not
exposed (the issuer check is trivially satisfied); a multi-issuer gateway without
the issuer check is an impersonation hole.

#### Deterministic naming and atomic dedup

Following the same pattern introduced for AgentRequests in #232 (which eliminated a
List→Create race): the gateway derives the K8s object name deterministically from
`agentIdentity`, making K8s `Create` the sole dedup primitive. No pre-flight existence
check, no race window.

```go
// registrationObjectName returns the stable K8s metadata.name for an agentIdentity.
// Format: <dns-slug>-<8 hex chars of sha256(agentIdentity)>
// The hash suffix prevents sanitization collisions:
//   "payment.bot" and "payment-bot" both sanitize to "payment-bot" but differ in hash.
// The hash deliberately excludes the issuer — see "Issuer binding" above. Hashing
// agentIdentity alone is what lets Create enforce one-registration-per-flat-name.
// Total length bounded to ≤ 63 chars.
func registrationObjectName(agentIdentity string) string {
    h := sha256.Sum256([]byte(agentIdentity))
    suffix := hex.EncodeToString(h[:])[:8]
    slug := sanitizeDNSSegment(agentIdentity, 54)
    return slug + "-" + suffix
}
```

On `POST /agent-registrations`:
- Gateway calls `registrationObjectName(body.AgentIdentity)` and attempts K8s `Create`.
- `AlreadyExists` → HTTP 409 (first-wins; same as AgentRequest dedup semantics).
- No list, no read-before-write, no race.

Deterministic naming is also the self-registration abuse control: one authenticated
identity can materialize at most **one** registration object, ever — a re-`POST`
hits 409. No in-process rate limiter is added (it would be incorrect under HA: each
gateway replica would track its own window). The residual surface — many *distinct*
identities flooding Pending — is an IdP provisioning-control question, bounded by
`--registration-pending-ttl` auto-deny (default 168h).

#### Watch cache

The gateway adds an `AgentRegistration` informer/cache watching its own deployment
namespace (from `POD_NAMESPACE`). Same pattern as `MCPServer` today
(`cmd/gateway/mcp_watch.go`).

```go
// cmd/gateway/registration_watch.go

// registrationCache is a read-through cache of AgentRegistration objects,
// keyed by agentIdentity (flat, not namespace-qualified).
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

**Credential ceiling enforcement:**

`providerFor` gates on `status.approvedServices` before returning a provider.
An `externalIdentities` binding for a service that was not approved at registration
time is inert — the caller falls through to the shared MCPServer token. This is the
scope ceiling: registration approval is the authorization boundary for which services
an agent may mint scoped credentials for.

**Inertness is never silent.** Fail-closed fall-through is correct behavior but an
operator debugging "why isn't my agent minting?" must not need gateway logs to find
out. The controller (which gains a Registration watch in Phase 10 for pending-TTL)
maintains a status condition per inert binding:

```yaml
status:
  conditions:
    - type: ServiceBindingInert
      status: "True"
      reason: ServiceNotApproved        # or: ServiceNotFound
      message: 'binding for "github" exists but "github" is not in approvedServices;
                admin must expand the ceiling via /approve'
```

`reason: ServiceNotFound` covers catalog drift: an MCPServer deleted *after* approval
leaves its `approvedServices` entry orphaned. The registration is not auto-modified
(no magic status writes from a different object's delete event); the binding is inert
by construction (no MCPServer = no route), the condition surfaces it, and the gateway
logs at `V(1)` on each fall-through. Recreating the MCPServer with the same name
restores the binding — approval was for the service name, which is the intended
semantic. `kubectl describe agentregistration <name>` answers the question in one look.

```go
// registrationCache.providerFor — enforcement point
func (c *registrationCache) providerFor(agentIdentity, service string) credential.Provider {
    c.mu.RLock()
    defer c.mu.RUnlock()
    reg := c.byAgent[agentIdentity]
    if reg == nil || reg.Status.Phase != "Approved" {
        return nil
    }
    // Ceiling check: service must be in status.approvedServices.
    if !slices.Contains(reg.Status.ApprovedServices, service) {
        return nil  // binding exists but was not approved — inert
    }
    svcProviders := c.providers[agentIdentity]
    if svcProviders == nil {
        return nil
    }
    return svcProviders[service]
}
```

Consequence: `auto` approval writes `status.approvedServices = spec.requestedServices`
at creation time. `manual` approval writes only the admin-selected subset. An operator
adding a new `externalIdentities` entry later must also get the new service approved
(or re-run auto-approval) before the ceiling lifts. This prevents credential-scope
creep via spec edits that bypass the approval gate.

**Ceiling changes invalidate caches immediately.** `upsert` diffs the old and new
registration on every watch event: if `status.approvedServices` shrank, `status.phase`
changed, or a binding changed, it calls `Invalidate()` on the affected providers.
Cached output tokens are dropped within one watch-event propagation (target: ≤5s);
the next mint re-evaluates against the new ceiling. Already-issued downstream tokens
are a separate question — most cannot be recalled:

| Provider | Cache drop | Issued-token recall |
|---|---|---|
| StaticSecret | immediate | n/a — secret rotation is the recall |
| GitHubApp | immediate | best-effort async `DELETE /installation/token` |
| AWSWebIdentity | immediate | none — drains within `durationSeconds` (≤1h default) |
| AzureWorkloadIdentity | immediate | none — drains within token TTL (~1h) |
| KubernetesOIDC | immediate | none — drains within exchanged-token TTL (~15m) |

This table is the honest answer to "you revoked it — is it dead?": new mints stop
within seconds; outstanding exchange-based tokens live out their (short) TTLs. Keep
provider TTLs short — that is the actual revocation latency knob.

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
    // Validate the token's (issuer, subject) pair against the registration's
    // binding. Replaces the existing loose `agentIdentity != sub` equality check.
    // issuer comes from the validated token in request context — never the body.
    if err := validateOIDCIdentity(reg, issuer, sub); err != nil {
        writeError(w, http.StatusForbidden, "IDENTITY_MISMATCH: "+err.Error())
        return
    }
}
```

**Identity validation fix — 400 → 403, equality → issuer-qualified allowedSubjects:**

The existing check (`agent_request_handlers.go:88`) fires only when `authRequired=true`
and uses exact equality (`body.AgentIdentity != sub`). This misses the case where an
agent legitimately uses a different OIDC subject name than its registered identity (e.g.,
`azp=payment-bot-prod` maps to `agentIdentity=payment-bot`), uses status 400 instead
of 403, and — critically — never checks the **issuer**, which becomes an impersonation
hole the moment a second `--oidc-issuer-url` is trusted.

The replacement:

```go
// validateOIDCIdentity checks the validated token's (issuer, sub) pair against
// the registration's OIDC binding. Both must match: subject membership alone is
// not sufficient under multi-issuer trust (issuer B could mint sub=cleanup-bot).
// Returns nil when the registration has no OIDC config (fallback to role checks).
func validateOIDCIdentity(reg *v1alpha1.AgentRegistration, issuer, sub string) error {
    if reg.Spec.OIDC == nil || len(reg.Spec.OIDC.AllowedSubjects) == 0 {
        return nil
    }
    if reg.Spec.OIDC.Issuer != issuer {
        return fmt.Errorf("token issuer %q does not match registered issuer for agent %q",
            issuer, reg.Spec.AgentIdentity)
    }
    if slices.Contains(reg.Spec.OIDC.AllowedSubjects, sub) {
        return nil
    }
    return fmt.Errorf("token subject %q not in allowedSubjects for agent %q",
        sub, reg.Spec.AgentIdentity)
}
```

The same check guards the MCP credential-resolution path: `resolveAgentCredential`
calls `validateOIDCIdentity` with the issuer and subject of the token on the MCP
request before any provider is consulted. Registration lookup by flat name plus
subject-only validation is exactly the fusion bug — the issuer check is what makes
flat names safe under multi-issuer trust.

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

### 6. MCPServer catalog and `requestedServices` validation

#### `GET /services` — the advertised catalog

```
GET /services    roleAgent, roleReviewer, roleAdmin
```

Returns the MCPServer names available for registration in this deployment. The
namespace is opaque to the caller — the gateway serves a flat list of names derived
from its MCPServer watch cache. `aipctl services list` and `aipctl register
--services ...` autocomplete consume this endpoint.

Response:
```json
{ "services": ["github", "k8s", "bedrock", "azure-devops"] }
```

The catalog = MCPServers in the gateway's own deployment namespace. These are
operator-installed services. No configuration needed — the gateway already watches
this namespace.

**Tenant filtering is built into the endpoint from day one, not a follow-up.**
The filter rule: an MCPServer without a `governance.aip.io/tenant` label is global
(visible to every authenticated caller); a tenant-labeled MCPServer is visible only
to callers whose verified tenant matches (Phase 13a semantics). In a single-tenant
deployment nothing carries the label, so the endpoint returns the full catalog —
that is the documented intended behavior, not a leak. The moment an operator labels
a service, the filter is already active; there is no window where tenant-scoped
services are exposed deployment-wide, and no flag to remember to turn on.

#### Validation at registration time

`spec.requestedServices` entries are validated against the catalog on
`POST /agent-registrations`. An unknown service name is a 400 with the available
names in the error body — the error teaches:

```json
{
  "error": "unknown service \"datadog\"; available: [github, k8s, bedrock]"
}
```

This enforces `requestedServices ⊆ knownMCPServers` at write time, which means
`approvedServices` (the ceiling) is always a subset of real, named objects.
No phantom service bindings can exist.

The `externalIdentities[].service` field undergoes the same validation — a binding
for an unknown MCPServer is rejected at registration, not silently ignored at runtime.

### 7. `--unregistered-agent-policy` flag

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

### Phase 8b — per-agent Keycloak credentials for GitHub MCP ✅ DONE (PR #245)

Extends `test/e2e/gateway_keycloak_test.go` (existing Phase 8) with a new Context.
No new suite file. Requires `AIP_E2E_GITHUB_PAT_AGENT1` and `AIP_E2E_GITHUB_PAT_AGENT2`.

```text
BeforeAll:
  - Register aip-agent-2 in Keycloak (new client, reuses kcSetup helpers)
  - Create per-agent PAT Secrets (agent1, agent2) via kubectl (infra-level Secret,
    not an AIP object — kubectl is acceptable for Secret management by cluster ops)
  - Create AgentRegistrations via gateway API (admin JWT):
      POST /agent-registrations  ← Phase 5 endpoint; no kubectl needed
  - Wait for ATP to exist (controller bootstrap from Registration)
  - Gateway subprocess already running with --oidc-issuer-url=keycloak (Phase 8 setup)

It "agent-1 Keycloak token → GitHub MCP call uses agent-1 PAT":       ✅
It "agent-2 Keycloak token → GitHub MCP call uses agent-2 PAT":       ✅
It "unregistered agent warn mode → annotated, admitted":               ✅
It "--unregistered-agent-policy=strict rejects unknown agent":         ✅
```

Note: Phase 8b was implemented before Phase 5 (gateway CRUD endpoints) landed.
The actual test code still uses `kubectl apply` for AgentRegistrations. Phase 5
will add a migration step to switch Phase 8b to use `POST /agent-registrations`.

### Phase 9 — Full identity propagation E2E + provider exchange mechanics

New `Describe` block in `test/e2e/gateway_keycloak_test.go`, gated behind
`OIDC_KIND_CLUSTER=true`. Requires a Kind cluster created with OIDC config
(see `test/fixtures/kind-oidc.yaml` below).

This phase consolidates:
- K8s MCP identity propagation with full audit trail verification (new)
- Azure WIF + AWS WebIdentity stub tests (moved from planned Phase 8c)

#### 9a — K8s MCP: Keycloak → token exchange → K8s audit trail

```text
Prerequisites (BeforeAll):
  - Kind cluster created from test/fixtures/kind-oidc.yaml:
      API server: --oidc-issuer-url=http://keycloak.keycloak.svc/realms/aip
                  --oidc-client-id=kubernetes
                  --oidc-username-claim=sub
                  --audit-log-path=/var/log/kubernetes/audit.log
  - Keycloak deployed with token exchange enabled:
      kcEnableTokenExchange(port, realm)           ← new helper
      kcCreateAudienceClient(port, realm, "kubernetes")  ← new helper
  - K8s MCP server deployed via kubectl apply -f config/mcp/k8s-mcp-server.yaml
    (infra-level Deployment/Service — kubectl acceptable here, same as AIP itself)
  - MCPServer CR picked up automatically by gateway's existing mcp_watch.go
  - Admin Keycloak JWT obtained; AgentRegistration created via gateway API:
      POST /agent-registrations   (adminToken)   ← Phase 5 endpoint, no kubectl
      {
        "agentIdentity": "aip-agent-1",
        "oidc": {"issuer": "...", "subjectClaim": "azp", "allowedSubjects": ["aip-agent-1"]},
        "externalIdentities": [{
          "service": "k8s-mcp",
          "type": "KubernetesOIDC",
          "kubernetesOIDC": {
            "tokenExchangeURL": "http://keycloak.keycloak.svc/realms/aip/protocol/openid-connect/token",
            "audience": "kubernetes"
          }
        }]
      }
  - K8s RBAC: ClusterRoleBinding binding aip-agent-1 (sub claim) to a Role
    that permits list/get on ConfigMaps in the test namespace

It "Keycloak JWT → KubernetesOIDC exchange → K8s audit shows agent identity":
  - Fetch Keycloak token for aip-agent-1 (aud=aip-gateway)
  - Submit AgentRequest, approve, execute MCP tool (e.g. list_configmaps)
  - Gateway: exchanges JWT at Keycloak → gets aud=kubernetes JWT
  - K8s MCP server: forwards aud=kubernetes token to K8s API
  - Read audit log from Kind control-plane node
  - Assert: audit entry user.username == "aip-agent-1" (the Keycloak sub)
  - Assert: NOT user.username == "system:serviceaccount:aip-k8s-system:aip-k8s-controller"

It "token exchange is cached: second call within TTL skips re-exchange":
  - Make two sequential MCP calls for same agent
  - Instrument Keycloak exchange endpoint call count via audit log
  - Assert exchange called exactly once; second call hits TokenCache

AfterAll:
  - DELETE /agent-registrations/reg-aip-agent-1 (adminToken) ← gateway API, no kubectl
  - kubectl delete clusterrolebinding aip-agent-1-k8s-mcp --ignore-not-found
  (MCPServer CR cleanup handled by existing gwCleanup helper)
```

#### Kind cluster config: `test/fixtures/kind-oidc.yaml`

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiServer:
      extraArgs:
        oidc-issuer-url: http://keycloak.keycloak.svc.cluster.local:8080/realms/aip
        oidc-client-id: kubernetes
        oidc-username-claim: sub
        audit-log-path: /var/log/kubernetes/audit/audit.log
        audit-policy-file: /etc/kubernetes/audit-policy.yaml
      extraVolumes:
      - name: audit-logs
        hostPath: /var/log/kubernetes/audit
        mountPath: /var/log/kubernetes/audit
        readOnly: false
        pathType: DirectoryOrCreate
      - name: audit-policy
        hostPath: /etc/kubernetes/audit-policy
        mountPath: /etc/kubernetes/audit-policy
        readOnly: true
        pathType: DirectoryOrCreate
```

Note: The K8s API server fetches Keycloak's OIDC discovery document lazily (on
first token validation), not at startup. Keycloak can therefore be deployed after
the cluster is up — no chicken-and-egg problem.

#### K8s MCP server

Manifests at `config/mcp/k8s-mcp-server.yaml`. The server must forward the
incoming Bearer token to the K8s API (not use its own ServiceAccount). Add a
`deployK8sMCPServer()` helper in the test suite following the same pattern as
`deployGitHubMCPServer()`.

Candidate image: `ghcr.io/manusa/kubernetes-mcp-server:latest` — verify it
forwards the incoming Bearer token before committing.

#### 9b — Provider exchange stubs (no cloud accounts)

Tests `AzureWorkloadIdentityProvider` and `AWSWebIdentityProvider` with
`httptest.NewServer` stubs. Lives in `gateway_keycloak_test.go`.

```text
It "AzureWorkloadIdentity uses client_credentials + federated identity, not OBO":
  - Stub Azure token endpoint: validates grant_type=client_credentials,
    client_assertion == agent OIDC token, returns synthetic Azure AD token
  - Stub upstream MCP server: captures Authorization header
  - AgentRegistration with AzureWorkloadIdentity pointing at stub endpoint
  - Submit + approve + execute MCP call
  - Assert stub MCP received the exchanged token (not the raw OIDC token)
  - Assert stub endpoint received client_credentials grant (not OBO)

It "AWSWebIdentity calls STS and uses session token for upstream MCP call":
  - Stub STS endpoint: validates Action=AssumeRoleWithWebIdentity,
    WebIdentityToken == agent OIDC token; returns synthetic temp credentials
  - Stub upstream MCP server: captures credential bundle in Authorization header
  - Assert STS stub was called once; second request within TTL hits cache

It "token cache: concurrent requests for same agent deduplicate exchange calls":
  - 10 concurrent MCP calls for same agent (singleflight)
  - Assert stub exchange endpoint called exactly once
```

These tests do NOT require `OIDC_KIND_CLUSTER=true` — they use httptest stubs
and run in any e2e environment.

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

### Phase 4 — E2e tests ✅ DONE (Phase 8b merged in PR #245)

Files completed:
- `test/e2e/gateway_keycloak_test.go` — Phase 8b (per-agent GitHub MCP credentials)

### Phase 5 — Gateway AgentRegistration CRUD endpoints

Admins must never need `kubectl` or direct K8s API access. `AgentRegistration`
objects are created, updated, and deleted via the gateway HTTP API using an admin
JWT — the same gateway that agents use for `AgentRequest` submission.

New routes:
```
POST   /agent-registrations           roleAdmin
GET    /agent-registrations           roleAdmin, roleReviewer
GET    /agent-registrations/{name}    roleAdmin, roleReviewer
PUT    /agent-registrations/{name}    roleAdmin
DELETE /agent-registrations/{name}    roleAdmin
```

Files:
- `cmd/gateway/agent_registration_handlers.go` — handler implementations
- `cmd/gateway/main.go` — register routes
- `cmd/gateway/integration_agent_registration_test.go` — integration tests using
  an in-process test OIDC provider (real JWT signing, no Keycloak needed):
    - Admin JWT → POST → 201; registrationCache upserted
    - Agent JWT → POST → 403
    - Admin DELETE → cache removes entry; next unregistered AgentRequest gets policy treatment
    - Reviewer GET → 200 (read-only access)
- `docs/api-reference.md` + auth rules table; `make docs-lint` passes
- Phase 8b test code migrated from `kubectl apply AgentRegistration` →
  `POST /agent-registrations` with admin JWT

### Phase 9 — Full identity propagation E2E

Prerequisites: Phase 5 landed; Kind cluster with OIDC + audit log config;
Keycloak token exchange enabled; K8s MCP server deployed.

Files:
- `test/fixtures/kind-oidc.yaml` — Kind cluster config with `--oidc-issuer-url` +
  audit log enabled
- `config/mcp/k8s-mcp-server.yaml` — K8s MCP server Deployment + Service
- `test/e2e/gateway_keycloak_test.go` — Phase 9 `Describe` block gated behind
  `OIDC_KIND_CLUSTER=true`, covering:
  - 9a: K8s MCP identity propagation + audit trail verification
  - 9b: Azure WIF + AWS WebIdentity httptest stub tests
- `test/e2e_azure/` — suite skeleton (build tag `azure_e2e`, skipped by default)
- `test/e2e_aws/` — suite skeleton (build tag `aws_e2e`, skipped by default)
- `.github/workflows/chart-e2e.yml` — document `OIDC_KIND_CLUSTER` env var
  requirement and note cluster recreation needed

### Phase 10 — Registration lifecycle (status.phase + approve/deny)

Files:
- `api/v1alpha1/agentregistration_types.go` — `spec.mode`, `spec.requestedServices`,
  `status.phase`, `status.registeredBy`, `status.approvedServices`,
  `status.approvedAt`, conditions; `make generate && make manifests`
- `cmd/gateway/agent_registration_handlers.go` — self-registration carve-out
  (identity == token subject, issuer stamped from token `iss`, exempt from strict
  policy), approve/deny handlers (single status patch with
  `MergeFromWithOptimisticLock`; mirror `agent_request_lifecycle_handlers.go`),
  SSE watch
- `cmd/gateway/agent_request_handlers.go` — `validateOIDCSubject` →
  `validateOIDCIdentity` (issuer + subject; see §3 Issuer binding). **Hard
  ordering: this lands in the same PR as — or before — repeatable
  `--oidc-issuer-url`; a multi-issuer gateway with subject-only validation is an
  impersonation hole**
- `cmd/gateway/registration_watch.go` — cache honors only `Approved`; `upsert`
  diffs approvedServices/phase and invalidates affected providers
- `cmd/gateway/main.go` — `--registration-policy=auto|manual` (default auto),
  `--registration-pending-ttl`, repeatable `--oidc-issuer-url`
- `internal/controller/` — pending-TTL deny; max-age re-attestation scan
  (`status.approvedAt`); `ServiceBindingInert` condition maintenance; ATP
  pre-create moves to Approved transition
- `docs/api-reference.md` + auth rules — `make docs-generate && make docs-lint`
- Integration tests: self-register under strict → Pending; agent cannot approve own
  registration; auto vs manual policy; deny-by-TTL; issuer mismatch → 403;
  concurrent approve/deny → one wins, loser gets 409

### Phase 11 — `aipctl`

Files:
- `cmd/aipctl/` — login (device flow + keychain), register, kubeconfig, token
  (ExecCredential protocol, interactive blocking wait, `--no-wait`), registrations
  list/approve/deny/revert
- `cmd/gateway/` — `GET /.well-known/aip` discovery endpoint
- Session-intent convention: `action: session.k8s` AgentRequest; gateway token
  endpoint accepts session-intent approvals; `act` claim populated in exchange
- e2e: scripted transcript from the Acceptance demo section against Kind + Keycloak

### Phase 12 — `aip-go` client library

Files:
- New module `aip-go/` (or `pkg/client/` until API settles) — `NewTransport`
  (`transport.WrapperFunc`), projected-SA-token identity source, session mint/refresh
  loop, `PendingApprovalError`, `WaitForApproval`
- Example operator under `examples/` consuming the wrapper in one line
- e2e: operator pod in governed mode exercising the wrapped client against Kind

### Phase 13 — Claims-scoped admin, GitHub App provider, chart hardening

#### Phase 13a — Claims-scoped admin (tenant filtering — not a security boundary)

`roleAdmin` is global today — any admin can approve/deny any registration. For
multi-team deployments, admin scope must be bounded to the set of agents the admin
is responsible for, without exposing K8s namespace topology to the registration
protocol.

**Threat model, stated up front:** the tenant label is a *filtering* mechanism
enforced by gateway code, not a security boundary. All registrations live in one
namespace; there is no RBAC, quota, or network isolation between tenants — a bug
in the claim check, or any principal with `patch` on that namespace, crosses
tenants. The real isolation boundary in this design is the **issuer binding**
(§3): authentication is issuer-qualified, so tenant A's IdP can never mint a token
that authenticates as tenant B's agent, regardless of label bugs. Label-scoped
admin is a workflow convenience on top of that. The migration path to hard
isolation is namespace-per-tenant storage with an admission webhook enforcing
namespace↔tenant affinity — the flat-name + issuer-binding identity model does
not foreclose it, because identity was never namespace-derived.

**Interface (to be settled before implementation):**

Each registration is stamped at create time with a tenant label:

```
governance.aip.io/tenant: <value>
```

**The tenant value derives from the verified issuer by default — no flag, no
self-assertion.** The gateway already validated the token's `iss` before reading
any claim; `tenant = normalize(issuer)` cannot be forged by a registrant and
needs no configuration. This is the only mode most deployments need: one IdP per
team is the common multi-team topology.

`--tenant-claim` (optional, **no default**) exists for sub-tenant grouping within
a single issuer. It must name a scalar claim. Positional array access — `groups[0]`
— is explicitly unsupported: OIDC array ordering is not guaranteed stable, and a
nondeterministic tenant assignment is worse than none. Using `--tenant-claim`
across *multiple* issuers additionally requires an explicit issuer→tenant trust
map (otherwise issuer A can assert issuer B's claim value); that map is deferred
until a deployment needs custom claims and multi-issuer simultaneously, and the
gateway refuses the combination at startup until it exists — fail-closed, not
fail-quiet.

An admin's approve/deny succeeds only when their own verified tenant matches the
registration's label, enforced server-side. One mechanism, three uses: who can
approve a registration, which MCPServers appear in a team's `GET /services`
catalog, and (later) where registrations are stored if multi-namespace lands.

No new CRD. Until this phase ships, global admin is the **documented** (not
implicit) model and the architecture lands this as an additive change with no
migration.

Files:
- `cmd/gateway/agent_registration_handlers.go` — stamp `tenant` label on create
  (issuer-derived); enforce tenant match on approve/deny
- `cmd/gateway/main.go` — optional `--tenant-claim` flag (no default); startup
  rejection of multi-issuer + custom claim without a trust map
- `cmd/gateway/registration_watch.go` — per-tenant catalog filtering by tenant
  label on MCPServer (no flag; gateway namespace is the anchor)
- Integration tests: admin with matching tenant approves; admin without gets 403;
  `GET /services` returns only tenant-matched + global services; registrant
  cannot influence its own tenant label

#### Phase 13b — GitHub App installation-token provider

Removes long-lived PATs from agents that write to GitHub: every minted token is
short-lived (≤1h) and scoped to explicit repos and permissions.

**Honest tiering — this is the deliberate degraded row, not an equivalent peer.**
The exchange-based providers hold nothing: the agent's live OIDC token *is* the
credential presented to STS/Entra/Keycloak. The GitHub App provider holds a
standing, high-authority secret — whoever has the App private key can mint
installation tokens for everything the App is installed on, for any agent. That
is the ambient-authority shape that got K8s impersonation rejected from this
design, accepted here knowingly because GitHub offers no web-identity federation
for installation tokens. The consequence is a hard rule: **every authority-bounding
field is required, none default open.**

```text
Provider tier table (normative):

  AWSWebIdentity         broker holds nothing — agent token IS the STS credential
  AzureWorkloadIdentity  broker holds nothing — agent token IS the client_assertion
  KubernetesOIDC         broker holds nothing — agent token IS the exchange input
  StaticSecret           broker holds a long-lived per-agent secret (legacy bridge)
  GitHubApp              broker holds a standing org-authority key (bounded bridge)
```

```go
// internal/credential/github_app.go

// GitHubAppProvider mints a GitHub App installation token. Unlike the
// exchange-based providers, the agent's OIDC token is NOT the credential —
// the App private key is. The agent token gates the mint (token-gated mint
// invariant: Token() errors on empty agentOIDCToken) but GitHub never sees it.
//
// Flow:
//   1. Sign a GitHub App JWT from the App's private key (RS256, 10-min TTL).
//   2. POST /app/installations/{installationID}/access_tokens with the
//      registration's exact repositories + permissions. Token valid ≤1h.
// No installation discovery: installationID is pinned in the binding.
type GitHubAppProvider struct { ... }
```

New `ExternalIdentityType`:

```go
ExternalIdentityGitHubApp ExternalIdentityType = "GitHubApp"

type GitHubAppCredential struct {
    // AppID is the GitHub App's numeric ID.
    AppID int64 `json:"appID"`
    // PrivateKeySecretRef points to the K8s Secret holding the App's PEM private key.
    PrivateKeySecretRef SecretKeyRef `json:"privateKeySecretRef"`
    // InstallationID pins the installation. Required — auto-discovery via
    // GET /app/installations would let a misconfigured org-wide App silently
    // select an installation; the blast radius is too high. One-time lookup:
    // `aipctl github installations --app-id <id>` (or the GitHub UI).
    // +kubebuilder:validation:Minimum=1
    InstallationID int64 `json:"installationID"`
    // Repositories scopes the token. Required and non-empty — "empty means
    // all installed repos" is a least-privilege footgun, rejected at admission.
    // +kubebuilder:validation:MinItems=1
    Repositories []string `json:"repositories"`
    // Permissions scopes the token (e.g. {"contents":"write","pull_requests":"write"}).
    // Required and non-empty — same rationale as Repositories. Rejected at
    // admission, not silently inert at runtime.
    // +kubebuilder:validation:MinProperties=1
    Permissions map[string]string `json:"permissions"`
}
```

Operator guidance (documented, not enforceable by AIP): install the App on the
specific repositories it needs, never org-wide; restrict RBAC on the private-key
Secret to the gateway SA; rotate the key on a schedule.

**Attribution loses fidelity at the GitHub boundary — say so.** GitHub's audit
log shows the App installation; there is no API field to carry the acting agent
or operator (the earlier draft's `actor` reference was wrong — no such parameter
exists on the installation-token endpoint). The two-level `act`-claim attribution
ends at AIP's edge for this provider. What remains, in order of authority:
AIP's own audit records (AgentRequest + approval + `act` claim — authoritative
for "who did this"); a correlation convention in the artifacts the agent creates
(commit author set to the agent identity, PR/commit body carrying
`AIP-Request: <name>`); and GitHub showing the App. If end-to-end attribution at
the GitHub boundary is a hard requirement, this provider is the wrong tool —
that requirement currently has no GitHub-side answer.

Revocation: installation tokens support recall — on `Invalidate()` the provider
additionally issues a best-effort asynchronous `DELETE /installation/token` for
outstanding tokens (the one provider in the table where issued-token recall exists).

Cache key: `(agentIdentity, appID, installationID, sha256(repos+perms))`.
Output TTL: token expiry from GitHub response (≤1h). Refresh within 5-min buffer.

Files:
- `api/v1alpha1/agentregistration_types.go` — `GitHubApp` type + credential struct
- `internal/credential/github_app.go` — provider implementation
- `internal/credential/github_app_test.go` — httptest stub for GitHub API
- `cmd/gateway/registration_watch.go` — construct `GitHubAppProvider` in `upsert`

#### Phase 13c — Chart hardening

Adoption blocker for any second deployer. All three items are independent and can
land as a single PR:

- `nodeSelector` and `tolerations` support for gateway and controller Deployments
  (patched locally by multiple adopters; trivial upstream addition)
- Remove `:latest` image tag default; use
  `{{ .Values.image.tag | default .Chart.AppVersion }}` (already required by
  CLAUDE.md Helm standards, just not applied)
- Bump `Chart.AppVersion` past `0.1.0` to reflect the phases already shipped

Files: `charts/aip-k8s/Chart.yaml`, `charts/aip-k8s/values.yaml`,
`charts/aip-k8s/templates/deployment-gateway.yaml`,
`charts/aip-k8s/templates/deployment-controller.yaml`

### Phase 14 — Cross-phase e2e (the version-skew net)

Phases 10–13 interlock: identity validation, dedup, ceiling, catalog, session
intents, SSE, and providers all move together, mostly authored by one person.
Per-phase tests verify each piece against the state of the world when it merged;
nothing verifies the *composition* as later phases shift the ground. That is the
silent-version-skew class of bug, and a journey test is the highest-ROI net for it.

One Ginkgo suite, one Kind + Keycloak cluster, exercising the full lifecycle in
order — each step consuming the artifacts of the previous, no per-step setup
that would mask drift:

```text
self-register (strict+manual → Pending)
  → admin approves a SUBSET of requestedServices (optimistic-lock patch)
  → ceiling enforced: unapproved service falls through, condition
    ServiceBindingInert visible via kubectl
  → approved service mints through the live-token path (invariant: no mint
    without the agent's token)
  → session intent → aipctl token → kubectl against Kind succeeds
  → admin narrows approvedServices → cache invalidated, next mint denied
  → deny registration → SSE notifies, mint stops, drains per SLO table
  → re-register attempt → 409 (deterministic name, first-wins)
```

Per the repo's e2e rules (CLAUDE.md): runs in both Kustomize and Helm modes —
every flag the suite depends on (`--registration-policy`, `--unregistered-agent-policy`,
`--registration-pending-ttl`) must be set in the `helm upgrade --install` command
in `.github/workflows/chart-e2e.yml`, cleanup deletes by `--all`, and the
suite uses the shared `serviceAccountName`/`controllerDeploymentName` constants.

Files:
- `test/e2e/registration_lifecycle_test.go` — the journey suite
- `.github/workflows/chart-e2e.yml` — flag parity for Helm mode

---

## Self-Service Registration and External Agents (`aipctl`)

Everything above assumes an operator provisions registrations via admin API and the
agent runs where credentials can be injected. This section covers the remaining — and
largest — agent population: **scripts, cron jobs, and developer-laptop agents** that
hold a kubeconfig today, plus in-cluster operators that cannot change their code.

Design constraint carried throughout: **the gateway never enters the K8s data path.**
Reads and writes go directly to the apiserver with minted credentials. AIP is the
credential mint and the audit brain, not a proxy. (A governing apiserver proxy was
considered and deferred — see Deferred Work below.)

### Registration lifecycle: `status.phase`

`AgentRegistration` gains a lifecycle so agents can request registration and admins
approve it — mirroring the AgentRequest approve/deny pattern (same roles, same audit
records, same handler shape).

**An `Approved` registration grants no capability by itself.** Approval makes the
agent *known* — eligible for credential flows and attributable in audit. Every
credential still flows through an approved intent / session-intent with its own
policy evaluation. `requestedServices` declares which bindings exist, not what the
agent may do with them; policies decide that per action or per session.

Spec additions:

```go
type AgentRegistrationSpec struct {
    // ... existing fields ...

    // Mode selects the credential posture for this agent.
    //   "Standing" (default) — agent keeps its existing access; AIP provides
    //   attribution, shadow policies, and audit. Pure addition, zero risk.
    //   "Governed" — writes flow through approved intents + JIT-minted tokens.
    //
    // Standing is deliberately the default despite being the more permissive
    // posture. Registration must never change an existing agent's behavior
    // without explicit opt-in (design principle 1: the affected party consents).
    // A Governed default would mean the act of registering cuts off an agent's
    // write path — imposed, not chosen. Defaulting to Standing keeps enrollment
    // a zero-risk observation step; the developer requests Governed, the admin
    // countersigns. This is a considered decision, not an oversight.
    // +kubebuilder:validation:Enum=Standing;Governed
    // +kubebuilder:default=Standing
    Mode string `json:"mode,omitempty"`

    // RequestedServices lists the services the agent wants credential
    // bindings for (e.g. "k8s", "github"). Admin may approve a subset.
    RequestedServices []string `json:"requestedServices,omitempty"`
}

type AgentRegistrationStatus struct {
    // Phase: "" | Pending | Approved | Denied
    Phase string `json:"phase,omitempty"`

    // RegisteredBy records the human identity whose login submitted this
    // registration (laptop/script agents). Set by the gateway from the
    // authenticated caller; immutable thereafter. This answers "who registered
    // it" — per-action operator attribution comes from the per-exchange `act`
    // claim, not this field (a shared or CI agent is operated by different
    // humans across sessions).
    RegisteredBy string `json:"registeredBy,omitempty"`

    // ApprovedServices is the admin-confirmed subset of spec.requestedServices.
    // Under auto policy it equals requestedServices. Under manual policy the
    // admin may approve a subset; bindings for unapproved services are inert
    // even if externalIdentities carries a matching entry. This is the scope
    // ceiling — see "Credential ceiling enforcement" in §3.
    // +optional
    ApprovedServices []string `json:"approvedServices,omitempty"`

    // ApprovedAt records when the registration last transitioned to Approved.
    // Drives --registration-max-age re-attestation and gives auditors the
    // approval timestamp without scraping conditions.
    // +optional
    ApprovedAt *metav1.Time `json:"approvedAt,omitempty"`

    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

Behavior:

- `registrationCache` only honors `Approved` registrations. Pending/Denied are inert.
- **Bootstrap carve-out**: an authenticated-but-unregistered agent may `POST` its own
  registration (`agentIdentity` must equal its token subject — enforced server-side)
  even under `--unregistered-agent-policy=strict`. This is the only endpoint exempt
  from the registration check. Authentication is still required. The gateway stamps
  `spec.oidc.issuer` from the validated token's `iss` claim — the caller cannot
  choose its issuer binding (see "Issuer binding" in §3). Admin-created
  registrations must set the issuer explicitly; the create handler rejects an
  OIDC block without one.
- **Approval gate is a dial, default open**: `--registration-policy=auto|manual`.
  `auto` approves immediately on creation — registration feels like installing a
  GitHub App — and grants **all** `requestedServices` as requested (subset selection
  is a manual-mode capability; there is no admin in the loop to choose one).
  `manual` holds at Pending for admin approve/deny, optionally with a service subset.
- **Defaults compose safely with `--unregistered-agent-policy`.** Under `strict`,
  auto self-registration would make any *authenticated* caller registered
  immediately — `strict` would degrade from a vetting gate to a mere authentication
  gate, which is not what operators selecting `strict` expect. Therefore the
  unset default of `--registration-policy` is mode-dependent:

  | `--unregistered-agent-policy` | `--registration-policy` default | Result |
  |---|---|---|
  | `allow` / `warn` | `auto` | frictionless self-service onboarding |
  | `strict` | `manual` | human-vetted registration |

  Explicitly setting `strict` + `auto` remains allowed (authenticated self-service —
  identity vetting delegated entirely to the IdP) but the gateway logs a startup
  warning naming the trade-off.

- **Registration is the authorization boundary, not the action gate.** An `Approved`
  registration makes the agent *known* and sets the credential ceiling — it does not
  grant the right to act. Every credential mint still flows through a separate approved
  intent (for Governed agents, an approved `AgentRequest`; for Standing agents, an
  approved session intent). The full authorization chain is:

  ```
  Authenticated (OIDC token valid)
    → Registered (AgentRegistration Approved)
      → Intent approved (AgentRequest / session intent Approved by policy/human)
        → Credential minted within approvedServices ceiling
          → Action executed
  ```

  Breaking any link blocks the action. `auto` registration shortens step 2 to
  milliseconds; it does not bypass steps 3–5. `strict` enforces step 2 by hand;
  steps 3–5 are unchanged regardless of registration policy.

- **`auto` sets `approvedServices = requestedServices` atomically at creation.** The
  approval handler writes `status.approvedServices` in the same patch that transitions
  `status.phase → Approved`. There is no window where Phase=Approved but
  `approvedServices` is empty. `manual` approval always requires an explicit body:

  ```json
  POST /agent-registrations/{name}/approve
  { "approvedServices": ["k8s"] }   ← required; omitting = deny all services
  ```

  Implementation constraint, not a suggestion: one `Status().Patch()` with
  `client.MergeFromWithOptimisticLock` on a base freshly read via APIReader.
  Phase, `approvedServices`, and `approvedAt` land together or not at all. The
  optimistic lock turns concurrent admin decisions (two admins approving different
  subsets, or approve racing deny) into a 409 for the loser instead of a silent
  last-write-wins merge. A failed patch leaves the registration Pending; the
  losing admin retries against fresh state.

- New routes (mirror AgentRequest lifecycle handlers):

```
POST /agent-registrations/{name}/approve   roleAdmin
POST /agent-registrations/{name}/deny      roleAdmin
GET  /agent-registrations/{name}/watch     SSE, owner or roleAdmin/roleReviewer
```

- Pending registrations are denied-by-timeout after `--registration-pending-ttl`
  (default 168h) so abandoned requests don't accumulate as standing approval risk.
- **Revocation and kill-switch paths are mode-specific.** The dashboard revoke flow
  and `aipctl registrations deny` must surface this table:

  | Mode | What approval gave | Revocation effect | One-command kill-switch |
  |---|---|---|---|
  | Standing | attribution + audit | removes attribution + credential bindings; agent's own RBAC unchanged | `kubectl delete rolebinding <agent>` (out of AIP) |
  | Governed | the only write path (mint stops) | agent loses all JIT tokens; in-flight sessions drain within TTL | `aipctl registrations deny <name>` |

  For Standing agents the RoleBinding revocation is outside AIP by design — AIP
  never created it. The dashboard must link to the relevant RBAC objects so an
  operator can revoke with one click, even though AIP does not execute the deletion.
  For Governed agents, `deny` transitions `status.phase → Denied`, removes the
  registration from `registrationCache`, and the SSE watch notifies active `aipctl`
  sessions so they surface an error immediately rather than waiting for token expiry.
- **Approved registrations can re-attest**: optional `--registration-max-age`
  (default off) transitions Approved → Pending after the configured age (measured
  from `status.approvedAt`, scanned by the controller), forcing re-approval so
  long-lived registrations don't quietly become standing risk. Re-attestation of
  an active agent under `auto` policy is a no-op by design. The default stays off —
  silently flipping long-running agents to Pending on upgrade would be a behavior
  surprise — but the production-posture documentation recommends `90d` alongside
  `strict` + `manual`.

### `aipctl`

A single client binary covering both agent-developer and admin verbs. Role
enforcement is server-side; the CLI exposes explicit verbs (no role-sniffing magic).

```
aipctl login --gateway https://aip.example          # OIDC device flow; token cached in OS keychain
aipctl register <name> [--mode governed] [--services k8s,github] [--wait]
aipctl kubeconfig <name>                             # emits exec-credential-plugin kubeconfig
aipctl token <name>                                  # ExecCredential protocol (used by kubectl, not humans)
aipctl registrations list [--pending]
aipctl registrations approve|deny <name>
aipctl registrations revert <name>                   # governed → standing, one command
```

Key behaviors:

- **Zero-config bootstrap**: the gateway serves a discovery document
  (`GET /.well-known/aip`) carrying issuer URL, client ID, and device endpoint.
  `aipctl login` needs only the gateway URL. The document exposes nothing beyond
  standard public OIDC discovery fields — the client ID is a public client
  (device flow / PKCE, no secret), and the issuer/device endpoints are already
  public at the IdP's own `/.well-known/openid-configuration`.
- **Exec credential plugin**: `aipctl kubeconfig` emits a kubeconfig whose
  `users[].user.exec` invokes `aipctl token`. kubectl/client-go calls it on demand;
  aipctl presents the cached login token, the gateway evaluates a session intent,
  and the minted token is returned via the `ExecCredential` JSON protocol with
  expiry. kubectl caches until expiry. **kubectl then talks directly to the
  apiserver** — the gateway is not in the request path.
- **Blocking approval UX**: when a session intent requires human approval and the
  terminal is interactive, `aipctl token` waits on the SSE stream and prints the
  approval URL — kubectl appears to pause politely, then completes when the admin
  approves. Non-interactive callers (`--no-wait`, no TTY) fail fast with a
  machine-readable receipt (AgentRequest name) and a non-zero exit; cron retries
  naturally on its next run.

### Session intents (no new CRD)

Laptop/script credential issuance is modeled as a plain `AgentRequest`:

```
action:    "session.k8s"
targetURI: scope URI, e.g. "k8s://prod/payments/deployment/*"
parameters: { "ttl": "15m" }
```

Policies, approval flow, audit records, and the dashboard apply unchanged. Approval
authorizes a mint via the existing token endpoint. Granularity is **per-session**
(JIT-access model, like Teleport/Azure PIM), not per-write — per-action granularity
remains available on the MCP path. This trade is deliberate; see Deferred Work.

### Two-level attribution: the `act` claim

A laptop agent's actions must attribute to both the agent and the human operating
it, or laptop agents become identity laundering. RFC 8693 — which the exchange
already implements — defines the actor claim for exactly this:

```
sub: cleanup-script          # the agent
act: { sub: ravi@example }   # the human whose login enabled it
```

The gateway populates `act` from the login identity **at each exchange** — this
per-session claim is the authoritative record of who operated a given action, and
it is what audit consumers must read. `status.registeredBy` is static provenance
("who registered this agent"), not operator attribution: a shared or CI agent is
operated by different humans across sessions, and only the per-exchange `act`
claim tracks that. The K8s audit log then answers: *"cleanup-script, operated by
ravi (this session), scaled payment-api, under approval X."*

### Client library: `aip-go` (operators that cannot change)

`rest.InClusterConfig()` does not support exec credential plugins, so the laptop
mechanism does not translate to in-cluster operators. The equivalent is a
transport-level wrapper — small, optional everywhere else, required only here:

```go
cfg, _ := rest.InClusterConfig()
cfg.Wrap(aipclient.NewTransport(gatewayURL, "my-operator"))   // one line in main.go
mgr, _ := ctrl.NewManager(cfg, opts)                          // unchanged
```

- `aipclient.NewTransport` sources the inbound identity from the pod's projected SA
  token (`aud: aip-gateway`), maintains the session-intent/mint/refresh loop, and
  injects the minted bearer token on outbound apiserver requests.
- Typed `aipclient.PendingApprovalError` (carries the AgentRequest name) so
  reconcile loops can requeue cleanly; optional `WaitForApproval(ctx, name)` helper
  subscribes to the SSE stream.
- **SDK is sugar, never plumbing**: nothing requires the library; an unwrapped
  client simply sees standard 403s on expired sessions and retries.
- **One client core, multiple skins**: `aipctl` and `aip-go` share a single Go
  package for login/device-flow, discovery, session-intent submission, and token
  refresh — the CLI and the transport wrapper are thin frontends over it, so the
  protocol cannot drift between them. The future Python port reimplements the
  same documented HTTP protocol (discovery + session intent + mint), not a
  second design.
- Python port follows once the Go shape settles.

### Multi-issuer trust

`--oidc-issuer-url` becomes repeatable. One trust list covers all runtimes:

| Runtime | Issuer | Credential delivery |
|---|---|---|
| In-cluster pods | cluster SA issuer (projected tokens, `aud: aip-gateway`) | mounted by kubelet |
| Developer laptops | enterprise IdP (Keycloak/Entra/Okta) | `aipctl login` device flow |
| CI pipelines | GitHub/GitLab OIDC issuer | ambient job token, zero stored secrets |
| SaaS agents | vendor or federated IdP | MCP endpoint OAuth |

The CI row is configuration plus a docs page — no code. It removes long-lived
kubeconfigs from repository secrets entirely.

### Design principles (apply to every feature in this EP)

1. **Who decided?** The affected party consents or can opt out. Mode changes are
   requested by the agent developer and countersigned by the admin — never imposed.
2. **Can you see it?** Every automated effect is inspectable with plain kubectl.
3. **Can you undo it?** One command back to yesterday (`aipctl registrations
   revert`), with no dependency on AIP being up — break-glass is restoring one
   RoleBinding.
4. **Does it fail safe?** Loss of AIP degrades to the ungoverned world for Standing
   agents and to read-only (not broken) for Governed agents; reads never depend on
   the gateway.

### Deferred Work

- **Governing apiserver proxy** (per-action holds for unmodified kubectl/client-go;
  agent's kubeconfig points at the gateway). Deferred: it is the only component that
  would put the gateway in the read/WATCH data path, and the session-intent model
  covers the same population at per-session granularity with zero data-path cost.
  Build trigger: a customer's legacy agent requires per-action holds and cannot
  adopt MCP.
- **Pod env injection webhook** (mutating webhook rewrites `KUBERNETES_SERVICE_HOST`
  on labeled pods). Only meaningful alongside the proxy; deferred with it. If built:
  pod-level opt-in annotation only (namespace-level fails principle 1).
- **Auto-approval registration policies as a CRD** (`match issuer/groups →
  auto-approve with bindings`). The `--registration-policy=auto` flag covers the
  current need without a new noun.
- **Per-tenant admin scoping** — moved to Phase 13a. Interface defined there;
  implementation deferred until a multi-tenant deployment demands it. Until then,
  global admin is the documented (not implicit) model.

### Acceptance demo (laptop segment)

Pure terminal, no cluster-side agent. This transcript is the acceptance test:

```
$ aipctl login --gateway https://aip.corp
$ aipctl register cleanup-script --services k8s
$ aipctl kubeconfig cleanup-script > ~/.kube/aip.yaml
$ export KUBECONFIG=~/.kube/aip.yaml

$ kubectl scale deploy payment-api --replicas=4      # within policy
deployment.apps/payment-api scaled                   # auto-approved, invisible

$ kubectl scale deploy payment-api --replicas=20     # exceeds policy threshold
⏳ aip: session requires approval — https://aip.corp/requests/sess-4d2a
✓ approved by admin@corp
deployment.apps/payment-api scaled

$ kubectl get auditrecords                           # agent + operator + approval
```

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
