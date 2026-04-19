# Agent Intent Protocol (AIP) Specification

**Version**: 0.1.0-draft
**Status**: Proposal

> [!WARNING]
> **Disclaimer**: This document is currently a **Draft Proposal**, not a finalized standard. The abstractions, schemas, and workflows described herein are subject to breaking changes based on initial implementation feedback and community input. It is published to solicit review from practitioners building autonomous infrastructure agents.

> [!NOTE]
> **Independence Notice**: This specification is an independent, personal initiative. It is not affiliated with, endorsed by, or related to any current or former employer of the contributors. All content represents the personal views and efforts of the individual contributors in their personal capacity.

## 1. Introduction

### 1.1 Purpose and Scope
The Agent Intent Protocol (AIP) establishes a standard governance framework requiring autonomous agents to explicitly declare their intentions before executing actions within managed infrastructure environments. AIP decouples the decision-making of individual agents from the safety, stability guarantees, and auditability of the overall system.

**Scope**: AIP governs autonomous agents that operate on infrastructure and platform resources — compute instances, containers, orchestrators, networking primitives, storage systems, databases, and managed services. The protocol is platform-agnostic: it does not prescribe a specific infrastructure platform but assumes the target environment consists of addressable resources with defined lifecycle operations.

AIP does not govern general-purpose AI agent interactions such as conversational agents, code generation agents, or agents whose primary actions are non-infrastructural (e.g., sending emails, browsing the web). Protocols such as MCP (Model Context Protocol) address agent-tool interaction at the application layer; AIP operates at the infrastructure governance layer.

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in RFC 2119.

### 1.2 Motivation

Autonomous infrastructure agents — including LLM-powered SRE agents, auto-remediators, and capacity planners — are increasingly deployed in production environments. These agents make real-time decisions to scale services, restart workloads, modify configurations, and reclaim resources. Without a governance layer, several failure modes emerge:

- **Conflicting actions**: Multiple agents (or agent swarms) independently decide to act on the same resource simultaneously — e.g., one agent scales up a service while another attempts to delete it during a cost-optimization pass.
- **Safety invariant violations**: An agent restarts a stateful workload during an active data migration, or scales down a service below its minimum availability threshold, because it lacks visibility into broader system constraints.
- **Unauditable autonomous actions**: When an agent modifies infrastructure without declaring intent, post-incident forensics cannot distinguish between agent-initiated changes and human-initiated changes, complicating root cause analysis.
- **Cascading failures from uninformed agents**: An agent deletes a parent resource without understanding that dependent resources will be destroyed, taking down an entire service chain.

These are not hypothetical risks. As organizations adopt frameworks such as LangChain, CrewAI, AutoGen, and custom LLM-based operators to manage infrastructure, the need for a standardized intent-declaration and safety-evaluation layer becomes critical.

AIP addresses this by requiring agents to declare intent *before* acting, enabling a control plane to enforce safety policies, manage concurrency, and maintain a complete audit trail of all autonomous infrastructure operations.

### 1.3 Related Work

AIP is designed to complement — not replace — existing infrastructure and agent ecosystem tools:

- **Model Context Protocol (MCP)**: MCP standardizes how AI agents discover and invoke tools. AIP operates at a different layer: MCP governs *how* an agent calls a tool; AIP governs *whether* the agent should be permitted to execute the infrastructure action that tool call implies. An MCP-connected agent that discovers a "delete-instance" tool would still submit an AIP `AgentRequest` before invoking it.
- **Open Policy Agent (OPA) / Gatekeeper**: OPA evaluates policies against structured input. AIP's `SafetyPolicy` abstraction is complementary — implementations SHOULD use OPA/Rego or CEL as the policy evaluation engine behind `SafetyPolicy` rules. AIP adds lifecycle management (intent → approval → execution → completion), concurrency control (`OpsLocks`), and audit trails that OPA alone does not provide.
- **Service Mesh Authorization (e.g., Istio, Linkerd)**: Service meshes govern service-to-service communication. AIP governs agent-to-infrastructure mutation. An agent operating within a service mesh still needs AIP to prevent conflicting infrastructure changes across agents.
- **Platform Admission Control**: Platform-native admission control (e.g., validating webhooks) evaluates requests synchronously at submission time. AIP extends this with asynchronous evaluation of dynamic state (live metrics, log analysis), multi-step coordination, and agent-specific concurrency semantics that admission control was not designed for.
- **Infrastructure as Code (Terraform, Pulumi, Crossplane)**: IaC tools declare desired state and converge. AIP governs the *intent to change* state, not the state itself. An agent that decides to modify a Terraform plan would submit an AIP `AgentRequest` before applying the change.

## 2. Conformance Levels

AIP defines two conformance levels to guide implementers:

### 2.1 Core Conformance (REQUIRED)
An implementation MUST support the following to claim AIP Core conformance:
- `AgentRequest` lifecycle management (all states defined in Section 3.1).
- `SafetyPolicy` evaluation with `FailClosed` semantics.
- `OpsLock` acquisition and automatic lease expiration.
- `AuditRecord` generation for all `AgentRequest` state transitions.
- Agent identity verification via the transport layer.
- The standard `Action` vocabulary defined in Section 3.1.2.

### 2.2 Extended Conformance (OPTIONAL)
An implementation MAY additionally support:
- Multi-step `IntentPlan` coordination (Section 3.5).
- `FailOpen` policy evaluation semantics.
- `RateLimit` rule types.
- `RequireApproval` actions with human-in-the-loop escalation.
- CloudEvents-formatted audit log export.
- Read-optimized lock modes (shared locks).
- Execution monitoring with heartbeats and approval revocation (Section 3.6).
- Custom action extensions (Section 3.1.2).
- Probabilistic reasoning and confidence scoring (Section 3.1.4).
- Confidence verification via externally-signed `CalibrationEvidence` (Section 3.1.5).
- Agent Trust Profiles — control-plane-maintained calibration history per agent identity (Section 3.7).
- Scoped execution mode for dynamic / ReAct-style agents (Section 3.1, `executionMode: scoped`).
- Swarm coordination and delegated authority (Section 3.3.3).
- Pre-execution verification via the `/verify` endpoint — cooperative T2→T3 TOCTOU protection for partner SDKs (Section 4.4).

## 3. Core Abstractions

AIP defines three primary entities and supporting coordination primitives to facilitate intent-based governance. These abstractions are platform-agnostic and MUST be implementable in any control plane architecture (e.g., RESTful APIs, gRPC services, event-driven message buses, platform-native resource APIs).

### 3.1 AgentRequests
An agent MUST submit an `AgentRequest` declaring the operation it intends to perform prior to executing the operation against a target system.

An `AgentRequest` MUST contain:
- **AgentIdentity**: A verifiable identifier for the requesting agent (see Section 6).
- **Action**: The specific operation requested from the standard vocabulary (see Section 3.1.2). REQUIRED when `executionMode: single` (the default). MUST be omitted when `executionMode: scoped` — the permitted actions are declared in `scopeBounds.permittedActions` instead.
- **Target**: A Universal Resource Identifier (URI) or strictly defined schema locating the resource the agent expects to mutate or observe.
- **Reason**: A human or machine-readable justification for the intended action.

An `AgentRequest` MAY contain:
- **IntentPlanRef**: A reference to an `IntentPlan` if this request is part of a multi-step operation (see Section 3.5).
- **Priority**: An integer priority hint (higher values indicate higher priority). The control plane MAY use this for lock contention ordering but MUST NOT allow priority to bypass safety policy evaluation.
- **CascadeModel**: An agent-provided causal model of expected downstream effects (see Section 3.4.2).
- **ReasoningTrace** (Extended Conformance): Information detailing how the agent formulated this intent, including confidence scores and chain-of-thought metadata (see Section 3.1.4).
- **Interruptibility** (Extended Conformance): A boolean flag indicating whether the agent can safely abort the operation if approval is revoked mid-execution. Defaults to `false`.
- **ExecutionMode** (Extended Conformance): `single` (default) or `scoped`. `single` means the agent declares a specific, pre-known action. `scoped` means the agent operates dynamically within declared bounds — for example, a ReAct agent that reasons and acts in a loop and cannot enumerate steps upfront. When `scoped`, `ScopeBounds` MUST also be provided.
- **ScopeBounds** (Extended Conformance, required when `executionMode: scoped`): Defines the operating envelope: permitted action types, permitted target URI patterns, and a time bound. The control plane approves the envelope, not a specific action. Individual actions taken within the scope are still evaluated against `SafetyPolicies`.

> **Design Note:** `ExecutionMode` and `ScopeBounds` exist on `AgentRequest` — not as a separate type — because the lifecycle is identical: submit, approve/deny, complete/fail. The only difference is what the control plane is approving (a specific action vs. a bounded envelope). Adding a new type for this would increase API surface area without adding lifecycle value. Static agents ignore these fields entirely.

An `AgentRequest` lifecycle MUST track the following states:
- `Pending`: Request received, awaiting policy evaluation and lock acquisition.
- `Approved`: Request validated, safe to execute.
- `Denied`: Request violates safety policies or lock cannot be acquired.
- `Executing`: Agent has acknowledged approval and execution is in progress (Extended Conformance, see Section 3.6).
- `Completed`: Agent has signaled successful execution of the approved intent.
- `Failed`: Agent has signaled failure during execution of the approved intent.

#### 3.1.1 Denial Response
When an `AgentRequest` transitions to `Denied`, the control plane MUST return a structured denial containing:
- **Code**: A machine-readable error code from the following taxonomy:
  - `POLICY_VIOLATION`: One or more SafetyPolicy rules triggered a Deny action.
  - `LOCK_CONTENTION`: The target resource is locked by another agent.
  - `LOCK_TIMEOUT`: The request waited for a lock but exceeded the configured timeout.
  - `RATE_LIMITED`: The agent has exceeded its allowed request frequency.
  - `EVALUATION_FAILURE`: A policy dependency (e.g., metrics endpoint) was unreachable and FailClosed applied.
  - `IDENTITY_INVALID`: The AgentIdentity could not be verified.
  - `PLAN_ABORTED`: The parent IntentPlan was aborted due to a step failure.
  - `ACTION_NOT_PERMITTED`: The AgentIdentity is not authorized for the requested Action on the Target.
  - `CASCADE_DENIED`: A safety policy on a transitively affected resource caused the denial.
  - `APPROVAL_REVOKED`: The control plane revoked a previously granted approval (Extended Conformance).
  - `STATE_DRIFTED`: The target resource's state changed between policy evaluation (T1) and human approval (T2), invalidating the evaluation context. The request must be re-submitted or re-evaluated.
  - `SCOPE_TOO_BROAD`: The `scopeBounds.permittedTargetPatterns` would match more resources than the control plane's configured `maxLockedResources` limit, preventing safe lock acquisition (Extended Conformance, see Section 3.3).
  - `GENERATION_MISMATCH`: A `/verify` call referenced an `EvaluationGeneration` that does not match the current generation on the `AgentRequest`, indicating the approval is stale (Extended Conformance, see Section 4.4).
