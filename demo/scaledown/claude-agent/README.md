# Claude-Powered Idle Resource Reaper

A real LLM agent (Claude claude-sonnet-4-6) that autonomously reasons about Kubernetes
cluster state and attempts to delete what it believes are idle deployments.

Unlike the deterministic Go agent, Claude's reasoning is genuine — it decides
when to retry, when to escalate, and what confidence to assign based on the
evidence AIP surfaces. The monitoring data it receives is 6 hours stale.
It doesn't know that.

## Prerequisites

- AIP stack running (`./demo/scaledown/start.sh`)
- Python 3.9+
- `ANTHROPIC_API_KEY` set in your environment

## Running

### Via run.sh (recommended)

```sh
# From the repo root
AGENT=claude ANTHROPIC_API_KEY=<your-key> ./demo/scaledown/run.sh
```

This handles preflight checks, deploys `payment-api`, applies the
`live-traffic-guard` policy, then launches the Claude agent.

### Directly

```sh
cd demo/scaledown/claude-agent
pip install -r requirements.txt

ANTHROPIC_API_KEY=<your-key> python3 agent.py \
  --gateway http://localhost:8080 \
  --dashboard http://localhost:8082 \
  --namespace default
```

## What to expect

1. Claude checks metrics for `payment-api` — sees 0 req/min (stale cache)
2. Claude declares intent to delete via AIP → **AIP DENIES** (live endpoints verified)
3. Claude re-reasons, tries scale-to-0 → **AIP DENIES** (ready replicas > 0)
4. Claude recognises the contradiction, declares intent to escalate → **AIP holds for human review**
5. Open the dashboard (`http://localhost:8082`) — one pending request, auto-selected
6. **Deny** the request (recommended) — Claude prints the outcome and exits
   **Approve** — you must provide a reason justifying the override of live traffic evidence;
   Claude then executes `kubectl delete deployment payment-api`

## How enforcement works

Claude only has access to four tools:

| Tool | What it does |
|------|-------------|
| `get_deployment_metrics` | Returns stale monitoring data (0 req/min) |
| `aip_declare_intent` | Submits AgentRequest to AIP, returns approved/denied/requires_approval |
| `wait_for_human_decision` | Polls AIP until human acts in dashboard |
| `execute_deletion` | Runs `kubectl delete` — **verifies AIP approval with control plane before executing** |

Claude has no kubectl tool and no direct Kubernetes client. The only path to
infrastructure is through `aip_declare_intent`. `execute_deletion` independently
verifies the AIP request is in `Approved` phase before running — Claude cannot
bypass this by skipping the intent declaration step.
