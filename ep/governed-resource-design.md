# Design: GovernedResource CRD

**Status**: Draft

## Problem

AIP currently has no concept of *what resources agents are permitted to mutate*. Platform engineering teams have no way to declare "Karpenter NodePools and GitHub PRs are in-scope for agent actions" without writing bespoke SafetyPolicy rules. There is no registry of governed resource types, no per-resource context fetchers, and no enforcement that an agent is even targeting a resource type the platform team has approved for agent mutation.

This gap means:

- An agent can submit an `AgentRequest` targeting *any* URI — there is no admission-time check that the target resource type is sanctioned.
- Reviewers have no live context when evaluating requests (current nodepool utilization, pending pods, PR diff) — they are approving based on agent-declared intent alone.
- There is no canonical mapping from resource type → which agents are permitted to mutate it.

## Design Goals

1. **Resource registry**: Platform engineering declares which resource types agents may mutate. Requests targeting unsanctioned resource types are rejected at admission time.
2. **Agent-to-resource binding**: A `GovernedResource` scopes which agent identities may target it, eliminating the need for a full RBAC system by leveraging naming conventions.
3. **Context fetchers**: When an `AgentRequest` is submitted, the control plane independently fetches live context for the target resource and surfaces it to the reviewer.
4. **Clear role separation**: Platform engineering owns `GovernedResource` creation. App teams own their agents and `SafetyPolicy`. Agents submit `AgentRequest` only.

## Non-Goals

- A full RBAC system with custom roles and bindings. The three-role model (admin / reviewer / agent) covers the majority of enterprise use cases.
- Namespace-scoped `GovernedResource`. Most governed resources (Karpenter NodePools, GitHub repos) are inherently cluster-scoped or external. Start cluster-scoped.
- A UI for `GovernedResource` management. The HTTP gateway is the management interface; kubectl is a fallback for break-glass scenarios.
- Partial PATCH updates to `GovernedResource` or `SafetyPolicy`. PUT (full replace) is the correct verb — it is auditable, atomic, and forces the admin to declare complete desired state.

## Roles

AIP defines three roles enforced at the gateway:

| Role | Gateway flag | Responsibilities |
|---|---|---|
| **Admin** | `--admin-subjects` | Create, replace (`PUT`), and delete `GovernedResource` and `SafetyPolicy` via the gateway API. Owned by platform engineering. |
| **Reviewer** | `--reviewer-subjects` | Approve or deny `AgentRequest`. Owned by platform engineering — not the team running the agent. |
| **Agent** | `--agent-subjects` | Submit `AgentRequest` only. Cannot modify policy or approve requests. |

All three roles interact exclusively through the HTTP gateway. Direct `kubectl` access to `GovernedResource` and `SafetyPolicy` is a break-glass path, not the operational one. This ensures a single auth layer (OIDC) and a complete audit trail for all governance operations.

### Why platform engineering owns review

The team proposing a change should not approve it. Platform engineering has cluster-wide context (cost, capacity, blast radius) that application teams lack. This separation is the same model as change management in mature engineering organizations and produces a stronger audit trail: "a platform engineer approved this agent action" is a defensible governance posture; "the agent's own team approved it" is not.

### Why a full RBAC system is not needed

Multi-team isolation is achieved through naming conventions and URI pattern matching in `GovernedResource`, not through a role/binding system. If Team A's nodepool is `nodepool/team-a-workers`, then:

```yaml
spec:
  uriPattern: "k8s://prod/karpenter/nodepool/team-a-*"
  permittedAgents: ["aip-agent-team-a"]
```

An agent submitting a request for `nodepool/team-b-workers` is rejected at admission — no `GovernedResource` matches that pattern for that agent identity. The naming convention *is* the access control. Full RBAC becomes relevant only when you have many teams with overlapping resource namespaces — add it then, not now.

## GovernedResource CRD

### Schema

