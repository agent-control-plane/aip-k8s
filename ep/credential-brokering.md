# AIP Credential Brokering — Architecture Review

**Status**: Discussion / RFC

---

## Context

AIP (Agent Intent Protocol) is a governance control plane for autonomous AI agents. Agents
submit requests declaring what they want to do. AIP evaluates safety policies, optionally
requires human approval, and — on approval — the agent executes the action.

The AIP gateway is a full MCP server. Claude Code, CLI harnesses, or any MCP-native agent
points at `https://aip-gateway/mcp`. Every tool call goes through the gateway. This is the
enforcement chokepoint — there is no other path to the downstream systems.

Downstream systems today are Kubernetes clusters and GitHub. More systems will follow.

---

## The Problem

When the AIP gateway forwards a tool call to an upstream MCP server (K8s MCP, GitHub MCP),
it currently uses a single shared credential configured per MCPServer. The upstream system
sees one identity: the gateway's service account.

```
Agent A ──┐
Agent B ──┤── AIP Gateway ──► K8s MCP Server ──► K8s API
Agent C ──┘    shared token                      audit: gateway SA ← wrong
```

K8s audit logs, GitHub audit logs, SIEM tooling — all of these must show the acting
agent's identity, not the gateway's. This is a hard compliance requirement.

---

## Constraints

1. **Audit trail is non-negotiable.** Every downstream system must record the agent's
   identity, not the gateway's.
2. **The gateway is the enforcement point.** Agents reach downstream systems exclusively
   through the MCP gateway. Any design that bypasses the gateway breaks the governance
   guarantee.
3. **MCP servers are thin pass-throughs.** The K8s MCP server and GitHub MCP server simply
   forward whatever Bearer token they receive to the backing system. They have no credential
   store or resolution logic of their own.
4. **The solution must generalise.** K8s is the immediate focus but the design must
   accommodate any downstream system — SaaS APIs, cloud providers, internal services —
   without rewriting AIP for each one.

---

## Approaches Considered

### Approach 1: OIDC Passthrough

Forward the agent's inbound OIDC token (e.g. a Keycloak JWT) through the gateway to the
upstream MCP server, which passes it to the backing system.

**How it would work:**
The agent authenticates to AIP with its OIDC token. The gateway, rather than substituting
its own credential, forwards that same token to the upstream MCP server.

**Problems:**
- The gateway does not hold the agent's original OIDC token at call time. AIP issues its
  own internal JWTs (`iss: aip-gateway`) after the agent authenticates. The original
  Keycloak token is consumed at the authentication layer and not available downstream.
- Even if it were available, the K8s MCP server is a thin proxy: it forwards whatever
  Bearer token it receives directly to the K8s API server. The K8s API server would reject
  an AIP-internal JWT.
- Requires every target cluster to be configured to trust the AIP gateway as an OIDC
  issuer — significant cluster-side operational burden.

**Verdict:** Architecturally unsound for this setup. The assumption that a single token can
traverse multiple trust boundaries without transformation does not hold.

---

### Approach 2: Push Credential Resolution to MCP Servers

