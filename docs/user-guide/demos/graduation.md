# Trust Graduation Demo

The trust graduation demo shows an agent progressing through all five AIP trust levels — Observer → Advisor → Supervised → Trusted → Autonomous — by building an accuracy signal through human-graded verdicts.

## What it demonstrates

1. **Observer** — Requests are graded but not executed. No action is taken.
2. **Advisor** — Requests require human approval before execution.
3. **Supervised** — Still requires human approval, but needs more executions to advance.
4. **Trusted** — Auto-approved by the trust gate if policy passes.
5. **Autonomous** — Maximum trust. Fully autonomous execution.

## Prerequisites

- AIP installed in dev mode (see [Quick Start](../../quick-start.md))
- Kubernetes cluster accessible via `kubectl`
- `go`, `curl`, and `kubectl` in PATH

## Run the demo

```bash
./demo/graduation/run.sh
```

This script will:
1. Build the gateway and controller binaries locally
2. Start both as background processes
3. Apply the demo `AgentGraduationPolicy` and `GovernedResource`
4. Run the graduation agent, which submits requests and grades them correct
5. Wait for the agent to progress through all five trust levels

The demo takes **3–5 minutes** depending on controller reconcile speed.

## What happens

### Phase 1: Observer
The agent submits its first request. The gateway routes it to `AwaitingVerdict` because the agent has no trust profile yet. The demo grades the request `correct`. The controller computes accuracy and promotes the agent to **Advisor**.

### Phase 2: Advisor
The agent submits 3 requests that require human approval. In dev mode (open auth), the demo simulates the reviewer by calling the approval endpoint. After 3 approved executions, the controller promotes the agent to **Supervised**.

### Phase 3: Supervised
3 more approved executions. The agent is promoted to **Trusted**.

### Phase 4: Trusted
4 auto-approved executions (no human in the loop). The trust gate auto-approves because the agent's level meets the policy threshold. The agent is promoted to **Autonomous**.

### Phase 5: Autonomous
2 final executions demonstrating full autonomy.

## Expected output

```text
🎉 Graduation complete!

The agent progressed through all five trust levels:
🔭 Observer → 📝 Advisor → 🛡️ Supervised → ⚡ Trusted → 🤖 Autonomous
```

## Inspect the results

After the demo completes:

```bash
# View the final trust profile
kubectl get agenttrustprofile -n default

# Review the full audit trail
kubectl get auditrecords -n default --sort-by=.spec.timestamp

# See only trust level changes
kubectl get auditrecords -n default \
  --field-selector spec.event=agent.trustprofile.updated
```

## Technical details

The demo uses an `AgentGraduationPolicy` with compressed thresholds for speed:

| Level | Accuracy threshold | Executions required |
|---|---|---|
| Observer | — | — |
| Advisor | ≥ 0.70 | — |
| Supervised | ≥ 0.80 | 3 |
| Trusted | ≥ 0.90 | 6 |
| Autonomous | ≥ 0.95 | 10 |

The `GovernedResource` allows the full trust ladder with `minTrustLevel: Observer` and `maxAutonomyLevel: Autonomous`.

For the agent implementation and demo manifests, see:
- [`demo/graduation/README.md`](https://github.com/agent-control-plane/aip-k8s/tree/main/demo/graduation)
- [`demo/graduation/k8s/policy.yaml`](https://github.com/agent-control-plane/aip-k8s/tree/main/demo/graduation/k8s)

## Clean up

```bash
./demo/cleanup.sh default
```

This deletes all demo resources including AgentRequests, AgentTrustProfiles, DiagnosticAccuracySummaries, and the demo GovernedResource.

## Learn more

- [Agent Graduation Ladder](../../agent-graduation-ladder.md) — Full conceptual guide
- [Trust Gate](../../trust-gate.md) — How the gateway enforces trust levels
