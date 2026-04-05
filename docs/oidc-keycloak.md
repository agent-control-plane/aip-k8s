# OIDC Authentication with Keycloak

This guide configures the AIP gateway to validate JWT tokens issued by
[Keycloak](https://www.keycloak.org) running inside a Kind cluster. No external
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
  Keycloak  (keycloak.keycloak.svc.cluster.local:8080)
      │
      │  access_token JWT  (azp = client_id, aud includes "aip-gateway")
      ▼
  AIP Gateway  (--oidc-issuer-url=http://keycloak.keycloak.svc.cluster.local:8080/realms/aip)
      │
      │  fetches /.well-known/openid-configuration at startup
      │  verifies JWT signature + aud + identity claim on every request
      ▼
  AgentRequest created
```

The gateway discovers Keycloak's JWKS endpoint automatically via OIDC
discovery. It extracts the caller identity from the `azp` (authorized party)
claim — Keycloak sets this to the `client_id` automatically in every
`client_credentials` token, with no custom mapper required. The identity is
then matched against `--agent-subjects` or `--reviewer-subjects`.

The identity claim is configurable via `--oidc-identity-claim` (default `azp`)
so the same gateway works with any OIDC-compliant provider. See the
[Provider compatibility](#provider-compatibility) section.

> **Why Keycloak, not Dex?** Dex is an identity broker — it federates upstream
> providers (LDAP, GitHub) but does not issue tokens for M2M flows itself.
> `client_credentials` grant requires a first-class IdP that owns identities.
> Keycloak is that IdP.

---

## Prerequisites

- Kind cluster running (`make setup-test-e2e` or `kind create cluster`)
- `kubectl` configured against it
- AIP deployed (`helm install aip-k8s charts/aip-k8s`)

---

## Step 1 — Deploy Keycloak inside Kind

A minimal manifest for Keycloak in dev mode (H2 in-memory database, no
PostgreSQL required) is included in the repository:

```bash
kubectl apply -f test/fixtures/keycloak-dev.yaml
```

Wait for Keycloak to be ready (first image pull takes ~60s):

```bash
kubectl rollout status deployment/keycloak -n keycloak --timeout=3m
```

Verify OIDC discovery from inside the cluster:

```bash
kubectl run -it --rm kc-check \
  --image=curlimages/curl:latest \
  --restart=Never \
  --namespace=keycloak \
  -- curl -s http://keycloak.keycloak.svc.cluster.local:8080/realms/master/.well-known/openid-configuration
```

You should see `"issuer":"http://keycloak.keycloak.svc.cluster.local:8080/realms/master"`.

---

## Step 2 — Configure Keycloak

Port-forward the admin console to your local machine:

```bash
kubectl port-forward svc/keycloak 8090:8080 -n keycloak &
```

### Get an admin token

```bash
ADMIN_TOKEN=$(curl -s -X POST http://localhost:8090/realms/master/protocol/openid-connect/token \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" \
  -d "grant_type=password" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])")
```

### Create the `aip` realm

```bash
curl -s -X POST http://localhost:8090/admin/realms \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"realm":"aip","enabled":true}' \
  -w "\nHTTP %{http_code}"
# Expected: HTTP 201
```

### Create clients

Each M2M identity is a Keycloak client with `serviceAccountsEnabled: true`.

```bash
# Agent identity
curl -s -X POST http://localhost:8090/admin/realms/aip/clients \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "clientId": "aip-agent-1",
    "enabled": true,
    "publicClient": false,
    "serviceAccountsEnabled": true,
    "standardFlowEnabled": false,
    "directAccessGrantsEnabled": false,
    "clientAuthenticatorType": "client-secret",
    "secret": "agent-1-secret"
  }' -w "\nHTTP %{http_code}"

# Reviewer identity
curl -s -X POST http://localhost:8090/admin/realms/aip/clients \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "clientId": "aip-reviewer-1",
    "enabled": true,
    "publicClient": false,
    "serviceAccountsEnabled": true,
    "standardFlowEnabled": false,
    "directAccessGrantsEnabled": false,
    "clientAuthenticatorType": "client-secret",
    "secret": "reviewer-1-secret"
  }' -w "\nHTTP %{http_code}"
