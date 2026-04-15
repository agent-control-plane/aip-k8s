# CLAUDE.md — aip-k8s development guide

> For generic kubebuilder patterns (scaffold commands, project layout, RBAC markers,
> webhook setup, logging style guidelines) see **AGENTS.md**.
> This file covers only what is **specific to this project**.

## Project overview

AIP (Agent Intent Protocol) Kubernetes implementation. A governance control plane
for autonomous agents: gateway (HTTP API), controller (reconciler), dashboard (UI),
and CRDs (AgentRequest, AgentDiagnostic, SafetyPolicy, GovernedResource).

## Build & test commands

```bash
make test                  # unit tests (envtest, no cluster needed)
make test-e2e              # e2e tests (requires Kind cluster named: aip-k8s-test-e2e)
make manifests             # regenerate CRDs and RBAC from markers
make generate              # regenerate DeepCopy methods
make lint                  # golangci-lint — always use this, never invoke the binary directly
make lint-fix              # auto-fix lint issues
make fmt                   # go fmt
make build                 # build all binaries
```

## Architecture

```
cmd/gateway/         — HTTP API server (stateless, talks to K8s API)
cmd/controller/      — controller-runtime reconciler for AgentRequest lifecycle
cmd/dashboard/       — static HTML dashboard (proxies to gateway)
api/v1alpha1/        — CRD type definitions with kubebuilder markers
internal/controller/ — reconciler logic
internal/evaluator/  — CEL policy evaluator
charts/aip-k8s/      — Helm chart
test/e2e/            — Ginkgo e2e test suite
```

## Project-specific conventions

### File organization
- **Max 500 lines per file.** Split by concern when exceeded. Gateway handlers should
  be split by resource type (requests, diagnostics, governed resources, safety policies).
- **One major type per file.** Helper types may share a file with their parent.

### Error handling
- Wrap errors with context: `fmt.Errorf("reconciling AgentRequest %s: %w", name, err)`
- Never ignore errors silently. If intentional, add a comment explaining why.
- Use `apierrors.IsNotFound()`, `apierrors.IsConflict()`, `apierrors.IsAlreadyExists()`
  — never match error strings.
- **`AlreadyExists` is not `Conflict`.** `retry.RetryOnConflict` only retries 409 Conflict;
  it does NOT handle AlreadyExists. Handle them separately in create-or-update patterns.

### Controller patterns
- **Patch, don't Update.** Use `client.MergeFrom(obj.DeepCopy())` for status patches.
  Compute the base immediately before patching — never reuse a stale base.
- **APIReader for consistency.** Use `r.APIReader.Get()` when you need a fresh read
  before a status transition to avoid stale informer cache.
- **Return after status patch.** After `Status().Patch()`, return immediately so the
  next reconcile gets a fresh object. Never continue with a stale copy.
- **Injectable clock.** Use `Clock func() time.Time` for testability; never call
  `time.Now()` directly in reconcile logic.

### HTTP handler patterns (gateway)
- Apply `http.MaxBytesReader(w, r.Body, 1<<20)` on **all** handlers that read a body —
  no exceptions, including admin endpoints.
- Validate required fields before any K8s API call.
- HTTP status codes: 400 bad request, 401 unauthenticated, 403 wrong role, 404 not
  found, 409 conflict/duplicate, 500 internal error.
- Use `writeJSON` for all JSON responses — never write directly to `http.ResponseWriter`.
- Use role constants (`roleAgent`, `roleReviewer`, `roleAdmin`) — never inline strings.

### Logging
- **Never log secrets, tokens, or credentials** — not even in error messages.
- Use `logger.V(1)` for debug-level, `logger.V(2)` for trace-level.

### e2e test cleanup
- **AfterAll/cleanup must delete by `--all`, never by individual name.**
  Deleting by name misses resources created with a different name in the same namespace
  (e.g., from a parallel phase), which leak and silently break unrelated tests.
  ```bash
  # Wrong — misses resources created with a different name:
  kubectl delete safetypolicy gw-require-human -n $NS --ignore-not-found
  # Correct:
  kubectl delete safetypolicy --all -n $NS --ignore-not-found
  ```
- Each phase owns its cleanup. Assume no ordering guarantee between teardown and the
  next phase's setup.

### Helm chart standards
- Never default image tag to `latest`. Use `{{ .Values.image.tag | default .Chart.AppVersion }}`.
- Always set resource requests AND limits.
- Always set security context: `runAsNonRoot`, `readOnlyRootFilesystem`, drop ALL capabilities.
- Quote user-supplied values: `{{ .Values.foo | quote }}`.
- Guard optional features with `{{ if .Values.feature.enabled }}`.

### Security
- Validate auth on every mutating endpoint. Health/ready probes are the only exceptions.
- Never trust `X-Remote-User` headers without CIDR validation — prefer JWT over proxy
  headers in any service-mesh environment.
- Use `crypto/rand` for random values, never `math/rand`.

## Common mistakes to avoid

1. **Reusing a `client.MergeFrom` patch base** after the object has been patched.
   Always recompute or return-and-requeue.
2. **Forgetting RBAC** when adding new K8s resource access. Update both the ClusterRole
   in the Helm chart AND run `make manifests` for controller RBAC markers.
3. **Missing `AlreadyExists` handling** inside `retry.RetryOnConflict` loops — they are
   different error classes and RetryOnConflict will not retry AlreadyExists.
4. **Hardcoded timeouts and magic numbers.** Use named constants or configurable flags.
5. **`//nolint:dupl`** without justification. Prefer extracting a generic helper. If
   duplication is intentional, document why with a comment.
6. **Hand-editing `zz_generated.deepcopy.go`.** Run `make generate` instead. Manual edits
   silently corrupt the file (e.g., dropping struct fields mid-rebase).
7. **Named-only cleanup in e2e AfterAll.** See e2e test cleanup section above.
8. **e2e tests must pass in both Kustomize and Helm modes.** The chart-e2e workflow runs
   the full suite with `HELM_DEPLOYED=true`. Every new e2e test must:
   - Use the `serviceAccountName` / `controllerDeploymentName` constants — both are
     `"aip-k8s-controller"`, which is the unified name for both Kustomize and Helm.
   - Ensure any feature flag tested (e.g. `--gc-enabled=true`) is also set in the
     `helm upgrade --install` command in `.github/workflows/chart-e2e.yml`.
   - Either work correctly with `HELM_DEPLOYED=true` or be explicitly skipped with
     `if os.Getenv("HELM_DEPLOYED") == "true" { Skip(...) }` and a clear reason.
