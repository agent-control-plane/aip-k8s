# Design: Agent Graduation Ladder

Status: Draft

## Problem

AIP today has no mechanism for an agent to earn autonomy over time. Every `AgentRequest`
goes through the same human approval workflow regardless of whether the agent has a perfect
track record or has never run in production. This creates two failure modes:

1. **Adoption friction**: teams that want to deploy trusted agents still route every action
   through a human reviewer. The governance layer becomes overhead, not a safety net.

2. **No soak-test path**: teams that want to validate a new agent before granting it real
   authority have no structured way to run it in observation mode, grade its reasoning, and
   promote it. They build ad-hoc shadow-mode tooling outside AIP or skip the soak test
   entirely.

The missing piece is a graduation ladder: a mechanism for agents to earn increasing autonomy
by demonstrating diagnostic accuracy during observation and execution correctness during
supervised operation, with cluster administrators controlling the thresholds.

## Non-Goals

- **Per-namespace graduation overrides.** Trust is cluster-wide. An agent that is `Trusted`
  in staging but `Observer` in production would produce inconsistent semantics. Teams that
  need different trust levels per environment should use separate clusters.

- **Automated mode switching in the agent SDK.** The agent does not decide whether it is
  in observation or execution mode. The control plane decides based on trust level. The
  agent SDK has one method.

- **Replacing `SafetyPolicy` CEL evaluation.** The graduation ladder is a floor and ceiling.
  `SafetyPolicy` can add further restrictions on top but cannot bypass the trust gate.

- **GitHub PR outcome as a trust signal (yet).** Merge outcome is a lagging, coarse-grained
  signal. Diagnosis grading is immediate and fine-grained. GitHub outcomes are additive —
  see `ep/external_resource_governance.md §8 Phase 7`.

## Core Design Decisions

### 1. The agent SDK has one method

```
agentRequest(target, action, reason)
```

No mode flag. No trust-level awareness in the agent. The agent always expresses intent:
"I want to do X to Y, here is my reasoning." The control plane decides what happens next
based on the agent's current trust level:

| Trust level | What happens to the request |
|---|---|
| `Observer` | Evaluated and graded. Action NOT taken. Agent receives verdict. |
| `Advisor` | Queued for human approval. Executed if approved. |
| `Supervised` | Queued for human approval. Executed if approved. |
| `Trusted` | Auto-approved if `SafetyPolicy` passes. Executed. |
| `Autonomous` | Auto-approved if `SafetyPolicy` passes. Executed. |

The distinction between `Observer` and the action-taking levels is enforced by the
control plane, not declared by the agent.

### 2. `AgentDiagnostic` is internal

`AgentDiagnostic` is not part of the agent SDK. Agent developers never create it directly.
It is an internal CRD used by the control plane to track grading state for `Observer`-level
requests. Exposing it would force agent developers to reason about two separate resources
and two separate workflows for what is ultimately one intent: "here is what I found and
what I would do."

### 3. Enforcement is prescriptive, not descriptive

The trust gate is enforced at the gateway on every request, regardless of whether a
`SafetyPolicy` exists. A `SafetyPolicy` that checks `agent.trustLevel` only works if
someone writes it. The graduation ladder works out of the box.

`SafetyPolicy` CEL evaluation runs after the trust gate and can only add restrictions —
it cannot grant permissions that the trust gate has blocked.

### 4. Cluster admin owns the thresholds and per-resource ceilings

Graduation thresholds are set once per cluster by the cluster admin via
`AgentGraduationPolicy`. Individual platform teams cannot lower the bar.

For high-risk resources (e.g. nodepools, cluster-critical configs), the cluster admin
sets a `trustRequirements` ceiling on the `GovernedResource`. No agent, regardless of
trust level, can act autonomously on that resource beyond the ceiling the admin has
configured. Only the cluster admin can raise it.

## The Three Control Plane Artifacts

### `AgentGraduationPolicy` (new CRD, cluster-scoped)

One per cluster. Set by cluster admin. Defines what it takes to reach each trust level
and what behavior is permitted at each level.

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: AgentGraduationPolicy
metadata:
  name: cluster-default
spec:
  levels:
    - name: Observer
      # Action is NOT taken. Request is graded.
      minObserveVerdicts: 0
      minDiagnosticAccuracy: 0.0
      canExecute: false

    - name: Advisor
      # Action is taken. Human approval required on every request.
      minObserveVerdicts: 10
      minDiagnosticAccuracy: 0.70
      minExecutions: 0
      requiresHumanApproval: true

    - name: Supervised
      minObserveVerdicts: 20
      minDiagnosticAccuracy: 0.85
      minExecutions: 20
      requiresHumanApproval: true

    - name: Trusted
      # Auto-approved if SafetyPolicy passes. No human in the loop.
      minObserveVerdicts: 50
      minDiagnosticAccuracy: 0.92
      minExecutions: 50
      requiresHumanApproval: false

    - name: Autonomous
      minObserveVerdicts: 100
      minDiagnosticAccuracy: 0.97
      minExecutions: 100
      requiresHumanApproval: false
```

The thresholds above are defaults shipped with the Helm chart. Cluster admins override
them for their risk tolerance. A conservative production cluster raises the bars. A
fast-moving internal platform lowers them.

### `GovernedResource.spec.trustRequirements` (new field on existing CRD)

Per-resource trust floor and ceiling. Owned by cluster admin. Only cluster admin RBAC
role can modify this field.

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: GovernedResource
metadata:
  name: nodepool-resources
spec:
  uriPattern: "k8s://*/nodepools/**"
  permittedActions: ["update"]
  trustRequirements:
    minTrustLevel: Trusted       # hard floor — Observer/Advisor/Supervised blocked entirely
    maxAutonomyLevel: Supervised # hard ceiling — even Trusted/Autonomous require human approval
```

