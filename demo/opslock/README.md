# AIP OpsLock Demo: Distributed Concurrency Control

This demo demonstrates how the AIP Control Plane prevents multiple autonomous AI agents from conflicting with each other when acting on the same infrastructure target.

## What it shows
- **Mutual Exclusion**: Only one agent can acquire the "OpsLock" (Kubernetes Lease) for a specific target URI at a time.
- **Contention Handling**: The second agent to request a lock is denied with a `LOCK_CONTENTION` or `LOCK_TIMEOUT` error.
- **Deterministic Locking**: Locks are named based on the target URI hash, ensuring global consistency across different agent identities.

## Prerequisites
1. A running Kubernetes cluster (e.g., KIND).
2. The AIP Controller deployed to the cluster.
3. The AIP Demo Gateway running locally (`go run demo/gateway/main.go`).

## How to Run
1. Open a terminal and watch the AgentRequests:
   ```bash
   kubectl get agentrequests -w
   ```
2. Run the demo script:
   ```bash
   chmod +x demo/opslock/run.sh
   ./demo/opslock/run.sh
   ```

## What to look for
- Both `agent-a` and `agent-b` will submit requests simultaneously.
- One agent will be transitioned to `Approved` and acquire the lock.
- The other agent will remain `Pending` (requeuing) and eventually be `Denied` once the first agent starts executing or the timeout is reached.
- Check the gateway logs to see the concurrency checks in action.