- **Message**: A human-readable explanation.
- **PolicyResults**: An array of individual policy evaluation outcomes, each including the policy name, rule name, result, and reason.
- **RetryAfterSeconds** (OPTIONAL): A hint indicating when the agent MAY retry the request.

#### 3.1.2 Action Vocabulary

AIP defines a standard set of actions for infrastructure operations. Implementations MUST support the standard actions and MAY support custom extensions.

**Standard Actions** (Core Conformance):
| Action    | Semantics                                                        | Mutating |
|-----------|------------------------------------------------------------------|----------|
| `create`  | Provision a new resource.                                        | Yes      |
| `read`    | Observe resource state without modification.                     | No       |
| `update`  | Modify an existing resource's configuration or specification.    | Yes      |
| `delete`  | Remove a resource and (potentially) its dependents.              | Yes      |
| `scale`   | Adjust the replica count or capacity of a resource.              | Yes      |
| `restart` | Terminate and re-initialize a resource without removing it.      | Yes      |

**Custom Action Extensions** (Extended Conformance):
Implementations MAY define additional actions using a namespaced format: `<domain>/<action>` (e.g., `database.example.com/failover`, `network.example.com/drain`). Custom actions MUST declare whether they are mutating or non-mutating. The control plane MUST reject `AgentRequests` with unrecognized actions.

#### 3.1.3 Intent Negotiation (Extended Conformance)

Advanced planning agents may benefit from interactive refinement of a `Denied` intent rather than repeatedly submitting distinct `AgentRequests`.
Implementations MAY introduce a `Negotiating` phase. When a request violates safety policies, rather than an immediate `Denied` transition, the control plane places the request in `Negotiating` and returns a structured list of acceptable parameter bounds (e.g., "Max replicas permitted is 5" instead of "Replicas out of bounds"). The agent MUST respond with updated parameters or explicitly abort the request. This provides a structured tight loop for LLM function-calling feedback.

The control plane MUST enforce two limits on `Negotiating` phase requests:
- **`negotiationTimeoutSeconds`** (default: 300 seconds): Wall-clock deadline from the time the request entered `Negotiating`. This does not reset per round.
- **`maxNegotiationRounds`** (default: 5): Maximum number of parameter-update cycles the agent may submit. If the agent has not produced a policy-compliant parameter set within this many rounds, the control plane MUST transition the request to `Denied` with code `POLICY_VIOLATION`. This prevents a pathological agent from flooding the control plane with rapid parameter mutations while technically responding within the timeout.

If either limit is exceeded, the control plane MUST transition the request to `Denied` with code `POLICY_VIOLATION` and release any resources held during negotiation. OpsLocks MUST NOT be acquired while a request is in `Negotiating` — lock acquisition occurs only after negotiation succeeds and the request is re-evaluated to `Approved`.

#### 3.1.4 Agent Reasoning and Confidence (Extended Conformance)

Agents operating on probabilistic models (e.g., LLM-based SRE agents) frequently encounter uncertainty when formulating intents. AIP Extended Conformance supports communicating this uncertainty via the `ReasoningTrace` field in the `AgentRequest`.

A `ReasoningTrace` SHOULD contain:
- **ConfidenceScore**: An overall float between 0.0 and 1.0 indicating the agent's confidence in its intended action.
- **ComponentConfidence** (OPTIONAL): A structured mapping providing granularity (e.g., `{"diagnosis": 0.95, "remediation_selection": 0.70}`).
- **CalibrationEvidence** (OPTIONAL): Cryptographically signed performance metrics or benchmark results attesting to the model's historical accuracy on similar tasks, required by some strictly configured control planes for high-risk operations.
- **TraceReference**: A link to the specific chain-of-thought or reasoning log that generated the intent.
- **Alternatives**: A list of alternative, lower-impact actions the agent considered.

Control planes MAY use `SafetyPolicies` to evaluate the `ConfidenceScore` or `ComponentConfidence` (e.g., denying destructive `Actions` like `delete` if the score is below 0.95). Furthermore, if a request is `Denied`, the control plane SHOULD include an alternative course of action in the `Message` or `Retry` payload when possible, informing the agent's next planning cycle.

> **Calibration Warning:** Generative AI systems (LLMs) produce `ConfidenceScore` values that are statistically uncalibrated — a self-reported score of 0.95 does not reliably correspond to 95% historical accuracy. Control planes MUST NOT use `ConfidenceScore` as a sole gate for safety decisions unless `CalibrationEvidence` is present and verifiable. Without `CalibrationEvidence`, `ConfidenceScore` MUST be treated as informational metadata only. Purpose-built probabilistic or causal systems with benchmark attestation SHOULD provide `CalibrationEvidence` to unlock policy-gated use of the score. This distinction exists because the spec is designed to work correctly for both generative AI agents today and calibrated causal systems — the same field serves both, but the enforcement path differs based on evidence.

#### 3.1.5 Confidence Verification (Extended Conformance)

Because `ConfidenceScore` is agent-self-reported, a conforming control plane MUST NOT treat it as a tamper-evident value without external attestation. This section defines the verification protocol for `CalibrationEvidence` and the behavioral contract for control planes that gate policy decisions on confidence.

##### CalibrationEvidence Format

`CalibrationEvidence` MUST be a JSON Web Token (JWT) signed by a trusted external evaluator — an entity independent of the agent runtime. The JWT payload MUST contain:

| Claim | Type | Description |
|-------|------|-------------|
| `sub` | string | `agentIdentity` the evidence pertains to |
| `iat` | integer | Unix timestamp of evidence issuance |
| `exp` | integer | Expiry — evidence MUST be rejected after this time |
| `measured_accuracy` | float | Empirically measured accuracy (0.0–1.0) on held-out tasks |
| `task_domain` | string | Domain the measurement applies to (e.g., `"k8s-deploy"`, `"db-migration"`) |
| `sample_size` | integer | Number of tasks used to compute `measured_accuracy` |
| `evaluator_id` | string | Identifier of the evaluation service that issued this token |

The JWT MUST be signed using RS256 or ES256. The control plane MUST verify the signature against a pre-configured set of trusted evaluator public keys before accepting the evidence.

##### Verification Semantics

When a `SafetyPolicy` rule's expression references `request.spec.reasoningTrace.confidenceScore`, the control plane MUST apply the following logic:

1. If `CalibrationEvidence` is **absent**: the `ConfidenceScore` field MUST be evaluated as `null` in policy expressions. Policies relying on confidence scoring MUST treat the request as if no confidence was declared.
2. If `CalibrationEvidence` is **present but invalid** (expired, bad signature, wrong `sub`): the policy evaluation MUST fail. If `failureMode` is `FailClosed`, the request is denied with `EVALUATION_FAILURE`.
3. If `CalibrationEvidence` is **present and valid**: the control plane MUST make the `measured_accuracy` claim available to policy expressions alongside the self-reported `ConfidenceScore`. Implementations SHOULD expose it as `request.spec.reasoningTrace.measuredAccuracy` in the CEL evaluation context.

This means a well-written policy gates on `measuredAccuracy`, not raw `confidenceScore`:

```
# Weak — agent can lie
request.spec.reasoningTrace.confidenceScore < 0.8

# Strong — gates on externally verified accuracy
request.spec.reasoningTrace.measuredAccuracy < 0.8
```

##### Evaluator Trust Configuration

Control plane implementations MUST provide an operator-configurable list of trusted evaluator public keys, expressed as a JWKS (JSON Web Key Set) endpoint or static key material. The control plane MUST NOT hard-code trusted evaluators.

#### 3.1.6 AgentRequest Status Fields

The control plane MUST maintain the following status fields on an `AgentRequest`:

- **Phase**: The current lifecycle state (Section 3.1).
- **Conditions**: Structured per-condition tracking (e.g., `PolicyEvaluated`, `Approved`, `RequiresApproval`).
- **Denial**: A structured `DenialResponse` (Section 3.1.1) when the request is denied.
- **EvaluationGeneration**: A monotonically increasing integer counter, starting at `0`, incremented each time the control plane performs a fresh policy evaluation. A value of `0` means the request has not yet been evaluated. Used by the TOCTOU protection mechanism (Section 3.6.2) to bind human approvals to a specific evaluation epoch.
- **ControlPlaneVerification**: A snapshot of independently fetched cluster state captured at the time of policy evaluation. This persists what the control plane verified so human reviewers can compare it against the agent's declared intent. MUST include:
  - `evaluatedStateFingerprint`: The StateFingerprint (Section 3.6.2) captured at T1. Opaque string. Never exposed to the agent.
  - `fetchedAt`: Timestamp of when the verification was performed.
  - Additional substrate-specific fields (e.g., replica counts, endpoint health) at the implementer's discretion.

### 3.2 SafetyPolicies
`SafetyPolicies` define the rules the control plane MUST evaluate before an `AgentRequest` MAY transition to the `Approved` state.

A `SafetyPolicy` MUST specify:
- **TargetSelector**: Determines which `Target` resources the policy applies to.
- **Rules**: A list of conditions to evaluate.
- **Action**: The outcome if a rule is triggered (`Allow`, `Deny`, `Log`, `RequireApproval`).

Common rule types SHOULD include:
- `StateEvaluation`: Checking target system state (e.g., metric queries, log analysis, health checks) to prevent invariant violations.
- `TimeWindow`: Restricting actions strictly to allowable temporal profiles.
- `RateLimit`: Limiting the frequency of specific `Actions` by an `AgentIdentity`. Implementations MUST document the type of temporal window used (e.g., sliding vs. fixed window).

Implementations SHOULD support expressing policy rules using established policy languages (e.g., CEL, OPA/Rego) to promote interoperability.

#### 3.2.1 Policy Conflict Resolution
When multiple `SafetyPolicies` match a given `Target`, the control plane MUST resolve conflicts using the following precedence (highest to lowest):
1. **Explicit `Deny`**: Any policy that evaluates to `Deny` MUST take precedence. The request transitions to `Denied`.
2. **`RequireApproval`**: If no `Deny` is triggered but a `RequireApproval` action is present, the request MUST be held pending external approval.
3. **`Log`**: Log-only actions MUST NOT block the request but MUST generate an `AuditRecord`.
4. **`Allow`**: The request proceeds only if no higher-precedence actions are triggered.

