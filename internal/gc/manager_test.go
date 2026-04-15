package gc

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGCManager_Start(t *testing.T) {
	t.Run("Manager disabled - Start returns with no activity", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		config := DefaultGCConfig()
		config.Enabled = false

		mgr := &GCManager{
			Config: config,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := mgr.Start(ctx)
		gm.Expect(err).To(gomega.Succeed())
	})

	t.Run("Start respects context cancellation", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		// Verifies that Start returns nil when the context is cancelled.
		// Worker invocation is exercised by the integration and worker unit tests;
		// a full mock-worker injection would require refactoring GCManager (Phase 2).

		config := DefaultGCConfig()
		config.Enabled = true
		config.Interval = 1 * time.Millisecond

		c := fake.NewClientBuilder().Build()
		mgr := &GCManager{
			APIReader: c,
			Client:    c,
			Config:    config,
			Now:       time.Now,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		err := mgr.Start(ctx)
		gm.Expect(err).To(gomega.Succeed())
	})
}
