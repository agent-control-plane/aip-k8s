# Design: Agent Identity and External Agent Registration

**Status**: Draft

## 1. Problem Statement

AIP's native authentication model assumes agents run inside a Kubernetes cluster and authenticate via ServiceAccount JWTs. This excludes a large class of real-world agents:

- Python scripts on a developer's laptop integrating with Salesforce
- AWS Lambda functions querying and updating Snowflake
- SaaS-hosted LangChain bots managing Jira workflows
- GitHub Actions workflows orchestrating infrastructure changes

Forcing these agents to understand K8s ServiceAccounts, CSR generation, or kubectl creates a poor developer experience and blocks adoption. AIP needs a first-class identity model for external agents that:

1. Requires no K8s knowledge from the agent developer
2. Keeps K8s as the authoritative source of truth for identity and policy
3. Connects directly to `GovernedResource.spec.permittedAgents` for authorization
4. Supports the community plugin ecosystem — `AgentIdentity` is the registration object for both agent identity and governed resource access

---

## 2. The `AgentIdentity` CRD

`AgentIdentity` is a cluster-scoped CRD that represents a named agent and its authentication configuration. It is the bridge between an external agent's credential and the AIP governance state machine.

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: AgentIdentity
metadata:
  name: salesforce-bot
spec:
  description: "Salesforce opportunity sync agent"

  # Authentication — one or more methods may be configured.
  # The gateway accepts any that matches.
  auth:
    # Option A: API Key (for scripts, local dev, SaaS agents)
    apiKeys:
      - secretRef: salesforce-bot-key-v2   # active key
      - secretRef: salesforce-bot-key-v1   # old key, valid during rotation window

    # Option B: OIDC Federation (for cloud-hosted agents — AWS Lambda, GitHub Actions)
    oidcBindings:
      - issuer: "https://oidc.eks.us-east-1.amazonaws.com/id/CLUSTER_ID"
        subjectPattern: "system:serviceaccount:prod:salesforce-operator"
      - issuer: "https://token.actions.githubusercontent.com"
        subjectPattern: "repo:myorg/salesforce-sync:*"
```

```go
type AgentIdentitySpec struct {
    Description string   `json:"description,omitempty"`
    Auth        AuthSpec `json:"auth"`
}

type AuthSpec struct {
    // APIKeys lists Secrets containing hashed API keys for this agent.
    // Multiple keys are supported for rotation — all active keys are valid simultaneously.
    APIKeys []APIKeyRef `json:"apiKeys,omitempty"`

    // OIDCBindings configures trust for external OIDC issuers (AWS, GitHub Actions, GCP, etc.)
    // The agent presents its native platform token — no AIP-specific secret required.
    OIDCBindings []OIDCBinding `json:"oidcBindings,omitempty"`
}

type APIKeyRef struct {
    SecretRef string `json:"secretRef"` // Name of Secret in the AIP system namespace
}

type OIDCBinding struct {
    Issuer         string `json:"issuer"`
    SubjectPattern string `json:"subjectPattern"` // glob, e.g. "repo:myorg/*"
}
```

---

## 3. Authorization Chain

`AgentIdentity.metadata.name` is the value that appears in `GovernedResource.spec.permittedAgents`. This is the authorization chain:

```
External credential (API key or OIDC token)
       ↓  gateway resolves
AgentIdentity.metadata.name  (e.g. "salesforce-bot")
       ↓  matched against
GovernedResource.spec.permittedAgents: ["salesforce-bot"]
       ↓  governs
AgentRequest.spec.agentIdentity: "salesforce-bot"  (set by gateway at admission)
       ↓  evaluated against
SafetyPolicy
```

Without `GovernedResource.spec.permittedAgents` including the agent's name, the gateway rejects the `AgentRequest` at admission regardless of valid authentication. Authentication proves identity; `GovernedResource` grants authorization.

### Full registration example

```bash
# 1. Register the agent
aip create agent salesforce-bot --description "Salesforce opportunity sync agent"
# → creates AgentIdentity CRD
# → generates API key, stores bcrypt hash in Secret salesforce-bot-key-v1
# → prints key once: aip_live_7f3k9m... (never stored in plaintext)

# 2. Register the governed resource, authorizing this agent
kubectl apply -f - <<EOF
apiVersion: governance.aip.io/v1alpha1
kind: GovernedResource
metadata:
  name: salesforce-opportunities
spec:
  uriPattern: "salesforce://myorg/objects/Opportunity/**"
  permittedActions: ["update", "close-won", "close-lost"]
  permittedAgents: ["salesforce-bot"]
  contextFetcher: salesforce
  description: "Salesforce Opportunity records for deal governance"
EOF

# 3. Agent developer uses the key — no K8s knowledge required
curl -X POST https://aip.internal/agent-requests \
  -H "Authorization: Bearer aip_live_7f3k9m..." \
  -d '{"spec": {"action": "close-won", "target": {"uri": "salesforce://myorg/objects/Opportunity/006Hs00000AbCdE"}, "reason": "deal signed"}}'
```

---

## 4. Authentication Methods

### 4.1 API Key

Used for agents that cannot obtain OIDC tokens — scripts, local dev, SaaS platforms.

**Key format**: `aip_live_{32-byte crypto/rand hex}` — prefixed for easy scanning/revocation.

**Storage**: the gateway stores only the bcrypt hash in a K8s Secret in the AIP system namespace. The plaintext key is returned once at creation and never stored.

```yaml
# Secret created by aip create agent — managed by AIP controller, not user-edited
apiVersion: v1
kind: Secret
metadata:
  name: salesforce-bot-key-v1
  namespace: aip-system
  labels:
    aip.io/agent: salesforce-bot
