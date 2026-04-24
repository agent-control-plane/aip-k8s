# Design: OpenAPI-first Gateway API (v1)

## Problem

The aip-k8s gateway (`cmd/gateway/main.go`) exposes ~25 HTTP endpoints across
AgentRequests, AgentDiagnostics, GovernedResources, and SafetyPolicies. None of
it is described by a machine-readable contract:

- Routes are registered with `mux.HandleFunc` (line 256-308). There is no
  OpenAPI spec, no Swagger annotations, no `go:generate` wiring.
- Request bodies are decoded with `json.NewDecoder(r.Body).Decode(...)` and
  validated by hand (`if body.AgentIdentity == "" { ... }`). Struct tags are
  `json:` only — no `validate:`, no `doc:`.
- Every caller hand-rolls the wire format. `cmd/dashboard/app.js` builds URLs
  and JSON by hand across ~1200 lines. `test/e2e/gateway_test.go` does the
  same in Go. `demo/scaledown/agent/main.go`, `demo/kiro/agent/main.go`, and
  `demo/opslock/agent/main.go` each redefine their own copies of
  `createAgentRequestBody` and friends.
- Three inconsistencies are baked into the current wire format and would be
  locked in if published as-is:
  1. **Pagination.** Some list endpoints return a bare JSON array; others
     return `{items, continue}`.
  2. **Errors.** `{"error": "msg"}` — a free-text string with no machine code
     or field pointer.
  3. **CRD passthrough.** `POST /governed-resources` and
     `POST /safety-policies` accept the full Kubernetes CRD object
     (`apiVersion`, `kind`, `metadata`, `status`, `resourceVersion`, …). Any
     future v1alpha1 → v1beta1 → v1 bump is a breaking HTTP change.

Adoption is currently low — every consumer lives in this repo. That window
makes breaking changes cheap *now* and expensive later, once CLIs, third-party
integrations, or external agents are generated from whatever spec we first
publish.

## Non-Goals

- Replacing the controller's client-go usage. The controller
  (`cmd/main.go`, `internal/controller/`) talks to the K8s API, not the
  gateway, and stays unchanged.
- gRPC or GraphQL. The gateway is REST-shaped today and the cost of migrating
  consumers to a different protocol is not justified.
- Client SDKs beyond Go and (optionally) TypeScript. Other languages can be
  generated ad-hoc by downstream consumers from the published spec.
- Replacing the existing CEL evaluator (`cmd/gateway/cel_validation.go`) or
  JSON-schema validator (`cmd/gateway/schema_validation.go`). These keep
  running as pre-handler steps; OpenAPI validation is additive.
- Multi-version coexistence beyond one release. `/v1/` is the first and only
  versioned surface; legacy unversioned routes are a short-lived migration
  aid, not a permanent API.

## Current state

All 25 active routes, from `cmd/gateway/main.go:256-308`:

| Route | Method | Body | Response shape | Notes |
|---|---|---|---|---|
| `/whoami` | GET | — | `{identity, role}` | |
| `/agent-requests` | GET | — | array **or** `{items, continue}` | pagination inconsistency |
| `/agent-requests` | POST | `createAgentRequestBody` | status object | |
| `/agent-requests/{name}` | GET | — | status object | |
| `/agent-requests/{name}/executing` | POST | status body | status object | |
| `/agent-requests/{name}/completed` | POST | status body | status object | |
| `/agent-requests/{name}/approve` | POST | verdict body | — | |
| `/agent-requests/{name}/deny` | POST | verdict body | — | |
| `/audit-records` | GET | — | array or paginated | pagination inconsistency |
| `/agent-diagnostics` | GET | — | array or paginated | pagination inconsistency |
| `/agent-diagnostics` | POST | `createAgentDiagnosticBody` | diagnostic | |
| `/agent-diagnostics/{name}` | GET | — | diagnostic | |
| `/agent-diagnostics/{name}/status` | PATCH | verdict body | — | |
| `/agent-diagnostics/recompute-accuracy` | POST | — | result | |
| `/diagnostic-accuracy-summaries` | GET | — | array | |
| `/governed-resources` | GET/POST | full CRD | full CRD | **CRD leak** |
| `/governed-resources/{name}` | GET/PUT/DELETE | full CRD | full CRD | **CRD leak** |
| `/safety-policies` | GET/POST | full CRD | full CRD | **CRD leak** |
| `/safety-policies/{name}` | GET/PUT/DELETE | full CRD | full CRD | **CRD leak** |

