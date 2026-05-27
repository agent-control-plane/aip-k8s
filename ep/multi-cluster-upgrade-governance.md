# Design: Multi-Cluster Upgrade Governance

Status: Draft

---

## Problem

Customers operating AIP as a governance control plane for AI agents want to extend
that governance to **Kubernetes cluster upgrades** — a high-risk, high-blast-radius
operation that today has no AIP coverage. Three gaps compound:

**Gap 1 — No cluster inventory.**
AIP has no concept of a managed downstream cluster. Agents may already be submitting
`AgentRequest`s that reference cluster upgrade URIs, but AIP has no cluster object to
validate against, no version metadata to expose to policy rules, and no upgrade history
to surface to human reviewers.

**Gap 2 — No upgrade-specific governance.**
Cluster upgrades have semantics that general-purpose policies cannot capture without
custom CEL: semver constraints (no skip-minor, no downgrade), maintenance windows,
canary wave ordering, provider-specific pre-flight checks. Operators currently write
this logic in CI pipelines, outside AIP's policy surface — meaning it is invisible to
the governance audit trail.

**Gap 3 — Agent identity is lost at upgrade execution time.**
When an approved upgrade executes, it runs under the gateway's service account
(`system:serviceaccount:aip-system:aip-gateway`) rather than the agent's own identity.
The cluster's audit log shows the gateway, not the acting agent. This breaks the
attribution chain that AIP is designed to preserve.

---

## Goals

1. Introduce `ManagedCluster` as the operator-declared inventory object for downstream
   K8s clusters: endpoint, auth, OIDC config, maintenance windows, provider metadata.
2. Extend the existing `GovernedResource` + `SafetyPolicy` CEL engine to cover cluster
   upgrades via a new `cluster-upgrade` context fetcher that exposes version, semver
   deltas, maintenance window status, and cluster labels to policy rules.
3. Preserve agent identity in every cluster API call made on behalf of an approved
   upgrade: the gateway uses the agent's OIDC token (passthrough or RFC 8693 exchange)
   rather than its service account. No K8s impersonation privileges required.
4. Support multi-cluster scenarios: a single agent registration binds per-cluster
   credentials, and a single GovernedResource policy governs all clusters or a selected
   subset by label.
5. Reuse maximum existing machinery: `AgentRequest` lifecycle, `SafetyPolicy` CEL,
   `AgentRegistration` `KubernetesOIDC` credential type. No new approval workflow.

## Non-Goals

- AIP as an upgrade executor. AIP governs whether the upgrade is permitted; the agent
  (or its CI pipeline) performs the actual upgrade API call using its approved identity.
  AIP does not call `eks.UpdateClusterVersion`, `container.UpdateMaster`, etc.
- Fleet-wide rollout orchestration (canary waves, progressive cluster sequencing). Phase
  ordering and wave dependencies are declared in the agent's `AgentRequest` parameters
  and cascade model — AIP enforces policy at each wave boundary, not the wave schedule.
- Node pool upgrades. Governed separately via existing `karpenter` context fetcher. This
  EP covers only the Kubernetes control-plane / API-server version upgrade.
- Cluster creation or deletion. Out of scope; governed by `GovernedResource` URI patterns
  if needed (e.g., `k8s-cluster://*/create`), but not addressed here.
- Provider-specific pre-flight validation (EKS deprecation checks, GKE compatibility
  matrix). These are a natural Phase 2 extension via per-provider context fetchers.

---

## Object Ownership

```
ManagedCluster     operator-owned   cluster inventory, OIDC config, maintenance windows
SafetyPolicy       operator-owned   upgrade allow/deny/require-approval rules
GovernedResource   operator-owned   URI pattern admission, trust floor, context fetcher
AgentRequest       agent-created    upgrade intent, target version, cascade model
AgentRegistration  operator-owned   per-cluster credential bindings (KubernetesOIDC)
AgentTrustProfile  controller-owned trust measurements (unchanged)
```

`ManagedCluster` status is controller-managed (version polling, upgrade history).
`ManagedCluster` spec is operator-managed. They do not share ownership.

---

## URI Scheme

Cluster upgrade requests use a new URI scheme:

```
k8s-cluster://{managedClusterName}/upgrade
```

Examples:
```
k8s-cluster://prod-west/upgrade
k8s-cluster://staging-eu/upgrade
```

`GovernedResource.spec.uriPattern` glob patterns:
```
k8s-cluster://*/upgrade          # all clusters
k8s-cluster://prod-*/upgrade     # production clusters only (by naming convention)
```