In summary: Deny > RequireApproval > Log > Allow.

This precedence applies **across targets as well as within a single target**. If a `SafetyPolicy` on a cascading resource evaluates to `Deny` (surfaced as `CASCADE_DENIED`) while a policy on the primary target evaluates to `RequireApproval`, the `Deny` outcome MUST win and the request MUST transition to `Denied`. A `Deny` on any target in the evaluation set — primary or cascading — cannot be overridden by human approval.

#### 3.2.2 Failure Semantics
Control planes MUST specify behavior when policy evaluation dependencies (e.g., an external metrics aggregator) fail:
- `FailClosed` (Default): If a `SafetyPolicy` cannot be evaluated, the `AgentRequest` MUST transition to `Denied` with error code `EVALUATION_FAILURE`.
- `FailOpen` (OPTIONAL, Extended Conformance): The control plane MAY permit the request, but MUST generate a critical-severity `AuditRecord` noting the evaluation failure.

### 3.3 OpsLocks
When multiple agents or swarms attempt to operate concurrently, `OpsLocks` provide a mandatory synchronization mechanism to prevent conflicting actions on the same targets.

- **Concurrency Control**: An `AgentRequest` with a mutating `Action` MUST successfully acquire an exclusive `OpsLock` on its `Target` before reaching the `Approved` state. Non-mutating actions (e.g., `read`) MUST NOT require an exclusive lock but MAY use shared (read) locks at the implementer's discretion (Extended Conformance).

  **Read-Write Lock Interaction**: When an exclusive write lock is held on a resource, any concurrent `read` `AgentRequest` on the same resource MUST be handled as follows: if shared locks are supported (Extended Conformance), the `read` request MAY be denied with `LOCK_CONTENTION` or queued — but it MUST NOT be approved into a shared lock that grants the agent a view of state that is actively being mutated. The rationale is that a `read` action whose results feed a subsequent safety decision MUST observe a consistent, non-transitional state. Implementations supporting shared locks MUST document whether they provide snapshot-isolation reads or may observe partially-applied write state. Implementations that cannot guarantee snapshot isolation SHOULD use Deny-on-Contention for `read` requests when an exclusive lock is held.
- **Scoped Execution Locking**: When an `AgentRequest` with `executionMode: scoped` is approved, the control plane MUST acquire OpsLocks on all resources currently matching the `scopeBounds.permittedTargetPatterns` at the moment of approval — not lazily at the moment each individual action executes. This eliminates the race window between scope approval and the first infrastructure API call. Any subsequent `AgentRequest` whose `Target` URI matches a pattern covered by an active scoped lock MUST be denied with code `LOCK_CONTENTION`, regardless of whether that exact URI was explicitly declared by the scope-holding agent. The `RetryAfterSeconds` hint SHOULD reflect the remaining `timeBoundSeconds` of the active scope.

  Scoped lock acquisition MUST be **all-or-nothing (atomic)**. If the control plane successfully acquires locks on N resources but fails to acquire the lock on resource N+1, it MUST release all N previously acquired locks and deny the request with `LOCK_CONTENTION`. Partial scoped locks MUST NOT be left in place.

  Control plane implementations MUST enforce a configurable `maxLockedResources` limit (default: 500) on the number of resources a single scoped `AgentRequest` may lock simultaneously. If the resolved set of matching resources exceeds this limit, the request MUST be denied with code `SCOPE_TOO_BROAD`. Operators SHOULD set patterns that target specific namespaces or resource subtrees rather than wildcards spanning an entire environment.

  > **Why this rule exists:** A `StateEvaluation` SafetyPolicy (e.g., "deny scale if cluster status is UPDATING") provides defense-in-depth but has a race window — the infrastructure status may not yet reflect the in-progress operation at the moment a conflicting request arrives. Scoped OpsLocks close this window by establishing the concurrency boundary at scope approval time, before any infrastructure calls are made.

- **Holder**: An `OpsLock` MUST identify the `AgentIdentity` and specific `AgentRequest` that owns it.
- **Lease Expiration**: Locks MUST define an explicit `LeaseDuration`. If the agent does not mark the request `Completed` or explicitly renew the lock within this duration, the control plane MUST automatically release the lock and transition the `AgentRequest` to `Failed` with a timeout reason.

#### 3.3.1 Lock Contention Strategy
The control plane MUST implement one of the following contention strategies and document which is in effect:
- **Deny-on-Contention**: If the target is already locked, the `AgentRequest` immediately transitions to `Denied` with code `LOCK_CONTENTION`. The denial SHOULD include `RetryAfterSeconds` based on the existing lock's remaining lease.
- **Queue-with-Timeout**: The `AgentRequest` enters a wait queue. If the lock is not acquired within a configurable `LockWaitTimeoutSeconds`, the request transitions to `Denied` with code `LOCK_TIMEOUT`. The queue MUST be ordered by `priority` descending, with submission timestamp as the tiebreaker (FIFO within same priority). This ensures high-priority emergency operations (e.g., operator-initiated incident remediation) are not blocked behind lower-priority routine work.

The control plane MUST NOT allow indefinite lock waiting to prevent resource starvation.

#### 3.3.2 Deadlock Prevention
When an agent holds an `OpsLock` on one resource and its `AgentRequest` (or `IntentPlan`) requires locking additional resources, the control plane MUST employ at least one deadlock prevention strategy:
- **Lock Ordering**: All locks within a single `IntentPlan` MUST be acquired in a deterministic, globally consistent order (e.g., lexicographic by Target URI).
- **Try-Lock with Rollback**: The control plane attempts to acquire all required locks atomically. If any lock cannot be acquired, all previously acquired locks for that plan MUST be released and the step retried after backoff.

Implementations MUST document their chosen deadlock prevention mechanism.

#### 3.3.3 Swarm Identity and Quotas (Extended Conformance)
When highly parallel "agent swarms" operate together, treating each sub-agent as a completely independent actor can overwhelm the control plane or violate aggregate safety bounds. 

Implementations MAY support a `SwarmIdentity` abstraction, which groups multiple `AgentIdentities` under a common hierarchy.
- **Lock Pooling and Delegation**: A swarm coordinator MAY acquire an `OpsLock` on a parent resource (e.g., a namespace) and temporarily delegate scoped sub-locks to subordinate agents. 
- **Delegation Revocation**: If the coordinator's lock expires or is revoked, the control plane MUST recursively revoke all delegated locks held by subordinates and fail their executing intents.
- **Aggregate Quotas**: `RateLimit` policies and Lock Starvation limits MAY be applied to the `SwarmIdentity` in aggregate, rather than the individual agent.
- **Audit Trails**: Actions performed under a delegated lock MUST record both the performing `AgentIdentity` and the authoring `SwarmIdentity` coordinator in the `AuditRecord`.

### 3.4 Cascading Effects

Actions on resources frequently produce side effects on dependent resources (e.g., deleting a parent resource cascades to its children, removing a service affects its consumers). Responsibility for cascade awareness is shared between the control plane and the agent, depending on the agent's capabilities.

#### 3.4.1 Control Plane Responsibilities (Primary Authority)
The control plane SHOULD maintain or query a dependency graph for the managed environment (e.g., resource ownership hierarchies, service dependency maps, infrastructure topology APIs). When evaluating an `AgentRequest`, the control plane SHOULD:
1. Resolve the set of resources that would be transitively affected by the `Action` on the `Target`.
2. Evaluate applicable `SafetyPolicies` against each affected resource.
3. If any affected resource would trigger a `Deny` policy, the primary `AgentRequest` MUST be denied. The denial response MUST include the specific cascading target(s) that caused the denial using code `CASCADE_DENIED`. A `Deny` outcome on a cascading target MUST override a `RequireApproval` outcome on the primary target — cascade safety is not subject to human override.

The control plane is the **primary authority** for cascade safety. It MUST NOT depend solely on agent-provided cascade information for safety decisions.

#### 3.4.2 Agent-Provided Causal Models (OPTIONAL)
An `AgentRequest` MAY include a `CascadeModel` field where the agent declares its understanding of expected downstream effects. This supports agents that maintain their own causal models, including but not limited to:
- Purpose-built agents with embedded dependency graphs or CMDB integrations.
- Planning agents (e.g., ReAct, tree-of-thought) that reason about downstream effects as part of their planning chain.
- Agents that pre-query the platform's dependency APIs before submitting intent.

A `CascadeModel` SHOULD contain:
- **AffectedTargets**: A list of resources the agent expects to be transitively impacted, each with the expected effect type (e.g., `deleted`, `modified`, `disrupted`).
- **ModelSource** (OPTIONAL): An identifier for the causal model used, classified into one of the following trust tiers:
  - `authoritative`: The model is derived from the platform's own dependency API or a verified CMDB (e.g., the agent queried the platform's topology API before submitting). Control planes SHOULD treat this with high trust.
  - `derived`: The model is built from heuristics, static analysis, or domain-specific knowledge graphs. Control planes SHOULD cross-validate against their own dependency data.
  - `inferred`: The model is produced by LLM reasoning or probabilistic inference. Control planes MUST cross-validate and MUST NOT rely on this tier for safety decisions.
- **ModelSourceID** (OPTIONAL): A specific identifier for traceability (e.g., `cmdb-v2.3`, `topology-api/2024-01`).

#### 3.4.3 Cross-Validation
When both the control plane's dependency graph and an agent-provided `CascadeModel` are available, the control plane SHOULD cross-validate:
- **Agent declares effects the control plane does not see**: The control plane SHOULD log an informational `AuditRecord` noting the discrepancy. This MAY indicate the agent has domain knowledge beyond the platform graph (e.g., application-level dependencies).
- **Control plane detects effects the agent did not declare**: The control plane MUST evaluate safety policies against these undeclared targets regardless. The control plane SHOULD include a `CASCADE_MISMATCH` annotation in the `AuditRecord` and MAY surface the discrepancy to the agent in the response, enabling the agent to refine its causal model over time.
- **Both agree**: The `AuditRecord` SHOULD note the corroboration, increasing confidence in the cascade assessment.

This cross-validation loop provides a feedback mechanism: agents with good causal models are validated, agents with poor models are surfaced, and safety is never compromised either way.

To prevent evaluation cascades from exploding exponentially across a complex dependency graph, the control plane MUST impose and document a maximum `CascadeDepth` limit (e.g., a maximum of 3 hops down the dependency graph).

