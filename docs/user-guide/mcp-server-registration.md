# Registering an MCP Server

## What is an MCPServer CRD?

An `MCPServer` custom resource registers an upstream MCP server (GitHub, Jira,
Azure DevOps, etc.) with the AIP governance plane. Once registered, the AIP
controller automatically discovers the server's tools by calling its
`tools/list` endpoint. The gateway serves the unified tool list to agents and
enforces governance policies on write tools.

## Prerequisites

You need a Kubernetes Secret containing the bearer token for the upstream MCP
server:

```shell
kubectl create secret generic aip-github-token \
  --namespace aip-k8s-system \
  --from-literal=token=<your-github-pat>
```

## Register the Server

Apply an `MCPServer` object. Here is a real sample:

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: MCPServer
metadata:
  name: github
spec:
  url: http://github-mcp-server.aip-k8s-system.svc.cluster.local
  secretNamespace: aip-k8s-system
  bearerTokenSecretRef:
    name: aip-github-token
    key: token
  readOnlyTools:
    - get_file_contents
    - list_pull_requests
    - search_issues
```

The `readOnlyTools` list declares tools that do **not** create an AgentRequest.
These are read operations that bypass the approval flow and pass through to the
upstream immediately. All other tools are treated as write tools and require
governance — they create an AgentRequest and wait for policy evaluation and
optional human approval.

## Verify Registration

Confirm the controller discovered the tools:

```shell
kubectl get mcpserver github -o jsonpath='{.status.tools[*].name}'
```

Expected output:

```
create_pull_request get_file_contents list_pull_requests search_issues
```

Check the sync status:

```shell
kubectl get mcpserver github
```

The `Synced` column shows whether the controller successfully connected to the
upstream server and discovered its tools.

## List Available Tools from the Gateway

Once the server is registered and synced, agents can discover the tools via MCP:

1.  Initialize a session:

    ```shell
    curl -s -X POST http://localhost:8080/mcp \
      -H "Content-Type: application/json" \
      -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}'
    ```

    Capture the `Mcp-Session-Id` from the response headers.

2.  List tools:

    ```shell
    curl -s -X POST http://localhost:8080/mcp \
      -H "Content-Type: application/json" \
      -H "Mcp-Session-Id: <session-id>" \
      -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
    ```

    Tools are returned with **prefixed names** that include the server name:

    ```json
    {
      "tools": [
        { "name": "github/create_pull_request",  "inputSchema": { ... } },
        { "name": "github/get_file_contents",     "inputSchema": { ... } },
        { "name": "github/list_pull_requests",    "inputSchema": { ... } },
        { "name": "github/search_issues",         "inputSchema": { ... } }
      ]
    }
    ```

## Read-Only vs Write Tools

- **Read-only tools** (listed in `spec.readOnlyTools`) bypass governance
  entirely. The gateway forwards the call to the upstream immediately and
  returns the result.
- **Write tools** (all other tools) create an AgentRequest. The request goes
  through policy evaluation, and if required, human approval. Only after
  approval is the call forwarded to the upstream server.

## Token Rotation

When the bearer token in the referenced Secret changes, the controller
automatically re-reads the Secret and reconnects to the upstream server within
seconds. No changes to the `MCPServer` object are needed — just update the
Secret.

```shell
kubectl delete secret aip-github-token -n aip-k8s-system
kubectl create secret generic aip-github-token \
  --namespace aip-k8s-system \
  --from-literal=token=<new-token>
```
