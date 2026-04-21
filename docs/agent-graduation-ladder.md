# Agent Graduation Ladder

Agents in AIP earn autonomy over time. A brand-new agent cannot take actions — it submits
requests that are graded but not executed. As its diagnosis quality is validated by human
reviewers, it graduates to levels where humans approve each action, then to levels where
actions are auto-approved by policy. The cluster admin controls the thresholds. The control
plane enforces them automatically.

This document explains how the graduation ladder works end-to-end using a concrete example:
a Karpenter nodepool agent that diagnoses cluster scaling pressure and opens GitHub PRs to
increase nodepool capacity.

---

## The five levels

| Level | What happens to a request |
|---|---|
| `Observer` | Evaluated and graded. Action **not** taken. |
| `Advisor` | Queued for human approval. Executed if approved. |
| `Supervised` | Queued for human approval. Executed if approved. |
| `Trusted` | Auto-approved if SafetyPolicy passes. No human in the loop. |
| `Autonomous` | Auto-approved if SafetyPolicy passes. No human in the loop. |

The difference between `Observer` and `Advisor/Supervised` is fundamental: at `Observer`
the control plane does not act on behalf of the agent at all. Requests are used purely
to build an accuracy signal.

The difference between `Supervised` and `Trusted` is who approves: a human reviewer vs
the policy engine. The cluster admin decides when an agent crosses that boundary.

---

## The agent SDK has one method

The agent does not declare its trust level or choose a mode. It always expresses intent:

```python
aip.request(
    target="github://myorg/infra/files/main/clusters/prod/karpenter/gpu-pool.yaml",
    action="update",
    reason="""
        15 pods in gpu-workloads pending for 8 minutes. All 10 nodes in gpu-pool occupied.
        Autoscaler cannot provision: maxNodes cap reached.
        Root cause: maxNodes=10 too low for current batch workload.
        Recommendation: increase maxNodes from 10 to 20.
    """
)
```

The control plane determines the outcome. The agent receives back the request status
(graded, pending approval, approved, denied) and acts accordingly.

---

## Control plane configuration

Before the agent runs, the cluster admin configures three things.

### 1. GovernedResource — what the agent is allowed to target

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: GovernedResource
metadata:
  name: karpenter-nodepools
spec:
  uriPattern: "github://myorg/infra/files/main/clusters/*/karpenter/**"
  permittedActions:
    - update
  trustRequirements:
    minTrustLevel: Observer     # grading requests accepted from any new agent
    maxAutonomyLevel: Supervised # even a Trusted/Autonomous agent must still get human
                                 # approval for nodepool changes — cluster admin sets this
```

`minTrustLevel` is the execution floor. Agents below this level cannot execute against
this resource. Observer-level requests (graded, not executed) always bypass this check
because no action is being taken.

`maxAutonomyLevel` is the ceiling. No matter how trusted an agent becomes, it cannot act
on this resource with more autonomy than the ceiling allows. For nodepool changes — which
affect cluster capacity and cost — the cluster admin deliberately keeps the ceiling at
`Supervised`. An SRE is always in the loop for these changes until the admin explicitly
raises it.

Only the cluster-admin RBAC role can modify `trustRequirements`.

### 2. AgentGraduationPolicy — what it takes to reach each level

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: AgentGraduationPolicy
metadata:
  name: cluster-default
spec:
  levels:
    - name: Observer
      # No execution. Request is graded only.
      minObserveVerdicts: 0
      minDiagnosticAccuracy: 0.0
      canExecute: false

    - name: Advisor
      # Execution allowed. Human approval required on every request.
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
      # Auto-approved if SafetyPolicy passes.
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

These thresholds are the defaults shipped with the Helm chart. The cluster admin overrides
them. A conservative production cluster raises the bars. An internal platform with a small
trusted team lowers them.

### 3. SafetyPolicy — additional guardrails (optional but recommended)

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: SafetyPolicy
metadata:
  name: nodepool-guardrails
  namespace: production
spec:
  targetGovernedResource: karpenter-nodepools
  rule: |
    # Only the designated nodepool agent can touch karpenter files
    request.agentIdentity == "karpenter-nodepool-agent"
```

