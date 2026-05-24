# Agent Intent Protocol Documentation

Welcome to the AIP Kubernetes Control Plane documentation. These guides explain how to
govern autonomous AI agents operating on critical infrastructure.

## What is AIP?

The [Agent Intent Protocol](https://github.com/agent-control-plane/agent-intent-protocol)
is an open standard that requires autonomous agents to **declare their intentions before
acting**. AIP decouples agent decision-making from system safety, giving operators:

- **Policy enforcement** — CEL-based rules that evaluate every request
- **Human-in-the-loop gates** — approval workflows for high-risk actions
- **Audit trails** — immutable records of every state transition
- **Earned autonomy** — agents graduate from Observer → Autonomous based on measured accuracy

## Getting started

Install the control plane in under a minute:

```bash
helm install aip-k8s \
  oci://ghcr.io/agent-control-plane/aip-k8s/charts/aip-k8s \
  --version 0.1.0 \
  --namespace aip-k8s-system \
  --create-namespace
```

Then follow the [Quick Start](./quick-start.md) to submit your first governed request
and see AIP block an agent operating on stale data.

> ⚠️ **Default install is dev mode** — no authentication. See [Dev Mode](./user-guide/dev-mode.md)
> and [Production Hardening](./user-guide/production-hardening.md) before deploying to a real cluster.

## Core concepts

| Guide | What you'll learn |
|---|---|
| [Agent Graduation Ladder](./agent-graduation-ladder.md) | How agents progress from Observer to Autonomous through measured accuracy |
| [Trust Gate](./trust-gate.md) | How the gateway enforces trust levels on every request |
| [Governed Resources](./governed-resources.md) | How to register infrastructure resources that agents may target |
| [Garbage Collection](./garbage-collection.md) | How to manage retention and export of diagnostic records |

## User guide

| Guide | What you'll learn |
|---|---|
| [Quick Start](./quick-start.md) | Install, verify, and submit your first request |
| [Dashboard Walkthrough](./dashboard.md) | Visual inspection of requests, policy decisions, and audit trails |
| [Dev Mode](./user-guide/dev-mode.md) | What the default install enables and disables |
| [Production Hardening](./user-guide/production-hardening.md) | OIDC, roles, JWT, and security checklist |
| [Scaledown Demo](./user-guide/demos/scaledown.md) | End-to-end: agent on stale metrics gets blocked |
| [Trust Graduation Demo](./user-guide/demos/graduation.md) | Watch an agent progress through all five trust levels |

## Authentication

| Guide | What you'll learn |
|---|---|
| [OIDC with Keycloak](./oidc-keycloak.md) | Configure JWT authentication with Keycloak (recommended for local dev) |

## Quick reference

### The five trust levels

| Level | Execution | Human approval |
|---|---|---|
| **Observer** | Graded only, no action | N/A (grading) |
| **Advisor** | Allowed | Required |
| **Supervised** | Allowed | Required |
| **Trusted** | Allowed | Auto-approved by policy |
| **Autonomous** | Allowed | Auto-approved by policy |

### Key CRDs

- `AgentRequest` — an agent declares intent to act on a target resource
- `GovernedResource` — platform engineering registers permitted resource types
- `SafetyPolicy` — CEL rules that further restrict what agents may do
- `AgentGraduationPolicy` — cluster admin defines accuracy thresholds per level
- `AgentTrustProfile` — controller-managed record of an agent's earned trust
- `DiagnosticAccuracySummary` — rolling accuracy computed from graded verdicts
- `AuditRecord` — immutable event log for every state transition

> **Resource access patterns:** `GovernedResource` and `SafetyPolicy` are managed
> through the gateway REST API as the primary path (`kubectl` is break-glass only).
> `AgentGraduationPolicy`, `AgentTrustProfile`, `DiagnosticAccuracySummary`, and
> `AuditRecord` have no gateway endpoints yet — inspect them with `kubectl`. See
> the [Trust Gate guide](trust-gate.md) for details.

## Support

- [GitHub Issues](https://github.com/agent-control-plane/aip-k8s/issues)
- [AIP Specification](https://github.com/agent-control-plane/agent-intent-protocol)
