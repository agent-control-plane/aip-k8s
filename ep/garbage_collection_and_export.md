# Design: Control Plane Garbage Collection and Export Engine

Status: Draft

## Problem

AIP components generate high-volume, append-only records:
- **AgentDiagnostics**: Observations and diagnoses (hundreds per day).
- **AgentRequests**: Intent declarations and state transitions.
- **AuditRecords**: Cryptographic event logs for every request transition.

Without a cleanup mechanism, these records accumulate indefinitely in etcd, leading to storage exhaustion and control plane degradation. Furthermore, compliance requirements (SOC2, PCI-DSS, FedRAMP) often mandate retaining these records for 1–6 years, which is cost-prohibitive to store in etcd.

A single, unified engine is needed to manage the **Retention → Export → Deletion** lifecycle for all AIP resource types.

## Goals

1. **Resource Agnostic**: A single engine capable of cleaning up any AIP GVK (`AgentDiagnostic`, `AgentRequest`, `AuditRecord`).
2. **Cluster Stability First**: Protect etcd from OOMs and tombstone spikes via paging and rate-limiting.
3. **Dry Run Mode**: Support "safe rollout" where the engine identifies and logs expired records without actually deleting them.
4. **Pluggable Export**: Emit records to external sinks (OTLP, Webhooks) before deletion.
5. **Hard TTL Safety Valve**: Ensure deletion occurs even if export sinks are down, preventing cluster failure.
6. **Linked Deletions**: Support coherent group deletions (e.g., an `AuditRecord` is not purged before its parent `AgentRequest`).

## Alternatives Considered

### TTL-after-finished controller (rejected for now)

The Kubernetes-native approach is a TTL controller: set a `ttl` annotation on each record at creation time and let a lightweight controller delete the object when expired (analogous to `batch/v1` TTL-after-finished). This eliminates a polling GC loop entirely, scales naturally with the API server, and makes object lifetime operationally transparent.

Rejected as the primary mechanism because:
1. It does not support the export hook — a finalizer could trigger export, but finalizer-based export creates its own failure modes (stuck finalizers if the export endpoint is permanently down).
2. Retention policy ("delete after N days unless exported") cannot be expressed with a static TTL annotation set at creation time.

TTL-after-finished remains the preferred long-term direction if the export hook requirement is dropped or moved out-of-band.

### Export at creation time (preferred long-term direction)

Writing records to an external sink at creation time via a controller sidesteps the export-before-delete coupling entirely. GC then becomes unconditional. This is the right architecture but requires infrastructure changes outside the scope of this EP.

## Proposed Architecture: `GCManager`

The `GCManager` is a background `Runnable` in the controller manager that orchestrates multiple `GCWorkers` — one per registered resource type.

**Key invariant: GC correctness must not depend on export success.** Export is best-effort; the Hard TTL enforces deletion unconditionally to protect the cluster. Export failure can delay deletion up to the Hard TTL but cannot prevent it.

### 1. Stability Primitives

- **Leader-Election Binding**: The `GCManager` runs only on the leader replica via controller-manager leader election, ensuring only one instance performs GC operations at a time. No additional manual coordination is required.
- **Paginated Scans**: Uses `Limit` and `Continue` tokens (configurable page size, default: 500) via a direct client (`APIReader`, not the informer cache) to ensure consistency and avoid stale reads.
- **Token-Bucket Rate Limiting**: Deletions are throttled at the **object level** (default: 100 objects/sec), not at the API-call level. Each deleted object emits a watch event to every watcher of that GVK; an unthrottled GC run on a large backlog can spike the API server's event queue.
- **Dry Run Mode**: When `--gc-dry-run=true`, the engine identifies expired records and logs them (e.g., `DRY-RUN: would delete AgentDiagnostic production/diag-123 (expired 2d ago)`) but does not issue the `Delete` call. This allows operators to verify paging and retention logic before enabling enforcement.
- **Global Safety Valve**: To prevent catastrophic misconfiguration (e.g., setting retention to 0 accidentally), GC for a resource type is **skipped** if the total count of that resource in the cluster is below a minimum threshold (default: 10). This ensures a "healthy minimum" remains in etcd for debugging even if GC rules are overly aggressive.
- **Deletion SLA**: When no export is configured, or when export succeeds, a record is guaranteed to be deleted within one GC interval after its retention window expires (e.g., 7-day retention + 1-hour interval → deleted between day 7 and day 7h1m). When export is configured and fails, deletion is delayed by export retries up to the Hard TTL, at which point deletion is unconditional (see Hard TTL Check in the lifecycle below).

