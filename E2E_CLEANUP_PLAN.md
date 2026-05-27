# E2e Test Cleanup Plan

**Status:** Deferred — complete after Phase 1 (AgentRegistration CRD) merges.

---

## Key finding

Integration tests (`cmd/gateway/integration_*.go`) use **envtest + all four real
controllers running** via `mgr.Start(ctx)`. They are NOT fake-client tests. The
controllers (`AgentRequestReconciler`, `GovernedResourceReconciler`,
`DiagnosticAccuracyReconciler`, `AgentTrustProfileReconciler`) all run for real.

The actual distinction:

| | Integration | E2e `gateway_test.go` |
|--|--|--|
| Gateway | In-process handler call (`httptest`) | Real built binary subprocess |
| Auth | `authRequired: false`, context injection | Real HTTP + middleware stack |
| K8s | envtest (in-process API server) | Real Kind cluster |
| RBAC | envtest (permissive) | Real RBAC enforced |

---

## Only `gateway_test.go` has real duplication

All other e2e files are unique and must stay:

| File | Unique value | Keep? |
|------|-------------|-------|
| `gateway_test.go` | 12 of ~14 Its duplicated by integration | Restructure |
| `soak_mode_test.go` | Verifies `DiagnosticAccuracySummary` (TotalReviewed, CorrectCount, DiagnosticAccuracy) — integration never checks this | **Keep** |
| `opslock_renewal_test.go` | Verifies `Lease.RenewTime` advances over real time; no integration renewal test at all | **Keep** |
| `gc_test.go` | No GC test in integration | **Keep** |
| `gateway_keycloak_test.go` | Real Keycloak; integration injects auth context directly | **Keep** |
| `gateway_oidc_test.go` | Real OIDC middleware stack; integration never exercises it | **Keep** |
| `helm_test.go` | Chart deployment + dashboard proxy (skipped by default, needs `GATEWAY_URL`) | **Keep** |
| `e2e_test.go` | Pod readiness, deployment RBAC, real cluster smoke | **Keep** |

---

## `gateway_test.go` — what to remove vs keep

The BeforeAll **must stay**: it builds and starts the real gateway binary subprocess.
That IS the binary smoke test.

### Remove (12 Its — exact integration duplicates)

| `It` description | Integration equivalent |
|-----------------|----------------------|
| POST /agent-requests → 201 | `Full lifecycle - Pending to Approved` |
| GET /agent-requests lists items | implicit in integration setup |
| GET /agent-requests/{name} → 200 | `GET /agent-requests/{name} - returns current state` |
| controller reconciles to Approved | `Full lifecycle - Pending to Approved` |
| duplicate POST → 200 (idempotent) | `Idempotent duplicate - returns 200 immediately` |
| POST creates request held for approval | `RequiresApproval condition - returns 201 early` |
| controller sets RequiresApproval condition and holds at Pending | same as above |
| POST /approve → Approved | `POST /approve transitions to Approved` |
| POST /deny → Denied | `POST /deny transitions to Denied` |
| GET /watch SSE for approved request | `GET /watch returns immediate result for terminal request` |
| GET /watch 400 without Accept header | `GET /watch returns 400 without Accept header` |
| GET /watch 404 for nonexistent | `GET /watch returns 404 for nonexistent request` |

### Keep (3 Its — unique to e2e)

| `It` description | Why unique |
|-----------------|-----------|
| CRD is visible in cluster after creation | Verifies real K8s API visibility; meaningless in envtest |
| AuditRecord for `request.approved` emitted | Integration only tests `request.submitted`; `request.approved` is a different event triggered by a different code path |
| SSE stream receives result event during human approval flow | Integration POST+SSE only tests the Approved path; this tests POST+SSE+RequiresApproval (Pending result in stream) — different code path |

### Restructured file shape

```text
Describe("Phase 6: Gateway API") {
  BeforeAll  // build + start binary subprocess
  AfterAll   // kill subprocess + cleanup

  It("binary smoke: create AR, CRD visible in cluster")
  It("AuditRecord for request.approved emitted")
  It("SSE POST streams Pending+RequiresApproval result")
}
```

---

## Implementation notes

- The `pendingName` variable in the Human decision flow context is set inside an
  `It` block ("POST creates request held for human approval"). When that `It` is
  removed, the AuditRecord and SSE tests must be restructured to create their own
  request in a `BeforeAll` or at the top of their own `It`.
- The SSE+RequiresApproval test currently depends on a `SafetyPolicy` created in
  `BeforeAll` of the "Human decision flow" context. When restructuring, that
  `SafetyPolicy` creation moves into the new single `It` or a local `BeforeAll`.
- Use `DeleteAllOf` in `AfterAll` (not by name) per CLAUDE.md convention.

---

## Estimated impact

- ~12 `It` blocks removed from `gateway_test.go`
- e2e runtime reduction: ~2–3 minutes (each removed test waits for
  controller reconciliation with 2-minute timeouts)
- No coverage loss: all removed scenarios remain fully tested in integration
