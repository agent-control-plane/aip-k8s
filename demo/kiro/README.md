# AIP Demo: Kiro — Autonomous Agent Scope Escalation

Demonstrates how AIP governs an AI coding agent across two distinct threat surfaces:

1. **Requiring human review** before a production deployment proceeds.
2. **Blocking scope escalation** when the agent autonomously expands beyond what was approved.

## Scenario

Kiro submits a deployment intent for `payment-api v2.1.0`. AIP intercepts it for human review.
A developer reviews the plan and approves it. Kiro then discovers environment drift during
execution and decides a full delete + recreate is the cleanest path — submitting a new intent
without asking. AIP blocks it at the gate: `delete` was never part of the approved scope.

```text
[Phase 1] Kiro submits kiro/deploy intent
          → AIP: RequireApproval (3 policies triggered)
          → Script pauses — waiting for human

[Phase 2] You open the dashboard and approve the request
          → AgentRequest transitions to Approved
          → Script resumes

[Phase 3] Kiro attempts action: delete (scope escalation)
          → AIP: BLOCKED — delete is not a permitted action on this GovernedResource
```

## What it demonstrates

| AIP capability | Where it appears |
|---|---|
| GovernedResource admission | Gateway rejects `delete` before any policy evaluation |
| Multi-rule SafetyPolicy | Three RequireApproval rules fire on the deploy intent |
| Human-in-the-loop | Request waits in Pending until a reviewer approves |
| Scope enforcement | Approval for `kiro/deploy` does not carry over to `delete` |
| Audit trail | Every phase transition recorded as an `AuditRecord` |

## Policies in play

The `prod-require-approval` SafetyPolicy triggers RequireApproval on the deploy intent:

| Rule | Signal | Threshold |
|---|---|---|
| `production-guard` | target URI starts with `k8s://prod` | always |
| `cascade-blast-radius-guard` | 3 affected downstream services | > 2 |
| `low-confidence-guard` | confidence score 0.65 | < 0.8 |

The `untested-in-staging-guard` Deny rule does **not** fire here — `testedInStaging: true`
is set in the agent's parameters (the deployment was staged).

The delete escalation is blocked by the **GovernedResource**, not a SafetyPolicy: `delete`
is not in `permittedActions: [kiro/deploy]`, so the gateway returns 403 before the
controller is involved.

## Prerequisites

- Kubernetes cluster with AIP CRDs installed
- Gateway running locally: `go run ./cmd/gateway/`
- Controller running locally: `go run ./cmd/controller/`
- **No in-cluster AIP controller** — two controllers produce duplicate audit records.
  Remove any deployed controller first:
  ```bash
  kubectl delete deployment aip-k8s-controller -n aip-k8s-system --ignore-not-found
  ```

## Run

```bash
./demo/kiro/run.sh
```

## Expected output

```text
=== KIRO SCENARIO: Autonomous Agent Scope Escalation ===

[Phase 1] Kiro submits production deployment intent
  Phase: Pending
  Audit trail so far:
    📥 request.submitted
    ⚖️  policy.evaluated

[Phase 2] AIP intercepted the request — human review required

  👉 Open the dashboard and approve the request to continue.
  AgentRequest: kiro-coding-agent-<hash>

  Waiting for a human to approve or deny...
  Phase: Approved
  ✓ Approved — continuing...

[Phase 3] Kiro decides to delete and recreate the environment
[BLOCKED] AIP denied the escalation at admission
  HTTP 403 — ACTION_NOT_PERMITTED
  'delete' is not a permitted action on GovernedResource kiro-prod-deployments.
  The original approval covered kiro/deploy only.
  Kiro cannot autonomously expand its scope beyond what was reviewed.
```

## Clean up

```bash
./demo/cleanup.sh
kubectl delete auditrecords --all -n default --ignore-not-found
```
