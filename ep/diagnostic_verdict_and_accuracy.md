# Design: Diagnostic Verdict and Accuracy Tracking

## Problem

`AgentDiagnostic` records are immutable after creation — correct for audit integrity, but it means there is no structured channel for SREs to record whether a diagnosis was actually right. This gap has two concrete consequences:

1. **No soak-test signal.** Teams deploying agents run them in observation mode before granting autonomous action rights. Today the only way to evaluate diagnostic quality is informally — an SRE reads the summary, nods or grimaces, and moves on. There is no machine-readable record of that judgement.

2. **`AgentTrustProfile` has no empirical diagnostic signal.** The AIP spec (Section 3.7) defines trust profiles derived from `AgentRequest` outcomes — did the agent's actions succeed or fail? That measures *execution accuracy*. It does not measure *diagnostic accuracy* — whether the agent correctly identified the problem in the first place. An agent can consistently act on the wrong diagnosis and still show a high `successRate` if K8s happens to recover anyway.

The missing link is a structured human verdict on each `AgentDiagnostic`, aggregated per agent over time.

## Non-Goals

- Replace `CalibrationEvidence` (spec Section 3.1.5). That mechanism attests model benchmark performance at submission time via a signed JWT. This design is complementary: it measures in-production diagnostic quality via SRE observation.
- Implement the full `AgentTrustProfile` CRD. That is tracked in [agent-intent-protocol#7](https://github.com/ravisantoshgudimetla/agent-intent-protocol/issues/7). This design introduces a precursor CR that will feed into it.
- Build a reconciler/controller for aggregation. The gateway handler is sufficient for v1alpha1.

## Design

### Part 1: `AgentDiagnostic` status subresource (verdict)

Add a `status` stanza to `AgentDiagnostic` via the standard Kubernetes status subresource mechanism. Spec remains fully immutable — agents cannot touch `status`. Only authorized reviewers (SREs) can write it via `PATCH` on `/status`.

```go
type AgentDiagnosticStatus struct {
    // Verdict is the SRE's assessment of this diagnostic.
    // +kubebuilder:validation:Enum=correct;incorrect;partial
    // +optional
    Verdict string `json:"verdict,omitempty"`

    // ReviewedBy is the identity of the reviewer. Set server-side from the
    // authenticated caller — never accepted from the request body.
    // +optional
    ReviewedBy string `json:"reviewedBy,omitempty"`

    // ReviewedAt is the timestamp of the review. Set server-side.
    // +optional
    ReviewedAt *metav1.Time `json:"reviewedAt,omitempty"`

    // ReviewerNote is an optional free-text annotation.
    // +kubebuilder:validation:MaxLength=512
    // +optional
    ReviewerNote string `json:"reviewerNote,omitempty"`
}
```

Verdict values:

| Value | Meaning |
|-------|---------|
| `correct` | The agent's diagnosis accurately identified the root cause |
| `partial` | The diagnosis was directionally right but incomplete or imprecise |
| `incorrect` | The diagnosis was wrong |

`reviewedBy` **MUST** be set from the authenticated caller identity on the server side. The gateway endpoint MUST NOT accept it from the request body — any caller-supplied `reviewedBy` field MUST be silently ignored. This is a security invariant: allowing the client to set `reviewedBy` would let anyone impersonate any reviewer.

### Part 2: `DiagnosticAccuracySummary` CR (aggregation)

A new Kind that stores running verdict counts and a computed accuracy ratio per `agentIdentity` per namespace. One CR per agent, upserted by the gateway whenever a verdict is saved.

```go
// DiagnosticAccuracySummarySpec identifies the agent this summary tracks.
type DiagnosticAccuracySummarySpec struct {
    // AgentIdentity specifies the agent this summary tracks.
    // The gateway derives the CR name from this value using sanitizeDNSSegment:
    //   1. Convert to lowercase
    //   2. Replace any character outside [a-z0-9-] with '-'
    //   3. Collapse consecutive hyphens to one
    //   4. Trim leading/trailing hyphens
    //   5. Truncate to 63 characters
    // The raw agentIdentity is preserved in spec; the sanitized form is used
    // only as the CR name for K8s API compliance.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:MinLength=1
    AgentIdentity string `json:"agentIdentity"`
}

type DiagnosticAccuracySummaryStatus struct {
    // TotalReviewed is the total number of AgentDiagnostic records
    // with a non-empty verdict for this agentIdentity.
    // +kubebuilder:validation:Minimum=0
    TotalReviewed int64 `json:"totalReviewed"`

    // CorrectCount is the number of verdicts set to "correct".
    // +kubebuilder:validation:Minimum=0
    CorrectCount int64 `json:"correctCount"`

    // PartialCount is the number of verdicts set to "partial".
    // +kubebuilder:validation:Minimum=0
    PartialCount int64 `json:"partialCount"`

    // IncorrectCount is the number of verdicts set to "incorrect".
    // +kubebuilder:validation:Minimum=0
    IncorrectCount int64 `json:"incorrectCount"`

    // DiagnosticAccuracy is the computed accuracy ratio:
    //   (correctCount + 0.5 * partialCount) / totalReviewed
    // Null if totalReviewed == 0.
    // +optional
    DiagnosticAccuracy *float64 `json:"diagnosticAccuracy,omitempty"`

    // LastUpdatedAt is the timestamp of the most recent verdict that
    // contributed to this summary.
    LastUpdatedAt *metav1.Time `json:"lastUpdatedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
type DiagnosticAccuracySummary struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   DiagnosticAccuracySummarySpec   `json:"spec"`
    Status DiagnosticAccuracySummaryStatus `json:"status,omitempty"`
}
```

Example CR after 42 reviews:

```yaml
apiVersion: governance.aip.io/v1alpha1
kind: DiagnosticAccuracySummary
metadata:
  name: k8s-debug-agent
  namespace: production