Request body types of note (`cmd/gateway/main.go:138-159`):
`createAgentDiagnosticBody`, `createAgentRequestBody`, and their nested
`cascadeModelBody` / `reasoningTraceBody` / `affectedTargetBody`. These are
the only custom DTOs; the governance endpoints have no DTO layer at all and
marshal `v1alpha1.GovernedResource` / `v1alpha1.SafetyPolicy` directly.

## Design

### Overview

Introduce a new versioned surface at `/v1/...` with:

1. A published OpenAPI 3.1 spec at `GET /openapi.yaml` (and `/openapi.json`).
2. Uniform list envelope, RFC 7807 error responses, and gateway-owned DTOs
   that never expose `apiVersion`/`kind`/`metadata`/`status`.
3. A generated Go client at `pkg/aipclient` that demos and e2e tests consume.
4. Legacy unversioned routes remain alive for one release cycle to stage the
   migration across separate PRs. They are then deleted before a tagged
   release.

Existing middleware (`cmd/gateway/oidc.go`, `cmd/gateway/metrics.go`) and
pre-handler validation (`cmd/gateway/cel_validation.go`,
`cmd/gateway/schema_validation.go`) are unchanged — they wrap or precede the
new handlers exactly as they wrap the current ones.

### Framework comparison

Three candidates were considered. The EP presents the tradeoffs; the final
pick is a review decision.

#### Option A — Huma v2 (code-first) *[lean]*

Annotate Go structs; Huma derives OpenAPI 3.1 at runtime and enforces
validation before the handler runs. Clean fit with the existing
`http.ServeMux`.

```go
type CreateAgentRequestInput struct {
    Namespace string `query:"namespace" doc:"Target namespace"`
    Body      struct {
        AgentIdentity string `json:"agentIdentity" required:"true" minLength:"1"`
        Action        string `json:"action"        required:"true"`
        TargetURI     string `json:"targetURI"     required:"true" format:"uri"`
        Reason        string `json:"reason,omitempty"`
    }
}

type CreateAgentRequestOutput struct {
    Body struct {
        Name  string `json:"name"`
        Phase string `json:"phase" enum:"Pending,Approved,Denied"`
    }
}

huma.Register(api, huma.Operation{
    OperationID: "createAgentRequest",
    Method:      http.MethodPost,
    Path:        "/v1/agent-requests",
    Summary:     "Submit an AgentRequest",
    Tags:        []string{"AgentRequests"},
}, func(ctx context.Context, in *CreateAgentRequestInput) (*CreateAgentRequestOutput, error) {
    // existing business logic: dedup, GovernedResource admission, CEL.
})
```

**Pros.** OpenAPI 3.1; runtime request validation for free; problem+json error
responses built in; keeps Go as source of truth. **Cons.** Framework lock-in;
spec is a build artifact, not a checked-in file (mitigated by committing the
generated spec and diffing it in CI).

#### Option B — oapi-codegen (spec-first YAML)

`openapi.yaml` is authoritative. `oapi-codegen` emits a server interface
stub and a typed Go client. Handlers implement the interface.

```yaml
# openapi.yaml (authoritative, checked in)
paths:
  /v1/agent-requests:
    post:
      operationId: createAgentRequest
      tags: [AgentRequests]
      parameters:
        - { name: namespace, in: query, schema: { type: string } }
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/CreateAgentRequest' }
      responses:
        '201':
          content:
            application/json:
              schema: { $ref: '#/components/schemas/AgentRequestResult' }
components:
  schemas:
    CreateAgentRequest:
      type: object
      required: [agentIdentity, action, targetURI]
      properties:
        agentIdentity: { type: string, minLength: 1 }
        action:        { type: string }
        targetURI:     { type: string, format: uri }
        reason:        { type: string }
```

```go
// server.gen.go (generated) — implemented by gateway.
type ServerInterface interface {
    CreateAgentRequest(w http.ResponseWriter, r *http.Request, params CreateAgentRequestParams)
    // ... 24 more
}

// client.gen.go (generated) — imported by demos and e2e.
```

**Pros.** Spec is the contract, reviewable as a single diff. Third parties can
generate clients without running Go. **Cons.** More upfront authoring; two
places to keep consistent (spec and handler plumbing); no runtime validation
unless a separate middleware is added.

#### Option C — swaggo/swag (annotations)

Comment directives on existing handlers produce `docs/swagger.json` (Swagger
2.0). No changes to handler signatures.