SafetyPolicy CEL evaluation runs after the trust gate. It can add restrictions but cannot
bypass the trust enforcement above.

---

## The graduation walkthrough

### Week 1–2: Observer level

The agent is new. No `AgentTrustProfile` exists. The gateway treats every new agent as
`Observer`.

**Agent files its first request:**

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: AgentRequest
metadata:
  name: nodepool-req-001
  namespace: production
spec:
  agentIdentity: karpenter-nodepool-agent
  action: update
  target:
    uri: github://myorg/infra/files/main/clusters/prod/karpenter/gpu-pool.yaml
  reason: |
    Observed 15 pending pods in gpu-workloads for 8 minutes.
    All 10 nodes in NodePool gpu-pool are occupied (CPU 94%, Memory 89%).
    Autoscaler blocked: maxNodes cap reached.
    Root cause: maxNodes=10 insufficient for current batch demand spike.
    Proposed change: increase maxNodes from 10 to 20 in gpu-pool.yaml.
```

**Gateway decision:**

```
1. GovernedResource match: karpenter-nodepools ✓  (uri matches pattern)
2. AgentTrustProfile: not found → treat as Observer
3. minTrustLevel check: SKIPPED (Observer, canExecute=false, no action taken)
4. AgentGraduationPolicy: Observer → canExecute: false
   → route to AwaitingVerdict phase
5. No OpsLock acquired. No GitHub PR opened.
```

**Request status:**

```yaml
status:
  phase: AwaitingVerdict
```

**SRE reviews the request on the dashboard.** They see the agent's reason, check the
cluster metrics themselves, and agree the diagnosis is correct. They click `correct`.

```
PATCH /agent-requests/nodepool-req-001/verdict
{ "verdict": "correct", "note": "confirmed — gpu-pool was saturated, 15 pods pending" }
```

**DiagnosticAccuracySummary after verdict 1:**

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: DiagnosticAccuracySummary
metadata:
  name: karpenter-nodepool-agent
  namespace: production
spec:
  agentIdentity: karpenter-nodepool-agent
status:
  totalReviewed: 1
  correctCount: 1
  partialCount: 0
  incorrectCount: 0
  diagnosticAccuracy: 1.0    # (1 + 0) / 1
  lastUpdatedAt: "2026-04-20T09:00:00Z"
```

**AgentTrustProfile after verdict 1:**

```yaml
status:
  trustLevel: Observer
  diagnosticAccuracy: 1.0
  totalObserveVerdicts: 1
  successRate: 0.0
  totalExecutions: 0
  nextLevelRequirements:
    level: Advisor
    remaining:
      minObserveVerdicts: 9    # needs 9 more graded requests
      minDiagnosticAccuracy: 0  # already above 0.70
      minExecutions: 0
```

The agent keeps running daily. Each time it detects scaling pressure, files a request,
gets graded. Over the next two weeks, 10 requests are graded:

| Request | Verdict | SRE note |
|---|---|---|
| nodepool-req-001 | correct | confirmed gpu-pool saturated |
| nodepool-req-002 | correct | spot-on, cpu-pool was also affected |
| nodepool-req-003 | partial | right resource, underestimated severity |
| nodepool-req-004 | correct | |
| nodepool-req-005 | incorrect | false positive — spike was transient, resolved in 2 min |
| nodepool-req-006 | correct | |
| nodepool-req-007 | correct | |
| nodepool-req-008 | partial | missed that a second pool was also constrained |
| nodepool-req-009 | correct | |
| nodepool-req-010 | correct | |

**DiagnosticAccuracySummary after 10 verdicts:**

