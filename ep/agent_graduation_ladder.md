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

- **Per-namespace graduation overrides.** `AgentTrustProfile` is namespace-scoped in
  v1alpha1 — trust accumulates independently per namespace. Cross-namespace aggregation
  (e.g., trust earned in `staging` automatically transferring to `production`) is
  explicitly out of scope. Teams that need a unified trust signal across environments
  should use a separate promotion process.

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

```text
agentRequest(target, action, reason)
```

No mode flag. No trust-level awareness in the agent. The agent always expresses intent:
"I want to do X to Y, here is my reasoning." The control plane decides what happens next
based on the agent's current trust level:

| Trust level | What happens to the request | Entry criteria |
|---|---|---|
| `Observer` | Evaluated and graded. Action NOT taken. Agent receives verdict. | Default for all new agents |
| `Advisor` | Queued for human approval. Executed if approved. | 10 verdicts, 0.70 accuracy |
| `Supervised` | Queued for human approval. Executed if approved. | 20 verdicts, 0.85 accuracy, 20 executions |
| `Trusted` | Auto-approved if `SafetyPolicy` passes. Executed. | 50 verdicts, 0.92 accuracy, 50 executions |
| `Autonomous` | Auto-approved if `SafetyPolicy` passes. Executed. | 100 verdicts, 0.97 accuracy, 100 executions |

`Advisor` and `Supervised` have identical runtime behavior — human approval required on
every request. They differ only in the evidence required to reach them. `Advisor` is the
first execution-capable level (low bar, human always in loop). `Supervised` requires
proven execution history on top of diagnostic accuracy. The thresholds above are defaults
from `AgentGraduationPolicy` and are configurable by the cluster admin.

The distinction between `Observer` and the action-taking levels is enforced by the
control plane, not declared by the agent.

### 2. `AgentDiagnostic` is internal and may be eliminated

`AgentDiagnostic` is not part of the agent SDK. Agent developers never create it directly.
Grading state for `Observer`-level requests lives on `AgentRequest` status — the verdict
is patched onto the same resource the agent created. `AgentDiagnostic` has no role in
this flow and can be removed without any agent-facing impact. It is retained for now only
to avoid breaking existing users of the direct `AgentDiagnostic` API; a deprecation
notice will be added when `AwaitingVerdict` phase ships.

### 3. Enforcement is prescriptive, not descriptive

The trust gate is enforced at the gateway on every request, regardless of whether a
`SafetyPolicy` exists. A `SafetyPolicy` that checks `agent.trustLevel` only works if
someone writes it. The graduation ladder works out of the box.

`SafetyPolicy` CEL evaluation runs after the trust gate and can only add restrictions —
it cannot grant permissions that the trust gate has blocked.

### 4. `spec.classification` on `AgentRequest` — optional, self-declared

Agents may declare the problem classification they believe applies to their request:

```yaml
spec:
  agentIdentity: karpenter-nodepool-agent
  classification: "nodepool/at-capacity"   # optional, format: category/subcategory
  action: update
  target:
    uri: github://myorg/infra/files/main/clusters/prod/karpenter/gpu-pool.yaml
```

The gateway validates format (`[a-z][a-z0-9-]*/[a-z][a-z0-9-]*`) but does not validate
the value. Classification is self-declared by the agent — the same way `agentIdentity`
is. If an agent consistently mislabels, verdicts for that classification degrade, which
is self-correcting.

For v1alpha1, classification is recorded on verdicts and `DiagnosticAccuracySummary`
but not used for per-classification accuracy enforcement. Per-classification trust levels
are deferred. Adding the field now ensures historical verdict data is available for
backfill when per-classification accuracy is implemented.

If absent, the dashboard warns: "This agent does not provide classification. Accuracy is
aggregate and may hide variance across problem types."

### 5. Cluster admin owns the thresholds and per-resource ceilings

Graduation thresholds are set once per cluster by the cluster admin via
`AgentGraduationPolicy`. Individual platform teams cannot lower the bar.

For high-risk resources (e.g. nodepools, cluster-critical configs), the cluster admin
sets a `trustRequirements` ceiling on the `GovernedResource`. No agent, regardless of
trust level, can act autonomously on that resource beyond the ceiling the admin has
configured. Only the cluster admin can raise it.

## The Three Control Plane Artifacts

### `AgentGraduationPolicy` (new CRD, cluster-scoped)

One per cluster. Set by cluster admin. Defines the accuracy and execution bands for each
trust level, the evaluation window, and the TTL for ungraded requests.

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: AgentGraduationPolicy
metadata:
  name: cluster-default