```go
// @Summary  Submit an AgentRequest
// @Tags     AgentRequests
// @Accept   json
// @Produce  json
// @Param    namespace  query   string                  false  "Target namespace"
// @Param    body       body    createAgentRequestBody  true   "Request payload"
// @Success  201  {object}  agentRequestResult
// @Failure  400  {object}  errorResponse
// @Router   /v1/agent-requests [post]
func (s *Server) handleCreateAgentRequest(w http.ResponseWriter, r *http.Request) {
    // unchanged
}
```

**Pros.** Smallest diff. **Cons.** Swagger 2.0, not OpenAPI 3.1 — limits
discriminated unions, nullable handling, and several generator features. No
runtime validation. Comments drift silently when handlers change.

#### Recommendation

~~Huma v2.~~ **Superseded by maintainer review.** The implementation uses
**Option B — stdlib + hand-written OpenAPI spec + oapi-codegen** for the
following reasons (per @ravisantoshgudimetla):

- 25 existing handlers create meaningful rewrite risk with a framework adoption.
- Watch/SSE endpoints require raw `net/http` (`http.Flusher`) regardless of
  framework, so Huma would not eliminate the need for stdlib handlers.
- Go 1.22+ `http.ServeMux` already provides method-aware routing.
- Keeping business logic decoupled from the framework allows a future swap
  without rewriting core handler logic.
- A hand-written spec is a first-class, reviewable artifact; `oapi-codegen`
  generates typed DTOs and clients from it without coupling the server to a
  framework.

The spec lives at `api/openapi/v1alpha1/` and types are regenerated via
`make generate-openapi`. Huma can be reconsidered in a future phase once the
v1alpha1 surface stabilises.

### `/v1/` routing and transition window

- All new endpoints live under `/v1/`. Legacy unversioned routes keep working
  unchanged.
- Both route sets are registered on the same mux and share handlers where
  possible — the `/v1/` layer is a thin request/response translator plus the
  new validation surface.
- After the five migration PRs land (see Migration plan), a follow-up PR
  deletes the legacy routes and the translator code.
- No `/v2/`, `/v1beta1/`, or similar in this EP. Future breaking changes
  require a new EP and a new major version.

### Standard list envelope

Every list endpoint returns:

```json
{
  "items": [ ... ],
  "nextPageToken": "opaque-string-or-empty"
}
```

No bare arrays. No `continue` field (renamed to `nextPageToken` so the
generated clients can expose a consistent `Paginate()` helper). `limit` stays
as a query parameter.

### RFC 7807 error schema

All non-2xx responses use `Content-Type: application/problem+json`:

```json
{
  "type": "https://aip.dev/errors/validation",
  "title": "Invalid request",
  "status": 400,
  "code": "MISSING_FIELD",
  "detail": "agentIdentity is required",
  "field": "agentIdentity"
}
```

`code` is machine-readable (stable enum); `field` is optional and only set for
validation errors. This replaces the current `{"error": "..."}` shape on v1
routes. Legacy routes keep the old shape during the transition window.

### Gateway-owned DTOs for governance resources

`POST|PUT /v1/governed-resources` accepts a flat DTO — not a Kubernetes CRD:

```json
{
  "name": "svc-x",
  "uriPattern": "k8s://apps/v1/deployments/*/*",
  "contextSchema": { "type": "object", ... },
  "allowedActions": ["scale", "restart"]
}
```

The gateway maps DTO → `v1alpha1.GovernedResource` internally. Same pattern
for `/v1/safety-policies`. Consequences:

- Clients never see `apiVersion`, `kind`, `metadata.resourceVersion`,
  `status.conditions`, etc. Status becomes a separate read-only DTO on GET
  responses (not a writable field on POST/PUT).
- `v1alpha1 → v1beta1 → v1` CRD bumps are invisible to HTTP clients as long
  as the DTO ↔ CRD mapping is maintained.
- Optimistic concurrency: the DTO carries an opaque `resourceVersion` string
  that the gateway round-trips to the CRD. Clients treat it as opaque; the
  server validates it.

### Validation layering

For every `/v1/*` handler the order is:

1. OpenAPI-derived validation (shape, required fields, formats) — by Huma or
   by middleware in the oapi-codegen path.
2. `cmd/gateway/schema_validation.go` — for `contextSchema` on
   `GovernedResource` DTOs.
3. `cmd/gateway/cel_validation.go` — for `SafetyPolicy` rule compilation.
4. Business-logic validation (dedup, GovernedResource admission, role
   checks).
5. K8s API call.