```

### Add the audience mapper

The gateway uses the `azp` (authorized party) claim for identity — Keycloak
sets this to the `client_id` automatically in every `client_credentials` token,
no custom mapper needed. The only mapper required is the audience mapper to
add `aud = aip-gateway`.

Fetch each client's internal ID:

```bash
AGENT_ID=$(curl -s "http://localhost:8090/admin/realms/aip/clients?clientId=aip-agent-1" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['id'])")

REVIEWER_ID=$(curl -s "http://localhost:8090/admin/realms/aip/clients?clientId=aip-reviewer-1" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['id'])")
```

Add the audience mapper to each client:

```bash
for CLIENT_ID in $AGENT_ID $REVIEWER_ID; do
  curl -s -X POST "http://localhost:8090/admin/realms/aip/clients/$CLIENT_ID/protocol-mappers/models" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{
      "name": "audience-aip-gateway",
      "protocol": "openid-connect",
      "protocolMapper": "oidc-audience-mapper",
      "config": {
        "included.custom.audience": "aip-gateway",
        "id.token.claim": "true",
        "access.token.claim": "true"
      }
    }' -w "\nHTTP %{http_code}"
done
```

---

## Step 3 — Configure the AIP gateway

| Flag | Value | Purpose |
|------|-------|---------|
| `--oidc-issuer-url` | `http://keycloak.keycloak.svc.cluster.local:8080/realms/aip` | OIDC discovery and JWKS |
| `--oidc-audience` | `aip-gateway` | Expected `aud` claim in the token |
| `--oidc-identity-claim` | `azp` | Which claim holds the caller identity (default: `azp`) |
| `--agent-subjects` | `aip-agent-1` | Identity values allowed to act as agents |
| `--reviewer-subjects` | `aip-reviewer-1` | Identity values allowed to approve/deny |

```bash
helm upgrade aip-k8s charts/aip-k8s \
  --reuse-values \
  --set gateway.auth.oidcIssuerURL="http://keycloak.keycloak.svc.cluster.local:8080/realms/aip" \
  --set gateway.auth.oidcAudience="aip-gateway" \
  --set gateway.auth.agentSubjects="aip-agent-1" \
  --set gateway.auth.reviewerSubjects="aip-reviewer-1"
```

Confirm the gateway completed OIDC discovery:

```bash
kubectl logs deployment/aip-k8s-gateway -n aip-k8s-system
# Success: "Starting AIP Demo Gateway on :8080"
# Failure: "OIDC setup failed: oidc provider: ..."
```

---

## Step 4 — Fetch a token (agent)

Tokens must be fetched from **inside the cluster** so the `iss` claim matches
the in-cluster URL the gateway was configured with.

```bash
kubectl run -it --rm agent-token \
  --image=curlimages/curl:latest \
  --restart=Never \
  -- curl -s -X POST http://keycloak.keycloak.svc.cluster.local:8080/realms/aip/protocol/openid-connect/token \
    -d "grant_type=client_credentials" \
    -d "client_id=aip-agent-1" \
    -d "client_secret=agent-1-secret" \
    -d "scope=openid"
```

Response:

```json
{
  "access_token": "eyJhbGciOiJSUzI1NiIs...",
  "token_type":   "Bearer",
  "expires_in":   300
}
```

Use the `access_token` — it is the correct token for API access. Verify its
claims:

```bash
TOKEN=eyJhbGciOiJSUzI1NiIs...
echo $TOKEN | cut -d. -f2 | python3 -c "import sys,base64,json; p=sys.stdin.read().strip(); p+='='*(-len(p)%4); print(json.dumps(json.loads(base64.urlsafe_b64decode(p)), indent=2))" | grep -E '"iss"|"azp"|"aud"'
```

Expected:

```json
"iss": "http://keycloak.keycloak.svc.cluster.local:8080/realms/aip",
"azp": "aip-agent-1",
"aud": ["aip-gateway", "account"],
```

---

## Step 5 — Send an AgentRequest through the gateway

```bash
kubectl port-forward svc/aip-k8s-gateway 8080:8080 -n aip-k8s-system &

AGENT_TOKEN=eyJhbGciOiJSUzI1NiIs...   # use access_token

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
  "name":  "aip-agent-1-x7k2m",
  "phase": "Approved",
  "conditions": [...]
}
```

