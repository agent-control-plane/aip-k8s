package gc

import (
	"context"
	"testing"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/onsi/gomega"
)

func TestNoopExporter(t *testing.T) {
	gm := gomega.NewWithT(t)
	exporter := NoopExporter{}
	obj := &governancev1alpha1.AgentDiagnostic{}

	t.Run("NoopExporter always returns nil", func(t *testing.T) {
		err := exporter.Export(context.Background(), obj)
		gm.Expect(err).To(gomega.Succeed())
	})

	t.Run("NoopExporter is safe with cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := exporter.Export(ctx, obj)
		gm.Expect(err).To(gomega.Succeed())
	})
}
