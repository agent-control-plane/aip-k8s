package gc

import (
	"context"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

// Exporter is implemented by all export providers.
// Export must be idempotent — the same object may be exported more than once
// (e.g., after a leader transition resets in-memory retry state).
// Export must return a non-nil error only for transient failures; permanent failures
// (e.g., the object cannot be serialised) should be logged and return nil so the
// GC loop continues and the hard TTL eventually cleans up the object.
type Exporter interface {
	Export(ctx context.Context, obj *governancev1alpha1.AgentDiagnostic) error
}

// NoopExporter discards all records. Used when ExportType == "none".
type NoopExporter struct{}

func (NoopExporter) Export(_ context.Context, _ *governancev1alpha1.AgentDiagnostic) error {
	return nil
}