The cluster name in the URI is resolved to a `ManagedCluster` object in the gateway's
namespace. If no `ManagedCluster` exists with that name, the upgrade request is rejected
at admission (`ACTION_NOT_PERMITTED`).

---

## Proposed Design

### 1. `ManagedCluster` CRD

`ManagedCluster` is the operator-declared registration for a downstream K8s cluster.
It is namespaced (matching `MCPServer` scope convention) and lives in the AIP control
plane namespace alongside other governance objects.

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: ManagedCluster
metadata:
  name: prod-west
  namespace: aip-system
spec:
  # Endpoint is the K8s API server URL. Used by the context fetcher and
  # by the agent for upgrade execution.
  endpoint: https://k8s-prod-west.company.com

  # authSecretRef references a Secret containing cluster admin credentials
  # for the AIP control plane's own operations (version polling, status updates).
  # Secret must contain either "kubeconfig" key (preferred) or
  # "token" + "ca.crt" keys (service account token + CA bundle).
  authSecretRef:
    name: prod-west-admin-kubeconfig

  # oidcConfig describes this cluster's OIDC configuration, enabling agent
  # identity passthrough. Must match the cluster's --oidc-issuer-url and
  # --oidc-client-id flags.
  oidcConfig:
    issuerURL: https://keycloak.company.com/realms/aip
    clientID: kubernetes

  # provider identifies the cluster provider for future provider-specific
  # pre-flight checks. Does not change current governance behaviour.
  provider: GKE   # Generic | EKS | GKE | AKS | KubeadmControlPlane

  # maintenanceWindows defines recurring windows during which upgrades are
  # permitted. The context fetcher exposes context.inMaintenanceWindow to
  # SafetyPolicy CEL rules.
  maintenanceWindows:
    - name: weeknight-maintenance
      cron: "0 22 * * 1-5"    # 10pm weeknights
      duration: 4h
      timezone: America/Los_Angeles
    - name: weekend-window
      cron: "0 8 * * 6"       # Saturday 8am
      duration: 8h
      timezone: America/Los_Angeles

  # clusterLabels is operator-declared metadata exposed to SafetyPolicy CEL as
  # context.cluster.labels. Separate from metadata.labels (which K8s uses for
  # object selection). Use for policy-significant attributes only.
  clusterLabels:
    environment: production
    region: us-west-2
    tier: critical
    team: platform
```

#### Go types

```go
// ManagedClusterSpec defines the desired state of a ManagedCluster.
type ManagedClusterSpec struct {
    // Endpoint is the K8s API server URL.
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:Pattern=`^https?://`
    Endpoint string `json:"endpoint"`

    // AuthSecretRef references a Secret containing cluster credentials for
    // the AIP control plane's management operations (version polling).
    // The Secret must contain either "kubeconfig" or ("token" + "ca.crt").
    AuthSecretRef corev1.LocalObjectReference `json:"authSecretRef"`

    // OIDCConfig describes this cluster's OIDC trust configuration.
    // Required for agent identity passthrough via KubernetesOIDC credential type.
    // +optional
    OIDCConfig *ManagedClusterOIDCConfig `json:"oidcConfig,omitempty"`

    // Provider identifies the cluster provider.
    // +kubebuilder:validation:Enum=Generic;EKS;GKE;AKS;KubeadmControlPlane
    // +kubebuilder:default=Generic
    // +optional
    Provider string `json:"provider,omitempty"`

    // MaintenanceWindows defines recurring windows when upgrades are permitted.
    // +optional
    MaintenanceWindows []MaintenanceWindow `json:"maintenanceWindows,omitempty"`

    // ClusterLabels is operator-declared metadata exposed to SafetyPolicy CEL
    // as context.cluster.labels. Not used for K8s object selection.
    // +optional
    ClusterLabels map[string]string `json:"clusterLabels,omitempty"`
}

// ManagedClusterOIDCConfig describes a cluster's OIDC trust anchor.
type ManagedClusterOIDCConfig struct {
    // IssuerURL is the OIDC provider URL trusted by this cluster.
    // +kubebuilder:validation:MinLength=1
    IssuerURL string `json:"issuerURL"`
    // ClientID is the expected aud claim. Defaults to "kubernetes".
    // +optional
    ClientID string `json:"clientID,omitempty"`
}

