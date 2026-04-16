# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- **GC engine Phase 1 (hard TTL)** — `GCManager` and `GCWorker` in
  `internal/gc/` protect etcd from unbounded AgentDiagnostic accumulation.
  Paginated list-and-delete with token-bucket rate limiting (100 obj/s),
  configurable hard TTL (default 14 days), dry-run mode (default on), and a
  safety valve that skips GC if total object count falls below a minimum
  threshold. Prometheus metrics: `aip_gc_objects_deleted_total`,
  `aip_gc_objects_skipped_total`, `aip_gc_scan_duration_seconds`,
  `aip_gc_scan_objects_evaluated_total`.
- **GC engine Phase 2 (OTLP export + soft retention)** — Adds the
  `Exporter` interface with `OTLPExporter` and `NoopExporter` implementations.
  Records are exported to an OTLP log collector before deletion when
  `--gc-export-type=otlp` and `--gc-diagnostic-retention-ttl` are set.
  Bounded async worker pool (`--gc-export-concurrency`, default 5) with
  exponential-backoff retry (base 5 s, max 10 min, ±20 % jitter via
  `crypto/rand`). Hard TTL overrides failed export unconditionally.
  New metric: `aip_gc_export_failures_total`.
- **AgentRequest CRD** — agents declare intent (action, target URI, reason) before
  acting; the control plane drives the full lifecycle: Pending → Approved →
  Executing → Completed / Failed / Denied.
- **SafetyPolicy CRD** — CEL-based governance rules (Allow / Deny /
  RequireApproval / Log) with TargetSelector filtering by action, resource type,
  and attributes. Supports FailClosed and FailOpen evaluation modes.
- **OpsLocks** — exclusive per-target locks backed by Kubernetes Leases
  (`aip-lock-<sha256-of-targetURI>`). Prevents concurrent conflicting operations.
  Configurable wait timeout with `LOCK_TIMEOUT` denial code on expiry.
- **Human approval path** — policies may emit `RequireApproval`; a reviewer
  patches `spec.humanApproval.decision` to `approved` or `denied` and the
  controller drives the state machine accordingly.
- **AuditRecord CRDs** — immutable, owner-referenced records emitted for every
  phase transition and governance event (`request.submitted`, `request.approved`,
  `request.denied`, `request.executing`, `request.completed`, `request.failed`,
  `lock.acquired`, `lock.released`, `lock.expired`, `policy.evaluated`,
  `cascade.mismatch`).
- **ControlPlaneVerification** — live cluster state (replica counts, endpoint
  health, downstream services) fetched independently before policy evaluation and
  persisted in `status.controlPlaneVerification`. Agents cannot influence this
  data.
- **Cascade model cross-verification** — the control plane independently verifies
  each target declared in `spec.cascadeModel` against the live cluster rather than
  trusting agent-provided assertions.
- **Execution timeout** — controller monitors the OpsLock Lease heartbeat during
  execution and transitions to Failed with a `lock.expired` audit event if the
  agent stops renewing before completion.
- **Demo gateway** (`demo/gateway`) — HTTP API bridge between agents and the
  Kubernetes control plane (`POST /agent-requests`, `GET /agent-requests/{name}`,
  `POST /agent-requests/{name}/executing`, `POST /agent-requests/{name}/completed`).
- **Visual audit dashboard** (`demo/dashboard`) — browser UI for listing
  AgentRequests and AuditRecords, and approving / denying requests awaiting human
  review.
- **`make demo-up` / `make demo-down`** — single entry point to build and start
  (or stop) the gateway and dashboard together.
