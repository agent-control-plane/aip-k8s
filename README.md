# Agent Intent Protocol (AIP) Kubernetes Control Plane

## Description
`aip-k8s` is a Kubernetes-native Control Plane implementation of the [Agent Intent Protocol (AIP)](https://github.com/ravisantoshgudimetla/agent-intent-protocol).

AIP is an open standard designed to govern autonomous AI agents interacting with critical infrastructure. By requiring agents to declare their intents as cryptographic `AgentRequests` *before* action, this control plane provides strict mutual exclusion (via locking), policy-based governance (via CEL rules), and irrefutable audit trails (via immutable `AuditRecords`).

This repository contains the `governance.aip.io` controller, which serves as the core authority for evaluating and approving AI agent operations across a Kubernetes cluster.

### Core APIs
- **AgentRequest**: The primary CRD agents create to request mutating actions on infrastructure.
- **GovernedResource**: Platform engineering declares which resource types agents may mutate, which agent identities may target them, and which context fetcher to invoke. Requests targeting unregistered resource types are rejected at admission. See [`docs/governed-resources.md`](docs/governed-resources.md).
- **SafetyPolicy**: CEL-based rules defined by administrators to govern which agents can perform what actions. Binds to `GovernedResource` objects via `governedResourceSelector`.
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

### Install via Helm (Recommended)

The quickest way to get the full AIP stack (controller + gateway + dashboard) running on any Kubernetes cluster:

```sh
helm install aip-k8s \
  oci://ghcr.io/ravisantoshgudimetla/aip-k8s/charts/aip-k8s \
  --version 0.1.0 \
  --namespace aip-k8s-system \
  --create-namespace
```

This single command installs CRDs, the governance controller, the gateway (port 8080), and the dashboard (port 8082). No separate `kubectl apply` for CRDs is needed — Helm hooks handle install and upgrade automatically.

**Verify the installation:**

```sh
kubectl get pods -n aip-k8s-system
# NAME                                          READY   STATUS    RESTARTS
# aip-k8s-controller-manager-...               1/1     Running   0
# aip-k8s-gateway-...                          1/1     Running   0
# aip-k8s-dashboard-...                        1/1     Running   0
```

**Access the gateway and dashboard:**

```sh
kubectl port-forward -n aip-k8s-system svc/aip-k8s-gateway 8080:8080 &
kubectl port-forward -n aip-k8s-system svc/aip-k8s-dashboard 8082:8082 &

curl http://localhost:8080/healthz   # → ok
curl http://localhost:8082/healthz   # → ok
```

### Prerequisites (for local development)
- `go` version v1.24.0+
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

## Gateway API

The AIP gateway (`cmd/gateway`) is the HTTP interface for agent clients. It runs as a standalone binary alongside the controller and translates HTTP requests into Kubernetes CRD operations — agents do not need `kubectl` or a kubeconfig.

### Starting the gateway

```sh
# Build
make build-gateway

# Run locally (uses ~/.kube/config by default)
./bin/gateway --addr :8080
```

### Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/agent-requests` | Submit an AgentRequest. Blocks (up to 90s) until the control plane reaches a terminal phase, then returns the result. |
| `GET` | `/agent-requests/{name}` | Poll an AgentRequest by name. |
| `POST` | `/agent-requests/{name}/executing` | Signal that the agent has started executing an approved request. Advances the phase to `Executing`. |
| `POST` | `/agent-requests/{name}/completed` | Signal that the agent successfully completed the action. Advances the phase to `Completed`. |
| `POST` | `/agent-diagnostics` | Record an agent observation before acting. Returns the generated resource name and the normalized label values written to Kubernetes. |
| `GET` | `/agent-diagnostics/{name}` | Retrieve a diagnostic record by name. |

All endpoints accept and return `application/json`. Pass `?namespace=<ns>` to target a namespace other than `default`.

### Example: record a diagnostic, then submit an intent

```sh
# 1. Record what the agent observed
curl -s -X POST http://localhost:8080/agent-diagnostics \
  -H "Content-Type: application/json" \
  -d '{
    "agentIdentity": "sre-agent-v1",
    "diagnosticType": "diagnosis",
    "correlationID": "incident-abc123",
    "summary": "OOMKilled on payment-api (3x in 10 min). Recommending restart.",
    "namespace": "production"
  }'
# Response includes the generated name and the exact label values stored,
# so you can use them in label-selector queries without guessing normalization:
# { "name": "diag-sre-agent-v1-x7k9p", "labels": { "aip.io/correlationID": "incident-abc123", ... } }

# 2. Submit the intent — pass the same correlationID so the gateway stamps
#    aip.io/correlationID on the AgentRequest label, linking both resources.
curl -s -X POST http://localhost:8080/agent-requests \
  -H "Content-Type: application/json" \
  -d '{
    "agentIdentity": "sre-agent-v1",
    "action": "restart",
    "targetURI": "k8s://production/deployment/payment-api",
    "reason": "OOMKilled 3 times in 10 minutes. Diagnostic: diag-sre-agent-v1-x7k9p",
    "correlationID": "incident-abc123",
    "namespace": "production"
  }'
```

### Querying the full incident chain

When a `correlationID` is supplied to both `POST /agent-diagnostics` and `POST /agent-requests`, the gateway stamps `aip.io/correlationID` on both resources as a label. The controller automatically propagates that label to every `AuditRecord` emitted for the request. Retrieve the complete chain with a single command:

```sh
kubectl get agentdiagnostics,agentrequests,auditrecords \
  -n production \
  -l aip.io/correlationID=incident-abc123 \
  --sort-by=.metadata.creationTimestamp
```

### Gateway Authentication

The gateway supports OIDC/JWT authentication. When enabled, every non-healthz request must carry a valid `Authorization: Bearer <token>` header. When disabled (default), the gateway falls back to proxy headers (`X-Remote-User` / `X-Forwarded-User`) injected by an upstream authenticating proxy.

#### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--oidc-issuer-url` | `""` | OIDC provider URL (e.g. `https://accounts.google.com`). When set, Bearer token validation is required. When unset, auth is disabled (dev/test only). |
| `--oidc-audience` | `aip-gateway` | Expected JWT `aud` claim. |
| `--oidc-identity-claim` | `sub` | JWT claim used as the agent identity (e.g. `sub`, `azp`, `email`). |
| `--agent-subjects` | `""` | Comma-separated JWT `sub` values permitted to create requests, record diagnostics, and transition state. Setting either `--agent-subjects` or `--reviewer-subjects` enables enforcement for **both** roles — open mode (any caller permitted) only applies when OIDC is unset and **both** allowlists are empty. When subjects are set without `--oidc-issuer-url`, `--trusted-proxy-cidrs` must also be set or the gateway will refuse to start. |
| `--reviewer-subjects` | `""` | Comma-separated JWT `sub` values permitted to approve/deny requests and write verdicts. See `--agent-subjects` for open-mode and enforcement semantics. |
| `--admin-subjects` | `""` | Comma-separated JWT `sub` values permitted to create, update, and delete `GovernedResource` and `SafetyPolicy` objects. |
| `--admin-groups` | `""` | Comma-separated JWT group claim values that grant admin role. Alternative to `--admin-subjects` for group-based identity providers. |
| `--require-governed-resource` | `false` | When `true`, every `AgentRequest` must match a `GovernedResource` or it is rejected. When `false` (default), the check is skipped if no `GovernedResource` objects exist, preserving backward compatibility. |
| `--trusted-proxy-cidrs` | `""` | CIDRs from which `X-Remote-User`/`X-Forwarded-User` headers are accepted. **Required** when using `--agent-subjects`/`--reviewer-subjects` without `--oidc-issuer-url` (otherwise the gateway refuses to start). When empty and no subjects are configured, any source is trusted (dev/test only). Ignored when `--oidc-issuer-url` is set. |

#### Authorization rules

| Endpoint | Required role |
|---|---|
| `GET /healthz`, `GET /readyz` | None |
| `GET /agent-requests`, `GET /agent-requests/{name}` | Any authenticated |
| `POST /agent-requests` | `agent` |
| `POST /agent-requests/{name}/executing` | `agent` (creator only) |
| `POST /agent-requests/{name}/completed` | `agent` (creator only) |
| `POST /agent-requests/{name}/approve` | `reviewer` (non-self-approval enforced) |
| `POST /agent-requests/{name}/deny` | `reviewer` (non-self-approval enforced) |
| `GET /agent-diagnostics`, `GET /agent-diagnostics/{name}` | Any authenticated |
| `POST /agent-diagnostics` | `agent` |
| `PATCH /agent-diagnostics/{name}/status` | `reviewer` |
| `POST /agent-diagnostics/recompute-accuracy` | `reviewer` |
| `GET /diagnostic-accuracy-summaries`, `GET /audit-records` | Any authenticated |
| `POST /governed-resources` | `admin` |
| `GET /governed-resources`, `GET /governed-resources/{name}` | `admin` |
| `PUT /governed-resources/{name}` | `admin` |
| `DELETE /governed-resources/{name}` | `admin` |
| `POST /safety-policies` | `admin` |
| `GET /safety-policies`, `GET /safety-policies/{name}` | `admin` |
| `PUT /safety-policies/{name}` | `admin` |
| `DELETE /safety-policies/{name}` | `admin` |

#### Production setup (Helm)

1. Configure your OIDC provider (Keycloak, Okta, Auth0, Google, etc.) to issue tokens with `aud: aip-gateway`.
2. Install with auth enabled:

```sh
helm upgrade --install aip-k8s \
  oci://ghcr.io/ravisantoshgudimetla/aip-k8s/charts/aip-k8s \
  --namespace aip-k8s-system --create-namespace \
  --set gateway.auth.oidcIssuerURL=https://accounts.google.com \
  --set gateway.auth.agentSubjects=<agent-service-account-sub> \
  --set gateway.auth.reviewerSubjects=sre1@example.com,sre2@example.com
```

3. Agents attach a Bearer token when calling the gateway. When auth is enabled, `agentIdentity` must equal the JWT `sub` claim of the token — the gateway rejects requests where they differ:

```sh
TOKEN=$(gcloud auth print-identity-token --audiences=aip-gateway)
# The agentIdentity value must match the `sub` claim in TOKEN (e.g. the service account email)
curl -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"agentIdentity":"<jwt-sub>","action":"restart","targetURI":"...","reason":"...","namespace":"production"}' \
     http://localhost:8080/agent-requests
```

#### Proxy-header fallback (no OIDC)

When `--oidc-issuer-url` is unset, the gateway reads `X-Remote-User` / `X-Forwarded-User` headers. To prevent header spoofing, restrict which source IPs may supply these headers:

```sh
./bin/gateway \
  --addr :8080 \
  --trusted-proxy-cidrs 10.0.0.0/8,172.16.0.0/12 \
  --agent-subjects sre-agent \
  --reviewer-subjects sre@example.com
```

Requests from outside the trusted CIDRs that supply proxy headers will have those headers ignored. For transport-layer security (mTLS, per-agent `AuthorizationPolicy`, rate limiting), place a service mesh such as Istio in front of the gateway.

## Documentation

| Doc | Description |
|---|---|
| [`docs/governed-resources.md`](docs/governed-resources.md) | Operator guide: creating GovernedResources, context fetchers, SafetyPolicy binding, admin API, schema evolution, deletion protection. |
| [`docs/oidc-keycloak.md`](docs/oidc-keycloak.md) | Step-by-step setup for OIDC authentication using Keycloak (agent, reviewer, and admin identities). |

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