`minTrustLevel`: agents below this level receive a 403 regardless of any `SafetyPolicy`.

`maxAutonomyLevel`: caps the autonomy level applied to this resource. An `Autonomous`
agent acting on this resource is treated as `Supervised` — human approval required. The
cluster admin must explicitly raise the ceiling when they are ready to trust autonomous
changes to high-risk resources.

### `AgentTrustProfile` (new CRD, namespace-scoped, controller-owned)

One per `agentIdentity` per namespace. Nobody writes to it — only the controller.
Computed from graded `Observer`-level requests and terminal `AgentRequest` history.

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: AgentTrustProfile
metadata:
  name: k8s-debug-agent
  namespace: production
status:
  trustLevel: Advisor
  diagnosticAccuracy: 0.81       # from graded Observer-level requests
  totalObserveVerdicts: 14
  successRate: 0.0               # from Advisor+ terminal transitions
  totalExecutions: 0
  lastEvaluatedAt: "2026-04-20T10:00:00Z"
  nextLevelRequirements:
    level: Supervised
    remaining:
      minObserveVerdicts: 6      # needs 6 more graded requests
      minDiagnosticAccuracy: 0.04 # needs 0.85 - 0.81 = 0.04 improvement
      minExecutions: 20          # needs first 20 supervised executions
```

`nextLevelRequirements` is the UX that makes graduation legible. An SRE can read this
and know exactly what the agent needs to advance — no policy YAML spelunking required.

## Gateway Enforcement Order

On every `AgentRequest`:

```
1. Find matching GovernedResource for spec.target.uri
   → 404 if no GovernedResource matches (ungoverned target)

2. Fetch AgentTrustProfile for spec.agentIdentity in this namespace
   → treat as Observer if no profile exists yet (first request from a new agent)

3. Check GovernedResource.trustRequirements.minTrustLevel
   → 403 "Insufficient trust. Current: Advisor, Required: Trusted" if below floor

4. Apply GovernedResource.trustRequirements.maxAutonomyLevel as ceiling
   → cap effective behavior regardless of actual trust level

5. Check AgentGraduationPolicy for this effective trust level:
   → canExecute: false  →  route to AwaitingVerdict (graded, no action)
   → requiresHumanApproval: true  →  route to Pending (human approval required)
   → requiresHumanApproval: false  →  proceed to SafetyPolicy evaluation

6. SafetyPolicy CEL evaluation
   → can add restrictions, cannot bypass steps 1–5
```

## Grading Flow (Observer Level)

When a request routes to `AwaitingVerdict`:

1. Agent's request sits in `AwaitingVerdict` phase. No OpsLock acquired. No action taken.
2. Dashboard surfaces it for grading alongside the agent's `spec.reason` and `spec.action`.
3. Reviewer calls `PATCH /agent-requests/{name}/verdict` with `correct / partial / incorrect`.
4. Gateway persists verdict on request status, upserts `DiagnosticAccuracySummary` for
   the agent.
5. `AgentTrustProfile` controller reconciles: recomputes `diagnosticAccuracy`,
   `trustLevel`, `nextLevelRequirements`.
6. Request transitions to `Completed` (graded). Agent is notified via status.

## Bootstrap Path for New Agents

A brand-new agent has no `AgentTrustProfile`. The gateway treats it as `Observer`.
Its first requests are graded and not executed. This is intentional — an agent must
demonstrate it can reason correctly before the control plane will act on its behalf.

For agents being migrated into AIP from an existing trusted system, the cluster admin
can create an `AgentTrustProfile` manually with an initial `trustLevel` override and a
`bootstrapReason` annotation. Normal accumulation resumes immediately after — the override
does not suppress ongoing accuracy tracking.

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: AgentTrustProfile
metadata:
  name: legacy-infra-agent
  namespace: production
  annotations:
    governance.aip.io/bootstrap-reason: "migrated from internal approval system, 18 months production history"
spec:
  trustLevelOverride: Supervised  # cluster admin sets this; controller never overwrites it
```

## Relationship to Existing CRDs

| CRD | Role in graduation |
|---|---|
| `AgentRequest` | Source of execution history (Advisor+ terminal transitions feed successRate) |
| `AgentDiagnostic` | Internal grading state for Observer-level requests; not in agent SDK |
| `DiagnosticAccuracySummary` | Running accuracy ratio per agent; intermediate aggregate feeding AgentTrustProfile |
| `SafetyPolicy` | Additional restrictions on top of trust gate; cannot bypass it |
| `GovernedResource` | Defines per-resource trust floor and ceiling via `trustRequirements` |
| `AgentGraduationPolicy` | Cluster-wide graduation thresholds; owned by cluster admin |
| `AgentTrustProfile` | Computed trust state per agent; controller-owned; feeds gateway enforcement |

## Open Questions

1. **`AgentTrustProfile` scope**: namespace-scoped means an agent builds separate trust
   in each namespace. This is the recommendation for v1alpha1 — trust earned in `staging`
   does not automatically transfer to `production`. A future version could support
   cross-namespace trust aggregation with explicit admin opt-in.

2. **Verdict authority**: today any authenticated reviewer can grade requests. Should
   grading be restricted to a specific RBAC role? The accuracy signal is only as good as
   the graders — a reviewer who randomly clicks `correct` inflates the score. A dedicated
   `agentgrader` role with explicit binding is worth considering before Phase 2 ships.

3. **Trust decay**: should `diagnosticAccuracy` and `successRate` decay over time if an
   agent goes inactive? A stale 0.97 accuracy from two years ago may not reflect the
   agent's current model. A configurable half-life on the `AgentGraduationPolicy` is the
   right hook; defer until real data shows decay is a problem.