| Response | Meaning |
|----------|---------|
| 201 | Request created and resolved |
| 401 | Missing or invalid token |
| 403 | Token valid but identity not in `--agent-subjects` |

---

## Step 6 — Reviewer approval flow

```bash
kubectl run -it --rm reviewer-token \
  --image=curlimages/curl:latest \
  --restart=Never \
  -- curl -s -X POST http://keycloak.keycloak.svc.cluster.local:8080/realms/aip/protocol/openid-connect/token \
    -d "grant_type=client_credentials" \
    -d "client_id=aip-reviewer-1" \
    -d "client_secret=reviewer-1-secret" \
    -d "scope=openid"

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

```go
import (
    "encoding/json"
    "net/http"
    "net/url"
)

func fetchToken(keycloakURL, realm, clientID, clientSecret string) (string, error) {
    resp, err := http.PostForm(
        keycloakURL+"/realms/"+realm+"/protocol/openid-connect/token",
        url.Values{
            "grant_type":    {"client_credentials"},
            "client_id":     {clientID},
            "client_secret": {clientSecret},
            "scope":         {"openid"},
        })
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()
    var result map[string]interface{}
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return "", err
    }
    return result["access_token"].(string), nil
}
```

Store `client_secret` in a Kubernetes Secret — never hardcode it in agent source.

---

## Adding more agents

Add the client via the admin API (same steps as Step 2), then extend
`gateway.auth.agentSubjects`:

```bash
helm upgrade aip-k8s charts/aip-k8s \
  --reuse-values \
  --set gateway.auth.agentSubjects="aip-agent-1,aip-agent-2"
kubectl rollout restart deployment/aip-k8s-gateway -n aip-k8s-system
```

---

## Troubleshooting

**`OIDC setup failed: oidc provider: ...` on gateway startup**

Keycloak is not reachable or the realm does not exist:

```bash
kubectl get pods -n keycloak
kubectl logs deployment/keycloak -n keycloak | tail -20
kubectl run -it --rm dns-check --image=curlimages/curl:latest --restart=Never -- \
  curl -s http://keycloak.keycloak.svc.cluster.local:8080/realms/aip/.well-known/openid-configuration
```

**Gateway returns 401 `invalid token`**

The token's `iss` does not match `--oidc-issuer-url`. Tokens fetched via
port-forward carry the localhost URL. Fetch from inside the cluster instead.

```bash
echo $AGENT_TOKEN | cut -d. -f2 | python3 -c "import sys,base64,json; p=sys.stdin.read().strip(); p+='='*(-len(p)%4); print(json.dumps(json.loads(base64.urlsafe_b64decode(p)), indent=2))" | grep '"iss"'
# Must equal: "http://keycloak.keycloak.svc.cluster.local:8080/realms/aip"
```

**Gateway returns 403 `agent role required`**

The `azp` claim does not match any value in `--agent-subjects`. Decode the token:

```bash
echo $AGENT_TOKEN | cut -d. -f2 | python3 -c "import sys,base64,json; p=sys.stdin.read().strip(); p+='='*(-len(p)%4); print(json.dumps(json.loads(base64.urlsafe_b64decode(p)), indent=2))" | grep '"azp"'
# Must equal the value in --agent-subjects, e.g. "aip-agent-1"
```

**Gateway returns 401 `aud claim does not match`**

The `audience-aip-gateway` mapper was not applied. The `aud` array must
include `aip-gateway`.

---

## Production considerations

- **Secrets management** — replace static `secret:` values with Kubernetes
  Secrets or a vault integration.
- **TLS** — enable TLS on Keycloak and use `https://` in `--oidc-issuer-url`.
  Plain HTTP is acceptable only inside a cluster with NetworkPolicy isolating
  pod-to-pod traffic.
- **Token expiry** — Keycloak defaults to 5 minutes. Agents must refresh tokens
  before expiry.
- **Keycloak storage** — `start-dev` uses H2 in-memory storage, lost on restart.
  Use production mode with an external database for persistence.
- **Realm isolation** — use a dedicated realm (not `master`) for AIP clients.

---

## Provider compatibility

The gateway works with any OIDC-compliant provider. The only moving parts are
which claim carries the identity (`--oidc-identity-claim`) and what value
appears in the token's `aud` claim (`--oidc-audience`). These vary by provider.