Stages 2–5 are unchanged from today.

### Spec publication

- `GET /openapi.yaml` and `GET /openapi.json` return the current spec.
  Unauthenticated (the spec itself is not sensitive). Health endpoints and
  `/openapi.*` are the only unauthenticated routes.
- A checked-in copy lives at `api/openapi/openapi.yaml`. CI regenerates it
  and fails the build on drift. The regen target is `make openapi`.

### `pkg/aipclient`

A typed Go client, generated from the spec. Example rewrite of
`demo/scaledown/agent/main.go`:

```go
import aip "github.com/ravisantoshgudimetla/aip-k8s/pkg/aipclient"

c, _ := aip.NewClientWithResponses(gatewayURL, aip.WithBearerToken(token))

resp, err := c.CreateAgentRequestWithResponse(ctx, &aip.CreateAgentRequestParams{
    Namespace: &ns,
}, aip.CreateAgentRequestJSONRequestBody{
    AgentIdentity: "scaledown-agent",
    Action:        "scale",
    TargetURI:     "k8s://apps/v1/deployments/demo/web",
    Reason:        "off-hours cost reduction",
})
```

Bearer tokens are injected via a `WithBearerToken` request editor rather than
stamped on each call. The client stays out of the auth-acquisition business —
callers bring their own token, same as today.

## Migration plan

Sequenced PRs, each independently reviewable:

1. **Gateway.** Add `/v1/` routes alongside legacy. Publish
   `/openapi.yaml`. Add `pkg/aipclient`. CI drift check for the spec.
2. **Dashboard** (`cmd/dashboard/app.js`). Switch every `fetch('/api/...')`
   to `fetch('/api/v1/...')`. Handle the new error shape. Update list
   iteration to use `items`/`nextPageToken`.
3. **E2E** (`test/e2e/gateway_test.go`, `gateway_oidc_test.go`,
   `gateway_keycloak_test.go`, `helm_test.go`). Replace `http.NewRequest`
   calls with `pkg/aipclient`.
4. **Demo agents.** Migrate `demo/scaledown/agent/main.go`,
   `demo/scaledown/claude-agent-go/tools.go`, `demo/kiro/agent/main.go`,
   `demo/opslock/agent/main.go` onto `pkg/aipclient`. Delete the duplicated
   request structs.
5. **Cleanup.** Delete legacy unversioned routes, the translator layer, and
   any remaining hand-rolled types. The gateway now speaks only `/v1/`.

Each PR is a clean bisect point. Step 1 ships no behavior change for current
consumers; steps 2–4 each change exactly one consumer; step 5 is a pure
deletion.

## Consumer impact

| Consumer | Path | Impact |
|---|---|---|
| Dashboard | `cmd/dashboard/app.js`, `cmd/dashboard/main.go:60,89` | URL prefix change; error parsing; list iteration. |
| E2E | `test/e2e/gateway_test.go` + 3 siblings | Replace hand-rolled `http.NewRequest` with `pkg/aipclient`. |
| Demo: scaledown | `demo/scaledown/agent/main.go`, `demo/scaledown/claude-agent-go/tools.go` | Delete duplicated structs; import `pkg/aipclient`. |
| Demo: kiro | `demo/kiro/agent/main.go` | Same. |
| Demo: opslock | `demo/opslock/agent/main.go` | Same. |
| Shell scripts | `demo/scaledown/start.sh`, `run.sh`, `stop.sh` | Unaffected — only hit `/healthz`. |
| Controller | `cmd/main.go`, `internal/controller/` | Unaffected — uses client-go, not HTTP. |

## Open questions

- **Dashboard client.** Do we also generate a TypeScript client for
  `cmd/dashboard/`, or keep the hand-written `fetch()` calls (now with a
  typed spec behind them)? The dashboard is the largest single block of
  boilerplate but also the one most likely to drift if no one is running the
  TS codegen.
- **Auth in `pkg/aipclient`.** Bearer token injection is straightforward. OIDC
  token acquisition (client-credentials, refresh, etc.) is out of scope —
  should the client ship any auth helpers at all, or stay transport-only?
- **Spec drift policy.** CI fails on drift, but what triggers regeneration?
  `make openapi` on every handler change is error-prone. A pre-commit hook or
  a required CI job are both viable.
- **Post-v1 evolution.** This EP defines `/v1/` but not the policy for the
  next breaking change. A lightweight companion doc on versioning (deprecation
  window, `Sunset` headers, compatibility rules) should follow before any v2
  is considered.
