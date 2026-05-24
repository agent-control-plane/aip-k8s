# Trust Graduation Demo

A self-running demo that takes an agent from **Observer** to **Autonomous** through
all five AIP trust levels, showing how each level changes what the control plane does
with the agent's requests.

```text
Observer → Advisor → Supervised → Trusted → Autonomous
```

The demo is fully automated — it builds the gateway and controller from source, starts
them as local processes, applies the graduation policy, and runs the agent through all
five phases. No manual controller/gateway startup required.

---

## What you'll see

| Phase | What happens |
|---|---|
| **Observer** | Requests graded but not executed. Agent builds accuracy signal. |
| **Advisor** | Requests queued for human approval before execution. |
| **Supervised** | Same as Advisor — more executions required to advance. |
| **Trusted** | Trust gate auto-approves. No human in the loop. |
| **Autonomous** | Maximum trust. Fully autonomous execution. |

The demo simulates the reviewer role (approving Advisor/Supervised requests) because
the gateway runs in **open mode** — no auth flags set. This is how all AIP demos run
locally. In a real deployment, a human reviewer (or a separate reviewer service) would
approve those requests.

---

## Prerequisites

- A Kubernetes cluster accessible via `kubectl` (local Kind, minikube, or remote)
- `kubectl` configured and pointing to your cluster
- `go` (1.25+) and `curl` in your PATH
- Ports **8080** (gateway) and **8081** (controller probe) available locally

Optional: if you already have AIP installed via Helm in your cluster, the demo will
scale those deployments to 0 to avoid port conflicts, then restore them on exit.

---

## Run the demo

```bash
./demo/graduation/run.sh
```

### What `run.sh` does

1. **Checks prerequisites** — verifies `kubectl`, `go`, `curl`, and a reachable cluster
2. **Installs CRDs** — applies AIP CRDs from `charts/aip-k8s/crds/`
3. **Clears port conflicts** — kills any local processes on ports 8080/8081, and scales
   in-cluster AIP deployments to 0 replicas
4. **Applies demo resources** — installs the `AgentGraduationPolicy` and `GovernedResource`
5. **Wipes previous state** — deletes old `AgentRequest`s, `AgentTrustProfile`s,
   `DiagnosticAccuracySummary`s, `AuditRecord`s, and stale `Lease`s so the agent starts
   fresh at Observer
6. **Builds binaries** — compiles the gateway and controller from source into `./bin/`
7. **Starts controller** — runs locally on probe port 8081
8. **Starts gateway** — runs locally in open mode on port 8080
9. **Runs the agent** — submits requests, grades them correct, and graduates through all levels
10. **Post-run summary** — prints the last 15 audit records
11. **Cleanup on exit** — kills the local controller/gateway, restores in-cluster deployments to 1 replica

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `GATEWAY_PORT` | `8080` | Local port for the gateway |
| `CTL_PROBE_PORT` | `8081` | Local port for the controller health probe |
| `NAMESPACE` | `default` | Kubernetes namespace for demo resources |
| `HELM_NAMESPACE` | `aip-k8s-system` | Namespace where AIP is Helm-installed (for scaling down) |
| `HELM_RELEASE` | `aip-k8s` | Helm release name (for scaling down) |

Example with custom ports:
```bash
GATEWAY_PORT=9090 CTL_PROBE_PORT=9091 ./demo/graduation/run.sh
```

The demo takes **3–5 minutes** depending on controller reconcile speed.

---

## What the demo applies

`run.sh` applies two cluster resources before starting the agent:

**`k8s/policy.yaml`** — `AgentGraduationPolicy` named `default` with compressed
thresholds (the gateway always looks up the policy named `default`):

| Level | Accuracy threshold | Executions required |
|---|---|---|
| Observer | — | — |
| Advisor | ≥ 0.70 | — |
| Supervised | ≥ 0.80 | 3 |
| Trusted | ≥ 0.90 | 6 |
| Autonomous | ≥ 0.95 | 10 |

**`k8s/resource.yaml`** — `GovernedResource` for `k8s://demo/default/deployment/*`
with `minTrustLevel: Observer` and `maxAutonomyLevel: Autonomous`, allowing the full
ladder to operate.

---

## Inspect the results

**Trust profile after the demo:**
```bash
kubectl get agenttrustprofiles -n default
kubectl describe agenttrustprofile <name> -n default
```

**Full audit trail:**
```bash
kubectl get auditrecords -n default --sort-by=.spec.timestamp
```

**Trust level changes only:**
```bash
kubectl get auditrecords -n default \
  --field-selector spec.event=agent.trustprofile.updated
```

---

## Clean up

The demo cleans up its local processes automatically on exit (via `trap cleanup EXIT`).

To clean up Kubernetes resources:

```bash
./demo/cleanup.sh default
```

Or manually:
```bash
kubectl delete agentrequests,auditrecords,agenttrustprofiles,diagnosticaccuracysummaries -n default --all
kubectl delete governedresource demo-deployments
kubectl delete agentgraduationpolicy default
kubectl delete leases -l governance.aip.io/managed-by=aip-controller -n default
```
