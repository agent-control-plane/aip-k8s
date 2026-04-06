# GovernedResource — Operator Guide

`GovernedResource` is a cluster-scoped CRD that declares which infrastructure resources agents are permitted to mutate, which agent identities may target them, and how AIP should independently fetch live context for reviewers.

Without a `GovernedResource`, the gateway either rejects all `AgentRequest` submissions (when `--require-governed-resource=true`) or operates in open mode with no resource-level admission checks (default). Once you create your first `GovernedResource`, admission enforcement activates automatically.

---

## Who creates GovernedResources?

Platform engineering. Not application teams, not agents.

`GovernedResource` is the policy layer. The team that runs agents should not also control what those agents are permitted to target — that is the same separation as change management in mature organizations. Use `--admin-subjects` on the gateway to restrict who may call the admin endpoints.

---

## Quick start

```bash
# 1. Create a GovernedResource (requires admin role)
curl -s -X POST http://localhost:8080/governed-resources \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "karpenter-nodepool-team-a",
    "uriPattern": "k8s://prod/karpenter/nodepool/team-a-*",
    "permittedActions": ["scale-up", "scale-down"],
    "permittedAgents": ["aip-agent-team-a"],
    "contextFetcher": "karpenter",
    "description": "Team A NodePools. Scaling requires platform engineering approval."
  }'

# 2. Agents can now submit requests targeting that pattern
curl -s -X POST http://localhost:8080/agent-requests \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "agentIdentity": "aip-agent-team-a",
    "action": "scale-up",
    "targetURI": "k8s://prod/karpenter/nodepool/team-a-workers",
    "reason": "peak traffic expected at 18:00 UTC"
  }'
```

A request targeting `k8s://prod/karpenter/nodepool/team-b-workers` from `aip-agent-team-a` will be rejected at admission — the pattern does not match.

---

## Fields