#### 3.4.4 Limitations
Implementations that do not support cascade evaluation MUST document this limitation and SHOULD warn operators that safety policies will only be evaluated against the primary target. This is particularly important for destructive actions (`delete`) where cascading effects are most impactful.

In environments where no control-plane dependency graph is available and the agent provides a `CascadeModel`, the control plane MAY evaluate safety policies against the agent-declared targets on a best-effort basis, but MUST log a warning-severity `AuditRecord` indicating that cascade safety is based solely on agent-asserted information.

### 3.5 Multi-Step Operations (Extended Conformance)

Agents frequently need to execute coordinated, multi-step workflows (e.g., isolate a resource, perform maintenance, then restore traffic). AIP defines the `IntentPlan` abstraction for this purpose.

An `IntentPlan` MUST contain:
- **PlanID**: A unique identifier for the plan.
- **AgentIdentity**: The agent (or agent swarm coordinator) that owns the plan.
- **PlanningMode**: `static` (default) or `dynamic`. `static` means all steps are declared upfront before approval. `dynamic` means steps are submitted incrementally as the agent observes execution results (e.g., a ReAct agent that cannot enumerate all steps before starting). In `dynamic` mode, the control plane approves the plan's scope and constraints rather than the full step sequence.
- **MaxPlanDurationSeconds**: The maximum wall-clock time the plan is permitted to be `Active`. If the plan has not reached `Completed` within this window, the control plane MUST transition it to `Aborted`, release all associated `OpsLocks`, and generate a critical-severity `AuditRecord`. This field is REQUIRED for `dynamic` plans (which have no predefined step count) and RECOMMENDED for `static` plans. There is no default — implementations MUST reject a `dynamic` plan submitted without this field.
- **Steps**: An ordered list of `AgentRequest` references, each with:
  - **StepOrder**: The execution sequence number.
  - **AgentRequestRef**: A reference to the `AgentRequest` for this step.
  - **DependsOn** (OPTIONAL): References to preceding steps that MUST be `Completed` before this step is submitted for evaluation.
  - **CompensatingAction** (OPTIONAL): A reference to an `AgentRequest` that SHOULD be executed if a later step fails and this step's effects need to be reversed (see Section 3.5.1).

> **Forward State Validation (static plans only):** The control plane MUST validate each step against the projected state after all preceding steps complete — not against current state at submission time. For example, if step 1 deletes resource A and step 2 updates a resource that depends on A, step 2 must be evaluated against the state where A no longer exists. A plan where a later step is provably invalid given earlier steps' effects MUST be denied at submission time with code `POLICY_VIOLATION` and a message identifying the conflicting steps. Implementations that cannot perform forward state simulation MUST document this limitation; in that case, the control plane evaluates each step against current state only and MUST generate a warning-severity `AuditRecord` noting the limitation.

An `IntentPlan` lifecycle MUST track the following states:
- `Active`: The plan is in progress; steps are being submitted and executed.
- `Completed`: All steps have reached `Completed`.
- `Aborted`: One or more steps were `Denied` or `Failed`, and the plan cannot proceed.
- `RollingBack`: Compensating actions are being executed for previously completed steps.

#### 3.5.1 Rollback and Compensation
When a step in an `IntentPlan` transitions to `Denied` or `Failed`:
- The control plane MUST transition the `IntentPlan` to `Aborted`.
- The control plane MUST release all `OpsLocks` held by any step of the aborted plan.
- If any previously `Completed` steps declared a `CompensatingAction`, the control plane MUST transition the `IntentPlan` to `RollingBack` and submit those compensating `AgentRequests` in reverse step order. Each compensating action is subject to the same policy evaluation and lock acquisition as any other `AgentRequest`.
- If no `CompensatingAction` is declared for a completed step, the control plane MUST NOT attempt automatic rollback for that step. The `AuditRecord` MUST note which completed steps lack compensating actions.
- If a compensating action itself fails, the control plane MUST halt the rollback, leave the `IntentPlan` in `RollingBack` state, and generate a critical-severity `AuditRecord` indicating manual intervention is required.
- Compensating `AgentRequest` records MUST carry `isCompensating: true`. This annotation allows operators to configure `SafetyPolicy` rules that explicitly relax `TimeWindow` or `RateLimit` restrictions for rollback operations. For example, a change-freeze `TimeWindow` policy MAY declare `allowCompensating: true` to permit rollback actions during a freeze window. Without such a policy override, a compensating action is subject to full policy evaluation and may itself be denied — which is a valid operator choice but MUST be documented as a known risk when change-freeze policies are in effect alongside multi-step plans.
- The `AuditRecord` for the aborted plan MUST include the full step history, including which steps completed, which compensating actions succeeded, and which failed.

### 3.6 Execution Monitoring (Extended Conformance)

The period between `Approved` and `Completed`/`Failed` is a critical window where the agent is actively mutating infrastructure. AIP defines optional execution monitoring to maintain governance during this period.

#### 3.6.1 Heartbeats
When execution monitoring is supported, agents MUST send periodic heartbeat signals to the control plane after transitioning an `AgentRequest` to `Executing`. The heartbeat interval MUST be configured by the control plane and communicated in the approval response.

If the control plane does not receive a heartbeat within the configured interval:
- The control plane MUST mark the `AgentRequest` as `Failed` with reason `HEARTBEAT_TIMEOUT`. This is not advisory — a silent agent holding an OpsLock indefinitely blocks all other agents on that resource.
- The associated `OpsLock` MUST be released.
- The `AuditRecord` MUST note the heartbeat failure.

A heartbeat MAY include:
- **ProgressPercent** (OPTIONAL): An estimate of execution progress (0-100).
- **StatusMessage** (OPTIONAL): A human-readable status update.

#### 3.6.2 TOCTOU Protection (Extended Conformance)

A fundamental race condition exists in any pre-execution governance system: cluster state observed at **T1** (policy evaluation) may differ from cluster state at **T2** (human approval) or **T3** (actual execution). This is the Time-of-Check to Time-of-Use (TOCTOU) problem.

AIP identifies three distinct TOCTOU windows:

| Window | From | To | Risk |
|--------|------|----|------|
| T1→T2 | Policy evaluation | Human approves | Long window (minutes to hours). Most dangerous. |
| T2→T3 | Human approves | Agent executes | Short window (seconds). Depends on partner execution path. |
| Concurrent | Any point | Any point | Two agents act simultaneously on the same resource. Addressed by OpsLocks (Section 3.3). |

##### StateFingerprint

To detect T1→T2 drift, the control plane MUST record a **StateFingerprint** at the time of policy evaluation — an opaque string that uniquely identifies the observed state of the target resource. The fingerprint is substrate-specific:

- **Kubernetes**: `resourceVersion` of the target object at evaluation time.
- **HTTP REST resources**: `ETag` header value.
- **Cloud provider resources**: version ID or entity tag from the provider API.
- **Systems without native versioning**: SHA-256 of the fields used in policy evaluation.

The StateFingerprint is recorded in `status.controlPlaneVerification.evaluatedStateFingerprint` alongside the other verified state. It is never exposed to the agent — the agent cannot forge or influence it.

##### EvaluationGeneration

The control plane MUST maintain an `EvaluationGeneration` counter in `status.evaluationGeneration`, incremented each time the control plane performs a fresh policy evaluation on the request. This counter is the **epoch** a human approval references.

##### T1→T2 Re-Verification

When a human reviewer approves a held request, the control plane MUST:

1. Re-fetch live state for the target resource.
2. Compute the current StateFingerprint.
3. Compare it to `status.controlPlaneVerification.evaluatedStateFingerprint`.

If the fingerprints differ:
- The control plane MUST NOT transition the request to `Approved`.
- The control plane MUST increment `EvaluationGeneration`.
- The control plane MUST re-evaluate all `SafetyPolicies` against the new live state.
- The control plane MUST emit an `AuditRecord` with event type `state.drifted`, recording both the old and new fingerprints.
- If the new evaluation result is still `RequireApproval`, the human reviewer MUST be presented with the updated context before approving again.
- If the new evaluation result is `Deny`, the request MUST transition to `Denied` with code `STATE_DRIFTED`.

If the fingerprints match, the state is stable and the control plane proceeds with lock acquisition and the `Approved` transition as normal.

##### ForGeneration Binding

A `HumanApproval` decision MUST reference the `EvaluationGeneration` it was made against via a `forGeneration` field. The control plane MUST reject an approval whose `forGeneration` does not match the current `status.evaluationGeneration`. This prevents replaying a stale approval after a drift has been detected and the generation incremented.

> **Why two layers?** AIP addresses TOCTOU through two complementary, independent mechanisms operating at different timescales:
>
> **Layer 1 — Reconcile Loop (T1→T2, non-cooperative):** The control plane re-verifies the StateFingerprint at human approval time, regardless of what the agent or partner SDK does. This handles the macroscopic window (minutes to hours) where a human pauses before approving a `RequireApproval` request. The control plane enforces this unconditionally.
>
> **Layer 2 — `/verify` Pre-Flight (T2→T3, cooperative, Extended Conformance):** The microscopic window between human approval and the first infrastructure API call (typically 50–500ms) is addressed by a lightweight pre-flight check. A partner SDK or MCP tool implementation SHOULD call `POST /agent-requests/{name}/verify` immediately before executing the approved action. The control plane re-fetches live state, computes the current StateFingerprint, and compares it to the fingerprint captured at evaluation time — without acquiring additional locks or changing any state. If the fingerprints match, the control plane returns a `verified: true` response and the partner proceeds. If they differ, the control plane returns `verified: false` with code `STATE_DRIFTED` and the partner MUST abort. See Section 4.4 for the full `/verify` specification.
>
> Both layers are complementary. Layer 1 is always enforced by the server. Layer 2 is cooperative — it relies on the partner SDK calling `/verify`. Neither is sufficient alone for all threat models; together they close the full TOCTOU surface.

#### 3.6.3 Approval Revocation
The control plane MAY revoke a previously granted `Approved` status if conditions change after approval but before completion — for example, if a new `SafetyPolicy` is deployed that would deny the in-progress action, or if an operator manually intervenes.

When revoking an approval:
- The control plane MUST transition the `AgentRequest` to `Denied` with code `APPROVAL_REVOKED`.
- The control plane MUST notify the agent via the established update mechanism (poll response, stream event, etc.).
- The agent runtime SHOULD attempt to halt or safely abort the in-progress operation upon receiving the revocation, but ONLY IF the agent previously explicitly declared `Interruptibility: true`. The spec acknowledges that not all infrastructure operations are safely interruptible; the agent MUST report the actual outcome (`Completed` if the action finished before revocation was processed, `Failed` if it was successfully aborted).
- The `AuditRecord` MUST capture the revocation event, the reason, and the agent's response.