```go
type GovernedResourceSpec struct {
    // URIPattern is a glob pattern matched against AgentRequest.spec.target.uri.
    // Requests targeting URIs that do not match any GovernedResource are rejected.
    // Examples:
    //   "k8s://prod/karpenter/nodepool/team-a-*"
    //   "github://org/repo-*"
    //   "k8s://*/default/deployment/*"
    // +kubebuilder:validation:MinLength=1
    URIPattern string `json:"uriPattern"`

    // PermittedActions lists the action strings agents may request on this resource.
    // Requests with actions not in this list are rejected.
    // +kubebuilder:validation:MinItems=1
    PermittedActions []string `json:"permittedActions"`

    // PermittedAgents lists agent identity values (matched against --oidc-identity-claim)
    // that may submit AgentRequests targeting this resource.
    // Empty means any authenticated agent may target this resource.
    // +optional
    PermittedAgents []string `json:"permittedAgents,omitempty"`

    // ContextFetcher names the built-in fetcher to invoke when an AgentRequest
    // targets this resource type. The fetcher populates status.providerContext
    // so reviewers see live resource state alongside the agent's declared intent.
    // Supported values: "karpenter", "github", "k8s-deployment", "none"
    // +kubebuilder:validation:Enum=karpenter;github;k8s-deployment;none
    // +kubebuilder:default=none
    ContextFetcher string `json:"contextFetcher"`

    // ContextSchema is an OpenAPI schema (restricted subset) that describes the
    // structure of the JSON object the named ContextFetcher will return.
    // SafetyPolicy CEL rules that reference context fields are type-checked against
    // this schema at creation time — malformed rules are rejected immediately rather
    // than failing at evaluation time.
    //
    // All GovernedResources with the same ContextFetcher value must declare
    // canonically-identical schemas (sorted-key JSON comparison). This invariant is
    // enforced at admission time.
    //
    // May be omitted when ContextFetcher is "none".
    // +optional
    ContextSchema *apiextensionsv1.JSON `json:"contextSchema,omitempty"`

    // Description is a human-readable explanation of this governed resource type,
    // shown to reviewers during the approval decision.
    // +optional
    Description string `json:"description,omitempty"`
}
```

### Example: Karpenter NodePool

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: GovernedResource
metadata:
  name: karpenter-nodepool-team-a
spec:
  uriPattern: "k8s://prod/karpenter/nodepool/team-a-*"
  permittedActions:
    - scale-up
    - scale-down
  permittedAgents:
    - aip-agent-team-a
  contextFetcher: karpenter
  description: "Team A Karpenter NodePools. Scaling requires platform engineering approval."
```

### Example: GitHub PRs

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: GovernedResource
metadata:
  name: github-prs-infra
spec:
  uriPattern: "github://myorg/infra-*"
  permittedActions:
    - open-pr
    - close-pr
  permittedAgents:
    - aip-agent-team-a
  contextFetcher: github
  description: "Infrastructure repos. PRs opened by agents require platform engineering review."
```

## URI Scheme

All URIs submitted in `AgentRequest.spec.target.uri` and matched in `GovernedResource.spec.uriPattern` follow this canonical format:

```
<scheme>://<cluster-or-org>/<group-or-type>/<name-or-path>
```

Examples:
- `k8s://prod/karpenter.sh/nodepool/team-a-workers` — Karpenter NodePool in the `prod` cluster
- `k8s://prod/apps/deployment/default/payment-api` — Kubernetes Deployment, with namespace before name
- `github://myorg/infra-platform` — GitHub repository

Agents and `GovernedResource` authors must agree on which URI format they use. The gateway performs literal glob matching against whatever URI the agent provides — it does not normalise or parse URIs.

## Glob Pattern Semantics