| Provider | `--oidc-identity-claim` | `aud` in token | Notes |
|----------|------------------------|----------------|-------|
| **Keycloak** | `azp` (default) | Custom audience via mapper | Validated ✓ |
| **Okta** | `sub` | Authorization server URL by default; configure custom audience in Okta Authorization Server | Expected to work |
| **Auth0** | `sub` | The API identifier registered in Auth0 (set this as `--oidc-audience`) | Expected to work |
| **Azure AD** | `appid` | Application ID URI (e.g. `api://your-app-id`) | Expected to work; `appid` is non-standard but always present |
| **Ping Identity** | `sub` or `client_id` | Configurable per-application | Check your Ping policy |
| **Google** | Not applicable | Google's M2M mechanism differs | See note below |

> **Google service accounts**: Google does not support the standard
> `client_credentials` grant. M2M tokens are obtained by signing a JWT with a
> service account key and exchanging it at `https://oauth2.googleapis.com/token`.
> The resulting token is opaque (not a JWT) for Google APIs. To protect a
> custom API with Google identity, use
> [Google Identity Platform](https://cloud.google.com/identity-platform) which
> issues OIDC-compliant JWTs — set `--oidc-identity-claim=email` to use the
> service account email as the identity.

### Okta example

```bash
helm upgrade aip-k8s charts/aip-k8s \
  --reuse-values \
  --set gateway.auth.oidcIssuerURL="https://your-org.okta.com/oauth2/default" \
  --set gateway.auth.oidcAudience="api://aip-gateway" \
  --set gateway.auth.oidcIdentityClaim="sub" \
  --set gateway.auth.agentSubjects="0oa1b2c3d4e5f6g7h8i9" \
  --set gateway.auth.reviewerSubjects="0oa9i8h7g6f5e4d3c2b1"
```

The `sub` for Okta service apps is the application's client ID. Find it in the
Okta Admin Console under Applications → your app → General → Client ID.

### Auth0 example

```bash
helm upgrade aip-k8s charts/aip-k8s \
  --reuse-values \
  --set gateway.auth.oidcIssuerURL="https://your-tenant.auth0.com/" \
  --set gateway.auth.oidcAudience="https://aip-gateway.your-org.com" \
  --set gateway.auth.oidcIdentityClaim="sub" \
  --set gateway.auth.agentSubjects="YOUR_CLIENT_ID@clients" \
  --set gateway.auth.reviewerSubjects="YOUR_REVIEWER_CLIENT_ID@clients"
```

Auth0 sets `sub` to `<client_id>@clients` for machine-to-machine applications.
The `aud` must match the API identifier you registered in Auth0.

### Azure AD example

```bash
helm upgrade aip-k8s charts/aip-k8s \
  --reuse-values \
  --set gateway.auth.oidcIssuerURL="https://login.microsoftonline.com/YOUR_TENANT_ID/v2.0" \
  --set gateway.auth.oidcAudience="api://YOUR_APP_ID" \
  --set gateway.auth.oidcIdentityClaim="appid" \
  --set gateway.auth.agentSubjects="YOUR_AGENT_APP_ID" \
  --set gateway.auth.reviewerSubjects="YOUR_REVIEWER_APP_ID"
```

Azure AD uses `appid` (the application's client ID) rather than `azp` or `sub`
for service principals. The `aud` must match your application's ID URI.

---

### Diagnosing token claims for an unfamiliar provider

If you are unsure which claim your IdP uses for client identity, decode a token
and inspect the claims before configuring the gateway:

```bash
TOKEN=<your access_token>
echo $TOKEN | cut -d. -f2 | python3 -c "import sys,base64,json; p=sys.stdin.read().strip(); p+='='*(-len(p)%4); print(json.dumps(json.loads(base64.urlsafe_b64decode(p)), indent=2))"
```

Look for a claim that consistently equals the client application's registered
name or ID. Set `--oidc-identity-claim` to that claim name and
`--agent-subjects` to its value.

---

## Next steps

- [Auth0 OIDC setup](./oidc-auth0.md) — managed provider, same gateway flags.
- [SafetyPolicy reference](../spec.md#safety-policies) — restrict which agents
  can request which actions.