> **Implementation Note**: The LLM component of an agent has no execution thread during tool dispatch — it is idle while the tool runs. It is therefore the **agent runtime or orchestrator** that receives the revocation signal and must act on it. Spec-compliant implementations SHOULD propagate revocation as a cancellation signal (e.g., context cancellation, workflow signals, or activity heartbeat checks) through the tool execution layer. The `Interruptibility: true` declaration by the agent is a contract with its runtime, not a guarantee of atomic abort. Orchestrators such as Temporal (via workflow signals and activity heartbeats) or LangGraph (via graph interrupts and state injection) provide suitable primitives for this.

### 3.7 Agent Trust Profiles (Extended Conformance)

Cryptographic `CalibrationEvidence` solves the attestation problem at submission time but does not detect agents that are well-calibrated in evaluation but systematically dishonest or degraded in production. Section 3.7 defines a complementary mechanism: the control plane's own first-party record of how each `agentIdentity` has performed historically against its own declared confidence.

#### 3.7.1 AgentTrustProfile

The control plane MUST maintain an `AgentTrustProfile` per `agentIdentity`. The profile is derived entirely from `AuditRecord` data — it is not self-reported by the agent and cannot be manipulated by the agent.

An `AgentTrustProfile` MUST track:

| Field | Description |
|-------|-------------|
| `agentIdentity` | The identity this profile belongs to |
| `totalRequests` | Total `AgentRequest` submissions observed |
| `successRate` | Fraction of requests that reached `Completed` vs `Failed`/`Denied` |
| `meanClaimedConfidence` | Rolling mean of `confidenceScore` declared by this agent |
| `calibrationError` | Mean absolute error between claimed confidence and actual binary outcome (0=failed, 1=succeeded). Lower is better. |
| `calibrationDrift` | Rolling delta of `calibrationError` over the last N requests. A rising drift signals the agent is becoming less honest over time. |
| `lastUpdated` | Timestamp of the most recent update |
| `windowSize` | Number of requests used in rolling calculations |

`calibrationError` is computed as:

```
calibrationError = mean(|claimed_confidenceScore_i - actual_outcome_i|)
  where actual_outcome_i = 1.0 if Completed, 0.0 if Failed
```

An agent that consistently claims `0.95` confidence but succeeds only 60% of the time has a `calibrationError` of `0.35` — measurably dishonest or poorly calibrated.

> **Scope Note:** `Denied` requests are intentionally excluded from `calibrationError` calculations. A `Denied` outcome means the governance system worked correctly — the agent's intent was intercepted before execution. It does not mean the agent's execution was poor. Including `Denied` in the outcome would penalize agents operating in environments with strict policies and conflate governance effectiveness with agent reliability. Implementers should be aware that a sophisticated agent could exploit this by submitting high-confidence requests it expects to be `Denied` (not counted) while only submitting requests it is certain will succeed. The `AgentTrustProfile` is complementary to — not a replacement for — transport-layer RBAC controls that restrict which actions an agent may even submit.

#### 3.7.2 Control Plane Update Semantics

The control plane MUST update the `AgentTrustProfile` whenever an `AgentRequest` transitions to a terminal state (`Completed` or `Failed`). The update MUST:

1. Increment `totalRequests`
2. Update `successRate` using an exponential moving average (EMA) with a configurable decay factor α. The default α = 0.1 is conservative (slow to adapt, ~22 requests to reflect a step change in agent behavior). For high-risk targets where rapid detection of agent degradation is required, operators SHOULD configure α ≥ 0.3 via the `SafetyPolicy` or control plane configuration. The decay factor MUST be operator-configurable and MUST NOT be hardcoded.
3. If the terminal request included a `ReasoningTrace.ConfidenceScore`, update `calibrationError` and `calibrationDrift`
4. Set `lastUpdated` to the current timestamp

The profile MUST be updated atomically. Implementations SHOULD use optimistic concurrency control (e.g., resource version checking) to prevent race conditions under high request volume.

#### 3.7.3 Policy Access to Trust Profiles

Control planes MUST expose `AgentTrustProfile` data to `SafetyPolicy` CEL expressions via the `agent` variable in the evaluation context, alongside the existing `request` variable. This enables trust-aware policies:

```cel
# Deny requests from agents with poor calibration track record
agent.calibrationError > 0.3

# Require approval if the agent's drift is worsening
agent.calibrationDrift > 0.1 && request.spec.target.uri.startsWith("k8s://prod")

# Only allow autonomous prod deploys from agents with proven track records
agent.successRate < 0.85 && request.spec.target.uri.startsWith("k8s://prod")
```

These expressions evaluate the agent's historical behavior as a first-class governance input — independent of anything the agent self-reports on the current request.

#### 3.7.4 Bootstrap and Cold Start

A newly observed `agentIdentity` with no prior history MUST be treated conservatively. Control planes SHOULD apply a configurable `coldStartPolicy` for agents with fewer than `minSampleSize` requests (default: 10). The `coldStartPolicy` MUST be one of:

- `RequireApproval` (default): All requests from unproven agents require human review
- `Deny`: No requests from unproven agents are permitted
- `Allow`: Unproven agents are treated as trusted (NOT RECOMMENDED for production)

#### 3.7.5 Trust Profile Immutability and Auditability

`AgentTrustProfile` updates MUST themselves be recorded as `AuditRecord` events with event type `agent.trustprofile.updated`. This ensures the profile's evolution is auditable and tamper-evident — an operator can reconstruct the full calibration history of any agent from the `AuditRecord` stream.

Control planes MUST NOT allow agents to read or modify their own `AgentTrustProfile`.

## 4. Transport and API Contract

AIP is transport-agnostic but requires a defined contract between the Agent and the Control Plane.

### 4.1 Transport Mechanisms
Agents MAY interact with the control plane via:
1. **REST/HTTP**: Submitting JSON/YAML payloads to standard endpoints (e.g., `POST /v1/intents`).
2. **gRPC**: Utilizing defined protobuf schemas for low-latency streaming.
3. **Platform-Specific APIs**: e.g., platform-native resource APIs with webhook or admission control mechanisms.

### 4.1.1 Consistency Requirements

OpsLock acquisition MUST be **linearizable** — the control plane MUST guarantee that at most one `AgentRequest` can successfully acquire an exclusive lock on a given `Target` URI at any point in time, even under concurrent submissions from multiple agents or across multiple control plane replicas. Implementations MUST use a consensus-backed store (e.g., etcd, ZooKeeper, or a database offering serializable transactions) as the backing store for OpsLock state. Control plane implementations that cannot provide this guarantee MUST document the consistency model in use and MUST emit a warning-severity `AuditRecord` for every lock acquisition, noting the weaker consistency guarantee.

### 4.2 Required Operations
The Control Plane MUST provide a mechanism for Agents to:
- Submit an `AgentRequest`.
- Poll or receive asynchronous updates (e.g., WebSockets, gRPC streams) regarding the `AgentRequest` phase changes.
- Signal `Completed` or `Failed` status along with execution logs to release resources.
- Renew an `OpsLock` lease during long-running operations.

### 4.3 Version Negotiation
The Control Plane MUST expose the AIP specification version(s) it supports (e.g., via a `/version` endpoint or capability discovery mechanism). Agents SHOULD verify compatibility before submitting requests. The Control Plane MUST reject requests referencing an unsupported AIP version with a clear error.

### 4.4 Pre-Execution Verification — `/verify` (Extended Conformance)

The `/verify` endpoint closes the T2→T3 TOCTOU window cooperatively. Partner SDKs and MCP tool implementations SHOULD call it immediately before issuing the infrastructure API call for an approved `AgentRequest`.

#### 4.4.1 Request