```yaml
status:
  totalReviewed: 10
  correctCount: 7
  partialCount: 2
  incorrectCount: 1
  diagnosticAccuracy: 0.80    # (7 + 0.5*2) / 10
  lastUpdatedAt: "2026-05-04T14:00:00Z"
```

**AgentTrustProfile graduates to Advisor:**

```yaml
status:
  trustLevel: Advisor          # 0.80 >= 0.70 and totalObserveVerdicts >= 10 ✓
  diagnosticAccuracy: 0.80
  totalObserveVerdicts: 10
  successRate: 0.0
  totalExecutions: 0
  nextLevelRequirements:
    level: Supervised
    remaining:
      minObserveVerdicts: 10   # needs 10 more
      minDiagnosticAccuracy: 0.05  # needs 0.85 - 0.80
      minExecutions: 20        # needs first 20 supervised executions
```

---

### Month 2: Advisor level — human approval on every request

**Agent files its 11th request. Now Advisor.**

Gateway decision:

```
1. GovernedResource match: karpenter-nodepools ✓
2. AgentTrustProfile: Advisor
3. minTrustLevel check: Observer <= Advisor ✓  (floor is Observer, agent is Advisor)
4. maxAutonomyLevel: Supervised → cap effective level at Supervised
5. AgentGraduationPolicy: Supervised → requiresHumanApproval: true
   → route to Pending phase
6. OpsLock acquired for gpu-pool.yaml
7. SafetyPolicy: agentIdentity == "karpenter-nodepool-agent" ✓
```

**Request status:**

```yaml
status:
  phase: Pending    # waiting for human approval
```

SRE reviews on the dashboard, sees the diagnosis, approves. The agent receives the
approval, opens the GitHub PR:

```
PR: increase gpu-pool maxNodes from 10 to 20
File: clusters/prod/karpenter/gpu-pool.yaml
```

Agent patches the request status to `Executing` with the PR number. PR is reviewed,
merged. Agent patches status to `Completed`.

```yaml
status:
  phase: Completed
  executionEvidence:
    prNumber: 4821
    repository: myorg/infra
    mergedAt: "2026-05-05T16:30:00Z"
```

**AgentTrustProfile after first successful execution:**

```yaml
status:
  trustLevel: Advisor
  successRate: 1.0             # 1/1 executions succeeded
  totalExecutions: 1
  nextLevelRequirements:
    level: Supervised
    remaining:
      minExecutions: 19        # needs 19 more
```

The agent continues operating for another month. It handles 25 execution requests.
3 are denied by SREs (two false positives, one timing issue). 22 succeed.

**After 25 executions and 20 total graded requests:**

```yaml
status:
  trustLevel: Supervised       # 0.86 >= 0.85, 20 verdicts, 22 executions >= 20 ✓
  diagnosticAccuracy: 0.86
  totalObserveVerdicts: 20
  successRate: 0.88            # 22/25
  totalExecutions: 25
  nextLevelRequirements:
    level: Trusted
    remaining:
      minObserveVerdicts: 30
      minDiagnosticAccuracy: 0.06
      minExecutions: 25
```

---

### Month 4–6: Supervised level — still human approval

The agent is `Supervised`. The `GovernedResource.maxAutonomyLevel` is `Supervised`.

Even if the agent eventually reaches `Trusted` in the graduation policy, the
`maxAutonomyLevel` ceiling on this resource keeps it at `Supervised` behavior —
human approval required for every nodepool change.

This is intentional. Nodepool changes affect cluster capacity and cost. The cluster
admin set the ceiling. An SRE is always in the loop.

**After 6 months, the agent reaches Trusted** (50+ verdicts, 0.93 accuracy, 55 executions,
0.96 success rate). The controller updates the profile:

```yaml
status:
  trustLevel: Trusted
  diagnosticAccuracy: 0.93
  totalObserveVerdicts: 52
  successRate: 0.96
  totalExecutions: 55
```

But requests still route through human approval because `maxAutonomyLevel: Supervised`.

---