`GovernedResource.spec.uriPattern` uses Go [`path.Match`](https://pkg.go.dev/path#Match) semantics:

- `*` matches any sequence of non-`/` characters within a single path segment.
- `?` matches any single non-`/` character.
- `[...]` character classes are supported.
- `**` is **not** supported. Use multiple patterns or a broader `*` match at the right segment.

This is intentionally restrictive: a pattern like `k8s://prod/karpenter.sh/nodepool/*` matches `k8s://prod/karpenter.sh/nodepool/team-a-workers` but not `k8s://prod/karpenter.sh/nodepool/team-a/extra-segment`. Platform engineers must be explicit about the segment depth they intend to govern.

## Backward Compatibility: Zero GovernedResources

When the `GovernedResource` CRD is installed but no `GovernedResource` objects exist, the admission check would reject all `AgentRequest` submissions — a breaking change for existing deployments.

The gateway flag `--require-governed-resource` (default `false`) controls this:

- `false` (default): if the GovernedResource list is empty, skip the admission check. Existing deployments are unaffected.
- `true`: enforce strictly — no `AgentRequest` is accepted unless a matching `GovernedResource` exists. Opt in when the registry is fully populated.

Once at least one `GovernedResource` exists, the check is always enforced regardless of this flag. The flag only governs the zero-resources case.

## Action Vocabulary

`permittedActions` is free-form. There is no built-in registry of action names. This is intentional — governed resource types are heterogeneous and a shared vocabulary would become a maintenance burden.

Convention: action strings should be lower-kebab-case verb phrases describing the mutation. The agent and the `GovernedResource` author must agree out-of-band on which strings to use.

Recommended vocabulary for built-in fetcher types:

| Fetcher | Recommended actions |
|---|---|
| `karpenter` | `scale-up`, `scale-down`, `update-limits` |
| `github` | `open-pr`, `close-pr`, `merge-pr` |
| `k8s-deployment` | `update`, `rollout-restart`, `scale` |

## Admission Enforcement

When an `AgentRequest` is submitted, the gateway validates in order:

1. **Resource is governed**: At least one `GovernedResource` URI pattern matches `spec.target.uri`. If none match → reject with `ACTION_NOT_PERMITTED`.
   - **Zero-resources bypass**: if `--require-governed-resource=false` (default) and no `GovernedResource` objects exist, this check is skipped entirely.
   - **Multiple matches**: When multiple `GovernedResource` patterns match `spec.target.uri`, the gateway selects the single `GovernedResource` with the longest matching URI pattern (most-specific match). If multiple patterns have the same length, selection is stable-sorted by `metadata.name` (alphabetically) and the first is chosen. This ensures deterministic, reproducible admission decisions.
   - **Permission evaluation**: Once the matching `GovernedResource` is selected, only that resource's `permittedActions` and `permittedAgents` are evaluated. There is no union or aggregation across multiple matches — the single most-specific `GovernedResource` is authoritative.

2. **Agent identity is authoritative**: The caller's identity parsed from the OIDC token (via `--oidc-identity-claim`) is the sole source of truth for `AgentRequest.spec.agentIdentity`. The gateway enforces this invariant:
   - When an `AgentRequest` is created, the gateway overwrites `spec.agentIdentity` with the parsed OIDC identity claim, ignoring any client-supplied value.
   - When validating an existing `AgentRequest`, the gateway verifies that `spec.agentIdentity` exactly matches the caller's OIDC identity. If they differ → reject with `IDENTITY_INVALID`.
   - This prevents identity spoofing: `spec.agentIdentity` cannot drift from the authenticated caller identity.

3. **Agent is permitted**: The caller's identity (from step 2) is in the matching `GovernedResource`'s `permittedAgents` list (or `permittedAgents` is empty, meaning any authenticated agent is allowed). If not → reject with `IDENTITY_INVALID`.

4. **Action is permitted**: `spec.action` is in `permittedActions` for the matching `GovernedResource` (selected in step 1). If not → reject with `ACTION_NOT_PERMITTED`.

These checks happen before `SafetyPolicy` evaluation — they are hard gates, not policy rules. A `SafetyPolicy` can further restrict what is allowed within a `GovernedResource`'s envelope, but cannot expand beyond it.

## Context Fetchers

After admission, the control plane invokes the context fetcher named in the matching `GovernedResource` (selected via the deterministic most-specific pattern match described in Admission Enforcement) and writes the result to `AgentRequestStatus.ProviderContext`. This field is surfaced to reviewers alongside the agent's declared intent, so the reviewer sees both sides: what the agent declared and what AIP independently verified.

### Karpenter fetcher

Reads the target `NodePool` via the K8s client:

```json
{
  "currentLimitCPU": "100",
  "currentLimitMemory": "400Gi",
  "currentNodeCount": 47,
  "pendingPods": 12,
  "estimatedCostDeltaPerHour": "$8.40",
  "recentScalingEvents": [
    {"time": "2026-04-05T02:14:00Z", "direction": "up", "delta": 5}
  ]
}
```

Reviewer sees: *"Agent wants to raise the CPU limit from 100→150. Right now: 47 nodes, 12 pods pending, estimated +$8.40/hr."*

### GitHub fetcher

Reads the PR draft via GitHub API (token from a cluster Secret):

```json
{
  "title": "chore: bump node image to 1.31",
  "filesChanged": 3,
  "linesAdded": 12,
  "linesRemoved": 8,
  "codeowners": ["@platform-team"],
  "ciStatus": "pending"
}
```

### k8s-deployment fetcher

The existing `ControlPlaneVerification` logic — ready replicas, active endpoints, downstream services. Already implemented; this fetcher wraps it under the new model.

## AgentRequestSpec extension

### GovernedResourceRef

The gateway's admission decision must be recorded in the `AgentRequest` spec — not derived at evaluation time. Admission is a **binding decision**: it tells the controller which GovernedResource governs this request and at what version. This is the same pattern as `spec.storageClassName` on a PVC or `spec.ingressClassName` on an Ingress.

```go
// GovernedResourceRef records which GovernedResource admitted this AgentRequest.
// Set by the gateway at admission time. Immutable after creation.
// Empty only when --require-governed-resource=false and no GovernedResources exist.
// +optional
GovernedResourceRef *GovernedResourceRef `json:"governedResourceRef,omitempty"`
```

```go
// GovernedResourceRef identifies the GovernedResource and the generation at
// which it was evaluated at admission time.
type GovernedResourceRef struct {
    // Name is the GovernedResource that matched spec.target.uri at admission.
    Name string `json:"name"`
    // Generation is GovernedResource.metadata.generation at the time of admission.
    // The controller uses this to detect policy changes after admission.
    Generation int64 `json:"generation"`
}
```

The controller reads `spec.governedResourceRef` to establish which `GovernedResource` admitted the request. The controller does **not** re-validate permissions on generation change — that would be incorrect. K8s objects follow eventual consistency: by the time the controller reconciles, the object may have changed again. More importantly, `generation` alone cannot distinguish a tightening change from a loosening one; treating every change as a denial would kill in-flight requests on description-only updates.

The only safe invariant to check is existence:

| Condition | Action |
|---|---|
| GovernedResource **deleted** | Deny with `GOVERNED_RESOURCE_DELETED` |
| GovernedResource **generation changed** | No action — re-admission is not the controller's job |

`GOVERNED_RESOURCE_DELETED` is the safety net: an admin explicitly removing a governed resource while requests are in flight is a decisive policy statement. The finalizer (section below) prevents accidental deletion.

This is the same model as how `kube-scheduler` handles a `Pod` whose `PriorityClass` is deleted after scheduling — the scheduler does not evict the pod; the garbage collection path is explicit and separate.

### New denial codes

```go
DenialCodeGovernedResourceDeleted = "GOVERNED_RESOURCE_DELETED"
```

## AgentRequestStatus extension

`ControlPlaneVerification` is currently hardcoded for K8s Deployments. To support arbitrary resource types, add:

```go
// ProviderContext holds live resource state fetched by the context fetcher
// named in the matching GovernedResource. Schema is fetcher-specific.
// +optional
ProviderContext *apiextensionsv1.JSON `json:"providerContext,omitempty"`
```

`ControlPlaneVerification` is retained for backward compatibility; new fetchers write to `ProviderContext`.

### PolicyResult generation tracking

`PolicyResult` must record the `SafetyPolicy` generation at evaluation time so that a re-evaluation after a policy change is distinguishable from the original:

```go
type PolicyResult struct {
    PolicyName       string `json:"policyName"`
    RuleName         string `json:"ruleName"`
    Result           string `json:"result"`
    PolicyGeneration int64  `json:"policyGeneration"` // SafetyPolicy.metadata.generation
}
```

## SafetyPolicy CRD

`SafetyPolicy` is namespace-scoped. It binds to `GovernedResource` objects via a label selector on the `GovernedResource` metadata, not via a hard name reference. This allows a single `SafetyPolicy` to cover a family of resources (e.g., all `GovernedResource` objects labelled `team: platform`) without requiring a new `SafetyPolicy` every time a new `GovernedResource` is created.

### Schema additions (delta from current)

```go
type SafetyPolicySpec struct {
    // GovernedResourceSelector selects which GovernedResources this policy applies to.
    // An empty selector matches all GovernedResources.
    // +optional
    GovernedResourceSelector metav1.LabelSelector `json:"governedResourceSelector,omitempty"`

    // ContextType binds this SafetyPolicy's CEL rules to a specific context fetcher type.
    // CEL expressions in this policy that reference `context.*` fields are type-checked
    // against the contextSchema of all GovernedResources whose contextFetcher matches
    // this value. Must be a valid contextFetcher value (e.g. "karpenter", "github").
    //
    // Empty means no context-aware rules — CEL expressions may not reference `context`.
    // +optional
    ContextType string `json:"contextType,omitempty"`

    // Rules is the list of CEL-based admission rules evaluated in order.
    Rules []SafetyPolicyRule `json:"rules"`
}
```

`TargetSelector` (the current `matchActions` / `matchResourceTypes` / `matchAttributes` struct) is **replaced** by `GovernedResourceSelector` + `ContextType`. `TargetSelector` relied on agent-declared metadata to select which policy applied — it could be spoofed and was not reliably tied to a physical resource type.

### Example: Karpenter NodePool policy

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: SafetyPolicy
metadata:
  name: karpenter-scale-policy
  namespace: platform
spec:
  governedResourceSelector:
    matchLabels:
      fetcher: karpenter
  contextType: karpenter
  rules:
    - name: cpu-limit-cap
      cel: "context.currentLimitCPU.toInt() + request.spec.cpuDelta <= 200"
      message: "CPU limit cannot exceed 200 cores"
```

### Selection semantics

When an `AgentRequest` is admitted against a `GovernedResource`:
1. The gateway evaluates all `SafetyPolicy` objects in the request's namespace.
2. A policy applies if its `governedResourceSelector` matches the `GovernedResource`'s labels.
3. Within an applicable policy, rules that reference `context.*` are only evaluated when the policy's `contextType` matches `GovernedResource.spec.contextFetcher`.

This binding is declarative and admin-controlled — no agent-supplied field influences which policy fires.

## Context Schema Validation

`GovernedResource.spec.contextSchema` encodes the OpenAPI schema for the JSON object a context fetcher returns. The schema is **data**, not code: it travels with the `GovernedResource` object and requires no recompilation or deployment change to add a new fetcher type.

### Restricted OpenAPI subset

To keep the admission surface small and auditable, `contextSchema` is restricted to:

| Keyword | Allowed |
|---|---|
| `type` | `object`, `string`, `integer`, `number`, `boolean`, `array` |
| `properties` | object field definitions |
| `items` | array element schema |
| `required` | required field names |
| `nullable` | `true` / `false` |
| `description` | human-readable field doc |
| All others | **rejected** at admission |

`additionalProperties`, `oneOf`, `anyOf`, `allOf`, `$ref`, and recursive schemas are explicitly excluded. The goal is a flat, readable schema — not a full JSON Schema metaschema.

### CEL type-checking

CEL expressions in `SafetyPolicy` rules that reference `context.*` fields are type-checked against the `contextSchema` of all `GovernedResources` where `contextFetcher == SafetyPolicy.spec.contextType`.

The gateway uses `k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel` directly — the same CEL library that backs `x-kubernetes-validations` in CRD schemas and `ValidatingAdmissionPolicy`. This is not a reimplementation; it is the standard K8s CEL machinery applied to a GovernedResource-owned schema.

At `SafetyPolicy` create/update time, the gateway:
1. Locates all `GovernedResource` objects with `contextFetcher == SafetyPolicy.spec.contextType`.
2. Builds a CEL type environment from the `contextSchema` of any one of them (all must be identical — see below).
3. Type-checks every CEL expression in the policy rules.
4. Rejects the `SafetyPolicy` if any expression fails type-checking.

Optional fields declared in `contextSchema` without being listed in `required` are typed as nullable in the CEL environment. The K8s CEL library enforces that callers use `has(context.fieldName)` before accessing optional fields, making nullability errors a compile-time rejection rather than a runtime panic.

### Schema consistency across GovernedResources

When a `GovernedResource` is created or updated with a `contextSchema`:
- If no other `GovernedResource` with the same `contextFetcher` value exists, the schema is accepted unconditionally.
- If at least one `GovernedResource` with the same `contextFetcher` exists, the new schema must be **canonically identical** (sorted-key JSON marshalling comparison) to the existing schema.

This invariant ensures that `SafetyPolicy.spec.contextType` binds to exactly one schema regardless of which `GovernedResource` is matched at runtime. It eliminates schema drift as the number of governed resources grows.

When the number of distinct `contextFetcher` types grows beyond ~10, the natural evolution is a standalone `ContextType` CRD (one object per fetcher type, holding the canonical schema). That migration is additive and deferred.

### Runtime schema validation

After the context fetcher runs, the gateway validates the fetched JSON against the `contextSchema` before writing it to `AgentRequestStatus.ProviderContext`:
- If the fetched JSON conforms → write to `ProviderContext` and proceed.
- If the fetched JSON violates the schema (missing required field, wrong type) → write a `FetcherSchemaViolation` condition on the `AgentRequest` status (recording the specific field and type mismatch), set `ProviderContext` to `null`, and **do not block the request**. Reviewers see the condition; the request proceeds without context.

Fail-open on fetcher errors is deliberate: a broken fetcher should not halt an agent that is operating correctly. The violation is surfaced, not hidden.

### Schema evolution

For `v1alpha1`, `contextSchema` is **append-only**: new optional fields may be added; existing fields may not be removed or have their types changed. A field removal is a breaking change for all `SafetyPolicy` CEL rules that reference that field.

Append-only evolution is enforced at admission: a `PUT /governed-resources/{name}` that removes a field or changes a field's type is rejected with a 409 that lists the affected fields.

### Bidirectional consistency validation

Schema validation is bidirectional:
- **GovernedResource schema change → re-validate SafetyPolicy**: When a `GovernedResource` schema changes (append-only, so only additions), all `SafetyPolicy` objects with `contextType == contextFetcher` are re-validated. Because additions are strictly non-breaking, this is informational — existing rules remain valid.
- **SafetyPolicy rule change → re-validate against current schema**: When a `SafetyPolicy` is created or updated, its CEL rules are re-type-checked against the current schema of any matching `GovernedResource`. This is the primary enforcement path.

## GovernedResource Finalizer

Deleting a `GovernedResource` while `AgentRequest` objects are Pending under it leaves those requests in a grey state — admitted under rules that no longer exist. A finalizer prevents this.

```
governance.aip.io/active-requests
```

**Controller behavior:**

1. On each reconcile of an `AgentRequest` in a non-terminal phase that has a `spec.governedResourceRef`, ensure the finalizer is present on the referenced `GovernedResource`.
2. On each reconcile, if the `AgentRequest` reaches a terminal phase (Approved, Denied, Completed, Failed), remove the finalizer if no other non-terminal `AgentRequest` references the same `GovernedResource`.
3. If an admin force-removes the finalizer and the `GovernedResource` is deleted, the controller detects `spec.governedResourceRef` points to a missing object and denies the request with `GOVERNED_RESOURCE_DELETED`.

This is the same pattern cert-manager uses on `Certificate` objects referencing an `Issuer`. It forces the admin to resolve in-flight requests before a policy object can disappear.

## Gateway Admin API

`GovernedResource` and `SafetyPolicy` are managed through the gateway by callers with the admin role (`--admin-subjects` / `--admin-groups`). This provides a single OIDC-authenticated interface for all roles and a complete audit trail for governance configuration changes.

`GovernedResource` is cluster-scoped (no namespace parameter). `SafetyPolicy` is namespace-scoped (`?namespace=`, defaulting to `default`).

### GovernedResource endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/governed-resources` | Create a new GovernedResource |
| `GET` | `/governed-resources` | List all GovernedResources |
| `GET` | `/governed-resources/{name}` | Get a GovernedResource by name |
| `PUT` | `/governed-resources/{name}` | Replace a GovernedResource (full spec replace, increments generation) |
| `DELETE` | `/governed-resources/{name}` | Delete (blocked by finalizer if live requests reference it) |

### SafetyPolicy endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/safety-policies` | Create a new SafetyPolicy |
| `GET` | `/safety-policies` | List SafetyPolicies in namespace |
| `GET` | `/safety-policies/{name}` | Get a SafetyPolicy by name |
| `PUT` | `/safety-policies/{name}` | Replace a SafetyPolicy (full spec replace, increments generation) |
| `DELETE` | `/safety-policies/{name}` | Delete a SafetyPolicy |

### Why PUT not PATCH

PUT replaces the entire spec atomically. The `GovernedResource.metadata.generation` increments on every PUT, giving the controller a reliable signal that the policy changed. PATCH with partial updates risks leaving objects in inconsistent states (e.g., an action permitted by no agent) and produces ambiguous merge semantics for array fields like `permittedActions`.

## Trust Boundaries

```
Platform Engineering (Admin + Reviewer)
  ├── Creates GovernedResource   — what resource types agents may touch
  ├── Creates SafetyPolicy       — what requires approval within that envelope
  └── Approves/denies AgentRequest — runtime decisions

App Team
  ├── Builds and operates their agent
  └── Configures agent identity and SafetyPolicy within GovernedResource bounds

Agent (runtime identity)
  └── Submits AgentRequest only
      Cannot create GovernedResource, SafetyPolicy, or approve requests
```

## GovernedResource Status (Milestone 2)

In milestone 1 the CRD has spec only. Operational visibility is deferred to milestone 2, when context fetchers are added:

```go
type GovernedResourceStatus struct {
    // Conditions surfaces fetcher health and last-match events.
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    // LastMatchedTime is the most recent time an AgentRequest matched this resource.
    // +optional
    LastMatchedTime *metav1.Time `json:"lastMatchedTime,omitempty"`
}
```

## Implementation Sequence

### Milestone 1 — Admission gates ✅ Done

| Step | What | Notes |
|---|---|---|
| 1 | Add `GovernedResource` CRD | `api/v1alpha1/governedresource_types.go`; cluster-scoped, spec only |
| 2 | Add `--admin-subjects` / `--admin-groups` flags to gateway | Admin role in `roleConfig`; `--require-governed-resource` backward compat flag |
| 3 | Admission check in gateway | URI glob match (`path.Match`) → agent identity → action; most-specific-match with alphabetical tiebreak |
| 4 | Add `ProviderContext` to `AgentRequestStatus` | `*apiextensionsv1.JSON`, additive; `ControlPlaneVerification` retained |

### Milestone 2 — Policy binding integrity ✅ Done

| Step | What | Notes |
|---|---|---|
| 5 | Add `spec.governedResourceRef` to `AgentRequestSpec` | Gateway sets name + generation at admission; immutable after creation |
| 6 | Add `policyGeneration` to `PolicyResult` | Controller records SafetyPolicy generation at evaluation time |
| 7 | `GOVERNED_RESOURCE_DELETED` detection in controller | If `spec.governedResourceRef.name` points to a missing object → deny with `GOVERNED_RESOURCE_DELETED` |
| 8 | Finalizer on `GovernedResource` | Controller adds/removes `governance.aip.io/active-requests`; blocks deletion during live requests |

### Milestone 3 — Admin gateway API ✅ Done

| Step | What | Notes |
|---|---|---|
| 9 | `GovernedResource` CRUD + PUT endpoints | POST, GET, PUT, DELETE behind admin role; cluster-scoped |
| 10 | `SafetyPolicy` CRUD + PUT endpoints | POST, GET, PUT, DELETE behind admin role; namespace-scoped |
| 11 | Denial code for `GOVERNED_RESOURCE_DELETED` | Additive constant in `agentrequest_types.go` |
| 12 | Replace `TargetSelector` with `governedResourceSelector` + `contextType` in SafetyPolicy | API change; update CRD schema and gateway handler |

### Milestone 4 — Context fetchers + schema validation ✅ Done

| Step | What | Notes |
|---|---|---|
| 13 | Add `contextSchema` field to `GovernedResourceSpec` | `*apiextensionsv1.JSON`; restricted OpenAPI subset enforced at admission |
| 14 | Schema consistency check at admission | Canonical JSON comparison across all GRs with same `contextFetcher`; identical or reject |
| 15 | CEL type-checking in SafetyPolicy admission | Use `k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel`; type-check rules against contextSchema |
| 16 | Append-only schema evolution enforcement | PUT that removes or changes a field type → 409 with affected field list |
| 17 | Wire `GovernedResource.spec.contextFetcher` into controller dispatch | Controller reads matched GR, dispatches to fetcher by name via registry |
| 18 | `k8s-deployment` fetcher | Wraps existing `ControlPlaneVerification` logic under new model |
| 19 | Karpenter context fetcher | Pure K8s client, no external credentials |
| 20 | GitHub context fetcher | Requires GitHub token Secret in cluster |
| 21 | Runtime schema validation + `FetcherSchemaViolation` condition | Validate fetched JSON against contextSchema; fail-open; record specific field/type mismatch |
| 22 | Helm chart + docs update | |

All milestones are complete.