```
POST /agent-requests/{name}/verify
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `forGeneration` | integer | REQUIRED | The `EvaluationGeneration` the approval was issued against. Binds the verify call to a specific evaluation epoch. |
| `namespace` | string | OPTIONAL | Namespace of the `AgentRequest`. Defaults to `default`. |

#### 4.4.2 Semantics

The control plane MUST reject `/verify` calls on requests that are not in the `Approved` phase. The response MUST return HTTP 409 Conflict with a structured error:

```json
{ "error": "INVALID_PHASE", "message": "AgentRequest is not in Approved phase.", "currentPhase": "Pending" }
```

This prevents partner SDKs from accidentally calling `/verify` on a request still awaiting approval, and prevents agents from probing the StateFingerprint comparison behavior on non-actionable requests.

The control plane MUST:
1. Re-fetch the current live state of the `AgentRequest`'s `Target` resource.
2. Compute the current StateFingerprint for that resource.
3. Compare it to `status.controlPlaneVerification.evaluatedStateFingerprint`.
4. Verify the `forGeneration` field matches `status.evaluationGeneration`.

The control plane MUST NOT:
- Acquire or release any `OpsLocks`.
- Change the `AgentRequest` phase or any status field.
- Generate an `AuditRecord` on a successful verify (to avoid log noise for high-frequency callers). A failed verify MUST generate an `AuditRecord` with event `state.drifted.verify`, not `state.drifted` — the two events represent different TOCTOU windows and MUST be distinguishable in the audit log.
- Expose the raw `StateFingerprint` value in the response. The response MUST return only `verified: true/false`, the denial code, and the current `evaluationGeneration`. The raw fingerprint MUST NOT be included — it is an internal control plane value never exposed to agents or partner SDKs.

#### 4.4.3 Response

**Verified (fingerprints match, generation matches):**
```json
{
  "verified": true,
  "agentRequestName": "my-agent-abc123",
  "evaluationGeneration": 1
}
```
The partner SHOULD proceed with the infrastructure API call immediately after receiving this response.

**Drifted (fingerprints differ or generation mismatch):**
```json
{
  "verified": false,
  "code": "STATE_DRIFTED",
  "message": "Target state changed since policy evaluation. Re-submit the AgentRequest.",
  "evaluationGeneration": 2
}
```
The partner MUST abort the infrastructure API call and SHOULD surface the drift to the agent runtime for re-planning.

**Stale approval (forGeneration does not match current evaluationGeneration):**
```json
{
  "verified": false,
  "code": "GENERATION_MISMATCH",
  "message": "Approval was issued against generation 1 but current generation is 2.",
  "evaluationGeneration": 2
}
```

#### 4.4.4 Latency Contract

Because `/verify` sits in the hot path between approval and infrastructure execution, implementations MUST complete the check within a configurable `verifyTimeoutMs` (default: 200ms). If the control plane cannot complete verification within this window (e.g., the backing store is degraded), it MUST return an error response with code `EVALUATION_FAILURE`. The partner MUST treat an unresponsive or errored `/verify` as a failed check and abort, consistent with FailClosed semantics.

> **Design analogy:** `/verify` is to AIP what `iam:SimulatePrincipalPolicy` is to AWS — a lightweight, read-only pre-flight that gives the caller high confidence the action is still safe to execute at T3, without the overhead of a full policy re-evaluation cycle. The key distinction from a full re-evaluation is that `/verify` only checks state freshness (StateFingerprint); it does not re-run `SafetyPolicy` expressions. Full policy re-evaluation is the control plane's job at T1 and T2; `/verify` guards against the narrow window of state mutation between human approval and execution.

### 4.5 Simulation and Dry-Runs (Extended Conformance)
Advanced planning agents (such as tree-of-thought or ReAct frameworks) benefit from evaluating alternatives before committing to a plan.

Implementers MAY provide an endpoint allowing an Agent to submit a "what-if" `AgentRequest` or `IntentPlan` for synchronous simulated evaluation. The control plane MUST evaluate policies based on the immediate real-time state, but MUST NOT acquire `OpsLocks` or change actual records. The response MUST return a simulated `Approved` or `Denied` result.

> **Note:** Simulation (§4.5) and Pre-Execution Verification (§4.4) serve different purposes. Simulation is for planning — an agent asks "would this be approved?" before it has any approval. Verification is for safety — a partner SDK asks "is this approval still valid?" immediately before executing. Do not conflate them.

## 5. Observability and Integration

### 5.1 Audit Logging
Auditability is a first-class requirement for AIP governance.

The Control Plane MUST generate an immutable `AuditRecord` for every state transition of an `AgentRequest`.
An `AuditRecord` MUST include:
- `Timestamp` (RFC 3339 formatted).
- `AgentIdentity`.
- `AgentRequest` details (`Action`, `Target`, `Reason`).
- `Phase` transition resulting from the event.
- `PolicyEvaluations`: Detailed outcomes of any `SafetyPolicies` applied, including the specific rules triggered.
- `LockStatus`: OpsLock acquisition or release events.

Audit logs SHOULD be exportable to standard SIEM (Security Information and Event Management) systems.

#### 5.1.1 Retention and Garbage Collection

`AgentRequest` records in terminal states (`Completed`, `Failed`, `Denied`) and their associated `AuditRecord` entries MUST be retained for a minimum configurable retention period (default: **365 days**) to satisfy audit and incident forensics requirements. After the retention period, implementations MAY garbage-collect terminal-state records. The retention period MUST be operator-configurable and MUST be documented. Operators in regulated industries SHOULD configure longer retention per applicable compliance requirements: SOC 2 Type II and PCI-DSS 10.7 require a minimum of 1 year; FedRAMP requires 3 years; HIPAA requires 6 years. The 365-day default satisfies SOC 2 and PCI-DSS out of the box.

Implementations MUST NOT garbage-collect `AuditRecord` entries independently of their parent `AgentRequest` — the two MUST be retained for the same duration to preserve a coherent audit trail. Active records (`Pending`, `Approved`, `Executing`, `Negotiating`) MUST NOT be garbage-collected.

### 5.2 Standards Alignment
Implementations SHOULD leverage existing industry observability standards:
- **CloudEvents**: `AuditRecords` SHOULD be expressible as CloudEvents for interoperable event routing.
- **OpenTelemetry**: Control plane operations SHOULD emit OpenTelemetry traces and metrics to enable distributed tracing across agent-to-control-plane interactions.
- **SPIFFE/SPIRE**: Implementations SHOULD support SPIFFE Verifiable Identity Documents (SVIDs) as an `AgentIdentity` mechanism.

## 6. Authentication and Identity

To prevent spoofing and ensure accountability, agents MUST authenticate with the Control Plane.

- **Identity Verification**: The Control Plane MUST extract and verify the `AgentIdentity` from the transport layer (e.g., mTLS certificates, OIDC tokens, SPIFFE SVIDs).
- Agents MUST NOT be permitted to arbitrarily assert an `AgentIdentity` within the `AgentRequest` payload without corresponding transport-layer cryptographic validation.
- The Control Plane SHOULD support binding `SafetyPolicies` to specific `AgentIdentity` values or groups, enabling per-agent or per-team policy scoping.
- **Composite Identity for Managed Platforms**: On managed agent platforms (e.g., AWS Bedrock, Azure AI Agent Service), the transport-layer credential identifies the executing infrastructure principal (e.g., a Lambda execution role ARN, an Azure Managed Identity) — not the specific AI agent or workflow. Multiple distinct AI agents may share the same infrastructure principal. In these environments, the `AgentIdentity` MUST be a composite of the executing principal and an AI tenant identifier (e.g., Bedrock Agent ID, Azure AI Agent name). The AI tenant identifier MUST be carried as a verified claim in the transport-layer auth token — for example, as a custom JWT claim (`aip_agent_id`) in an OIDC token, or as a request-context header validated via a platform-specific sidecar (e.g., an AWS Lambda extension that injects and signs the Bedrock Agent ID). The Control Plane MUST verify this claim before constructing the composite identity — accepting an unverified agent ID from the request payload alone is insufficient and MUST be treated as `IDENTITY_INVALID`. The Control Plane MUST use the composite identity — not the infrastructure principal alone — as the unit for policy binding, OpsLock ownership, and audit attribution.

## 7. Security Considerations

- **Least Privilege**: Agents SHOULD request only the minimum `Action` scope required. Control planes SHOULD support RBAC-style bindings restricting which `Actions` and `Targets` a given `AgentIdentity` may request.
- **Priority Inflation**: Agents may attempt to bypass queueing by asserting artificially high `Priority` values. The control plane MUST treat `Priority` strictly as a hint and SHOULD enforce maximum priority bounds per `AgentIdentity` (e.g., via RBAC) to prevent lower-tier agents from starving out legitimate critical operations.
- **Lock Starvation**: Malicious or misconfigured agents could starve others by repeatedly acquiring locks. Implementations MUST enforce `LeaseDuration` limits and SHOULD implement per-agent lock quotas.
- **Policy Tampering**: `SafetyPolicies` define the security boundary. The control plane MUST restrict creation and modification of `SafetyPolicies` to privileged administrators. Audit records MUST capture policy changes.
- **Replay Attacks**: Each `AgentRequest` MUST be uniquely identifiable. The control plane MUST reject duplicate request IDs within a configurable deduplication window (default: 24 hours). Implementations MUST document the window duration. In distributed control plane deployments, the deduplication store MUST be shared across all replicas — per-replica deduplication is insufficient and MUST NOT be used.
- **Denial of Service**: The control plane SHOULD implement rate limiting on `AgentRequest` submission per `AgentIdentity` to prevent resource exhaustion.
- **Cascade Model Poisoning**: An agent could submit a deliberately misleading `CascadeModel` to distract or overwhelm the control plane. The control plane MUST NOT trust agent-asserted cascade information for safety decisions when its own dependency graph is available (Section 3.4.1).

## 8. The Governance Workflow

The lifecycle of an agent's operation MUST proceed as follows:

1. **Intent Declaration**: The authenticated agent issues an `AgentRequest`. The control plane records it as `Pending`.
2. **Policy Resolution**: The control plane fetches all `SafetyPolicies` targeting the requested resource.
3. **Safety Evaluation**: The control plane evaluates dynamic states. If a `Deny` rule is triggered, the request MUST transition to `Denied`. Policy conflicts are resolved per Section 3.2.1.
4. **Cascade Evaluation** (if supported): The control plane resolves the dependency graph for the target resource and evaluates safety policies against transitively affected resources (Section 3.4).
5. **Concurrency Acquisition**: The control plane attempts to assign an `OpsLock` on the target to the requesting agent, following the configured contention strategy (Section 3.3.1).
6. **Approval**: Upon safety verification and lock acquisition, the `AgentRequest` transitions to `Approved`.
6.5. **Pre-Execution Verification** (Extended Conformance): Immediately before issuing the infrastructure API call, the partner SDK or MCP tool SHOULD call `POST /agent-requests/{name}/verify` with the `forGeneration` value from the approval epoch (Section 4.4). If the control plane returns `verified: false` (code `STATE_DRIFTED` or `GENERATION_MISMATCH`), the partner MUST abort the infrastructure call and SHOULD surface the event to the agent runtime for re-planning. A `state.drifted.verify` `AuditRecord` is emitted. If the control plane returns `verified: true`, the partner proceeds immediately.
7. **Execution**: The agent performs the action against the target system. If execution monitoring is enabled (Section 3.6), the agent transitions the request to `Executing` and begins sending heartbeats.
8. **Completion/Release**: The agent signals success or failure. The control plane updates the `AgentRequest` to `Completed` or `Failed`, generates final audit records, and explicitly releases the associated `OpsLock`.

## 9. Wire Format

To ensure interoperability between independently developed agents and control planes, AIP defines canonical JSON schemas for its core entities. Implementations MUST accept these formats over REST/HTTP transports. Implementations using other transports (gRPC, platform-native) MUST define equivalent schemas that map bidirectionally to these JSON representations.

### 9.1 AgentRequest

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "AgentRequest",
  "type": "object",
  "required": ["apiVersion", "kind", "id", "agentIdentity", "target", "reason"],
  "allOf": [
    {
      "if": {
        "properties": { "executionMode": { "const": "single" } },
        "required": ["executionMode"]
      },
      "then": { "required": ["action"] },
      "else": { "comment": "When executionMode is scoped, action is omitted — scopeBounds.permittedActions replaces it." }
    },
    {
      "if": {
        "properties": { "executionMode": { "const": "scoped" } },
        "required": ["executionMode"]
      },
      "then": { "required": ["scopeBounds"] },
      "else": { "comment": "scopeBounds is only required when executionMode is scoped." }
    }
  ],
  "properties": {
    "apiVersion": {
      "type": "string",
      "const": "aip/v1alpha1"
    },
    "kind": {
      "type": "string",
      "const": "AgentRequest"
    },
    "id": {
      "type": "string",
      "description": "Globally unique request identifier (e.g., UUID v4)."
    },
    "agentIdentity": {
      "type": "string",
      "description": "Verified agent identifier, populated or validated by the control plane from the transport layer."
    },
    "action": {
      "type": "string",
      "description": "Standard action (create, read, update, delete, scale, restart) or namespaced custom action (domain/action)."
    },
    "target": {
      "type": "object",
      "required": ["uri"],
      "properties": {
        "uri": {
          "type": "string",
          "description": "Canonical resource identifier. Format is implementation-defined but MUST be consistent within a control plane."
        },
        "resourceType": {
          "type": "string",
          "description": "The type/kind of the target resource (e.g., 'Instance', 'Service', 'Database')."
        },
        "attributes": {
          "type": "object",
          "description": "Additional key-value attributes for policy matching (e.g., labels, tags, environment).",
          "additionalProperties": { "type": "string" }
        }
      }
    },
    "reason": {
      "type": "string",
      "description": "Human or machine-readable justification."
    },
    "priority": {
      "type": "integer",
      "minimum": 0,
      "description": "Priority hint. Higher values indicate higher priority."
    },
    "intentPlanRef": {
      "type": "string",
      "description": "Reference to a parent IntentPlan ID, if this request is part of a multi-step operation."
    },
    "interruptibility": {
      "type": "boolean",
      "default": false,
      "description": "Declares whether the agent runtime can safely abort mid-execution if approval is revoked. See Section 3.6.2."
    },
    "isCompensating": {
      "type": "boolean",
      "default": false,
      "description": "True if this AgentRequest was submitted as a compensating (rollback) action for a failed IntentPlan step. Allows SafetyPolicy rules with allowCompensating: true to relax TimeWindow or RateLimit restrictions during rollback. Set by the control plane when submitting compensating actions — agents MUST NOT set this field directly."
    },
    "executionMode": {
      "type": "string",
      "enum": ["single", "scoped"],
      "default": "single",
      "description": "single: agent declares a specific pre-known action. scoped: agent operates dynamically within declared bounds (e.g., ReAct loop). When scoped, scopeBounds MUST be provided."
    },
    "scopeBounds": {
      "type": "object",
      "description": "Required when executionMode is scoped. Defines the operating envelope. Individual actions within the scope are still evaluated against SafetyPolicies.",
      "required": ["permittedActions", "permittedTargetPatterns", "timeBoundSeconds"],
      "properties": {
        "permittedActions": {
          "type": "array",
          "items": { "type": "string" },
          "description": "Action types the agent is permitted to take within this scope."
        },
        "permittedTargetPatterns": {
          "type": "array",
          "items": { "type": "string" },
          "description": "URI glob patterns the agent is permitted to operate on."
        },
        "timeBoundSeconds": {
          "type": "integer",
          "minimum": 1,
          "description": "Maximum duration for the scoped operation. The control plane MUST revoke the scope after this period."
        }
      }
    },
    "cascadeModel": {
      "$ref": "#/$defs/CascadeModel"
    },
    "reasoningTrace": {
      "$ref": "#/$defs/ReasoningTrace"
    },
    "parameters": {
      "type": "object",
      "description": "Action-specific parameters (e.g., {\"replicas\": 5} for scale, {\"config\": {...}} for update).",
      "additionalProperties": true
    },
    "humanApproval": {
      "type": "object",
      "description": "Written by a human reviewer when a policy requires manual approval. The control plane drives the state machine when this field is set.",
      "required": ["decision", "forGeneration"],
      "properties": {
        "decision": {
          "type": "string",
          "enum": ["approved", "denied"],
          "description": "The reviewer's explicit decision."
        },
        "reason": {
          "type": "string",
          "description": "Optional free-text justification."
        },
        "forGeneration": {
          "type": "integer",
          "description": "The EvaluationGeneration this decision was made against. The control plane MUST reject the approval if this does not match status.evaluationGeneration, preventing replay of stale approvals after a state.drifted event."
        }
      }
    }
  },
  "$defs": {
    "CascadeModel": {
      "type": "object",
      "required": ["affectedTargets"],
      "properties": {
        "affectedTargets": {
          "type": "array",
          "items": {
            "type": "object",
            "required": ["uri", "effectType"],
            "properties": {
              "uri": { "type": "string" },
              "effectType": {
                "type": "string",
                "enum": ["deleted", "modified", "disrupted", "orphaned"]
              }
            }
          }
        },
        "modelSourceTrust": {
          "type": "string",
          "enum": ["authoritative", "derived", "inferred"],
          "description": "Trust tier of the causal model source."
        },
        "modelSourceID": {
          "type": "string",
          "description": "Specific identifier for the causal model (e.g., 'cmdb-v2.3')."
        }
      }
    },
    "ReasoningTrace": {
      "type": "object",
      "required": ["confidenceScore", "traceReference"],
      "properties": {
        "confidenceScore": {
          "type": "number",
          "minimum": 0.0,
          "maximum": 1.0,
          "description": "Overall confidence in the intended action."
        },
        "componentConfidence": {
          "type": "object",
          "description": "Structured mapping of confidence scores by reasoning component.",
          "additionalProperties": { "type": "number" }
        },
        "calibrationEvidence": {
          "type": "string",
          "description": "Signed performance metrics or benchmarks."
        },
        "traceReference": {
          "type": "string",
          "description": "Link to the reasoning log or chain-of-thought trace."
        },
        "alternatives": {
          "type": "array",
          "items": { "type": "string" },
          "description": "Alternative actions considered by the agent."
        }
      }
    }
  }
}
```

