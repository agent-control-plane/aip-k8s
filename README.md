# Agent Intent Protocol (AIP) Kubernetes Control Plane

## Description
`aip-k8s` is a Kubernetes-native Control Plane implementation of the [Agent Intent Protocol (AIP)](https://github.com/ravisantoshgudimetla/agent-intent-protocol).

AIP is an open standard designed to govern autonomous AI agents interacting with critical infrastructure. By requiring agents to declare their intents as cryptographic `AgentRequests` *before* action, this control plane provides strict mutual exclusion (via locking), policy-based governance (via CEL rules), and irrefutable audit trails (via immutable `AuditRecords`).

This repository contains the `governance.aip.io` controller, which serves as the core authority for evaluating and approving AI agent operations across a Kubernetes cluster.

### Core APIs
- **AgentRequest**: The primary CRD agents create to request mutating actions on infrastructure.
- **SafetyPolicy**: CEL-based rules defined by administrators to govern which agents can perform what actions.
- **AuditRecord**: Immutable event logs generated on every state transition of an AgentRequest.
- **AgentDiagnostic**: Agent-written, immutable records of observations and diagnoses made before acting. No controller involved — agents write directly. Designed for stateless k8s controller-based agents that need to persist diagnostic state without misusing `AgentRequest`. See [`ep/agent_diagnostic_design.md`](ep/agent_diagnostic_design.md).

## Why AIP? (Real-World Validation)

Traditional "black-box" AI agents can fail catastrophically when interacting with production systems. Recent high-profile incidents (like the [2.5-year data wipe at DataTalks.Club](https://alexeyondata.substack.com/p/how-i-dropped-our-production-database)) highlight the need for AIP:

*   **The Problem**: An AI agent, trying to be "helpful," executed `terraform destroy` on a production state file it mistakenly unarchived, wiping the entire database and all backups.
*   **How AIP Prevents This**:
    *   **Blast Radius Declaration**: The agent would have been forced to declare all `AffectedTargets` (Database, VPC, LB) in its `AgentRequest`. A human reviewer would instantly see that a "cleanup" task was actually targeting production.
    *   **Reasoning Traces**: AIP requires agents to expose their internal logic. The agent would have had to declare: *"I am destroying resources defined in the unarchived production state file to ensure a clean state."*
    *   **Hard Guardrails**: A `SafetyPolicy` can enforce "Manual Approval" for any `delete` or `destroy` actions on production URIs, ensuring a human line-of-defense.

**See it in action**: The [`demo/scaledown`](demo/scaledown/) scenario runs this exact failure mode against a live Kubernetes cluster. An idle-resource-reaper agent — operating on 6-hour-stale metrics — attempts to delete `payment-api` (and cascade-delete `payment-worker` and `payment-db`). The AIP control plane independently verifies live endpoints and blocks every attempt before a single byte of infrastructure is touched.

## Demos

| Demo | What it shows |
|------|--------------|
| [`demo/scaledown`](demo/scaledown/) | **DataTalks incident, reproduced and prevented.** An idle-resource-reaper agent tries to delete a production service it misclassifies as unused (stale monitoring data). AIP independently verifies live traffic and blocks the deletion. ReACT loop: `delete` → `scale-to-0` → human escalation. |
| [`demo/opslock`](demo/opslock/) | Two concurrent agents attempt conflicting operations on the same resource. OpsLock mutual exclusion ensures only one proceeds; the other receives `LOCK_CONTENTION`. |
| [`demo/kiro`](demo/kiro/) | An autonomous deployment agent is blocked by a `RequireApproval` policy on production targets, triggering the human-in-the-loop escalation path with a full audit trail. |

### The Scenario in 60 Seconds

![AIP scaledown demo](demo/scaledown/demo.gif)

### Running the scaledown demo

```sh
# Start the full stack (controller, gateway, dashboard)
./demo/scaledown/start.sh

# In a second terminal — run the agent scenario
./demo/scaledown/run.sh
```

The agent will attempt to delete `payment-api` twice (direct delete, then scale-to-0), be blocked both times, and escalate to the dashboard for human review. Open the dashboard URL printed by `start.sh` to see what AIP independently verified versus what the agent declared, then deny the request.

## Getting Started

### Prerequisites
- `go` version v1.22.0+
- `docker` version 17.03+.
- `kind` version v0.31.0+ (for local testing).
- `kubectl` version v1.11.3+.

### Running Locally (Development in KIND)
You can automatically spin up a local Kubernetes cluster using `kind` and deploy the `aip-k8s` controller directly to it for integration testing using our provided Makefile targets:

```sh
# This will: 
# 1. Create a local 'aip-test' kind cluster (if it doesn't exist)
# 2. Build the 'aip-controller:test' docker image
# 3. Load the image into the cluster
# 4. Generate & apply all CRDs
# 5. Deploy the controller to the cluster
make kind-deploy IMG=aip-controller:test
```

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/aip-k8s:tag
```

**Install the CRDs and deploy the Manager:**
```sh
make deploy IMG=<some-registry>/aip-k8s:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin privileges.

## Testing
This project uses `envtest` for rapid integration testing without a full cluster.
```sh
make test
```

## OSS Scope and Known Limitations

This repository implements the AIP Core conformance tier. We are launching this as an OSS MVP to gather early community feedback on the core Agent Intent Protocol design.

The following capabilities defined in the AIP specification are intentionally deferred in this MVP:

| Capability | Tier | Why it matters |
|------------|------|---------------|
| **Transport-layer Identity Verification** (spec §6) | **Core** | Missing `MutatingAdmissionWebhook` to extract and enforce `agentIdentity` from the K8s ServiceAccount. Currently, agents self-declare their identity. |
| **Hard API Enforcement** | **Core** | Missing `ValidatingAdmissionWebhook` to physically block raw K8s mutations. Safety currently relies on agents voluntarily using the AIP gateway. |
| **CalibrationEvidence verification** (spec §3.1.5) | Extended | `confidenceScore` is currently agent-self-reported rather than cryptographically verified via a signed evaluator JWT. |
| **TOCTOU protection** (spec §3.6.2) | Extended | State can drift between policy evaluation (T1) and human approval (T2). Re-verifying live state via `ForGeneration` binding is deferred. |
| **Approval revocation** (spec §3.6.3) | Extended | If cluster state changes after approval but before execution, a conforming control plane must automatically signal the agent. |
| **AgentTrustProfile** (spec §3.7) | Extended | Per-agent calibration history and measured accuracy tracking is deferred. |

**What this means in practice**: In this MVP, `agentIdentity` and `confidenceScore` are self-reported. Operators writing policies should treat these fields as unverified. However, the primary safety workflow — intent declaration, independent live state evaluation against CEL policies, OpsLocks, and immutable audit trails — is fully functional and ready to be tested!

See `spec.md` for the complete protocol specification.

## Contributing
All new features must conform to the core AIP specification.

**NOTE:** Run `make help` for more information on all potential `make` targets.

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
