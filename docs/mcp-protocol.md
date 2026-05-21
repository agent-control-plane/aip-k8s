# MCP Protocol Gateway

The AIP gateway exposes an MCP (Model Context Protocol) endpoint at `POST /mcp` that lets
AI agents call tools on upstream MCP servers while enforcing AIP governance. Agents interact
with the gateway as if it were a single MCP server; the gateway handles session management,
policy evaluation, and human-approval coordination transparently.

---

## How it works

```text
Agent ──POST /mcp──► Gateway ──evaluate GovernedResource + SafetyPolicy──► K8s
                        │
                 pending? ──► create AgentRequest (phase: Pending)
                        │         human approves → JWT minted
                        │
                approved? ──► forward tool call ──► upstream MCP server
                        │         complete AgentRequest (phase: Completed)
                        │
                denied? ──► return { status: "denied", reason: "..." }
```

Every `tools/call` on a write tool creates an `AgentRequest` in Kubernetes. The agent's
trust level and any matching `GovernedResource` / `SafetyPolicy` determine whether that
request is auto-approved, queued for human review, or rejected outright.

---

## MCP session setup

The gateway implements the [MCP 2025-03-26 streamable HTTP transport](https://spec.modelcontextprotocol.io/).

**Initialize** (required before any tool call):

```sh
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"my-agent","version":"1.0"}}}'
# Response sets Mcp-Session-Id header — include it on all subsequent requests.
```

**List tools**:

```sh
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: <session-id>" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
```

The tool list includes:

- All tools from all configured upstream MCP servers (prefixed with the server name,
  e.g. `github/create_pull_request`).
- The built-in `aip/await_approval` governance tool (see below).

Upstream session initialization is lazy — the first `tools/call` to a server triggers
its MCP handshake. `tools/list` always succeeds immediately with the statically configured
schema.

---

## Built-in tool: `aip/await_approval`

`aip/await_approval` is a native governance tool injected by the gateway. Agents call this
tool only when a write tool returns a `pending_approval` status — it is returned as a
follow-up instruction when a write tool requires human approval.

### Schema

```json
{
  "name": "aip/await_approval",
  "description": "Wait for an AIP governance request to be approved or denied. Returns the AIP JWT on approval for use as _aip_authorization in the subsequent tool call.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "requestId": {
        "type": "string",
        "description": "The AgentRequest ID returned in the pending_approval response"
      }
    },
    "required": ["requestId"]
  }
}
```

### Typical flow for a write tool

1. Agent calls `github/create_pull_request` via `POST /mcp`.
2. Gateway submits an `AgentRequest` and, if human approval is needed, returns:
   ```json
   {
     "status": "pending_approval",
     "requestId": "mcp-a3f2b1c0",
     "message": "This action requires approval. Call aip/await_approval with the requestId to wait for the decision."
   }
   ```
3. Agent calls `aip/await_approval` with `{"requestId": "mcp-a3f2b1c0"}`. The gateway
   long-polls the Kubernetes watch stream and blocks until the request is resolved.
4. On **approval**, the response contains:
   ```json
   {"status": "approved", "approvedBy": "alice", "requestId": "mcp-a3f2b1c0", "jwt": "<aip-jwt>"}
   ```
   The agent re-calls the original tool, passing the JWT as the `_aip_authorization`
   argument (for `POST /mcp` JSON-RPC clients):
   ```json
   {
     "jsonrpc": "2.0", "id": 3, "method": "tools/call",
     "params": {
       "name": "github/create_pull_request",
       "arguments": {
         "_aip_authorization": "<aip-jwt>",
         "owner": "acme", "repo": "infra", "title": "..."
       }
     }
   }
   ```
   For `POST /mcp-proxy` (REST) clients, pass the JWT as `X-AIP-Authorization: Bearer <aip-jwt>` instead.
5. On **denial**, the response contains:
   ```json
   {"status": "denied", "reason": "policy: replicas exceeds cap", "requestId": "mcp-a3f2b1c0"}
   ```
   The agent stops and surfaces the denial reason to the user.

Other terminal responses: `{"status": "expired"}`, `{"status": "failed"}`,
`{"status": "graded", "verdict": "correct", "reasonCode": "ok", "note": "..."}` (the last
one indicates a completed Observer-mode request that was graded).

---

## AIP-specific tool arguments

The gateway recognises three reserved argument keys that are stripped before the call is
forwarded to the upstream MCP server. They are available on any tool call via `POST /mcp`.

| Argument | Description |
|---|---|
| `_aip_authorization` | AIP JWT returned by `aip/await_approval`. Pass this to re-execute a write tool after approval instead of sending the `X-AIP-Authorization` header. |
| `_aip_target_uri` | Override the governance target URI that the gateway derives automatically from tool arguments. Use this when the tool arguments don't follow standard K8s or GitHub conventions (e.g. `"_aip_target_uri": "github://owner/repo/files/main/path/to/config.json"`). |
| `_aip_reason` | Custom reason string attached to the `AgentRequest`. Visible in the audit trail and the dashboard. Defaults to `"Agent tool call: <toolName>"` if omitted. |

### When to use `_aip_target_uri`

The gateway auto-derives the target URI from well-known argument names (`namespace`, `name`,
`kind`, `owner`, `repo`). For GitHub tools it builds `github://<owner>/<repo>`. For K8s tools
it builds `k8s://<namespace>/<kind>/<name>`.

Supply `_aip_target_uri` when:

- The tool operates on a **specific file or branch** and you want the `GovernedResource`
  selector to match precisely (e.g. `github://acme/infra/files/main/clusters/prod/config.json`).
- The tool arguments don't contain standard fields recognisable by the gateway.

```json
{
  "name": "github/create_pull_request",
  "arguments": {
    "_aip_target_uri": "github://acme/infra/files/main/clusters/prod/config.json",
    "owner": "acme",
    "repo": "infra",
    "title": "Bump replicas"
  }
}
```

---

## Configuring upstream MCP servers

Set the `MCP_REGISTRY` environment variable to a JSON array of server descriptors before
starting the gateway. Example:

```sh
export MCP_REGISTRY='[
  {
    "name": "github",
    "url": "http://localhost:8090",
    "bearer_token": "ghp_..."
  }
]'
./bin/gateway --addr :8080
```

| Field | Required | Description |
|---|---|---|
| `name` | Yes | Server identifier; becomes the tool name prefix (e.g. `github/create_pull_request`) |
| `url` | Yes | Base URL of the upstream MCP server |
| `bearer_token` | No | Token forwarded as `Authorization: Bearer` to the upstream server |

In Kubernetes / Helm, mount the token from a Secret and pass it via an environment variable:

```yaml
env:
  - name: MCP_REGISTRY
    valueFrom:
      secretKeyRef:
        name: mcp-registry-config
        key: registry.json
```

---

## Trust gate and policy enforcement

When a write tool is called via `POST /mcp`, the gateway runs the same trust gate and
safety policy evaluation as `POST /agent-requests`:

1. The tool name and arguments are used to construct a `targetURI` (e.g. `github://owner/repo/...`).
2. The gateway matches the URI against all `GovernedResource` objects.
3. If a match is found, the agent's `AgentTrustProfile` is checked against
   `trustRequirements.minTrustLevel`. If the agent's level is below the floor, the request
   routes to `AwaitingVerdict` instead of the normal approval flow.
4. `SafetyPolicy` rules are evaluated by the controller. A rule with `action: Deny`
   immediately denies the request; a rule with `action: RequireApproval` queues it for human
   review.

See [Trust Gate](trust-gate.md) and [Agent Graduation Ladder](agent-graduation-ladder.md) for
full details on these mechanisms.

---

## REST proxy (non-MCP clients)

For clients that cannot speak MCP JSON-RPC, the REST proxy at `POST /mcp-proxy/{server}/{tool}`
accepts a flat JSON body and returns the tool result directly.

```sh
curl -X POST http://localhost:8080/mcp-proxy/github/get_file_contents \
  -H "Authorization: Bearer $OIDC_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"owner":"acme","repo":"infra","path":"clusters/prod/config.json"}'
```

Write tools additionally require `X-AIP-Authorization: Bearer <aip-jwt>`.

The REST proxy does **not** expose `aip/await_approval` — it is only available via the
native MCP `POST /mcp` path.