Define a credential resolution interface at the MCP server tier. The gateway forwards its
JWT (containing the agent's identity) to the MCP server. The MCP server validates it and
resolves per-agent credentials before calling its backing system.

**How it would work:**
Each MCP server receives the agent identity from the gateway and holds a local credential
store mapping agents to system-native tokens.

**Problems:**
- MCP servers in practice are thin proxies with no credential stores or resolution logic.
  Requiring every MCP server to implement this creates a distributed, inconsistent mess.
- Each MCP server would independently re-implement the same resolution pattern, each with
  its own failure modes and operational surface area.
- AIP loses central visibility and control over which credentials were used for a given
  approved action.

**Verdict:** Distributes the complexity without reducing it. Centralised governance implies
centralised credential resolution.

---

### Approach 3: Mint System-Native Tokens, Hand Off to Agent

After governance approval, AIP mints a system-native token (e.g. a K8s ServiceAccount
token) and hands it to the agent. The agent then executes directly against the backing
system using that token.

**How it would work:**
Post-approval, the gateway performs a K8s TokenRequest and returns the resulting SA token
to the agent. The agent calls `kubectl` or the K8s API directly with that token.

**Problems:**
- This breaks the enforcement guarantee. Once the agent holds a valid K8s SA token, it can
  call the K8s API directly — bypassing the gateway entirely.
- The governance proxy is the only reliable enforcement point for MCP-native agents. Moving
  execution outside the proxy makes governance advisory, not enforced.
- There is no way to scope the minted token to exactly the approved action.

**Verdict:** Fatally undermines the architecture. The proxy is the enforcement point; handing
system-native tokens to agents means the proxy is optional.

---

### Approach 4: Gateway as Credential Broker (Selected)

The gateway resolves a per-agent, per-service credential before forwarding each upstream
call. The agent never holds the downstream credential. The gateway stays in the call path.
Downstream systems see the agent's identity.

```
Agent ──► AIP Gateway ──► [resolve: upgrade-bot → SA token] ──► K8s MCP ──► K8s API
                                                                               audit: upgrade-bot ✓
```

**How it works:**
When an approved agent makes a tool call through the gateway, the gateway looks up the
credential binding for `(agentIdentity, service)`, resolves the appropriate token — either
from a secret store, by calling a system API, or via an external webhook — and substitutes
it for the shared MCPServer credential before forwarding the call.

**Credential resolution strategies (all implement the same interface):**

- **Static Secret** — read a bearer token from a K8s Secret. Covers GitHub PAT, Jira API
  key, or any SaaS token. Simple, works immediately.
- **Kubernetes ServiceAccount (TokenRequest)** — call the K8s TokenRequest API to mint a
  short-lived SA token scoped to the approved action. K8s audit log records the agent's SA.
  No cluster OIDC reconfiguration required.
- **External Webhook** — call `POST /resolve {agentIdentity, service}` and receive a token.
  The operator implements the webhook backed by Vault, CyberArk, or any credential
  infrastructure they already run. AIP remains narrow; the operator owns the integration.

**Pros:**
- Gateway stays in the call path — governance is enforced, not advisory.
- Downstream audit logs show the agent's identity on every system.
- Single credential resolution point — no distributed credential stores in MCP servers.
- Generalises to any downstream system via the webhook extension point.
- Agent never holds a system-native credential directly.

**Cons:**
- The gateway becomes a credential broker — additional complexity and a new failure mode
  (credential resolution failure blocks the tool call).
- Credential cache must be managed carefully (TTL, refresh, multi-replica consistency).
- Per-agent credential configuration is an operator burden: each agent must have its
  bindings declared before it can act on a service.

**Verdict:** The complexity is real and deliberate. The alternatives either break the audit
requirement or break the enforcement guarantee. This is the only approach that satisfies
both simultaneously.

---

## Open Questions

1. **Cache placement with horizontal scaling.** The credential cache currently lives
   in-process in the gateway. With multiple gateway replicas, each replica resolves
   independently. Is per-replica resolution acceptable, or is a shared cache (Redis, or
   similar) required? What are the failure semantics if the cache backend is unavailable?

2. **Token TTL vs action duration.** The gateway mints a short-lived credential per
   approval. If an action runs longer than the token TTL (e.g. a cluster upgrade taking
   45 minutes), does the gateway need to re-resolve and re-inject mid-execution? How does
   the agent know to use the refreshed token? Or should the token TTL be tied to the
   approved action's time bound?

3. **CRD ownership of outbound credential config.** Outbound credential bindings
   (`agentIdentity × service → credential strategy`) and inbound identity config (OIDC
   bindings, allowed subjects) are two distinct concerns. Should they live in the same
   object or separate CRDs? Coupling them is convenient but conflates "who is this agent"
   with "what credentials does it use on each system."