| Field | Required | Description |
|---|---|---|
| `name` | Yes | Unique name for this governed resource type. |
| `uriPattern` | Yes | Glob pattern matched against `AgentRequest.spec.target.uri`. See [URI patterns](#uri-patterns). |
| `permittedActions` | Yes | Action strings agents may request. Requests with other actions are rejected. |
| `permittedAgents` | No | Agent identity values permitted to target this resource. Empty means any authenticated agent may target it. |
| `contextFetcher` | Yes | Built-in fetcher invoked after admission to populate live context for reviewers. One of: `karpenter`, `github`, `k8s-deployment`, `none`. |
| `contextSchema` | No | OpenAPI schema (restricted subset) describing the fetcher's output JSON. Used for CEL type-checking in SafetyPolicy rules. Required when CEL rules reference context fields. |
| `description` | No | Human-readable explanation shown to reviewers during approval. |

---

## URI patterns

`uriPattern` uses Go [`path.Match`](https://pkg.go.dev/path#Match) semantics:

- `*` matches any sequence of non-`/` characters within a single path segment.
- `**` is **not** supported.
- Agents and `GovernedResource` authors must agree on the URI format out-of-band. The gateway performs literal glob matching — it does not normalise URIs.

**Common URI formats:**

| Resource type | Example URI | Example pattern |
|---|---|---|
| Karpenter NodePool | `k8s://prod/karpenter.sh/nodepool/team-a-workers` | `k8s://prod/karpenter.sh/nodepool/team-a-*` |
| Kubernetes Deployment | `k8s://prod/apps/deployment/default/payment-api` | `k8s://prod/apps/deployment/default/*` |
| GitHub repository | `github://myorg/infra-platform` | `github://myorg/infra-*` |

**When multiple patterns match** a URI, the gateway selects the one with the longest pattern (most-specific match). Ties are broken alphabetically by `metadata.name`. Only the selected `GovernedResource`'s `permittedActions` and `permittedAgents` apply — there is no union across matches.

---

## Admission enforcement order

When an `AgentRequest` is submitted, the gateway enforces in this order (before any SafetyPolicy evaluation):

1. **At least one pattern matches** the target URI. If none match → `ACTION_NOT_PERMITTED`.
2. **Agent identity matches** the caller's OIDC token claim. The gateway overwrites `spec.agentIdentity` with the token value — it cannot be spoofed.
3. **Agent is in `permittedAgents`** (or `permittedAgents` is empty). If not → `IDENTITY_INVALID`.
4. **Action is in `permittedActions`**. If not → `ACTION_NOT_PERMITTED`.

`SafetyPolicy` rules run after these checks. They can further restrict what is allowed inside the `GovernedResource` envelope but cannot expand beyond it.

---

## Context fetchers

After admission, the controller invokes the named fetcher and writes the result to `AgentRequestStatus.ProviderContext`. Reviewers see live resource state alongside the agent's declared intent.

### `karpenter`

Reads the target `NodePool` via the Kubernetes API. Returns:

```json
{
  "currentLimitCPU": "100",
  "currentLimitMemory": "400Gi",
  "currentNodeCount": 47,
  "pendingPods": 12,
  "estimatedCostDeltaPerHour": "$0.00",
  "recentScalingEvents": []
}
```

No external credentials needed — uses the controller's in-cluster service account.

### `github`

Reads repository metadata via the GitHub REST API. Returns:

```json
{
  "title": "infra-platform",
  "defaultBranch": "main",
  "openPRCount": 3,
  "ciStatus": "passing"
}
```

**Requires** a Kubernetes Secret named `aip-github-token` in the `aip-system` namespace with a `token` key containing a GitHub personal access token scoped to `repo:read`.

```bash
kubectl create secret generic aip-github-token \
  -n aip-system \
  --from-literal=token=<your-github-pat>
```

### `k8s-deployment`

Reads a Kubernetes Deployment and its EndpointSlices. Returns:

```json
{
  "targetExists": true,
  "hasActiveEndpoints": true,
  "activeEndpointCount": 6,
  "readyReplicas": 3,
  "specReplicas": 3,
  "stateFingerprint": "<resourceVersion>"
}
```

Target URI format: `k8s://<cluster>/<namespace>/deployment/<name>`

### `none`

No context is fetched. Use for resources where independent verification is not available or not needed.

### FetcherSchemaViolation

If a fetcher returns data that violates the `GovernedResource`'s `contextSchema`, the controller writes a `FetcherSchemaViolation` condition on the `AgentRequest` and sets `ProviderContext` to null. The request **is not blocked** — it proceeds without context. Reviewers see the condition as a warning. This fail-open behavior is intentional: a broken fetcher should not halt an agent operating correctly.

---

## SafetyPolicy binding

`SafetyPolicy` uses `governedResourceSelector` (a standard Kubernetes `LabelSelector`) to select which `GovernedResource` objects its rules apply to. Set labels on your `GovernedResource` to control which policies bind to it.

```yaml
# GovernedResource with a label
apiVersion: governance.aip.io/v1alpha1
kind: GovernedResource
metadata:
  name: karpenter-nodepool-team-a
  labels:
    team: team-a
    resource-type: karpenter
spec:
  uriPattern: "k8s://prod/karpenter.sh/nodepool/team-a-*"
  permittedActions: ["scale-up", "scale-down"]
  contextFetcher: karpenter
```

```yaml
# SafetyPolicy that applies to all karpenter GovernedResources
apiVersion: governance.aip.io/v1alpha1
kind: SafetyPolicy
metadata:
  name: karpenter-scale-policy
  namespace: default
spec:
  governedResourceSelector:
    matchLabels:
      resource-type: karpenter
  contextType: karpenter
  rules:
    - name: require-approval-above-threshold
      type: StateEvaluation
      action: RequireApproval
      expression: "true"
```

`contextType` binds the SafetyPolicy's CEL rules to the named fetcher's schema. Rules that reference context fields (e.g. `context.currentNodeCount > 50`) are type-checked against `contextSchema` at SafetyPolicy creation time — type errors are rejected immediately, not at evaluation time.

---

## Admin API

All `GovernedResource` and `SafetyPolicy` management requires the admin role (`--admin-subjects` / `--admin-groups` on the gateway).

### GovernedResource endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/governed-resources` | Create a GovernedResource. |
| `GET` | `/governed-resources` | List all GovernedResources. |
| `GET` | `/governed-resources/{name}` | Get a GovernedResource by name. |
| `PUT` | `/governed-resources/{name}` | Full replace (optimistic concurrency — retries on conflict). |
| `DELETE` | `/governed-resources/{name}` | Delete. Returns 409 if active AgentRequests reference this resource. |

### SafetyPolicy endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/safety-policies` | Create a SafetyPolicy. |
| `GET` | `/safety-policies` | List SafetyPolicies. Pass `?namespace=<ns>` to filter. |
| `GET` | `/safety-policies/{name}` | Get by name. Pass `?namespace=<ns>`. |
| `PUT` | `/safety-policies/{name}` | Full replace. Pass `?namespace=<ns>`. |
| `DELETE` | `/safety-policies/{name}` | Delete. Pass `?namespace=<ns>`. |

---

## Schema evolution

`contextSchema` follows append-only evolution rules:

- **Adding a field**: allowed. Existing SafetyPolicy CEL rules are not affected.
- **Removing or changing a field type**: rejected with 409. The gateway returns the list of affected fields.

To remove a field, first remove any SafetyPolicy CEL rules that reference it, then resubmit the PUT.

---

## Deletion protection

The controller places a finalizer (`governance.aip.io/active-requests`) on every `GovernedResource` that has at least one non-terminal `AgentRequest` referencing it. Attempting to delete such a `GovernedResource` returns 409 from the gateway. The finalizer is removed automatically once all referencing requests reach a terminal phase (Approved, Denied, Completed).

To force-delete in a break-glass scenario:

```bash
kubectl patch governedresource <name> \
  --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]'
kubectl delete governedresource <name>
```

Any in-flight `AgentRequest` referencing the deleted resource will be denied with `GOVERNED_RESOURCE_DELETED`.

---

## Backward compatibility flag

```
--require-governed-resource   default: false
```

- `false`: if no `GovernedResource` objects exist, the admission check is skipped. Existing deployments are unaffected.
- `true`: every `AgentRequest` must match a `GovernedResource` or it is rejected. Enable once your registry is fully populated.

Once at least one `GovernedResource` exists, enforcement is always active regardless of this flag.
