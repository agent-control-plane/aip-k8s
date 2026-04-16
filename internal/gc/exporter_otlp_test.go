package gc

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"
	governancev1alpha1 "github.com/ravisantoshgudimetla/aip-k8s/api/v1alpha1"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type fakeOTLPServer struct {
	collogspb.UnimplementedLogsServiceServer
	mu       sync.Mutex
	requests []*collogspb.ExportLogsServiceRequest
}

func (s *fakeOTLPServer) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	return &collogspb.ExportLogsServiceResponse{}, nil
}

func startFakeOTLPServer(t *testing.T) (*fakeOTLPServer, string) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	fake := &fakeOTLPServer{}
	collogspb.RegisterLogsServiceServer(s, fake)
	go func() {
		if err := s.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("failed to serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		s.Stop()
	})
	return fake, lis.Addr().String()
}

func TestOTLPExporter(t *testing.T) {
	t.Run("Export sends a LogRecord with correct attributes", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		fake, endpoint := startFakeOTLPServer(t)
		ctx := context.Background()
		fixedTime := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
		exporter, err := NewOTLPExporter(ctx, endpoint, true)
		gm.Expect(err).NotTo(gomega.HaveOccurred())
		exporter.Clock = func() time.Time { return fixedTime }

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "diag1",
				Namespace:         "default",
				UID:               types.UID("uid1"),
				CreationTimestamp: metav1.NewTime(time.Now().Truncate(time.Second)),
			},
			Spec: governancev1alpha1.AgentDiagnosticSpec{
				AgentIdentity:  "agent1",
				DiagnosticType: "type1",
				CorrelationID:  "corr1",
				Summary:        "summary1",
			},
			Status: governancev1alpha1.AgentDiagnosticStatus{
				Verdict:    "correct",
				ReviewedBy: "reviewer1",
			},
		}

		err = exporter.Export(ctx, diag)
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		// Shutdown to flush batches
		err = exporter.Shutdown(ctx)
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		gm.Expect(fake.requests).To(gomega.HaveLen(1))
		record := fake.requests[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
		gm.Expect(record.Body.Value.(*commonpb.AnyValue_StringValue).StringValue).To(gomega.Equal("summary1"))

		attrs := make(map[string]string)
		for _, attr := range record.Attributes {
			attrs[attr.Key] = attr.Value.Value.(*commonpb.AnyValue_StringValue).StringValue
		}

		gm.Expect(attrs["aip.diagnostic.agent_identity"]).To(gomega.Equal("agent1"))
		gm.Expect(attrs["aip.diagnostic.correlation_id"]).To(gomega.Equal("corr1"))
		gm.Expect(attrs["aip.diagnostic.verdict"]).To(gomega.Equal("correct"))
		gm.Expect(record.ObservedTimeUnixNano).To(gomega.Equal(uint64(fixedTime.UnixNano())))
	})

	t.Run("Export omits verdict attribute when Status.Verdict is empty", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		fake, endpoint := startFakeOTLPServer(t)
		ctx := context.Background()
		exporter, err := NewOTLPExporter(ctx, endpoint, true)
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "diag2"},
			Spec:       governancev1alpha1.AgentDiagnosticSpec{Summary: "summary2"},
		}

		err = exporter.Export(ctx, diag)
		gm.Expect(err).NotTo(gomega.HaveOccurred())
		err = exporter.Shutdown(ctx)
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		gm.Expect(fake.requests).To(gomega.HaveLen(1))
		record := fake.requests[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
		for _, attr := range record.Attributes {
			gm.Expect(attr.Key).NotTo(gomega.Equal("aip.diagnostic.verdict"))
		}
	})

	t.Run("Shutdown is idempotent — second call returns nil", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		_, endpoint := startFakeOTLPServer(t)
		ctx := context.Background()
		exporter, err := NewOTLPExporter(ctx, endpoint, true)
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		err1 := exporter.Shutdown(ctx)
		gm.Expect(err1).NotTo(gomega.HaveOccurred())

		err2 := exporter.Shutdown(ctx)
		gm.Expect(err2).NotTo(gomega.HaveOccurred())
	})
}
