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
3. **Pluggable Export**: Emit records to external sinks (OTLP, Webhooks) before deletion.
4. **Hard TTL Safety Valve**: Ensure deletion occurs even if export sinks are down, preventing cluster failure.
5. **Linked Deletions**: Support coherent group deletions (e.g., an `AuditRecord` is not purged before its parent `AgentRequest`).

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

- **Leader-Election Binding**: The `GCManager` runs only on the leader replica — it relies on controller-manager leader election. This is a correctness requirement; introducing a second controller manager instance without understanding this will cause concurrent GC workers and doubled deletions.
- **Paginated Scans**: Uses `Limit` and `Continue` tokens (configurable page size, default: 500) via a direct client (`APIReader`, not the informer cache) to ensure consistency and avoid stale reads.
- **Token-Bucket Rate Limiting**: Deletions are throttled (default: 100/sec) to prevent watch-event fan-out from overwhelming other controllers. Each deletion emits a watch event to every `AgentDiagnostic`/`AgentRequest`/`AuditRecord` watcher; an unthrottled GC run on a large backlog can spike the API server's event queue.
- **Deletion SLA**: A record is guaranteed to be deleted within one GC interval after its retention window expires (e.g., 7-day retention + 1-hour interval → deleted between day 7 and day 7h1m).

**Note on `DeleteCollection`:** `DeleteCollection` reduces client-to-API-server round trips compared to individual `Delete` calls. It does **not** reduce etcd tombstone pressure — each deletion still writes a tombstone; only etcd compaction removes tombstones. The benefit is purely fewer network calls.

### 2. The Export-and-Purge Lifecycle

For each expired record, the engine follows a strict state machine:

1. **Identify**: Find records where `now() - metadata.creationTimestamp > retentionWindow`.
2. **Hard TTL Check**: If `now() - creationTimestamp > hardTTL`, skip export and **delete immediately**. Cluster health takes precedence over data retention. Log a warning so operators know export was skipped.
3. **Export (Optional)**: Hand the object to the bounded async worker pool. The GC loop is never blocked by this step.
4. **Retry with Backoff**: If export fails, retain the record and retry with exponential backoff (base: 5s, max: 10m, jitter: ±20%). Retry state is tracked in memory; on GC worker restart, all eligible records are re-evaluated from scratch (export is assumed idempotent for both OTLP and webhook providers).
5. **Purge**: Issue a `DeleteCollection` (per page) or `Delete` call once export is confirmed or Hard TTL is reached.

### 3. Export Worker Pool

Export is handled by a fixed-size worker pool (configurable `concurrency`, default: 5). Workers are fed from a **bounded channel** (capacity: `concurrency × 10`). When the channel is full, the record is **skipped and retried in the next GC cycle** — the GC loop must never block waiting for an export slot. An unbounded queue or unbounded goroutine-per-record is explicitly prohibited due to OOM risk under high diagnostic churn.

### 4. Linked Deletions (Dependency Handling)

The engine supports optional `DependencyProvider` per resource type to enforce coherent group deletions.

**Semantics:**
- A record with a dependency is only eligible for GC-initiated deletion if all its dependencies are also past their retention window (or have already been deleted).
- If a dependency was **manually deleted** (outside GC), the dependent record becomes immediately eligible — the dependency check only prevents GC from racing ahead, it does not enforce referential integrity.
- **Hard TTL overrides dependency checks.** If a record reaches its Hard TTL, it is deleted unconditionally regardless of dependency state. This prevents a stuck dependency from leaking records past the safety valve.
- All resources in a dependency group must use the **same or shorter retention window** for the parent. Using a longer retention on the parent than the child will cause the child to be held indefinitely until the parent expires; this is a misconfiguration and will be validated at startup.

**Current dependency:** `AuditRecord` → `AgentRequest`. An `AuditRecord` is not purged before its parent `AgentRequest` is also expired (or gone), preserving the coherent audit trail required by the AIP spec.

## Configuration

```yaml
gc:
  enabled: false   # disabled by default; operators must opt in
  interval: 1h
  defaults:
    pageSize: 500
    deleteRatePerSec: 100
    concurrency: 5  # export worker pool size per resource type

  resources:
    agentDiagnostics:
      enabled: true
      retentionDays: 7
      hardTTLDays: 14   # set to the maximum tolerable export-pipeline outage duration
                         # hardTTLDays: 0 disables the safety valve — strongly discouraged in production
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
      retentionDays: 365  # must be <= agentRequests.retentionDays (dependency constraint)
      hardTTLDays: 400
      export:
        type: webhook      # configure independently; do not rely on agentRequests export
        webhook:
          url: "https://audit-sink.internal/v1/ingest"
```

## Export Hook Providers

The `Exporter` interface is generic: `Export(ctx context.Context, obj runtime.Object) error`.

- **OTLP Provider**: Maps Kubernetes object fields to OTLP LogRecord attributes. Sends as log entries (not traces/spans) to the configured collector endpoint.
- **Webhook Provider**: POSTs the raw JSON with a `X-AIP-Resource-Kind` header.
- **S3/Blob Provider (Future)**: Batch upload of JSONL files for high-volume compliance use cases. Track in a dedicated issue before implementing.

## Implementation Checklist

- [ ] Create `internal/gc/` package containing `GCManager` and `GCWorker`.
- [ ] Define `Exporter` interface; implement OTLP and Webhook providers.
- [ ] Implement bounded worker pool with bounded input channel and skip-on-full overflow policy.
- [ ] Implement exponential-backoff retry (base 5s, max 10m, ±20% jitter) bounded by Hard TTL.
- [ ] Implement paginated list + `DeleteCollection` with configurable page size and token-bucket rate limiter.
- [ ] Implement `DependencyProvider` interface and `AuditRecord → AgentRequest` registration.
- [ ] Add startup validation: reject config where parent `retentionDays` > child `retentionDays` in a dependency pair.
- [ ] Register `AgentDiagnostic` as the first managed resource; add `AgentRequest` and `AuditRecord` with dependency checks.
- [ ] Wire `GCManager` into `cmd/main.go` using `mgr.Add()`; document leader-election reliance in code comments.
- [ ] Update RBAC: `list`, `delete`, `deletecollection` on all managed GVKs.
- [ ] Unit tests: paging stability, rate-limiter correctness, Hard TTL forced deletion, export retry/backoff, bounded-channel skip-on-full, dependency blocking, Hard TTL override of dependency, startup validation rejection.

## Relationship to other EPs

- `ep/agent_diagnostic_design.md`: This engine fulfills the retention requirements for diagnostics.
- `ep/diagnostic_verdict_and_accuracy.md`: Exporting diagnostics ensures accuracy metrics are preserved long-term.
- **Replaces**: `ep/agent_diagnostic_retention.md` (superseded by this generic design).
