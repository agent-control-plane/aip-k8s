# Garbage Collection and Export

The garbage collection (GC) engine is a background service in the AIP controller that cleans up `AgentDiagnostic` records. It ensures cluster stability by protecting etcd from unbounded storage growth while optionally providing an export pipeline for long-term audit trails and compliance.

---

## Quick start (hard TTL only)

By default, the GC engine is disabled. To protect your cluster from etcd pressure with a 14-day retention window, use the following minimal configuration.

> [!WARNING]
> Dry-run mode is enabled by default (`--gc-dry-run=true`). You must explicitly set it to `false` for any records to be deleted.

### Helm configuration

```yaml
# values.yaml — minimal safe configuration
gc:
  enabled: true
  dryRun: false
  diagnosticHardTTL: 14d
  safetyMinCount: 10
```

### CLI flags

```bash
/manager \
  --gc-enabled=true \
  --gc-dry-run=false \
  --gc-diagnostic-hard-ttl=14d \
  --gc-safety-min-count=10
```

---

## Soft retention and OTLP export

For organizations requiring long-term audit trails or compliance reporting, AIP supports exporting diagnostic records to an OTLP-compatible log collector (e.g., OpenTelemetry Collector, Grafana Loki, or Datadog) before they are deleted from Kubernetes.

When soft retention is enabled, records older than the `--gc-diagnostic-retention-ttl` window are submitted to an async export pool. Once successfully exported, the records are deleted from the cluster.

### Helm configuration with OTLP

```yaml
gc:
  enabled: true
  dryRun: false
  diagnosticHardTTL: 30d
  diagnosticRetentionTTL: 7d
  exportType: otlp
  otlpEndpoint: otel-collector.monitoring:4317
```

In this configuration:
- Records older than 7 days are exported.
- Successfully exported records are deleted immediately.
- If the export endpoint is down, records remain in the cluster until they reach the hard TTL (30 days), at which point they are forcibly deleted to protect the cluster.

---

## Deletion decision logic

For every `AgentDiagnostic` object, the GC worker executes the following logic in order:

1. **Eligible?**: If age is less than `--gc-diagnostic-retention-ttl`, skip (not yet eligible).
2. **Hard TTL override**: If age is greater than or equal to `--gc-diagnostic-hard-ttl`, delete unconditionally. The hard TTL always wins to protect etcd, even if export fails.
3. **Immediate delete**: If `--gc-export-type` is `none` (or retention TTL is 0), delete the record immediately once it passes the retention window.
4. **Export and delete**: Otherwise, submit the record to the export pool. The record is only deleted after the exporter returns success.

---

## Safe rollout procedure

To enable garbage collection safely in a production cluster:

1. **Enable dry-run**: Set `gc.enabled=true` and `gc.dryRun=true`.
2. **Observe metrics**: Monitor the `aip_gc_objects_skipped_total{reason="dry_run"}` metric. This shows you exactly how many objects would be deleted.
3. **Verify export**: If using OTLP, check your collector logs. In dry-run mode, records **are** exported to the OTLP endpoint — only the Kubernetes deletion is suppressed. This lets you validate the export pipeline before enabling real deletion.
4. **Disable dry-run**: Once you are confident in the configuration, set `gc.dryRun=false`.

---

## Observability

The GC engine emits Prometheus metrics to the controller's metrics endpoint (default `:8080/metrics`).

| Metric | Labels | Description |
|---|---|---|
| `aip_gc_objects_deleted_total` | `resource`, `reason` | Total records purged. Reasons: `hard_ttl`, `expired`. |
| `aip_gc_objects_skipped_total` | `resource`, `reason` | Records skipped. Reasons: `dry_run`, `safety_valve`, `export_pending`. |
| `aip_gc_export_failures_total` | `resource` | Total transient failures communicating with the OTLP collector. |
| `aip_gc_scan_duration_seconds` | `resource` | Histogram of the time taken for a full GC scan cycle. |
| `aip_gc_scan_objects_evaluated_total` | `resource` | Total records read from etcd during the scan. |

### Useful queries

**Deletion rate (objects/sec):**
```promql
rate(aip_gc_objects_deleted_total[5m])
```

**Export failure count (last hour):**
```promql
increase(aip_gc_export_failures_total[1h])
```

**Export skip rate (due to pending export):**
```promql
rate(aip_gc_objects_skipped_total{reason="export_pending"}[5m])
```

---

## Safety mechanisms

- **Safety valve**: The GC engine will skip its cycle entirely if the total number of `AgentDiagnostic` objects in the cluster is below `--gc-safety-min-count` (default: 10). This prevents catastrophic data loss in case of misconfiguration.
- **Dry-run by default**: The default state of the engine is to log its intent without performing any deletions.
- **Rate limiting**: Deletions are throttled to `--gc-delete-rate-per-sec` (default: 100) to prevent overwhelming the Kubernetes API server or the underlying etcd.
- **Leader only**: GC logic only runs on the leader instance of the controller.

> [!WARNING]
> Disabling dry-run mode causes permanent, irreversible deletion of AgentDiagnostic records. Verify with dry-run=true first and confirm metrics before setting dryRun=false.

---

## Configuration reference

| Flag | Default | Phase | Description |
|---|---|---|---|
| `--gc-enabled` | `false` | Phase 1 | Enable the GC engine background loop. |
| `--gc-interval` | `1h` | Phase 1 | Time between GC scan cycles. |
| `--gc-dry-run` | `true` | Phase 1 | If true, log deletions without acting. |
| `--gc-diagnostic-hard-ttl` | `14d` | Phase 1 | Forced deletion age regardless of export state. |
| `--gc-delete-rate-per-sec` | `100` | Phase 1 | Max object deletions per second (token bucket). |
| `--gc-page-size` | `500` | Phase 1 | Objects per list page during GC scan. |
| `--gc-safety-min-count` | `10` | Phase 1 | Skip GC if total object count is below this threshold. |
| `--gc-diagnostic-retention-ttl` | `0` | Phase 2 | Soft retention window. Records older than this are exported. |
| `--gc-export-type` | `none` | Phase 2 | Export provider: `none` or `otlp`. |
| `--gc-otlp-endpoint` | `""` | Phase 2 | OTLP gRPC endpoint (required when export-type=otlp). |
| `--gc-export-concurrency` | `5` | Phase 2 | Number of concurrent async export workers. |
