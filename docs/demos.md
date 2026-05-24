# Demos

| Demo | What it shows |
|------|--------------|
| [`demo/scaledown`](https://github.com/agent-control-plane/aip-k8s/tree/main/demo/scaledown) | **DataTalks incident, reproduced and prevented.** An idle-resource-reaper agent tries to delete a production service it misclassifies as unused (stale monitoring data). AIP independently verifies live traffic and blocks the deletion. ReACT loop: `delete` → `scale-to-0` → human escalation. |
| [`demo/opslock`](https://github.com/agent-control-plane/aip-k8s/tree/main/demo/opslock) | Two concurrent agents attempt conflicting operations on the same resource. OpsLock mutual exclusion ensures only one proceeds; the other receives `LOCK_CONTENTION`. |
| [`demo/kiro`](https://github.com/agent-control-plane/aip-k8s/tree/main/demo/kiro) | An autonomous deployment agent is blocked by a `RequireApproval` policy on production targets, triggering the human-in-the-loop escalation path with a full audit trail. |
| [`demo/graduation`](https://github.com/agent-control-plane/aip-k8s/tree/main/demo/graduation) | **Trust graduation ladder.** An agent progresses from Observer → Advisor → Supervised → Trusted → Autonomous by building an accuracy signal through human-graded verdicts. Demonstrates the full five-level trust progression with compressed thresholds. |

## The Scenario in 60 Seconds

![AIP scaledown demo](https://github.com/agent-control-plane/aip-k8s/raw/main/demo/scaledown/demo.gif)

## Running the scaledown demo

```sh
# Start the full stack (controller, gateway, dashboard)
./demo/scaledown/start.sh

# In a second terminal — run the agent scenario
./demo/scaledown/run.sh
```

The agent will attempt to delete `payment-api` twice (direct delete, then scale-to-0), be blocked both times, and escalate to the dashboard for human review. Open the dashboard URL printed by `start.sh` to see what AIP independently verified versus what the agent declared, then deny the request.

## Why AIP? (Real-World Validation)

Traditional "black-box" AI agents can fail catastrophically when interacting with production systems. Recent high-profile incidents (like the [2.5-year data wipe at DataTalks.Club](https://alexeyondata.substack.com/p/how-i-dropped-our-production-database)) highlight the need for AIP:

*   **The Problem**: An AI agent, trying to be "helpful," executed `terraform destroy` on a production state file it mistakenly unarchived, wiping the entire database and all backups.
*   **How AIP Prevents This**:
    *   **Blast Radius Declaration**: The agent would have been forced to declare all `AffectedTargets` (Database, VPC, LB) in its `AgentRequest`. A human reviewer would instantly see that a "cleanup" task was actually targeting production.
    *   **Reasoning Traces**: AIP requires agents to expose their internal logic. The agent would have had to declare: *"I am destroying resources defined in the unarchived production state file to ensure a clean state."*
    *   **Hard Guardrails**: A `SafetyPolicy` can enforce "Manual Approval" for any `delete` or `destroy` actions on production URIs, ensuring a human line-of-defense.
