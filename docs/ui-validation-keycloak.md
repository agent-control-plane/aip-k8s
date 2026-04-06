# UI Validation Guide — Dashboard + Keycloak OIDC

This guide walks through manual validation of every dashboard feature against a live Keycloak instance. Run this after making UI changes to confirm nothing regressed.

**Prerequisites**: Keycloak is already deployed and the `aip` realm + clients are created. If not, complete `docs/oidc-keycloak.md` Steps 1–3 first.

---

## Setup: Add an Admin Client to Keycloak

The existing doc creates `aip-agent-1` and `aip-reviewer-1`. For dashboard validation you also need an admin identity. Run this once:

```bash
# Port-forward Keycloak if not already running
kubectl port-forward svc/keycloak 8090:8080 -n keycloak &

ADMIN_TOKEN=$(curl -s -X POST http://localhost:8090/realms/master/protocol/openid-connect/token \
  -d "client_id=admin-cli" -d "username=admin" -d "password=admin" \
  -d "grant_type=password" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])")

# Create aip-admin-1 client
curl -s -X POST http://localhost:8090/admin/realms/aip/clients \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "clientId": "aip-admin-1",
    "enabled": true,
    "publicClient": false,
    "serviceAccountsEnabled": true,
    "standardFlowEnabled": false,
    "clientAuthenticatorType": "client-secret",
    "secret": "admin-1-secret"
  }' -w "\nHTTP %{http_code}"

# Add audience mapper to aip-admin-1
ADMIN_CLIENT_ID=$(curl -s "http://localhost:8090/admin/realms/aip/clients?clientId=aip-admin-1" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['id'])")

curl -s -X POST "http://localhost:8090/admin/realms/aip/clients/$ADMIN_CLIENT_ID/protocol-mappers/models" \
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
```

Restart the gateway with all three roles configured:

```bash
helm upgrade aip-k8s charts/aip-k8s \
  --reuse-values \
  --set gateway.auth.oidcIssuerURL="http://keycloak.keycloak.svc.cluster.local:8080/realms/aip" \
  --set gateway.auth.oidcAudience="aip-gateway" \
  --set gateway.auth.oidcIdentityClaim="azp" \
  --set gateway.auth.agentSubjects="aip-agent-1" \
  --set gateway.auth.reviewerSubjects="aip-reviewer-1" \
  --set gateway.auth.adminSubjects="aip-admin-1"
```

---

## Fetch Tokens

Tokens must be fetched from inside the cluster (the `iss` claim must match the in-cluster URL).

```bash
# Open three terminals, one per identity
kubectl run -it --rm get-agent-token --image=curlimages/curl:latest --restart=Never -- \
  curl -s -X POST http://keycloak.keycloak.svc.cluster.local:8080/realms/aip/protocol/openid-connect/token \
  -d "grant_type=client_credentials" -d "client_id=aip-agent-1" \
  -d "client_secret=agent-1-secret" -d "scope=openid" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])"

kubectl run -it --rm get-reviewer-token --image=curlimages/curl:latest --restart=Never -- \
  curl -s -X POST http://keycloak.keycloak.svc.cluster.local:8080/realms/aip/protocol/openid-connect/token \
  -d "grant_type=client_credentials" -d "client_id=aip-reviewer-1" \
  -d "client_secret=reviewer-1-secret" -d "scope=openid" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])"

kubectl run -it --rm get-admin-token --image=curlimages/curl:latest --restart=Never -- \
  curl -s -X POST http://keycloak.keycloak.svc.cluster.local:8080/realms/aip/protocol/openid-connect/token \
  -d "grant_type=client_credentials" -d "client_id=aip-admin-1" \
  -d "client_secret=admin-1-secret" -d "scope=openid" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])"
```

Save each token:
```bash
AGENT_TOKEN=<paste agent access_token>
REVIEWER_TOKEN=<paste reviewer access_token>
ADMIN_TOKEN_GW=<paste admin access_token>   # renamed to avoid clash with Keycloak admin token
```

