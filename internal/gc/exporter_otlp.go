package gc

import (
	"context"
	"time"

	governancev1alpha1 "github.com/ravisantoshgudimetla/aip-k8s/api/v1alpha1"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

var _ Exporter = (*OTLPExporter)(nil)

// OTLPExporter implements the Exporter interface by sending records to an OTLP collector.
type OTLPExporter struct {
	provider *sdklog.LoggerProvider
	logger   log.Logger
	// Clock is the clock function. Set to time.Now in production; injectable in tests.
	Clock func() time.Time
	// insecure indicates if the connection to the OTLP collector should be insecure.
	insecure bool
}

// NewOTLPExporter creates an OTLPExporter that sends records to the given
// gRPC endpoint (e.g. "otel-collector:4317").
func NewOTLPExporter(ctx context.Context, endpoint string, insecure bool) (*OTLPExporter, error) {
	opts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(endpoint),
	}
	if insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}

	exporter, err := otlploggrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	processor := sdklog.NewBatchProcessor(exporter)
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(processor),
	)
	logger := provider.Logger("aip-k8s/gc")

	return &OTLPExporter{
		provider: provider,
		logger:   logger,
		Clock:    time.Now,
		insecure: insecure,
	}, nil
}

// Shutdown shuts down the exporter's provider.
func (e *OTLPExporter) Shutdown(ctx context.Context) error {
	return e.provider.Shutdown(ctx)
}

// Export maps an AgentDiagnostic to an OTLP log record and emits it.
// logger.Emit is asynchronous and best-effort; Export returning nil only
// indicates the record was successfully queued, not acknowledged by the collector.
func (e *OTLPExporter) Export(ctx context.Context, diag *governancev1alpha1.AgentDiagnostic) error {
	now := e.Clock()
	record := log.Record{}
	record.SetTimestamp(diag.CreationTimestamp.Time)
	record.SetObservedTimestamp(now)
	record.SetSeverity(log.SeverityInfo)
	record.SetBody(log.StringValue(diag.Spec.Summary))

	attrs := []log.KeyValue{
		log.String("aip.diagnostic.name", diag.Name),
		log.String("aip.diagnostic.namespace", diag.Namespace),
		log.String("aip.diagnostic.uid", string(diag.UID)),
		log.String("aip.diagnostic.agent_identity", diag.Spec.AgentIdentity),
		log.String("aip.diagnostic.type", diag.Spec.DiagnosticType),
		log.String("aip.diagnostic.correlation_id", diag.Spec.CorrelationID),
		log.String("aip.diagnostic.summary", diag.Spec.Summary),
		log.String("aip.diagnostic.created_at", diag.CreationTimestamp.UTC().Format(time.RFC3339)),
	}

	if diag.Status.Verdict != "" {
		attrs = append(attrs, log.String("aip.diagnostic.verdict", diag.Status.Verdict))
	}
	if diag.Status.ReviewedBy != "" {
		attrs = append(attrs, log.String("aip.diagnostic.reviewed_by", diag.Status.ReviewedBy))
	}

	record.AddAttributes(attrs...)

	e.logger.Emit(ctx, record)
	return nil
}
