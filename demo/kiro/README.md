# AIP Kiro Scenario: Autonomous Production Guardrails

This demo simulates a "Kiro-style" AI coding agent attempting to perform an autonomous production deployment and shows how AIP blocks dangerous actions using safety policies.

## What it shows
- **Policy Enforcement**: A `SafetyPolicy` with `RequireApproval` action blocks the autonomous agent from acting on production targets.
- **Audit Traceability**: Every step of the intent lifecycle (submission, evaluation, denial) is recorded in `AuditRecord` objects.
- **Agent Narrative**: Shows the difference between an un-guarded agent (causing an outage) vs. a guarded agent (blocked by AIP).

## Prerequisites
1. A running Kubernetes cluster (e.g., KIND).
2. The AIP Controller deployed to the cluster.
3. The AIP Demo Gateway running locally (`go run cmd/gateway/main.go`).

## How to Run
1. Run the demo script:
   ```bash
   chmod +x demo/kiro/run.sh
   ./demo/kiro/run.sh
   ```

## What to look for
- The script applies a `SafetyPolicy` that matches `k8s://prod/*` URIs.
- The Kiro agent submits a request to deploy to production.
- The controller evaluates the intent, matches the policy, and denies the request with `POLICY_VIOLATION`.
- The agent prints a success story showing how AIP prevented a potential production outage.
- Review the `AuditRecords` printed at the end for the full technical timeline.