Start the dashboard and port-forward the gateway:

```bash
kubectl port-forward svc/aip-k8s-gateway 8080:8080 -n aip-k8s-system &
go run ./cmd/dashboard --gateway-url=http://localhost:8080 --port=8082
# Open http://localhost:8082
```

---

## Validation Checklist

Run each section in order. Each check has a **setup** (curl commands to create test data), **action** (what to do in the browser), and **expected result**.

---

### Section 1 — OIDC: Token Acquisition (Gap G-2)

**Check 1.1 — Unauthenticated state**
- Open `http://localhost:8082` with no token entered.
- **Expected**: "Not authenticated" banner is visible. Agent Requests list does not load (or shows auth error, not an empty list).

**Check 1.2 — Agent token accepted**
- Paste `$AGENT_TOKEN` into the token field. Submit.
- **Expected**: Agent Requests tab loads. No banner. Token stored in `sessionStorage` (verify in browser DevTools → Application → Session Storage).

**Check 1.3 — Expired token shows 401 banner**
```bash
# Keycloak default token TTL is 5 minutes. Wait for it to expire, or:
# Set token TTL to 10 seconds in Keycloak admin: Realm Settings → Tokens → Access Token Lifespan → 10
```
- Wait for expiry. Next poll triggers a `fetchRequests()` call.
- **Expected**: "Session expired — please re-enter your token" banner appears. Dashboard does not silently show stale data.

**Check 1.4 — Invalid token rejected**
- Paste the string `notavalidtoken` into the token field.
- **Expected**: Gateway returns 401. Dashboard shows authentication error, not a blank list.

---

### Section 2 — OIDC: Role Enforcement (Gap G-3)

**Check 2.1 — Agent sees no Approve/Deny buttons**
Setup: create a request needing approval (see Section 3 setup below).
- Log in with `$AGENT_TOKEN`.
- Select the pending request.
- **Expected**: No Approve or Deny buttons visible. "Awaiting reviewer approval" message shown instead.

**Check 2.2 — Reviewer sees Approve/Deny**
- Log in with `$REVIEWER_TOKEN`.
- Select the same pending request.
- **Expected**: Approve and Deny buttons are visible.

**Check 2.3 — Agent sees no Admin tab**
- Log in with `$AGENT_TOKEN`.
- **Expected**: No "Governed Resources" or "Safety Policies" tab in the nav bar.

**Check 2.4 — Admin sees Admin tabs**
- Log in with `$ADMIN_TOKEN_GW`.
- **Expected**: "Governed Resources" and "Safety Policies" tabs visible.

**Check 2.5 — Reviewer cannot access Admin tabs**
- Log in with `$REVIEWER_TOKEN`.
- **Expected**: Admin tabs not visible (or if visible, every action returns 403 with a clear message).

---

### Section 3 — Agent Requests Tab (existing + new fields)

**Setup**: create a pending request requiring approval.
```bash
# Create a GovernedResource first (as admin)
curl -s -X POST http://localhost:8080/governed-resources \
  -H "Authorization: Bearer $ADMIN_TOKEN_GW" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "nodepool-validation",
    "uriPattern": "k8s://prod/karpenter/nodepool/*",
    "permittedActions": ["scale-up", "scale-down"],
    "permittedAgents": ["aip-agent-1"],
    "contextFetcher": "none"
  }'

# Create a SafetyPolicy that requires approval for scale-up
curl -s -X POST http://localhost:8080/safety-policies \
  -H "Authorization: Bearer $ADMIN_TOKEN_GW" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "require-approval",
    "namespace": "default",
    "governedResourceSelector": {"matchLabels": {}},
    "rules": [{"name": "always-review", "type": "StateEvaluation", "action": "RequireApproval", "expression": "true"}]
  }'

# Submit the agent request
curl -s -X POST http://localhost:8080/agent-requests \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "agentIdentity": "aip-agent-1",
    "action": "scale-up",
    "targetURI": "k8s://prod/karpenter/nodepool/team-a",
    "reason": "peak traffic expected",
    "namespace": "default"
  }'
```