type: Opaque
data:
  key-hash: <bcrypt hash of the plaintext key>
  created-at: <RFC3339 timestamp>
```

**Rotation**:
1. `aip rotate key salesforce-bot` — creates a new Secret, adds it to `AgentIdentity.spec.auth.apiKeys`
2. Both old and new keys are valid simultaneously
3. Agent developer updates their environment with the new key
4. `aip revoke key salesforce-bot --secret salesforce-bot-key-v1` — removes old Secret from `apiKeys`

**Gateway validation**: on each request, the gateway extracts the Bearer token, bcrypt-compares against all active key hashes for all `AgentIdentity` objects. The match resolves the identity name. This is O(n) over active keys — acceptable at human-scale agent counts.

### 4.2 OIDC Federation

Used for cloud-hosted agents that already carry a platform identity — AWS Lambda, GitHub Actions, GCP Cloud Run. No AIP-specific secrets required.

```
AWS Lambda  →  fetches AWS STS OIDC token  →  sends to AIP gateway
AIP Gateway →  validates against AWS JWKS endpoint
            →  maps role ARN to AgentIdentity "salesforce-bot"
            →  resolves agentIdentity for the AgentRequest
```

**Configuration in `AgentIdentity`**:

```yaml
spec:
  auth:
    oidcBindings:
      - issuer: "https://oidc.eks.us-east-1.amazonaws.com/id/CLUSTER_ID"
        subjectPattern: "system:serviceaccount:prod:salesforce-operator"
```

The gateway fetches the issuer's JWKS endpoint (cached with key rotation), validates the token, and checks that the `sub` claim matches `subjectPattern`. On match, the `AgentIdentity` name is resolved.

This is already specified in `spec.md §6: Composite Identity`. `AgentIdentity` CRD is the K8s object that backs that spec.

---

## 5. Gateway Resolution

The gateway resolves an incoming request's credential to an `AgentIdentity` name before any admission logic runs:

```go
// cmd/gateway/identity.go
func (g *Gateway) resolveIdentity(r *http.Request) (string, error) {
    bearer := extractBearerToken(r)
    if bearer == "" {
        return "", ErrUnauthenticated
    }

    // Try API key first (prefix check avoids bcrypt on non-key tokens)
    if strings.HasPrefix(bearer, "aip_live_") {
        name, err := g.resolveAPIKey(r.Context(), bearer)
        if err == nil {
            return name, nil
        }
    }

    // Try OIDC federation (for platform tokens from AWS, GitHub, GCP, etc.)
    name, err := g.resolveOIDCToken(r.Context(), bearer)
    if err == nil {
        return name, nil
    }

    // Try K8s ServiceAccount JWT (for in-cluster agents — existing path)
    name, err = g.resolveServiceAccountToken(r.Context(), bearer)
    if err == nil {
        return name, nil
    }

    return "", ErrUnauthenticated
}
```

The resolved name is set as `spec.agentIdentity` on the `AgentRequest` at admission. The agent cannot set this field directly — it is always overwritten by the gateway.

---

## 6. Enforcement at Admission

The gateway enforces the `permittedAgents` check before creating the `AgentRequest`:

```go
func (g *Gateway) admitRequest(ctx context.Context, spec AgentRequestSpec, agentIdentity string) error {
    gr, err := g.matchGovernedResource(ctx, spec.Target.URI)
    if err != nil {
        return err
    }
    if len(gr.Spec.PermittedAgents) > 0 && !contains(gr.Spec.PermittedAgents, agentIdentity) {
        return fmt.Errorf("agent %q is not permitted to act on %s: %w",
            agentIdentity, spec.Target.URI, ErrForbidden)
    }
    return nil
}
```

An `AgentIdentity` with no `GovernedResource` listing it cannot create any `AgentRequest`. Authentication without authorization grants nothing.

---

## 7. What This Does Not Solve (Phase 2)

1. **Dynamic GovernedResource creation from `AgentIdentity`**: When a platform engineer registers a new `AgentIdentity` for Snowflake, the system could auto-suggest or auto-create a `GovernedResource` based on the agent's declared scope. Deferred — the two-step manual process (create identity, create governed resource) is clear and auditable.

2. **SCIM / bulk agent provisioning**: Enterprise customers may want to sync agent identities from an IdP (Okta, Azure AD). `AgentIdentity` CRD schema is compatible with SCIM mapping but the sync controller is out of scope for Phase 1.

3. **Agent identity federation across clusters**: A `salesforce-bot` identity registered in cluster A should be recognized in cluster B. Requires cross-cluster identity sync or a federated control plane. Deferred.

---

## 8. Implementation

- Add `AgentIdentity` CRD to `api/v1alpha1/`
- Add `aip create agent` / `aip rotate key` / `aip revoke key` commands to CLI
- Update gateway `resolveIdentity` to check API keys and OIDC bindings against `AgentIdentity` objects
- Update gateway admission to enforce `GovernedResource.spec.permittedAgents` against resolved identity
- Update `AgentRequestSpec` to mark `agentIdentity` as gateway-set (immutable after creation, like `governedResourceRef`)
- Run `make manifests && make generate`