spec:
  # evaluationWindow controls which verdicts drive the trust level.
  # count: use the last N verdicts (rolling window).
  # maxAge is reserved for future time-based decay — additive, no schema change needed.
  evaluationWindow:
    count: 50

  # awaitingVerdictTTL: how long an ungraded Observer request waits before
  # transitioning to Expired. Expired requests are excluded from accuracy counts.
  awaitingVerdictTTL: 168h   # 7 days default

  levels:
    - name: Observer
      # Action is NOT taken. Request is graded only.
      accuracy: { max: 0.70 }
      canExecute: false

    - name: Advisor
      # Action is taken. Human approval required on every request.
      # demotionBuffer: accuracy must drop this far below band.min to trigger demotion,
      # preventing flapping at the boundary.
      accuracy: { min: 0.70, max: 0.85, demotionBuffer: 0.02 }
      executions: { min: 0, max: 20 }
      requiresHumanApproval: true

    - name: Supervised
      accuracy: { min: 0.85, max: 0.92, demotionBuffer: 0.02 }
      executions: { min: 20, max: 50 }
      requiresHumanApproval: true

    - name: Trusted
      # Auto-approved if SafetyPolicy passes. No human in the loop.
      accuracy: { min: 0.92, max: 0.97, demotionBuffer: 0.02 }
      executions: { min: 50, max: 100 }
      requiresHumanApproval: false

    - name: Autonomous
      accuracy: { min: 0.97 }
      executions: { min: 100 }
      requiresHumanApproval: false
```

**Promotion rule**: the agent reaches the highest level where BOTH
`recentAccuracy >= band.accuracy.min` AND `totalExecutions >= band.executions.min`.
Both dimensions must be satisfied to promote.

**Demotion rule**: triggers on accuracy only —
`recentAccuracy < (currentBand.accuracy.min - demotionBuffer)`.
Execution count is monotonically increasing and can never trigger demotion.

**Level resolution when dimensions disagree**: if `recentAccuracy` is in the Trusted
band (0.95) but `totalExecutions` is 10, the effective level is Advisor — the highest
level where both dimensions are satisfied simultaneously. The controller always resolves
to the highest fully-satisfied level.

The bands above are defaults shipped with the Helm chart. Cluster admins override them
for their risk tolerance.

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
  diagnosticAccuracy: 0.81       # all-time accuracy — for audit and trend visibility
  recentAccuracy: 0.79           # rolling window accuracy — drives trustLevel computation
  totalObserveVerdicts: 14
  successRate: 0.0               # from Advisor+ terminal transitions
  totalExecutions: 0
  lastEvaluatedAt: "2026-04-20T10:00:00Z"
  nextLevelRequirements:
    level: Supervised
    remaining:
      verdicts: 6        # needs 6 more graded requests in window
      accuracy: 0.06     # recentAccuracy needs to reach 0.85
      executions: 20     # needs first 20 supervised executions
```

`diagnosticAccuracy` is the all-time average — preserved for audit and trend analysis.
`recentAccuracy` is the rolling window value (last N verdicts per `evaluationWindow.count`)
that the controller uses to compute `trustLevel`. An agent cannot coast on historical
performance — recent behavior drives the level.

`nextLevelRequirements` makes graduation legible. An SRE can see exactly what the agent
needs to advance without reading policy YAML.

## Gateway Enforcement Order

On every `AgentRequest`:

```text
1. Find matching GovernedResource for spec.target.uri
   → 404 if no GovernedResource matches (ungoverned target)

2. Fetch AgentTrustProfile for spec.agentIdentity in this namespace
   → treat as Observer if no profile exists yet (first request from a new agent)

3. Check AgentGraduationPolicy for agent's trust level:
   → canExecute: false  →  skip steps 4–5, route directly to AwaitingVerdict
      (Observer requests bypass the minTrustLevel floor — grading has no blast radius)

4. Check GovernedResource.trustRequirements.minTrustLevel
   → 403 "Insufficient trust. Current: Advisor, Required: Trusted" if below floor

5. Apply GovernedResource.trustRequirements.maxAutonomyLevel as ceiling
   → cap effective behavior regardless of actual trust level

6. Check AgentGraduationPolicy for effective trust level:
   → requiresHumanApproval: true  →  route to Pending (human approval required)
   → requiresHumanApproval: false  →  proceed to SafetyPolicy evaluation

7. SafetyPolicy CEL evaluation
   → can add restrictions, cannot bypass steps 1–6
```

## Grading Flow (Observer Level)

When a request routes to `AwaitingVerdict`:

1. Agent's request sits in `AwaitingVerdict` phase. No OpsLock acquired. No action taken.
2. Dashboard surfaces it for grading alongside the agent's `spec.reason` and `spec.action`.
3. Reviewer calls `PATCH /agent-requests/{name}/verdict` with:
   - `verdict`: `correct | partial | incorrect`
   - `reasonCode` (required when verdict is `incorrect` or `partial`):
     `wrong_diagnosis | bad_timing | scope_too_broad | precautionary | policy_block`
   - `note`: optional free-text annotation

   Only `wrong_diagnosis` counts against accuracy. The other reason codes are recorded
   for audit but do not affect the graduation ladder — bad timing or scope says nothing
   about diagnostic quality.

4. **Gateway** writes the verdict to `AgentRequest.status.verdict` and stops. No
   accuracy computation in the gateway.
5. **Controller** watches `AgentRequest` verdict changes → upserts
   `DiagnosticAccuracySummary`. Only `correct` and `wrong_diagnosis` verdicts update
   accuracy counters.
6. **Controller** watches `DiagnosticAccuracySummary` changes → reconciles
   `AgentTrustProfile`: recomputes `recentAccuracy`, `diagnosticAccuracy`, `trustLevel`,
   `nextLevelRequirements`.
7. Request transitions to `Completed` (graded). Agent is notified via status.

**If nobody grades (TTL expires)**: request transitions to `Expired`. Expired requests
are excluded from accuracy counts — they are neither correct nor incorrect.

## Demotion Behavior

When the controller reconciles `AgentTrustProfile` and finds
`recentAccuracy < currentBand.accuracy.min - demotionBuffer`, it downgrades `trustLevel`
to the highest level where both dimensions are still fully satisfied.

**In-flight requests are not revoked on demotion.** A request that was auto-approved
while the agent was `Trusted` and is currently in `Executing` phase continues to
completion under the original approval. The demotion applies to the next request, not
the current one. This matches K8s semantics — revoking a ServiceAccount token does not
kill running pods. The blast radius of any single auto-approved action is bounded by
the OpsLock duration.

**Trust level is re-evaluated on every controller reconcile.** There is no hysteresis
on the time dimension — if accuracy recovers, the agent can be re-promoted on the next
reconcile cycle. The `demotionBuffer` prevents accuracy-boundary flapping between two
adjacent reconciles, not time-based oscillation.

## State Machine

```text
Observer path (grading):
  Submitted → AwaitingVerdict → Completed (graded, verdict recorded)
                              → Expired   (awaitingVerdictTTL exceeded, not graded,
                                           excluded from accuracy counts)

Advisor / Supervised path (human approval):
  Submitted → Pending → Approved → Executing → Completed
                      → Denied   (human rejected)

Trusted / Autonomous path (auto-approval):
  Submitted → Approved (synchronous, gateway returns immediately)
            → Denied   (SafetyPolicy blocked)
  Approved  → Executing → Completed
```

Each request is evaluated at submission time. If an agent promotes between request N
and request N+1, request N completes under its original Observer/Advisor treatment.
Request N+1 gets the new level. There is no retroactive re-evaluation.

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
| `AgentDiagnostic` | Legacy CRD; grading state now lives on `AgentRequest` status. Deprecated when `AwaitingVerdict` phase ships. |
| `DiagnosticAccuracySummary` | Running accuracy ratio per agent; intermediate aggregate feeding AgentTrustProfile |
| `SafetyPolicy` | Additional restrictions on top of trust gate; cannot bypass it |
| `GovernedResource` | Defines per-resource trust floor and ceiling via `trustRequirements` |
| `AgentGraduationPolicy` | Cluster-wide graduation thresholds; owned by cluster admin |
| `AgentTrustProfile` | Computed trust state per agent; controller-owned; feeds gateway enforcement |

## Known Limitations

**Aggregate accuracy hides per-classification variance.**

`DiagnosticAccuracySummary` and `AgentTrustProfile` track a single aggregate
`diagnosticAccuracy` per agent. This can give false confidence. An agent with aggregate
0.91 could be 0.98 on `Nodepool/AtCapacity` (safe to auto-approve) and 0.40 on
`Network/Partition` (dangerous). The graduation ladder would promote it to `Trusted` and
auto-approve a network partition diagnosis it has rarely gotten right.

The correct fix is per-classification accuracy: key `DiagnosticAccuracySummary` by
`(agentIdentity, rootCauseCategory)` and compute per-class scores. `AgentTrustProfile`
exposes `agent.accuracy['Nodepool/AtCapacity'].score` to CEL. `GovernedResource` can
require a minimum accuracy for the relevant classification.

This is deferred to a follow-up. For v1alpha1, cluster admins should set conservative
`AgentGraduationPolicy` thresholds (0.90+) and use `SafetyPolicy` CEL to restrict
auto-approval to the action types the agent has demonstrably handled well.

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