**Check 3.1 — governedResourceRef shown (Gap G-7)**
- Log in with `$REVIEWER_TOKEN`. Select the request.
- **Expected**: Detail panel shows "Governed by: nodepool-validation (generation 1)".

**Check 3.2 — Approve works end-to-end**
- Click Approve (no active endpoints, no reason needed).
- **Expected**: Request transitions to Approved. Phase badge updates in the list within one poll cycle (3s).

**Check 3.3 — Self-approval rejected**
Setup: create a request where `agentIdentity == "aip-reviewer-1"` — i.e. log in as reviewer and create a request. Then try to approve it with the same reviewer token.
- **Expected**: Dashboard shows "self-approval is not permitted" error message. Request stays Pending.

**Check 3.4 — Deny works**
- Create a new pending request. Log in as reviewer. Click Deny.
- **Expected**: Request transitions to Denied. Phase badge shows Denied (red).

---

### Section 4 — GovernedResource Tab (Gap G-5)

Log in with `$ADMIN_TOKEN_GW` for all checks in this section.

**Check 4.1 — Create GovernedResource**
- Click "Governed Resources" tab → "+ Create".
- Fill: name=`test-gr`, uriPattern=`k8s://prod/apps/*`, permittedActions=`deploy,rollback`, contextFetcher=`none`.
- Submit.
- **Expected**: 201. New row appears in the table.
- Verify via gateway:
  ```bash
  curl -s http://localhost:8080/governed-resources \
    -H "Authorization: Bearer $ADMIN_TOKEN_GW" | python3 -m json.tool | grep test-gr
  ```

**Check 4.2 — List shows all GovernedResources**
- **Expected**: Table shows all existing GovernedResources including the one from Section 3 setup and the one just created.

**Check 4.3 — Edit (PUT) GovernedResource**
- Click `test-gr` row → edit `permittedActions` to add `restart`.
- Submit.
- **Expected**: 200. Table row updates. gateway GET confirms change.

**Check 4.4 — Non-admin cannot create**
- Log in as `$AGENT_TOKEN`. Navigate to Governed Resources tab (should not be visible — but if it is, try to create).
- **Expected**: 403. Error message shown. No row created.

**Check 4.5 — Delete blocked by active requests**
- With `nodepool-validation` still referenced by a Pending AgentRequest: click Delete on `nodepool-validation`.
- **Expected**: Dashboard shows "Cannot delete: active requests are referencing this GovernedResource." (gateway returns 409 / conflict from finalizer).

**Check 4.6 — Delete succeeds when no active requests**
- Delete `test-gr` (no requests reference it).
- **Expected**: 204. Row disappears from table.

---

### Section 5 — SafetyPolicy Tab (Gap G-6)

Log in with `$ADMIN_TOKEN_GW`.

**Check 5.1 — Create SafetyPolicy**
- Click "Safety Policies" tab → "+ Create".
- Fill: name=`test-sp`, namespace=`default`, contextType=`none`, add a rule: name=`allow-all`, expression=`true`, action=`Allow`.
- Submit.
- **Expected**: 201. Row appears.

**Check 5.2 — Edit (PUT) SafetyPolicy**
- Click `test-sp` → change rule action to `Log`.
- **Expected**: 200. Change persists.

**Check 5.3 — Delete SafetyPolicy**
- Delete `test-sp`.
- **Expected**: 204. Row disappears.