### 9.2 SafetyPolicy

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "SafetyPolicy",
  "type": "object",
  "required": ["apiVersion", "kind", "id", "targetSelector", "rules"],
  "properties": {
    "apiVersion": {
      "type": "string",
      "const": "aip/v1alpha1"
    },
    "kind": {
      "type": "string",
      "const": "SafetyPolicy"
    },
    "id": {
      "type": "string",
      "description": "Unique policy identifier."
    },
    "targetSelector": {
      "type": "object",
      "description": "Determines which targets this policy applies to.",
      "properties": {
        "matchAttributes": {
          "type": "object",
          "description": "Key-value pairs that must match the target's attributes.",
          "additionalProperties": { "type": "string" }
        },
        "matchResourceTypes": {
          "type": "array",
          "items": { "type": "string" },
          "description": "Resource types this policy applies to (e.g., ['Instance', 'Service'])."
        },
        "matchActions": {
          "type": "array",
          "items": { "type": "string" },
          "description": "Actions this policy applies to (e.g., ['delete', 'restart']). If omitted, applies to all actions."
        }
      }
    },
    "rules": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "required": ["name", "type", "action"],
        "properties": {
          "name": {
            "type": "string",
            "description": "Human-readable rule identifier."
          },
          "type": {
            "type": "string",
            "enum": ["StateEvaluation", "TimeWindow", "RateLimit"],
            "description": "The kind of check to perform."
          },
          "action": {
            "type": "string",
            "enum": ["Allow", "Deny", "Log", "RequireApproval"],
            "description": "Outcome if the rule triggers."
          },
          "message": {
            "type": "string",
            "description": "Human-readable explanation of the rule."
          },
          "expression": {
            "type": "string",
            "description": "Policy expression in the implementation's supported policy language (e.g., CEL, Rego). The expression receives the AgentRequest and target state as input and MUST return a boolean."
          },
          "config": {
            "type": "object",
            "description": "Type-specific configuration. Schema depends on the rule type.",
            "additionalProperties": true
          },
          "allowCompensating": {
            "type": "boolean",
            "default": false,
            "description": "If true, this rule is bypassed when the AgentRequest carries isCompensating: true. Intended for TimeWindow and RateLimit rules that should not block rollback operations during change freezes. MUST NOT be set on Deny rules — safety Deny rules apply to compensating actions unconditionally."
          }
        }
      }
    },
    "failureMode": {
      "type": "string",
      "enum": ["FailClosed", "FailOpen"],
      "default": "FailClosed",
      "description": "Behavior when policy evaluation dependencies are unreachable."
    },
    "cascadeDepth": {
      "type": "integer",
      "minimum": 0,
      "default": 3,
      "description": "Maximum number of dependency hops the control plane will traverse when evaluating cascade effects for targets matching this policy. A value of 0 disables cascade evaluation for this policy. MUST be explicitly documented by the implementation."
    }
  }
}
```

### 9.3 AuditRecord

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "AuditRecord",
  "type": "object",
  "required": ["apiVersion", "kind", "timestamp", "agentIdentity", "agentRequestID", "event"],
  "properties": {
    "apiVersion": {
      "type": "string",
      "const": "aip/v1alpha1"
    },
    "kind": {
      "type": "string",
      "const": "AuditRecord"
    },
    "timestamp": {
      "type": "string",
      "format": "date-time",
      "description": "RFC 3339 formatted timestamp."
    },
    "agentIdentity": {
      "type": "string"
    },
    "agentRequestID": {
      "type": "string",
      "description": "Reference to the AgentRequest this record pertains to."
    },
    "event": {
      "type": "string",
      "enum": [
        "request.submitted",
        "request.approved",
        "request.denied",
        "request.executing",
        "request.completed",
        "request.failed",
        "request.revoked",
        "lock.acquired",
        "lock.released",
        "lock.expired",
        "plan.activated",
        "plan.completed",
        "plan.aborted",
        "plan.timeout",
        "plan.rolling_back",
        "policy.evaluated",
        "cascade.mismatch",
        "heartbeat.timeout",
        "state.drifted",
        "state.drifted.verify"
      ],
      "description": "The specific event that generated this record. Use `state.drifted` for T1→T2 drift detected at human approval time; use `state.drifted.verify` for T2→T3 drift detected by a `/verify` call."
    },
    "phaseTransition": {
      "type": "object",
      "properties": {
        "from": { "type": "string" },
        "to": { "type": "string" }
      },
      "description": "The state transition that occurred."
    },
    "policyEvaluations": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["policyID", "ruleName", "result"],
        "properties": {
          "policyID": { "type": "string" },
          "ruleName": { "type": "string" },
          "result": {
            "type": "string",
            "enum": ["Allow", "Deny", "Log", "RequireApproval", "EvaluationFailed"]
          },
          "reason": { "type": "string" }
        }
      }
    },
    "lockStatus": {
      "type": "object",
      "properties": {
        "lockID": { "type": "string" },
        "targetURI": { "type": "string" },
        "event": {
          "type": "string",
          "enum": ["acquired", "released", "expired", "contention"]
        }
      }
    },
    "annotations": {
      "type": "object",
      "description": "Additional metadata (e.g., CASCADE_MISMATCH details).",
      "additionalProperties": { "type": "string" }
    },
    "details": {
      "type": "object",
      "description": "Event-specific details (e.g., denial reason, heartbeat data, revocation cause).",
      "additionalProperties": true
    }
  }
}
```

