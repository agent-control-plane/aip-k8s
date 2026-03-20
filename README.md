# Agent Intent Protocol (AIP) Kubernetes Control Plane

## Description
`aip-k8s` is a Kubernetes-native Control Plane implementation of the [Agent Intent Protocol (AIP)](https://aip.io).

AIP is an open standard designed to govern autonomous AI agents interacting with critical infrastructure. By requiring agents to declare their intents as cryptographic `AgentRequests` *before* action, this control plane provides strict mutual exclusion (via locking), policy-based governance (via CEL rules), and irrefutable audit trails (via immutable `AuditRecords`).

This repository contains the `governance.aip.io` controller, which serves as the core authority for evaluating and approving AI agent operations across a Kubernetes cluster.

### Core APIs
- **AgentRequest**: The primary CRD agents create to request mutating actions on infrastructure.
- **SafetyPolicy**: CEL-based rules defined by administrators to govern which agents can perform what actions.
- **AuditRecord**: Immutable event logs generated on every state transition of an AgentRequest.

## Why AIP? (Real-World Validation)

Traditional "black-box" AI agents can fail catastrophically when interacting with production systems. Recent high-profile incidents (like the [2.5-year data wipe at DataTalks.Club](https://alexeyondata.substack.com/p/how-i-dropped-our-production-database)) highlight the need for AIP:

*   **The Problem**: An AI agent, trying to be "helpful," executed `terraform destroy` on a production state file it mistakenly unarchived, wiping the entire database and all backups.
*   **How AIP Prevents This**:
    *   **Blast Radius Declaration**: The agent would have been forced to declare all `AffectedTargets` (Database, VPC, LB) in its `AgentRequest`. A human reviewer would instantly see that a "cleanup" task was actually targeting production.
    *   **Reasoning Traces**: AIP requires agents to expose their internal logic. The agent would have had to declare: *"I am destroying resources defined in the unarchived production state file to ensure a clean state."*
    *   **Hard Guardrails**: A `SafetyPolicy` can enforce "Manual Approval" for any `delete` or `destroy` actions on production URIs, ensuring a human line-of-defense.

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

## Contributing
Please see `control_plane_implementation.md` and `implementation_phases.md` for our current architectural state and roadmap. All new features must conform to the core AIP specification. 

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