spec:
  agentIdentity: k8s-debug-agent
status:
  totalReviewed: 42
  correctCount: 35
  partialCount: 4
  incorrectCount: 3
  diagnosticAccuracy: 0.881   # (35 + 0.5*4) / 42
  lastUpdatedAt: "2026-03-31T10:00:00Z"
```

### Accuracy formula

```text
diagnosticAccuracy = (correctCount + 0.5 × partialCount) / totalReviewed
```

This is intentionally a simple ratio, not an EMA. Reasons:

- **Order-independent.** The gateway handler may fail and retry; optimistic concurrency conflicts may cause re-reads. An EMA would produce different results depending on processing order. A ratio always produces the same result from the same set of counters.
- **Recomputable.** If the summary CR is ever deleted or corrupted, a `POST /agent-diagnostics/recompute-accuracy` endpoint (see below) can reconstruct it by scanning all `AgentDiagnostic` CRs in the namespace.
- **Legible.** "81% of diagnostics were correct" is immediately interpretable by an SRE. An EMA value is not.

The spec proposal in agent-intent-protocol#7 will advocate for this formula rather than EMA.

### Part 3: Gateway write path

`PATCH /agent-diagnostics/{name}/status` executes a three-step write (one read, two writes):

1. Read the existing `AgentDiagnostic` to capture the current verdict (may be empty) and `agentIdentity`.
2. Patch the `AgentDiagnostic` status subresource with the new verdict, `reviewedBy` (from the authenticated caller's identity), and `reviewedAt` (server time).
3. Upsert the `DiagnosticAccuracySummary` for the diagnostic's `agentIdentity`:
   - If old verdict was **empty** (first-time review): increment `totalReviewed` and the new verdict's counter.
   - If old verdict was **non-empty** (changing an existing verdict): decrement the old verdict's counter, increment the new verdict's counter. `totalReviewed` is unchanged — the diagnostic was already counted.
   - Recompute `diagnosticAccuracy` ratio. Write with `resourceVersion` for optimistic concurrency.

Step 3 is retried on `409 Conflict` (optimistic concurrency failure) using `retry.DefaultRetry` from `k8s.io/client-go/util/retry`: 5 attempts, 10ms base duration, linear backoff with 10% jitter (~55ms total worst case). If all retries are exhausted the verdict is still persisted (step 2 succeeded) but the summary counters are stale — the recompute endpoint is the recovery path.

#### Consistency trade-offs

**Step-failure drift.** Steps 2 and 3 are **not atomic** — Kubernetes has no cross-resource transactions. If step 2 succeeds and step 3 fails after exhausting retries, the diagnostic holds the new verdict but the summary counters are stale. This is acceptable for v1alpha1: `DiagnosticAccuracySummary` is an observability signal, not a safety-critical gate.

**Concurrent double-review race.** If two SREs submit verdicts for the same previously-unreviewed diagnostic simultaneously, both requests will read `oldVerdict = ""` at step 1 and both will increment `totalReviewed`, causing double-counting. The optimistic concurrency retry on step 3 serializes the summary writes but does not eliminate the double-increment because both requests already read the same empty old verdict before either write lands.

Both scenarios are mitigated by the recompute endpoint: `POST /agent-diagnostics/recompute-accuracy?namespace=X` reconstructs the summary from scratch by scanning all `AgentDiagnostic` CRs in the namespace and recomputing counters from their current `status.verdict` values. Operators should run this after any suspected drift.

### Part 4: Dashboard

The diagnostics tab gains:

- A **Verdict** column (8th column). Reviewed diagnostics show a colored badge: `correct` = green, `partial` = yellow, `incorrect` = red. Unreviewed diagnostics show a "Review" button.
- Clicking "Review" expands an inline form (same pattern as the existing Details toggle) with a verdict dropdown (`correct` / `partial` / `incorrect`) and an optional note textarea. Submitting calls `PATCH /agent-diagnostics/{name}/status` on the gateway (proxied by the dashboard as `PATCH /api/agent-diagnostics/{name}/status`).
- A **per-agent accuracy chip** above the diagnostics table, showing `diagnosticAccuracy` from the `DiagnosticAccuracySummary` for the currently-filtered agent. Visible only when the namespace has at least one reviewed diagnostic.

## RBAC

| Subject | Resource | Verbs |
|---------|----------|-------|
| Agent service account | `agentdiagnostics` | `create`, `get`, `list` |
| Agent service account | `agentdiagnostics/status` | — (no access) |
| Agent service account | `diagnosticaccuracysummaries` | — (no access) |
| SRE / editor | `agentdiagnostics/status` | — (no access; reviews submitted via gateway HTTP API only) |
| SRE / editor | `diagnosticaccuracysummaries` | `get`, `list`, `watch` |
| SRE / editor | `diagnosticaccuracysummaries/status` | — (no access) |
| Gateway service account | `agentdiagnostics` | `get`, `list` |
| Gateway service account | `agentdiagnostics/status` | `update`, `patch` |
| Gateway service account | `diagnosticaccuracysummaries` | `get`, `list`, `create` |
| Gateway service account | `diagnosticaccuracysummaries/status` | `update`, `patch` |
| Viewer | `agentdiagnostics`, `diagnosticaccuracysummaries` | `get`, `list`, `watch` |

## Migration path to `AgentTrustProfile`

`DiagnosticAccuracySummary` is a precursor. Once `AgentTrustProfile` is defined in the spec (agent-intent-protocol#7) and implemented as a CRD:

- `DiagnosticAccuracySummary.status.diagnosticAccuracy` maps directly to `AgentTrustProfile.status.diagnosticAccuracy`.
- Migration is a one-time batch: read all `DiagnosticAccuracySummary` CRs, write the `diagnosticAccuracy` field onto the corresponding `AgentTrustProfile`.
- `DiagnosticAccuracySummary` can then be deprecated and removed in a subsequent release.

## Implementation Checklist

- [ ] Add `AgentDiagnosticStatus` struct to `api/v1alpha1/agentdiagnostic_types.go`; add `Status AgentDiagnosticStatus` field to `AgentDiagnostic`; add `+kubebuilder:subresource:status` marker to the `AgentDiagnostic` type
- [ ] Add `DiagnosticAccuracySummary` type to `api/v1alpha1/`; add `+kubebuilder:subresource:status` marker
- [ ] Run `make manifests generate` to regenerate CRDs and deep copy (manifests first, then generate)
- [ ] Update RBAC roles: `agentdiagnostic_editor_role.yaml` must NOT grant `/status` write — remove `patch`/`update` from SRE/editor role; new `diagnosticaccuracysummary_editor_role.yaml` (read-only on `/status`)
- [ ] Add gateway service account `ClusterRole` granting `get`/`list` on `agentdiagnostics`, `update`/`patch` on `agentdiagnostics/status`, `get`/`list`/`create` on `diagnosticaccuracysummaries`, and `update`/`patch` on `diagnosticaccuracysummaries/status`
- [ ] Gateway: `PATCH /agent-diagnostics/{name}/status` handler with three-step write (read + two writes) and retry on 409 for summary upsert; use `sanitizeDNSSegment(agentIdentity, 63)` as the `DiagnosticAccuracySummary` CR name — never the raw `agentIdentity`
- [ ] Gateway: `POST /agent-diagnostics/recompute-accuracy` handler (scan + reconstruct summary)
- [ ] Dashboard: verdict badge column + inline review form
- [ ] Dashboard: per-agent accuracy chip above diagnostics table

## Relationship to `AgentDiagnostic` EP

See `ep/agent_diagnostic_design.md` for the original `AgentDiagnostic` design rationale (immutability, no-controller, retention). This EP extends that design; the trust model table in that EP remains accurate — `DiagnosticAccuracySummary` is authored by the control plane (gateway), not the agent.