**Note on Deletion Mechanism:** Phase 1 uses individual `Delete` calls per object rather than `DeleteCollection`. `DeleteCollection` cannot selectively target individual expired objects within a page — it deletes all matching objects by label selector, not by the per-object TTL check performed in the scan loop. Individual `Delete` calls enable precise rate limiting (1 token per object) and failure isolation: a 404 (already deleted by another actor) or a transient API error on one object does not abort the rest of the batch.

### 2. The Export-and-Purge Lifecycle

For each expired record, the engine follows a strict state machine:

1. **Identify**: Find records where `now() - metadata.creationTimestamp >= retentionWindow`. Equality is treated as expired. Implementations must apply this operator consistently at the boundary.
2. **Hard TTL Check**: If `now() - creationTimestamp >= hardTTL`, skip export and **delete immediately**. Cluster health takes precedence over data retention. Log a warning so operators know export was skipped. (Same boundary rule: equality is expired.)
3. **Export (Optional)**: Hand the object to the bounded async worker pool. The GC loop is never blocked by this step.
4. **Retry with Backoff**: If export fails, retain the record and retry with exponential backoff (base: 5s, multiplier: 2×, max: 10m, jitter: ±20% — example sequence: 5s → 10s → 20s → 40s → … → 10m). Retry state is tracked in memory only; see Leader-Transition Semantics below.
5. **Purge**: Issue a per-object `Delete` call once export is confirmed or Hard TTL is reached. (Phase 1 and Phase 2 both use individual `Delete` calls — `DeleteCollection` cannot selectively target only the expired objects identified by the scan loop.)

### 3. Leader-Transition Semantics

Export retry state is intentionally ephemeral (in-memory only). On leadership loss:

- All in-flight exports in the bounded worker pool are abandoned.
- The new leader re-evaluates all eligible records from scratch on its next GC cycle.
- **Safety:** Export idempotency (both OTLP and webhook providers are assumed idempotent) ensures that a record exported by the previous leader and re-exported by the new leader causes no harm — at-least-once delivery is acceptable.
- **Operational implication:** A leadership transition resets exponential backoff for all in-flight retries. A recovering export endpoint may receive a burst of retries from the new leader before backoff re-establishes. This is a known trade-off of stateless GC workers; a follow-up issue should track persistent retry state if this becomes operationally problematic.

### 4. Export Worker Pool

Export is handled by a fixed-size worker pool (configurable `concurrency`, default: 5). Workers are fed from a **bounded channel** (capacity: `concurrency × 10`). When the channel is full, the record is **skipped and retried in the next GC cycle** — the GC loop must never block waiting for an export slot. An unbounded queue or unbounded goroutine-per-record is explicitly prohibited due to OOM risk under high diagnostic churn.

### 4. Linked Deletions (Dependency Handling)

The engine supports optional `DependencyProvider` per resource type to enforce coherent group deletions.

**Semantics:**
- A record with a dependency is only eligible for GC-initiated deletion if all its dependencies are also past their retention window (or have already been deleted).
- If a dependency was **manually deleted** (outside GC), the dependent record becomes immediately eligible — the dependency check only prevents GC from racing ahead, it does not enforce referential integrity.
- **Hard TTL overrides dependency checks.** If a record reaches its Hard TTL, it is deleted unconditionally regardless of dependency state. This prevents a stuck dependency from leaking records past the safety valve.
- All resources in a dependency group must use the **same or shorter retention window** for the parent. Using a longer retention on the parent than the child will cause the child to be held indefinitely until the parent expires; this is a misconfiguration and will be validated at startup.

**Current dependency:** `AuditRecord` → `AgentRequest`. An `AuditRecord` is not purged before its parent `AgentRequest` is also expired (or gone), preserving the coherent audit trail required by the AIP spec.

### 5. DiagnosticAccuracySummary Lifecycle

`DiagnosticAccuracySummary` records store aggregate reputation data for agents. Unlike diagnostics themselves, these are **intentionally long-lived**.

