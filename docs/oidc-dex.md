# OIDC Authentication with Dex

This guide configures the AIP gateway to validate JWT tokens issued by
[Dex](https://dexidp.io) running inside a Kind cluster. No external
accounts or services required — everything runs in the cluster.

This is the recommended setup for local development and the reference
starting point before moving to a managed provider (Auth0, Okta, Google).

---

## How it works

```
  Agent pod  (client_id + client_secret)
      │
      │  POST /token  (client_credentials grant, in-cluster)
      ▼
     Dex  (dex.dex.svc.cluster.local:5556)
      │
      │  JWT  (iss = http://dex.dex.svc.cluster.local:5556, sub = client_id)
      ▼
  AIP Gateway  (--oidc-issuer-url=http://dex.dex.svc.cluster.local:5556)
      │
      │  fetches /.well-known/openid-configuration from Dex at startup
      │  verifies JWT signature + aud + sub on every request
      ▼
  AgentRequest created
```

The gateway discovers Dex's JWKS endpoint automatically via OIDC
discovery. The `sub` claim in the token (equal to the client ID) is
matched against `--agent-subjects` or `--reviewer-subjects`.

> **Kind note:** The Dex issuer URL must be resolvable from inside the
> cluster (where the gateway runs) and must match the `iss` claim in
> tokens. Using the in-cluster DNS name
> `http://dex.dex.svc.cluster.local:5556` satisfies both. Token fetching
> is done from inside the cluster via `kubectl run` — this mirrors how
> real agent pods operate.

---

## Prerequisites

- Kind cluster running (`make setup-test-e2e` or `kind create cluster`)
- `kubectl` configured against it
- `helm` v3+
- AIP deployed (`helm install aip-k8s charts/aip-k8s`)

---

## Step 1 — Deploy Dex inside Kind

Add the Dex Helm chart:

```bash
helm repo add dex https://charts.dexidp.io
helm repo update
```

Create `dex-values.yaml`. This config defines two static OAuth2 clients —
one agent and one reviewer. Static clients authenticate with a client
secret directly, with no user login required.

```yaml
# dex-values.yaml
config:
  # Must be the in-cluster DNS name. The gateway uses this URL for
  # OIDC discovery, and Dex embeds it as the `iss` claim in every token.
  issuer: http://dex.dex.svc.cluster.local:5556

  storage:
    type: memory

  oauth2:
    grantTypes:
      - client_credentials   # required for M2M agents (no user present)
      - authorization_code   # required for human reviewer flows later

  staticClients:
    # Agent identity — sub claim will be "aip-agent-1"
    - id: aip-agent-1
      secret: agent-1-secret
      name: AIP Agent 1
      public: false

    # Reviewer identity — sub claim will be "aip-reviewer-1"
    - id: aip-reviewer-1
      secret: reviewer-1-secret
      name: AIP Reviewer 1
      public: false

  enablePasswordDB: false

expiry:
  idTokens: 15m     # short-lived tokens; increase for local dev if needed
  signingKeys: 6h
```

Install Dex:

```bash
helm install dex dex/dex \
  --namespace dex \
  --create-namespace \
  --values dex-values.yaml
```

Wait for Dex to be ready:

```bash
kubectl rollout status deployment/dex -n dex --timeout=60s
```

Verify Dex is serving OIDC discovery:

```bash
kubectl run -it --rm dex-check \
  --image=curlimages/curl:latest \
  --restart=Never -- \
  curl -s http://dex.dex.svc.cluster.local:5556/.well-known/openid-configuration
```

Expected — the `issuer` field must match exactly:

```json
{
  "issuer": "http://dex.dex.svc.cluster.local:5556",
  "authorization_endpoint": "http://dex.dex.svc.cluster.local:5556/auth",
  "token_endpoint": "http://dex.dex.svc.cluster.local:5556/token",
  "jwks_uri": "http://dex.dex.svc.cluster.local:5556/keys",
  ...
}
```

---

## Step 2 — Configure the AIP gateway

The gateway needs four flags set:

| Flag | Value | Purpose |
|------|-------|---------|
| `--oidc-issuer-url` | `http://dex.dex.svc.cluster.local:5556` | Where to fetch OIDC discovery and JWKS |
| `--oidc-audience` | `aip-gateway` | Expected `aud` claim in tokens |
| `--agent-subjects` | `aip-agent-1` | `sub` values allowed to act as agents |
| `--reviewer-subjects` | `aip-reviewer-1` | `sub` values allowed to approve/deny |

If you installed AIP with the default Helm chart, upgrade with the OIDC values:

```bash
helm upgrade aip-k8s charts/aip-k8s \
  --reuse-values \
  --set gateway.auth.oidcIssuerURL="http://dex.dex.svc.cluster.local:5556" \
  --set gateway.auth.oidcAudience="aip-gateway" \
  --set gateway.auth.agentSubjects="aip-agent-1" \
  --set gateway.auth.reviewerSubjects="aip-reviewer-1"
```

Alternatively add them to a values override file:

```yaml
# values-override.yaml
gateway:
  auth:
    oidcIssuerURL: "http://dex.dex.svc.cluster.local:5556"
    oidcAudience: "aip-gateway"
    agentSubjects: "aip-agent-1"
    reviewerSubjects: "aip-reviewer-1"
```

```bash
helm upgrade aip-k8s charts/aip-k8s -f values-override.yaml
```

Restart the gateway and confirm it starts without OIDC errors:

```bash
kubectl rollout restart deployment/aip-k8s-gateway -n aip-k8s-system
kubectl rollout status  deployment/aip-k8s-gateway -n aip-k8s-system
kubectl logs -l app.kubernetes.io/component=gateway -n aip-k8s-system | head -5
```

A successful startup looks like:

```
2026/01/01 00:00:00 Starting AIP Demo Gateway on :8080
```

If OIDC discovery fails the process exits immediately with:

```
OIDC setup failed: oidc provider: ...
```

---

## Step 3 — Fetch a token (agent)

Agents fetch tokens from inside the cluster using the client credentials
grant. Run a temporary pod to simulate this:

```bash
kubectl run -it --rm agent-token \
  --image=curlimages/curl:latest \
  --restart=Never -- \
  curl -s -X POST http://dex.dex.svc.cluster.local:5556/token \
    -d "grant_type=client_credentials" \
    -d "client_id=aip-agent-1" \
    -d "client_secret=agent-1-secret" \
    -d "scope=openid" \
    -d "audience=aip-gateway"
```

Response:

```json
{
  "access_token": "eyJhbGciOiJSUzI1NiIs...",
  "token_type": "bearer",
  "expires_in": 900,
  "id_token": "eyJhbGciOiJSUzI1NiIs..."
}
```

Use the `id_token` — it contains the `sub` and `aud` claims the gateway
validates. The `access_token` is opaque and will be rejected.

Inspect the token claims to verify they are correct:

```bash
# Replace TOKEN with the id_token value
TOKEN=eyJhbGciOiJSUzI1NiIs...

echo $TOKEN | cut -d. -f2 | base64 -d 2>/dev/null | python3 -m json.tool
```

Expected claims:

```json
{
  "iss": "http://dex.dex.svc.cluster.local:5556",
  "sub": "aip-agent-1",
  "aud": "aip-gateway",
  "exp": 1234567890,
  ...
}
```

---

## Step 4 — Send an AgentRequest through the gateway

Forward the gateway port to your local machine:

```bash
kubectl port-forward svc/aip-k8s-gateway 8080:8080 -n aip-k8s-system &
```

Send a request using the agent token from Step 3:

```bash
AGENT_TOKEN=eyJhbGciOiJSUzI1NiIs...

curl -s -X POST http://localhost:8080/agent-requests \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "agentIdentity": "aip-agent-1",
    "action":        "scale-down",
    "targetURI":     "k8s://prod/default/deployment/payment-api",
    "reason":        "scheduled maintenance window"
  }'
```

Expected response (HTTP 201):

```json
{
  "name": "aip-agent-1-x7k2m",
  "phase": "Approved",
  "conditions": [...]
}
```

| Response | Meaning |
|----------|---------|
| 201 | Request created and resolved (Approved, Denied, or requires human) |
| 401 | Missing or invalid token |
| 403 | Token is valid but `sub` is not in `--agent-subjects` |

---

## Step 5 — Reviewer approval flow

Fetch a reviewer token using the `aip-reviewer-1` client:

```bash
kubectl run -it --rm reviewer-token \
  --image=curlimages/curl:latest \
  --restart=Never -- \
  curl -s -X POST http://dex.dex.svc.cluster.local:5556/token \
    -d "grant_type=client_credentials" \
    -d "client_id=aip-reviewer-1" \
    -d "client_secret=reviewer-1-secret" \
    -d "scope=openid" \
    -d "audience=aip-gateway"
```

Approve a pending request:

```bash
REVIEWER_TOKEN=eyJhbGciOiJSUzI1NiIs...
REQUEST_NAME=aip-agent-1-x7k2m

curl -s -X POST http://localhost:8080/agent-requests/$REQUEST_NAME/approve \
  -H "Authorization: Bearer $REVIEWER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"reason": "approved for maintenance window"}'
```

Reviewers cannot approve their own requests — the gateway enforces
self-approval prevention regardless of role.

---

## How an agent fetches tokens in code

In production an agent pod runs inside the cluster and fetches its own
token at startup (or refreshes before expiry). Example in Go:

```go
import "net/http"
import "net/url"

func fetchToken(dexURL, clientID, clientSecret string) (string, error) {
    resp, err := http.PostForm(dexURL+"/token", url.Values{
        "grant_type":    {"client_credentials"},
        "client_id":     {clientID},
        "client_secret": {clientSecret},
        "scope":         {"openid"},
        "audience":      {"aip-gateway"},
    })
    // parse resp.Body for id_token
}
```

Example in Python:

```python
import requests

def fetch_token(dex_url, client_id, client_secret):
    resp = requests.post(f"{dex_url}/token", data={
        "grant_type":    "client_credentials",
        "client_id":     client_id,
        "client_secret": client_secret,
        "scope":         "openid",
        "audience":      "aip-gateway",
    })
    return resp.json()["id_token"]

token = fetch_token(
    "http://dex.dex.svc.cluster.local:5556",
    "aip-agent-1",
    "agent-1-secret",
)
```

Store `client_secret` in a Kubernetes Secret and mount it as an
environment variable — never hardcode it in agent source.

---

## Adding more agents

Add the new client to `dex-values.yaml`:

```yaml
    - id: aip-agent-2
      secret: agent-2-secret
      name: AIP Agent 2
      public: false
```

Upgrade Dex:

```bash
helm upgrade dex dex/dex -n dex --values dex-values.yaml
```

Add the new client ID to `gateway.auth.agentSubjects` (comma-separated)
and restart:

```bash
helm upgrade aip-k8s charts/aip-k8s \
  --reuse-values \
  --set gateway.auth.agentSubjects="aip-agent-1,aip-agent-2"
kubectl rollout restart deployment/aip-k8s-gateway -n aip-k8s-system
```

---

## Troubleshooting

**`OIDC setup failed: oidc provider: ...` on gateway startup**

Dex is not reachable from the gateway pod. Check Dex is running:

```bash
kubectl get pods -n dex
kubectl logs deployment/dex -n dex
```

Verify in-cluster DNS resolves the Dex service:

```bash
kubectl run -it --rm dns-check --image=busybox --restart=Never -- \
  nslookup dex.dex.svc.cluster.local
```

**`unsupported_grant_type` from Dex token endpoint**

`client_credentials` is not enabled. Ensure `grantTypes` includes
`client_credentials` in `dex-values.yaml` and re-run `helm upgrade dex`.

**Gateway returns 401 `invalid token`**

The `aud` claim in the token does not match `--oidc-audience`. Decode the
token and compare:

```bash
echo $AGENT_TOKEN | cut -d. -f2 | base64 -d 2>/dev/null | python3 -m json.tool | grep -E '"aud"|"iss"'
```

Both `aud` and `iss` must match the gateway flags exactly.

**Gateway returns 403 `agent role required`**

The token's `sub` is not in `--agent-subjects`. Print the current gateway
args and compare:

```bash
kubectl get deployment aip-k8s-gateway -n aip-k8s-system \
  -o jsonpath='{.spec.template.spec.containers[0].args}' | tr ',' '\n'
```

---

## Production considerations

- **Secrets management** — replace static `secret:` values in
  `dex-values.yaml` with references to Kubernetes Secrets.
- **TLS** — enable TLS on Dex and use `https://` in
  `--oidc-issuer-url`. Plain HTTP is only acceptable inside a cluster
  with NetworkPolicy restricting pod-to-pod traffic.
- **Token expiry** — `15m` is a safe default. Agents must handle token
  refresh before expiry.
- **Dex storage** — `type: memory` loses all sessions on Dex restart.
  Use `type: kubernetes` for persistence.

---

## Next steps

- [Auth0 OIDC setup](./oidc-auth0.md) — managed provider, same gateway
  flags, for teams with an existing Okta/Auth0 tenant.
- [SafetyPolicy reference](../spec.md#safety-policies) — restrict which
  agents can request which actions.