### Raising the ceiling — cluster admin decision

After reviewing 6 months of history, the cluster admin decides the agent has earned the
right to open PRs without human review during business hours. They update the
`GovernedResource`:

```yaml
spec:
  uriPattern: "github://myorg/infra/files/main/clusters/*/karpenter/**"
  permittedActions:
    - update
  trustRequirements:
    minTrustLevel: Observer
    maxAutonomyLevel: Trusted    # raised from Supervised
```

They also tighten the SafetyPolicy to compensate for removing the human:

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: SafetyPolicy
metadata:
  name: nodepool-guardrails
  namespace: production
spec:
  targetGovernedResource: karpenter-nodepools
  rule: |
    request.agentIdentity == "karpenter-nodepool-agent"
    && request.action == "update"
    && !request.target.uri.contains("clusters/prod/karpenter/system-pool.yaml")
```

The `system-pool.yaml` exclusion is explicit: the system pool is too sensitive for
autonomous changes regardless of trust level. The SafetyPolicy enforces this.

**Now a Trusted agent request routes as:**

```
1. GovernedResource match: karpenter-nodepools ✓
2. AgentTrustProfile: Trusted
3. minTrustLevel: Observer <= Trusted ✓
4. maxAutonomyLevel: Trusted — no downgrade applied
5. AgentGraduationPolicy: Trusted → requiresHumanApproval: false
   → proceed to SafetyPolicy
6. SafetyPolicy: identity check ✓, action check ✓, not system-pool ✓
   → Approved automatically
7. OpsLock acquired. Agent proceeds to open PR without waiting for human.
```

---

## How DiagnosticAccuracySummary feeds AgentTrustProfile

The `AgentTrustProfile` controller watches two sources:

**Source 1 — graded Observer requests** → `DiagnosticAccuracySummary`

Every time a verdict is submitted, the gateway:
1. Persists the verdict on the `AgentRequest` status
2. Upserts the `DiagnosticAccuracySummary` CR for that `agentIdentity`:
   - First verdict: increment `totalReviewed` and the verdict counter
   - Changed verdict: decrement old counter, increment new counter, `totalReviewed` unchanged
3. Recomputes `diagnosticAccuracy = (correct + 0.5 × partial) / totalReviewed`

**Source 2 — terminal AgentRequest transitions** → `successRate`

Every time an Advisor+ request reaches a terminal phase (`Completed`, `Failed`, `Denied`),
the controller increments the execution counters and recomputes `successRate`.

**The controller reconciles `AgentTrustProfile` whenever either source changes:**

```
DiagnosticAccuracySummary updated
        OR
AgentRequest reaches terminal phase
        ↓
AgentTrustProfile controller reconciles:
  1. Read DiagnosticAccuracySummary for this agentIdentity
  2. Count terminal AgentRequests for this agentIdentity
  3. Evaluate against AgentGraduationPolicy thresholds
  4. Compute trustLevel, nextLevelRequirements
  5. Patch AgentTrustProfile status
```

The join key is `agentIdentity` — the same string in `AgentRequest.spec.agentIdentity`,
`DiagnosticAccuracySummary.spec.agentIdentity`, and `AgentTrustProfile.metadata.name`.

---

## Summary of resources created for this example

| Resource | Kind | Who creates it |
|---|---|---|
| `karpenter-nodepools` | `GovernedResource` | Cluster admin |
| `cluster-default` | `AgentGraduationPolicy` | Cluster admin (once at setup) |
| `nodepool-guardrails` | `SafetyPolicy` | Platform engineer |
| `karpenter-nodepool-agent` | `AgentTrustProfile` | Controller (automatic) |
| `karpenter-nodepool-agent` | `DiagnosticAccuracySummary` | Gateway (automatic on first verdict) |
| `nodepool-req-*` | `AgentRequest` | Agent |

The agent creates nothing except `AgentRequest`. Everything else is either admin
configuration or control plane bookkeeping.