- **Decision**: `DiagnosticAccuracySummary` records are **not** managed by the GC engine. They remain in etcd as a permanent "reputation trail" even after the individual diagnostics that contributed to them are purged.
- **Rationale**: Purging summaries would break the "Agent Maturity Model" (#105) which requires tracking agent performance over months/years. The etcd footprint of one summary record per agent is negligible compared to the diagnostic stream.

### 6. Observability (Prometheus Metrics)

The GC engine exports the following metrics to monitor performance and health:

| Metric | Labels | Implemented label values | Description |
|---|---|---|---|
| `aip_gc_objects_deleted_total` | `resource`, `reason` | `reason`: `"hard_ttl"`, `"expired"` | Total objects purged. `"hard_ttl"`: forced deletion past hard TTL. `"expired"`: deleted after soft retention window (Phase 2+). |
| `aip_gc_objects_skipped_total` | `resource`, `reason` | `reason`: `"dry_run"`, `"safety_valve"`, `"export_pending"` | Objects eligible but not deleted. `"export_pending"`: export in-flight or backing off (Phase 2+). `"dependency"` added in Phase 3. |
| `aip_gc_scan_duration_seconds` | `resource` | — | Duration of the full list-and-evaluate cycle |
| `aip_gc_scan_objects_evaluated_total` | `resource` | — | Total objects scanned from etcd |
| `aip_gc_export_failures_total` | `resource` | — | Failures sending records to external sinks (Phase 2+) |

> For the current implemented set see `internal/gc/metrics.go`.

## Configuration

Configuration is delivered via **CLI flags** on the controller manager. The Helm chart (`charts/aip-k8s/values.yaml` → `charts/aip-k8s/templates/controller/deployment.yaml`) maps `values.yaml` keys to these flags.

| Flag | Default | Phase | Description |
|---|---|---|---|
| `--gc-enabled` | `false` | 1 | Enable the GC engine |
| `--gc-interval` | `1h` | 1 | Time between GC cycles |
| `--gc-dry-run` | `true` | 1 | Log deletions without acting (default true for safety) |
| `--gc-diagnostic-hard-ttl` | `14d` | 1 | Forced deletion window (safety valve) |
| `--gc-delete-rate-per-sec` | `100` | 1 | Rate limit for object deletions (tokens per second) |
| `--gc-page-size` | `500` | 1 | Objects per list page during GC scan |
| `--gc-safety-min-count` | `10` | 1 | Skip GC if total object count is below this threshold |
| `--gc-diagnostic-retention-ttl` | `0` (disabled) | 2 | Soft retention window; `0` means hard TTL only |
| `--gc-export-type` | `none` | 2 | Export provider: `none` or `otlp` |
| `--gc-otlp-endpoint` | `""` | 2 | OTLP gRPC endpoint (required when `--gc-export-type=otlp`) |
| `--gc-export-concurrency` | `5` | 2 | Export worker pool size per resource type |

**Important:** `--gc-diagnostic-retention-ttl=0` disables soft retention — the export pipeline is never invoked and only the hard TTL applies. Setting `--gc-export-type=otlp` without also setting a non-zero `--gc-diagnostic-retention-ttl` has no effect.

The Helm values schema (under `gc:` in `values.yaml`) is now active:

```yaml
gc:
  enabled: false         # disabled by default; operators must opt in
  interval: 1h
  dryRun: true           # set false to enable actual deletion
  diagnosticHardTTL: 14d
  diagnosticRetentionTTL: "0"  # "0" disables soft retention; use e.g. "7d" to enable
  exportType: "none"           # "none" or "otlp"
  otlpEndpoint: ""             # required when exportType=otlp
  exportConcurrency: 5
  deleteRatePerSec: 100
  pageSize: 500
  safetyMinCount: 10

  # Phase 3 (future) — per-resource structured config:
  resources:
    agentDiagnostics:
      enabled: true
      retentionDays: 7
      hardTTLDays: 14   # set to the maximum tolerable export-pipeline outage duration
                         # hardTTLDays: 0 disables the hard TTL check entirely (the safety valve is
                         # skipped; only soft retention applies). It does NOT mean immediate expiry.
                         # Disabling is strongly discouraged in production — a degraded export
                         # pipeline will hold records in etcd indefinitely.
      export:
        type: otlp
        otlp:
          endpoint: "otel-collector:4317"

    agentRequests:
      enabled: true
      retentionDays: 365
      hardTTLDays: 400
      export:
        type: webhook
        webhook:
          url: "https://audit-sink.internal/v1/ingest"

    auditRecords:
      enabled: true
      retentionDays: 365  # agentRequests.retentionDays must be <= this value (dependency constraint:
                          # parent must not outlive child — if AgentRequest expires after AuditRecord,
                          # the AuditRecord is held past its own retention until the parent expires)
      hardTTLDays: 400
      export:
        type: webhook      # configure independently; do not rely on agentRequests export
        webhook:
          url: "https://audit-sink.internal/v1/ingest"
```

## Export Hook Providers

The `Exporter` interface (`internal/gc/exporter.go`): `Export(ctx context.Context, obj *v1alpha1.AgentDiagnostic) error`.

- **OTLP Provider**: Maps Kubernetes object fields to OTLP LogRecord attributes. Sends as log entries (not traces/spans) to the configured collector endpoint.
- **Webhook Provider**: POSTs the raw JSON with a `X-AIP-Resource-Kind` header.
- **S3/Blob Provider (Future)**: Batch upload of JSONL files for high-volume compliance use cases. Track in a dedicated issue before implementing.

## Implementation Checklist

### Phase 1 — Hard TTL deletion for AgentDiagnostic ✅ Done
_Goal: etcd protection in production with no export complexity. Pure deletion only._

- [x] Create `internal/gc/` package containing `GCManager` and `GCWorker`.
- [x] Wire `GCManager` into `cmd/main.go` using `mgr.Add()`; document leader-election reliance in code comments.
- [x] Implement paginated list via direct client (`APIReader`) with configurable page size (default: 500).
- [x] Implement individual `Delete` calls per expired object with token-bucket rate limiter (default: 100 objects/sec); acquire 1 token per object before issuing the call. (`DeleteCollection` cannot target specific expired objects from a page — see Note on Deletion Mechanism above.)
- [x] Hard TTL only — delete records where `now() - creationTimestamp >= hardTTL` unconditionally; log a single startup warning when export is not configured (not per-deletion).
- [x] Register `AgentDiagnostic` as the first and only managed resource in Phase 1.
- [x] Update RBAC: `list`, `delete`, `deletecollection` on `agentdiagnostics`.
- [x] Define full `gc:` config shape now (including `retentionDays` and export fields) even though unused in Phase 1, to avoid a breaking config change in Phase 2.
- [x] Unit tests: paging stability, rate-limiter token consumption for batches, Hard TTL boundary (`>=`) correctness, startup warning log.

### Phase 2 — Export pipeline for AgentDiagnostic ✅ Done
_Goal: add OTLP export with bounded async worker pool and retry before deletion._

- [x] Define `Exporter` interface: `Export(ctx context.Context, obj *v1alpha1.AgentDiagnostic) error`. `NoopExporter` used when `--gc-export-type=none`.
- [x] Implement OTLP provider (`internal/gc/exporter_otlp.go`): maps object fields to OTLP LogRecord attributes (log entries, not traces). Compile-time interface check included.
- [x] Implement bounded export worker pool (`internal/gc/pool.go`): fixed size (`concurrency`), bounded input channel (capacity: `concurrency × 10`), skip-on-full overflow (never block the GC loop).
- [x] Implement exponential-backoff retry (base: 5s, multiplier: 2×, max: 10m, ±20% jitter using `crypto/rand`) bounded by Hard TTL; retry state is in-memory only (see Leader-Transition Semantics).
- [x] Activate soft retention check (`--gc-diagnostic-retention-ttl`). Guard: `retentionTTL=0` disables soft retention entirely — export pipeline is never invoked regardless of `--gc-export-type`.
- [x] Unit tests: bounded-channel skip-on-full, export retry/backoff sequence, Hard TTL forced deletion when export fails, leader-transition retry-reset behavior, `retentionTTL=0` guard.

### Phase 3 — AgentRequest, AuditRecord, and dependency handling
_Goal: extend GC to all AIP resource types with coherent ordered deletion._

- [ ] **Remove the existing `ctrl.SetControllerReference` calls** in `agentrequest_controller.go` (`emitAuditRecord` at line ~878 and the `policy.evaluated` path at line ~471). The controller already sets `AgentRequest` as owner of every `AuditRecord`, which means Kubernetes is silently cascade-deleting `AuditRecords` today whenever an `AgentRequest` is manually deleted — bypassing any retention policy. Replace the owner reference with a non-owning label (`aip.io/agentRequestRef`) and rely on `DependencyProvider` for retention-aware deletion.
- [ ] Implement `DependencyProvider` interface and register `AuditRecord → AgentRequest`. `AuditRecords` are exclusively linked to `AgentRequests` (the `AgentRequestRef` field is mandatory; there is no `AgentDiagnosticRef`). Use a non-owning label reference and rely on `DependencyProvider` for retention-aware deletion.
- [ ] Implement Webhook export provider: POST raw JSON with `X-AIP-Resource-Kind` header.
- [ ] Add startup validation: reject config where `agentRequests.retentionDays > auditRecords.retentionDays` (parent must not outlive child).
- [ ] Register `AgentRequest` and `AuditRecord` GCWorkers with dependency checks; Hard TTL overrides dependency checks unconditionally.
- [ ] Update RBAC: `list`, `delete`, `deletecollection` on `agentrequests` and `auditrecords`.
- [ ] Unit tests: dependency blocking, Hard TTL override of dependency, startup validation rejection, manual-deletion orphan path.

## Relationship to other EPs

- `ep/agent_diagnostic_design.md`: This engine fulfills the retention requirements for diagnostics.
- `ep/diagnostic_verdict_and_accuracy.md`: Exporting diagnostics ensures accuracy metrics are preserved long-term.
- **Replaces**: `ep/agent_diagnostic_retention.md` (superseded by this generic design).