**Check 5.4 — Namespace selector works**
```bash
# Create a SafetyPolicy in namespace "kube-system"
curl -s -X POST "http://localhost:8080/safety-policies?namespace=kube-system" \
  -H "Authorization: Bearer $ADMIN_TOKEN_GW" \
  -H "Content-Type: application/json" \
  -d '{"name":"kube-sys-sp","governedResourceSelector":{},"rules":[{"name":"r","type":"StateEvaluation","action":"Allow","expression":"true"}]}'
```
- Change namespace selector in the tab to `kube-system`.
- **Expected**: Table shows only `kube-sys-sp`.

---

### Section 6 — providerContext rendering (Gap G-8)

This section requires Phase 4 (fetchers) to be implemented. Skip until then or mock the data manually.

**Setup** (manual mock):
```bash
# Directly patch an AgentRequest's providerContext via kubectl for testing
kubectl patch agentrequest <name> -n default --type=merge --subresource=status \
  --patch='{"status":{"providerContext":{"currentLimitCPU":"100","currentNodeCount":47,"pendingPods":12}}}'
```

**Check 6.1 — providerContext panel visible**
- Select the patched request.
- **Expected**: "Provider Context" panel appears below "Control Plane Verified" with the JSON fields rendered in a readable format.

**Check 6.2 — FetcherSchemaViolation warning (Gap G-9)**
```bash
kubectl patch agentrequest <name> -n default --type=merge --subresource=status \
  --patch='{"status":{"conditions":[{"type":"FetcherSchemaViolation","status":"True","reason":"SchemaMismatch","message":"fetcher returned field pendingPods of type string, expected integer","lastTransitionTime":"2026-04-05T00:00:00Z"}]}}'
```
- **Expected**: Amber warning banner above the context panel: "Context fetch failed — reviewer is operating without live resource data."

---

### Section 7 — GOVERNED_RESOURCE_DELETED (Gap G-10)

**Setup**:
```bash
# Force-remove the finalizer and delete the GovernedResource while a request is Pending
kubectl patch governedresource nodepool-validation \
  --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]'
kubectl delete governedresource nodepool-validation
```
- The controller should deny the in-flight request with `GOVERNED_RESOURCE_DELETED`.

**Check 7.1**
- Refresh the dashboard. Select the now-denied request.
- **Expected**: Red banner: "This request was denied because the GovernedResource that admitted it was deleted." Phase badge shows Denied.

---

### Section 8 — Regression: existing features still work

After all UI changes, verify these core flows are not broken:

| Check | Steps | Expected |
|---|---|---|
| 8.1 Diagnostics tab loads | Switch to Diagnostics tab | Table renders |
| 8.2 Diagnostic review submits | Select a diagnostic, set verdict to "correct", submit | Verdict badge updates |
| 8.3 Accuracy summaries visible | Create a DiagnosticAccuracySummary via kubectl | Chip appears above diagnostics table |
| 8.4 Governance timeline renders | Open any AgentRequest | 4-step timeline shows correct step highlighted |
| 8.5 Audit trail shows events | Open a completed AgentRequest | Phase transitions listed in order |
| 8.6 3s auto-refresh works | Submit a new request via curl | It appears in the list within 3s without page reload |

---

## Common Failure Modes

| Symptom | Likely cause | Check |
|---|---|---|
| All API calls return 401 | G-1 not fixed (Authorization header dropped) | DevTools → Network → check request headers on an `/api/` call |
| Token accepted but all actions return 403 | Wrong role configured in `--agent-subjects` / `--reviewer-subjects` | Decode token: `echo $TOKEN \| cut -d. -f2 \| base64 -d \| python3 -m json.tool \| grep azp` |
| "invalid token" from gateway | Token fetched via localhost port-forward, `iss` mismatch | Fetch token from inside cluster (see token fetch commands above) |
| Dashboard shows blank list, no errors | Token expired silently | Check Network tab — responses should be 401, not 200 with empty body |
| GovernedResource delete returns 409 | Finalizer blocking — expected when active requests exist | Check test is using a GR with no live requests |
| SafetyPolicy create returns 422 | CEL expression type-check failed against contextSchema | Use `"expression": "true"` for tests not needing context fields |