### 9.4 OpsLock

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "OpsLock",
  "type": "object",
  "required": ["apiVersion", "kind", "targetURI", "lockedBy", "leaseExpiresAt"],
  "properties": {
    "apiVersion": {
      "type": "string",
      "const": "aip/v1alpha1"
    },
    "kind": {
      "type": "string",
      "const": "OpsLock"
    },
    "targetURI": {
      "type": "string",
      "description": "The URI of the target resource being locked."
    },
    "lockedBy": {
      "type": "object",
      "required": ["agentIdentity", "agentRequestID"],
      "properties": {
        "agentIdentity": { "type": "string" },
        "agentRequestID": { "type": "string" },
        "swarmIdentity": { "type": "string", "description": "Optional coordinator ID for delegation." }
      }
    },
    "lockMode": {
      "type": "string",
      "enum": ["exclusive", "shared"],
      "default": "exclusive"
    },
    "leaseExpiresAt": {
      "type": "string",
      "format": "date-time",
      "description": "RFC 3339 timestamp when the lock will automatically be released."
    }
  }
}
```

### 9.5 IntentPlan (Extended Conformance)

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "IntentPlan",
  "type": "object",
  "required": ["apiVersion", "kind", "planID", "agentIdentity"],
  "if": {
    "properties": { "planningMode": { "const": "static" } }
  },
  "then": {
    "required": ["steps"],
    "comment": "static plans must declare all steps upfront for forward state validation."
  },
  "else": {
    "comment": "dynamic plans submit steps incrementally; steps array is empty or absent at submission time."
  },
  "properties": {
    "apiVersion": {
      "type": "string",
      "const": "aip/v1alpha1"
    },
    "kind": {
      "type": "string",
      "const": "IntentPlan"
    },
    "planID": {
      "type": "string",
      "description": "Globally unique plan identifier."
    },
    "agentIdentity": {
      "type": "string"
    },
    "planningMode": {
      "type": "string",
      "enum": ["static", "dynamic"],
      "default": "static",
      "description": "static: all steps declared upfront, validated via forward state simulation before approval. dynamic: steps submitted incrementally as agent observes results (e.g., ReAct); control plane approves scope and constraints, not the full step sequence."
    },
    "maxPlanDurationSeconds": {
      "type": "integer",
      "minimum": 1,
      "description": "Maximum wall-clock seconds the plan may remain Active. REQUIRED for dynamic plans; RECOMMENDED for static plans. The control plane MUST abort the plan and release all OpsLocks if this duration is exceeded."
    },
    "steps": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["stepOrder", "agentRequestRef"],
        "properties": {
          "stepOrder": { "type": "integer" },
          "agentRequestRef": { "type": "string" },
          "dependsOn": {
            "type": "array",
            "items": { "type": "integer" },
            "description": "List of stepOrders that must complete before this step."
          },
          "compensatingAction": {
            "type": "string",
            "description": "Reference to an AgentRequest ID to execute upon rollback."
          }
        }
      }
    }
  }
}
```

### 9.6 AgentTrustProfile (Extended Conformance)

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "AgentTrustProfile",
  "type": "object",
  "required": ["apiVersion", "kind", "agentIdentity", "totalRequests", "successRate", "lastUpdated"],
  "properties": {
    "apiVersion": {
      "type": "string",
      "const": "aip/v1alpha1"
    },
    "kind": {
      "type": "string",
      "const": "AgentTrustProfile"
    },
    "agentIdentity": {
      "type": "string",
      "description": "The agent identity this profile belongs to. MUST match the agentIdentity field on AgentRequests."
    },
    "totalRequests": {
      "type": "integer",
      "minimum": 0,
      "description": "Total number of AgentRequests observed for this identity."
    },
    "successRate": {
      "type": "number",
      "minimum": 0.0,
      "maximum": 1.0,
      "description": "Exponential moving average of terminal outcomes. 1.0 = always Completed, 0.0 = always Failed."
    },
    "meanClaimedConfidence": {
      "type": "number",
      "minimum": 0.0,
      "maximum": 1.0,
      "description": "Rolling mean of confidenceScore declared by this agent across all requests that included a ReasoningTrace."
    },
    "calibrationError": {
      "type": "number",
      "minimum": 0.0,
      "maximum": 1.0,
      "description": "Mean absolute error between claimed confidenceScore and actual binary outcome. 0.0 = perfectly calibrated, 1.0 = maximally dishonest."
    },
    "calibrationDrift": {
      "type": "number",
      "description": "Rolling delta of calibrationError. Positive = agent is becoming less calibrated over time. Negative = improving."
    },
    "windowSize": {
      "type": "integer",
      "minimum": 1,
      "description": "Number of most recent requests used in rolling metric calculations."
    },
    "coldStart": {
      "type": "boolean",
      "description": "True if totalRequests is below the configured minSampleSize threshold. When true, coldStartPolicy applies."
    },
    "lastUpdated": {
      "type": "string",
      "format": "date-time",
      "description": "RFC 3339 timestamp of the most recent profile update."
    }
  }
}
```

The following additional event types MUST be supported in `AuditRecord.event` when the corresponding Extended Conformance feature is implemented:

| Event | Feature | Trigger |
|-------|---------|---------|
| `plan.timeout` | IntentPlan max duration (Section 3.5) | Emitted when the control plane aborts a plan because `maxPlanDurationSeconds` was exceeded. Distinct from `plan.aborted` (which is emitted when a step fails). The `details` field MUST include `planID`, `maxPlanDurationSeconds`, and `elapsedSeconds`. |
| `state.drifted` | TOCTOU Protection — T1→T2 (Section 3.6.2) | Emitted when the StateFingerprint at **human approval time** differs from the fingerprint captured at policy evaluation time. The `details` field MUST include `previousFingerprint`, `currentFingerprint`, `evaluationGeneration`, and `"window": "T1T2"`. |
| `state.drifted.verify` | TOCTOU Protection — T2→T3 (Section 4.4) | Emitted when a `/verify` call detects that the StateFingerprint has changed between human approval and the partner SDK's pre-execution check. The `details` field MUST include `previousFingerprint`, `currentFingerprint`, `evaluationGeneration`, and `"window": "T2T3"`. |
| `agent.trustprofile.updated` | AgentTrustProfile (Section 3.7) | Emitted whenever an `AgentTrustProfile` is recalculated after a terminal `AgentRequest` transition |

## 10. Future Considerations

The following topics are deferred to future specification revisions. Each is scoped with rationale for deferral:

- **Agent Capability Negotiation**: Allowing agents to advertise their capabilities and the control plane to match requests to capable agents. Deferred pending real-world usage patterns from initial implementations.
- **Cross-Environment Federation**: Extending governance across multiple control planes or environments. Deferred due to complexity of distributed consensus for OpsLocks across network boundaries.
- **Policy Versioning and Migration**: Strategies for evolving `SafetyPolicies` without disrupting active agents. Deferred pending stabilization of the SafetyPolicy schema.

## Appendix A. Implementation Notes

### A.1 Cross-Standard Integration Guidance (Non-Normative)

**Bridging the MCP / AIP Chasm:**
The Model Context Protocol (MCP) standardizes how agents interact with tools, while AIP standardizes how infrastructure actions are governed. When an MCP tool implies infrastructure mutation, the ideal hand-off occurs within the tool execution boundary:
1. The Agent invokes an MCP tool (e.g., `call_tool("restart_pod", {"pod_id": "api-123"})`).
2. The MCP Server hosting that tool authenticates the agent.
3. *Crucially, the tool does not immediately execute the restart.* Instead, the tool acts as an AIP Client. It generates an `AgentRequest`, injects the authenticated `AgentIdentity`, and submits it to the AIP Control Plane.
4. The MCP tool waits for the `AgentRequest` to reach `Approved` before executing the actual infrastructure change. Once complete, it signals `Completed` to AIP and returns the result (along with any audit trace IDs) back to the Agent via MCP.

This pattern allows framework authors to adopt AIP governance without rewriting agent reasoning loops.

### A.2 Target URI Conventions (Non-Normative)

While Section 3.1 dictates that `Target.URI` formats are implementation-defined, implementations SHOULD adopt recognizable, hierarchical schemes to improve cross-platform interoperability.

Examples of reasonable URI schemes include:
- **Kubernetes**: `k8s://<cluster-name>/<namespace>/<kind>/<name>`
  - *Example*: `k8s://prod-us-east/default/deployment/payment-api`
- **AWS**: `aws://<accountId>/<region>/<service>/<resourceType>/<resourceId>`
  - *Example*: `aws://123456789012/us-west-2/ec2/instance/i-0abcd1234efgh5678`
- **Azure**: Valid Azure Resource Manager (ARM) IDs.
  - *Example*: `/subscriptions/<subId>/resourceGroups/<rgName>/providers/Microsoft.Compute/virtualMachines/<vmName>`
- **GitHub**: `github://{org}/{repo}/files/{branch}/{path}`
  - `{branch}` must be percent-encoded if it contains `/` (e.g., `feature/foo` → `feature%2Ffoo`). Parsers split on the first unencoded `/` after `files/` to separate branch from path, then decode `%2F` in the branch segment.
  - *Example (simple branch)*: `github://myorg/infra/files/main/nodepools/us-east-1.yaml`
  - *Example (slash branch)*: `github://myorg/infra/files/feature%2Ffoo/nodepools/us-east-1.yaml`

### A.3 Reference Bindings
This specification is accompanied by platform-specific reference bindings that demonstrate how AIP abstractions map to concrete infrastructure platforms. These bindings are informational and not normative:

- **Kubernetes Binding**: Maps AIP entities to Custom Resource Definitions (CRDs) under the `governance.aip.example.com` API group. `AgentRequest`, `SafetyPolicy`, and `OpsLock` are implemented as namespaced CRDs with status subresources. The Kubernetes binding uses ownerReferences for cascade graph resolution and label selectors for `TargetSelector` matching. *(Note: The reference implementation for this binding is housed in a separate repository.)*

Additional platform bindings (e.g., AWS, Azure, bare-metal) are expected as the specification matures.

### A.4 Conformance Testing
A conformance test suite is planned for the next specification revision. The test suite will validate:
- Core lifecycle: submit → evaluate → approve/deny → complete/fail.
- FailClosed behavior when policy evaluation dependencies are unavailable.
- OpsLock acquisition, lease expiration, and automatic release.
- AuditRecord generation for all state transitions.
- Denial response structure and error code correctness.

Crucially, the next iteration of the test suite will emphasize **Adversarial Edge Cases**, specifically testing how the implementation behaves when encountering requests generated via known "LLM Hallucination" patterns, such as poisoned `CascadeModels` or extreme lock starvation requests.

Implementations SHOULD self-report their conformance level (Core or Extended) and which Extended features they support.