// MaintenanceWindow defines a recurring upgrade window.
type MaintenanceWindow struct {
    // Name is a human-readable label.
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name"`
    // Cron is a 5-field cron expression (minute hour dom month dow).
    // Example: "0 22 * * 1-5" = 10pm on weekdays.
    // +kubebuilder:validation:MinLength=9
    Cron string `json:"cron"`
    // Duration is the window length. Format: number followed by "m" or "h".
    // +kubebuilder:validation:Pattern=`^\d+[mh]$`
    Duration string `json:"duration"`
    // Timezone for the cron expression. IANA timezone name. Defaults to UTC.
    // +optional
    Timezone string `json:"timezone,omitempty"`
}

// ManagedClusterPhase describes the cluster's current state.
// +kubebuilder:validation:Enum=Reachable;Unreachable;Upgrading
type ManagedClusterPhase string

const (
    ManagedClusterReachable   ManagedClusterPhase = "Reachable"
    ManagedClusterUnreachable ManagedClusterPhase = "Unreachable"
    ManagedClusterUpgrading   ManagedClusterPhase = "Upgrading"
)

// UpgradeHistoryEntry records a single completed upgrade.
type UpgradeHistoryEntry struct {
    FromVersion string       `json:"fromVersion"`
    ToVersion   string       `json:"toVersion"`
    StartedAt   metav1.Time  `json:"startedAt"`
    CompletedAt *metav1.Time `json:"completedAt,omitempty"`
    // AgentIdentity is the agent that requested the upgrade.
    AgentIdentity string `json:"agentIdentity"`
    // AgentRequestRef links to the governing AgentRequest.
    AgentRequestRef string `json:"agentRequestRef"`
    Outcome         string `json:"outcome"` // "completed" | "failed" | "rolled-back"
}

