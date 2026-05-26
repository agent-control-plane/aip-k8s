# Claude Code Integration

## Overview

Claude Code can use the AIP gateway as its MCP server. Every tool call that
Claude Code makes passes through AIP governance automatically — no changes to
Claude Code itself are needed. This means governance policies (permitted
actions, trust gates, human approval) apply seamlessly to AI-driven operations.

## Add AIP Gateway to Claude Code

Create or edit `~/.config/claude/claude_desktop_config.json` (or `.claude.json`
in dev mode):

```json
{
  "mcpServers": {
    "aip": {
      "type": "http",
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer <your-oidc-token>"
      }
    }
  }
}
```

!!! note
    In production, replace `localhost:8080` with the in-cluster gateway
    service URL, e.g.
    `http://aip-k8s-gateway.aip-k8s-system.svc.cluster.local:8080/mcp`.

## Authentication

If the gateway is running with `--oidc-issuer-url`, Claude Code must pass a
valid OIDC Bearer token in the `Authorization` header. Obtain a token from your
OIDC provider and set it in the configuration file.

In development mode (no `--oidc-issuer-url`), the `Authorization` header can be
omitted entirely — the gateway trusts the caller based on the `X-Remote-User`
header or a configured proxy trust boundary.

## Write Tools and Governance

When Claude Code calls a write tool (any tool not listed in
`readOnlyTools`):

1. The gateway creates an `AgentRequest` and runs governance policy evaluation.
2. If governance rules require human approval, the request waits in a
   `PendingApproval` state.
3. Claude Code calls the built-in `aip/await_approval` tool to poll for the
   verdict.
4. Once approved, the gateway forwards the tool call to the upstream MCP server
   and returns the result to Claude Code.

The `aip/await_approval` tool is injected by the gateway into every `tools/list`
response. It needs no special registration.

## Example Session

Discover tools via the AIP gateway:

```shell
# Initialize a session
curl -s -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}'

# List tools — notice the built-in aip/await_approval alongside upstream tools
curl -s -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: <session-id>" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
```

Response (abbreviated):

```json
{
  "tools": [
    { "name": "github/create_pull_request",  "inputSchema": { ... } },
    { "name": "github/get_file_contents",     "inputSchema": { ... } },
    { "name": "github/search_issues",         "inputSchema": { ... } },
    { "name": "aip/await_approval",           "inputSchema": { ... } }
  ]
}
```

## Configuring a GovernedResource for Claude Code

To control which operations Claude Code is allowed to perform, create a
`GovernedResource` that targets the relevant URI pattern and permits the
claude-code agent identity:

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: GovernedResource
metadata:
  name: github-infra
spec:
  uriPattern: github://**/infra/**
  permittedActions:
    - github/create_pull_request
    - github/get_file_contents
  trustRequirements:
    minTrustLevel: Trusted
    maxAutonomyLevel: Supervised
```

This configuration:

- Restricts Claude Code to the `github://**/infra/**` URI space.
- Only `create_pull_request` and `get_file_contents` are permitted.
- Trust level must be at least `Trusted` (automatic approval for known
  profiles).
- Maximum autonomy is `Supervised` — even trusted agents need human sign-off
  for the final execution.