// ManagedClusterStatus defines the observed state of a ManagedCluster.
type ManagedClusterStatus struct {
    // Phase is the current cluster state.
    Phase ManagedClusterPhase `json:"phase,omitempty"`
    // CurrentVersion is the Kubernetes server version last observed.
    CurrentVersion string `json:"currentVersion,omitempty"`
    // LastVersionProbe is when AIP last successfully read the server version.
    // +optional
    LastVersionProbe *metav1.Time `json:"lastVersionProbe,omitempty"`
    // UpgradeHistory records the last N completed upgrades (N=10).
    // +optional
    UpgradeHistory []UpgradeHistoryEntry `json:"upgradeHistory,omitempty"`
    // Conditions reflect the cluster's reachability and sync state.
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    // ObservedGeneration is the generation last reconciled by the controller.
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
```

### 2. `cluster-upgrade` context fetcher

A new built-in context fetcher, referenced in `GovernedResource.spec.contextFetcher`.
When an `AgentRequest` targets a `k8s-cluster://{name}/upgrade` URI, the gateway calls
this fetcher before policy evaluation and stores the result in
`AgentRequest.status.providerContext`.

**Fetcher inputs:**
- `ManagedCluster` object (from gateway watch cache)
- `AgentRequest.spec.parameters.targetVersion` — the version the agent wants to upgrade to
- Current wall clock time (for maintenance window calculation)

**Context schema (exposed to SafetyPolicy CEL as `context.*`):**

```json
{
  "cluster": {
    "name": "prod-west",
    "endpoint": "https://k8s-prod-west.company.com",
    "provider": "GKE",
    "labels": {
      "environment": "production",
      "region": "us-west-2",
      "tier": "critical"
    }
  },
  "currentVersion": "1.29.3",
  "targetVersion": "1.30.0",
  "semverDelta": {
    "majorDelta": 0,
    "minorDelta": 1,
    "patchDelta": -3
  },
  "inMaintenanceWindow": false,
  "nextMaintenanceWindow": "2026-05-27T22:00:00-07:00",
  "upgradeHistorySummary": {
    "lastUpgrade": "2026-03-01T22:30:00Z",
    "lastUpgradeFrom": "1.28.5",
    "lastUpgradeTo": "1.29.3",
    "upgradeCount30d": 1
  }
}
```

**Semver helper functions in CEL:**

The CEL environment is extended with three pure functions:

```
semverMajor(version string) int    // major component of a semver string
semverMinor(version string) int    // minor component
semverPatch(version string) int    // patch component
```

These are registered in the CEL environment alongside existing AIP helpers. They handle
the `v` prefix (`v1.30.0` and `1.30.0` both parse correctly). Invalid inputs return -1.

**GovernedResource definition for cluster upgrades:**

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: GovernedResource
metadata:
  name: cluster-upgrades
spec:
  uriPattern: "k8s-cluster://*/upgrade"
  permittedActions: ["upgrade"]
  contextFetcher: cluster-upgrade
  contextSchema:
    type: object
    properties:
      cluster:
        type: object
        properties:
          name:      {type: string}
          provider:  {type: string}
          labels:    {type: object}
      currentVersion:      {type: string}
      targetVersion:       {type: string}
      semverDelta:
        type: object
        properties:
          majorDelta: {type: integer}
          minorDelta: {type: integer}
          patchDelta:  {type: integer}
      inMaintenanceWindow: {type: boolean}
      nextMaintenanceWindow: {type: string}
  trustRequirements:
    minTrustLevel: Supervised
    maxAutonomyLevel: Supervised
  description: >
    Controls Kubernetes control-plane version upgrades on AIP-managed clusters.
    All upgrades require at least human approval (maxAutonomyLevel: Supervised).
```

### 3. `SafetyPolicy` for upgrade governance

No changes to the `SafetyPolicy` type. Operators write upgrade rules in the existing
CEL engine using the context variables exposed by the `cluster-upgrade` fetcher.

**Example: production upgrade policy**

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: SafetyPolicy
metadata:
  name: cluster-upgrade-production
spec:
  contextType: cluster-upgrade
  failureMode: FailClosed
  rules:

    # Production clusters always require human approval — no exceptions.
    - name: require-approval-production
      type: StateEvaluation
      action: RequireApproval
      expression: >
        context.cluster.labels["environment"] == "production"
      message: "Production cluster upgrades require human approval."

    # Never skip a minor version (e.g. 1.28 → 1.30 is not allowed).
    - name: deny-skip-minor-version
      type: StateEvaluation
      action: Deny
      expression: >
        context.semverDelta.majorDelta == 0 &&
        context.semverDelta.minorDelta > 1
      message: >
        Cannot skip minor versions. Upgrade one minor version at a time.
        Current: context.currentVersion, Target: context.targetVersion.

    # Deny major version changes (must be explicitly permitted separately).
    - name: deny-major-version-change
      type: StateEvaluation
      action: Deny
      expression: >
        context.semverDelta.majorDelta != 0
      message: "Major version upgrades are not permitted via this policy."

    # Deny downgrades.
    - name: deny-downgrade
      type: StateEvaluation
      action: Deny
      expression: >
        context.semverDelta.minorDelta < 0 ||
        (context.semverDelta.minorDelta == 0 && context.semverDelta.patchDelta < 0)
      message: "Kubernetes version downgrades are not permitted."

    # Production upgrades may only run during maintenance windows.
    - name: deny-outside-maintenance-window
      type: StateEvaluation
      action: Deny
      expression: >
        context.cluster.labels["environment"] == "production" &&
        !context.inMaintenanceWindow
      message: >
        Production upgrades are only permitted in maintenance windows.
        Next window: context.nextMaintenanceWindow.

    # Critical tier clusters require the canary wave annotation before upgrade.
    # The upgrading agent must annotate the request after validating a canary cluster.
    - name: require-canary-completed
      type: StateEvaluation
      action: Deny
      expression: >
        context.cluster.labels["tier"] == "critical" &&
        !(has(request.metadata.annotations) &&
          request.metadata.annotations["upgrade.aip.io/canary-completed"] == "true")
      message: >
        Critical-tier clusters require a completed canary wave. Annotate the
        AgentRequest with upgrade.aip.io/canary-completed=true after canary validation.
```

**Example: staging policy (less restrictive)**

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: SafetyPolicy
metadata:
  name: cluster-upgrade-staging
spec:
  contextType: cluster-upgrade
  rules:
    # Staging: require approval for minor bumps, allow patch-only autonomously.
    - name: require-approval-minor-bump
      type: StateEvaluation
      action: RequireApproval
      expression: >
        context.semverDelta.minorDelta > 0
      message: "Minor version upgrades require human approval even in staging."

    # Still deny skip-minor.
    - name: deny-skip-minor-version
      type: StateEvaluation
      action: Deny
      expression: >
        context.semverDelta.majorDelta == 0 &&
        context.semverDelta.minorDelta > 1
      message: "Cannot skip minor versions."
```

**Policy-to-cluster binding via `GovernedResourceSelector`:**

Both policies above apply to the same `GovernedResource` (`cluster-upgrades`).
Operators select which policy applies to which clusters by labeling the `ManagedCluster`
objects and binding the policy to labeled `GovernedResource` objects. For finer control,
separate `GovernedResource` objects can be created per environment:

```yaml
# Separate GovernedResources for env-specific policy binding:
---
apiVersion: governance.aip.io/v1alpha1
kind: GovernedResource
metadata:
  name: cluster-upgrades-production
  labels:
    environment: production
spec:
  uriPattern: "k8s-cluster://prod-*/upgrade"
  permittedActions: ["upgrade"]
  contextFetcher: cluster-upgrade
  trustRequirements:
    minTrustLevel: Supervised
    maxAutonomyLevel: Supervised
---
apiVersion: governance.aip.io/v1alpha1
kind: SafetyPolicy
metadata:
  name: cluster-upgrade-production
spec:
  governedResourceSelector:
    matchLabels:
      environment: production
  contextType: cluster-upgrade
  rules: [...]
```

### 4. Agent identity flow to managed clusters

Agent identity propagation reuses `AgentRegistration.spec.externalIdentities` with
`type: KubernetesOIDC`. The `service` field maps to `ManagedCluster.metadata.name`,
exactly as it maps to `MCPServer.metadata.name` today.

**Multi-cluster agent registration example:**

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: AgentRegistration
metadata:
  name: upgrade-bot
  namespace: aip-system
spec:
  agentIdentity: upgrade-bot
  oidc:
    issuer: https://keycloak.company.com/realms/aip
    subjectClaim: azp
    allowedSubjects:
      - upgrade-bot
      - upgrade-bot-staging

  externalIdentities:

    # prod-west uses direct OIDC passthrough.
    # Requires prod-west cluster's --oidc-issuer-url == gateway OIDC issuer.
    - service: prod-west          # matches ManagedCluster.metadata.name
      type: KubernetesOIDC
      kubernetesOIDC:
        audience: kubernetes      # matches cluster's --oidc-client-id

    # prod-east uses the same issuer; passthrough also works here.
    - service: prod-east
      type: KubernetesOIDC
      kubernetesOIDC:
        audience: kubernetes

    # staging uses a different OIDC issuer; exchange mode with Dex.
    - service: staging-eu
      type: KubernetesOIDC
      kubernetesOIDC:
        tokenExchangeURL: https://dex.staging.company.com/token
        audience: kubernetes

    # On-prem cluster with the same issuer; passthrough.
    - service: onprem-dc1
      type: KubernetesOIDC
      kubernetesOIDC: {}
```

**Gateway credential resolution (unchanged logic, new lookup path):**

The existing `registrationCache.providerFor(agentIdentity, service)` call already looks
up by service name string. No API changes are needed. The gateway must distinguish
whether `service` refers to a `MCPServer` or a `ManagedCluster` to decide how to use
the resolved token:
- `MCPServer` service: token is used as `Authorization: Bearer <token>` in the MCP
  protocol call.
- `ManagedCluster` service: token is returned to the agent in the approved
  `AgentRequest` response as `status.executionCredential.token`, so the agent can use
  it directly against the cluster API server.

#### `status.executionCredential` on `AgentRequest`

A new optional status field carries the resolved agent credential back to the agent
after approval:

```go
// ExecutionCredential carries the agent's cluster-specific token after approval.
// Set only for AgentRequests targeting k8s-cluster:// URIs.
// The token is scoped to the target cluster per the KubernetesOIDC credential binding.
// Expires at TokenExpiry; agents should not cache beyond that.
type ExecutionCredential struct {
    // Token is the Bearer token for the target cluster.
    // +kubebuilder:validation:MinLength=1
    Token string `json:"token"`
    // TokenExpiry is when Token expires.
    TokenExpiry metav1.Time `json:"tokenExpiry"`
    // ClusterEndpoint is the target cluster API server URL (from ManagedCluster.spec.endpoint).
    ClusterEndpoint string `json:"clusterEndpoint"`
}
```

This design keeps AIP as a governance control plane. The agent receives the credential
and performs the upgrade itself. AIP does not call any provider upgrade API.

**K8s RBAC on the target cluster:**

For passthrough mode, the agent's OIDC subject (e.g., `upgrade-bot`) must have a
`ClusterRoleBinding` on the target cluster granting upgrade permissions. For EKS:
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aip-upgrade-bot
subjects:
  - kind: User
    name: upgrade-bot    # matches OIDC token sub claim
    apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: system:masters   # or a custom role with minimal upgrade permissions
  apiGroup: rbac.authorization.k8s.io
```

The gateway service account requires no additional permissions on the target cluster.
Only the agent's own OIDC identity is used for upgrade API calls. This matches the
existing K8s MCP server pattern from the `AgentRegistration` EP.

### 5. Full request flow

```
Agent                     AIP Gateway               Controller
  |                            |                         |
  | POST /agent-requests       |                         |
  | {agentIdentity: upgrade-bot|                         |
  |  action: upgrade           |                         |
  |  target: {                 |                         |
  |    uri: k8s-cluster://     |                         |
  |         prod-west/upgrade} |                         |
  |  parameters: {             |                         |
  |    targetVersion: "1.30.0"}|                         |
  |  reason: "CVE mitigation"} |                         |
  |--------------------------->|                         |
  |                            |                         |
  |            Admission checks (GovernedResource URI match,
  |            permittedAgents, trust floor)              |
  |                            |                         |
  |            cluster-upgrade context fetcher:           |
  |            reads ManagedCluster "prod-west",          |
  |            reads current server version via           |
  |            authSecretRef kubeconfig                   |
  |            → context.currentVersion = "1.29.3"       |
  |            → context.inMaintenanceWindow = false      |
  |                            |                         |
  |            SafetyPolicy evaluation (CEL):            |
  |            require-approval-production → RequireApproval
  |            deny-outside-maintenance-window → Deny    |
  |                            |                         |
  |            Result: Deny (maintenance window check)   |
  |<---------------------------|                         |
  | 403 POLICY_VIOLATION       |                         |
  | "Production upgrades only  |                         |
  |  in maintenance windows."  |                         |
  |                            |                         |
  ~~ (agent retries during maintenance window) ~~        |
  |                            |                         |
  | POST /agent-requests       |                         |
  | {same, but now 22:05 Mon}  |                         |
  |--------------------------->|                         |
  |                            |                         |
  |            context.inMaintenanceWindow = true        |
  |            SafetyPolicy:                             |
  |            require-approval-production → RequireApproval
  |                            |                         |
  |            AgentRequest created, Phase=Pending       |
  |                            |                         |
  |<---------------------------|                         |
  | 202 Accepted               |                         |
  | {phase: Pending}           |                         |
  |                            |                         |
  ~~ Human reviewer approves via dashboard ~~            |
  |                            |                         |
  |                            | PATCH AgentRequest      |
  |                            | humanApproval.decision: |
  |                            |   approved              |
  |                            |------------------------>|
  |                            |                         |
  |                            |     Resolves upgrade-bot's KubernetesOIDC
  |                            |     credential for "prod-west"            |
  |                            |     (passthrough: returns agent's JWT)    |
  |                            |                         |
  |                            |     Sets status.executionCredential       |
  |                            |     token = <agent JWT>  |
  |                            |     clusterEndpoint = https://k8s-prod-west
  |                            |     tokenExpiry = now+1h |
  |                            |                         |
  | GET /agent-requests/{id}   |                         |
  |--------------------------->|                         |
  |<---------------------------|                         |
  | {phase: Approved,          |                         |
  |  executionCredential: {    |                         |
  |    token: <JWT>,           |                         |
  |    clusterEndpoint: ...    |                         |
  |    tokenExpiry: ...}}      |                         |
  |                            |                         |
Agent calls cluster API directly using executionCredential.token.
K8s audit log records: user: upgrade-bot, verb: patch, resource: ...
  |                            |                         |
  | POST /agent-requests/{id}/complete                   |
  |   {outcome: succeeded}     |                         |
  |--------------------------->|                         |
  |                            |                         |
  |                            |     Controller updates ManagedCluster
  |                            |     status.upgradeHistory                 |
  |                            |     status.currentVersion = "1.30.0"     |
```

### 6. `ManagedCluster` controller

The `ManagedClusterReconciler` performs:

1. **Version polling**: Every `N` minutes (configurable, default 10m), reads the cluster's
   server version via the `authSecretRef` kubeconfig. Updates
   `status.currentVersion` and `status.phase` (Reachable/Unreachable).
2. **Upgrade history tracking**: Watches for `AgentRequest` objects targeting
   `k8s-cluster://{name}/upgrade` that transition to Completed or Failed phase. Appends
   `UpgradeHistoryEntry` to `status.upgradeHistory` (capped at 10 entries).
3. **Upgrading phase detection**: Sets `status.phase = Upgrading` when a governing
   `AgentRequest` is in `Executing` phase.

The controller uses a `ManagedCluster`-namespaced `APIReader` (not the informer cache)
for the authSecretRef resolution, consistent with the project convention of using
`r.APIReader.Get()` before status transitions.

The controller does **not** call any cloud provider upgrade API. It only reads cluster
version and tracks AIP's own request history.

---

## `AgentRequest` Lifecycle Changes

### New `complete` endpoint

After upgrade execution, the agent reports the outcome:

```
POST /agent-requests/{id}/complete
Body: { "outcome": "succeeded" | "failed", "note": "optional" }
```

The gateway transitions the `AgentRequest` from `Approved` → `Completed` (or `Failed`).
The controller's upgrade-history watch fires and updates `ManagedCluster.status`.

This endpoint is authenticated with `roleAgent` and validates that the agent identity
on the request matches the caller.

---

## Gateway Changes

### `ManagedCluster` watch cache

Mirrors the existing `mcpServerCache` pattern (`cmd/gateway/mcp_watch.go`). The gateway
maintains an in-process watch on `ManagedCluster` objects.

```go
// cmd/gateway/cluster_watch.go

type clusterCache struct {
    mu       sync.RWMutex
    byName   map[string]*v1alpha1.ManagedCluster
}

func (c *clusterCache) get(name string) *v1alpha1.ManagedCluster

func (c *clusterCache) upsert(mc *v1alpha1.ManagedCluster)

func (c *clusterCache) remove(name string)
```

### `cluster-upgrade` context fetcher

Registered alongside existing fetchers (`karpenter`, `github`, `k8s-deployment`) in the
fetcher registry:

```go
// cmd/gateway/context_fetcher.go (new file)

// ClusterUpgradeFetcher fetches current cluster version and computes maintenance
// window status for the cluster-upgrade context type.
type ClusterUpgradeFetcher struct {
    clusterCache *clusterCache
    clock        func() time.Time
}

func (f *ClusterUpgradeFetcher) Fetch(
    ctx context.Context,
    req *v1alpha1.AgentRequest,
    gr *v1alpha1.GovernedResource,
) (*apiextensionsv1.JSON, error)
```

The fetcher:
1. Extracts cluster name from the URI (`k8s-cluster://prod-west/upgrade` → `prod-west`).
2. Reads `ManagedCluster` from cache.
3. Reads `parameters.targetVersion` from the AgentRequest.
4. Parses semver deltas (rejects non-semver targets with 400).
5. Evaluates maintenance windows against the current clock.
6. Returns the context JSON.

The fetcher does **not** re-read the live cluster version on every request — it uses
`status.currentVersion` from the cached `ManagedCluster` object (written by the
controller's polling loop). This avoids adding cluster API round-trips to every upgrade
request's admission path.

### `registrationCache` lookup extension

`registrationCache.providerFor(agentIdentity, service)` currently maps `service` to
`MCPServer.metadata.name`. For cluster upgrades, `service` maps to
`ManagedCluster.metadata.name`. No change to the lookup logic is needed — the map key
is a string regardless of whether it refers to an MCPServer or ManagedCluster. The
resolved `KubernetesOIDCProvider` token is returned to the `AgentRequest` response
instead of being used internally by the gateway for a proxy call.

---

## Implementation Phases

### Phase 1 — `ManagedCluster` CRD and controller

Files:
- `api/v1alpha1/managedcluster_types.go`
- `api/v1alpha1/zz_generated.deepcopy.go` — `make generate`
- `config/crd/bases/` — `make manifests`
- `internal/controller/managedcluster_controller.go` — version polling, upgrade history
- `internal/controller/managedcluster_controller_test.go`
- `charts/aip-k8s/` — RBAC for ManagedCluster get/list/watch; Secret read for
  authSecretRef resolution

### Phase 2 — Gateway cluster watch + context fetcher

Files:
- `cmd/gateway/cluster_watch.go` — `clusterCache`, watch loop
- `cmd/gateway/context_fetcher.go` — `ClusterUpgradeFetcher`, semver helper functions,
  maintenance window evaluator
- `cmd/gateway/main.go` — wire cluster cache and fetcher registration
- `cmd/gateway/agent_request_handlers.go` — `k8s-cluster://` URI validation at
  admission, `executionCredential` population on approval
- `cmd/gateway/integration_test.go` — cluster upgrade admission tests

### Phase 3 — Credential resolution and `complete` endpoint

Files:
- `cmd/gateway/registration_watch.go` — `providerFor` path for ManagedCluster service
  names (returns token rather than using it internally)
- `cmd/gateway/agent_request_handlers.go` — `POST /agent-requests/{id}/complete`
  endpoint
- `internal/controller/managedcluster_controller.go` — watch AgentRequests for upgrade
  history tracking
- `cmd/gateway/integration_test.go` — token passthrough, exchange mode, complete endpoint

### Phase 4 — E2E tests

Files:
- `test/e2e/managed_cluster_upgrade_test.go` — full upgrade governance suite:
  - `ManagedCluster` registration + version polling
  - Upgrade denied outside maintenance window
  - Upgrade allowed inside maintenance window, requires human approval
  - Upgrade approved: `executionCredential` present in response
  - `complete` endpoint transitions phase to Completed
  - Upgrade history in `ManagedCluster.status`
- `.github/workflows/chart-e2e.yml` — ensure `ManagedCluster` RBAC and feature flags
  are set for Helm mode

---

## Alternatives Considered

### Dedicated `ClusterUpgradePolicy` CRD instead of extending `SafetyPolicy`

Would provide richer sugar for maintenance windows, semver constraints, and canary waves.
Rejected because the existing `SafetyPolicy` CEL engine, augmented with `semverMajor` /
`semverMinor` / `semverPatch` helpers and the `cluster-upgrade` context type, covers all
required semantics without a new admission surface. A new CRD would duplicate the rule
evaluation engine and add a second policy configuration layer for operators to learn.

### AIP as upgrade executor (calling cloud provider APIs)

AIP would call `eks.UpdateClusterVersion`, `container.UpdateMaster`, etc. after
approval. Rejected for two reasons: (1) it requires cloud credentials in the AIP control
plane, expanding its blast radius significantly; (2) it makes AIP a K8s upgrade
operator, not a governance control plane — conflating two responsibilities. The agent
retains upgrade execution; AIP governs the decision and preserves the audit trail.

### Cluster-scoped `ManagedCluster` (like `GovernedResource`)

`GovernedResource` is cluster-scoped because it governs URI patterns that span
namespaces. `ManagedCluster` is namespaced because it contains auth credentials and is
operator-owned per team or environment. Namespaced scope matches `MCPServer` and
`AgentRegistration`, making RBAC delegation to team-level operators consistent with
existing patterns.

### Extend `MCPServer` to cover clusters

`MCPServer` models protocol-level MCP endpoints. A K8s cluster is not an MCP server;
it is a governance target. Mixing the two would require provider-type discrimination
in the MCP handler, complicating the watch cache and tool-discovery logic with
cluster-specific paths. A separate `ManagedCluster` type keeps each concern clean.

---

## Open Questions

1. **`executionCredential` TTL and refresh**: The token returned is the agent's raw OIDC
   JWT (passthrough mode), which expires when the JWT expires (typically 5–60 minutes).
   For long-running upgrades, the agent may need to re-fetch. Should AIP provide a
   `/agent-requests/{id}/refresh-credential` endpoint? **Deferred to Phase 3b.**

2. **Semver parsing for EKS/GKE platform versions**: EKS version strings include a
   platform suffix (`1.29.3-eks-036c24b`). The `semver*` CEL helpers must strip these
   before parsing. **Resolve before Phase 2 ships; add test cases in Phase 2 unit tests.**

3. **Multi-cluster `GovernedResource` per-cluster URI vs. shared pattern**: Using
   `k8s-cluster://*/upgrade` as a single `GovernedResource` means all clusters share
   one trust requirement floor. Operators who need per-cluster trust floors must create
   one `GovernedResource` per cluster (e.g., `k8s-cluster://prod-west/upgrade`). Is a
   `clusterSelector` on `GovernedResource` needed? **Tentatively no: named cluster URIs
   cover the need without a new selector field.**

4. **Maintenance window evaluation at request-time vs. policy-evaluation-time**: If a
   human approves an upgrade 5 minutes before the maintenance window closes, and the
   window expires before the agent fetches the credential, the upgrade proceeds outside
   the window. Options: (a) re-evaluate window at credential-fetch time, (b) bind the
   credential TTL to the window expiry, (c) document as operator responsibility. **Lean
   toward (b): `executionCredential.tokenExpiry = min(jwt.exp, windowClose)`. Confirm
   in Phase 3.**

5. **`AgentRequest.status.executionCredential` is sensitive material**: It contains a
   bearer token. The `AgentRequest` object in etcd would hold this token. This is
   consistent with how other Kubernetes Secret-adjacent patterns work (e.g., bootstrap
   tokens), but requires that access to `AgentRequest` status is RBAC-restricted to the
   agent identity that created the request. **Requires `SubjectAccessReview` check at
   `GET /agent-requests/{id}` time. Resolve in Phase 2.**
