package gc

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

// failingClient is a client that can fail specific operations
type failingClient struct {
	client.Client
	failDeleteName string
	notFoundDelete bool
}

func (f *failingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if f.notFoundDelete {
		return apierrors.NewNotFound(schema.GroupResource{Group: "governance.aip.io", Resource: "agentdiagnostics"}, obj.GetName())
	}
	if f.failDeleteName != "" && obj.GetName() == f.failDeleteName {
		return fmt.Errorf("simulated error")
	}
	return f.Client.Delete(ctx, obj, opts...)
}

// Shared test constants. Using named values avoids magic numbers drifting
// independently across subtests.
const (
	testHardTTL          = 14 * 24 * time.Hour
	testPageSize         = 500
	testDeleteRatePerSec = 100
	testSafetyMinCount   = 2 // low so most tests need only a couple of objects
)

func TestGCWorker_Run(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := governancev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to register scheme: %v", err)
	}

	now := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)
	config := GCConfig{
		Enabled:           true,
		DryRun:            false,
		DiagnosticHardTTL: testHardTTL,
		PageSize:          testPageSize,
		DeleteRatePerSec:  testDeleteRatePerSec,
		SafetyMinCount:    testSafetyMinCount,
	}

	t.Run("Hard TTL boundary - object exactly at cutoff is deleted", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		// Cutoff is now - testHardTTL.
		// diagnostic creation timestamp <= cutoff should be deleted.
		expiredTime := metav1.NewTime(now.Add(-testHardTTL))

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "default", CreationTimestamp: expiredTime},
		}
		// Add another one to pass safety valve
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		worker := &GCWorker{
			APIReader: c,
			Client:    c,
			Config:    config,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		// Verify deletion
		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(1))
		gm.Expect(list.Items[0].Name).To(gomega.Equal("safety"))
	})

	t.Run("Hard TTL boundary - object one second before cutoff is NOT deleted", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		notExpiredTime := metav1.NewTime(now.Add(-testHardTTL).Add(time.Second))

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "not-expired", Namespace: "default", CreationTimestamp: notExpiredTime},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		worker := &GCWorker{
			APIReader: c,
			Client:    c,
			Config:    config,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(2))
	})

	t.Run("Dry-run mode - expired object is logged but not deleted", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		dryRunConfig := config
		dryRunConfig.DryRun = true

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "expired-but-dry-run",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-20 * 24 * time.Hour)),
			},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		worker := &GCWorker{
			APIReader: c,
			Client:    c,
			Config:    dryRunConfig,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(2))
	})

	t.Run("Safety valve - total count below threshold skips scan", func(g *testing.T) {
		gm := gomega.NewWithT(g)

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "expired",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-20 * 24 * time.Hour)),
			},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag).Build()
		worker := &GCWorker{
			APIReader: c,
			Client:    c,
			Config:    config, // SafetyMinCount = 2
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		// Should not be deleted due to safety valve
		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(1))
	})

	t.Run("Paging - expired objects across multiple pages are all deleted", func(g *testing.T) {
		gm := gomega.NewWithT(g)

		var diags []runtime.Object
		for i := range 5 {
			diags = append(diags, &governancev1alpha1.AgentDiagnostic{
				ObjectMeta: metav1.ObjectMeta{
					Name:              fmt.Sprintf("expired-%d", i),
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(now.Add(-20 * 24 * time.Hour)),
				},
			})
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diags...).Build()

		// Page size = 2
		pagingConfig := config
		pagingConfig.PageSize = 2

		worker := &GCWorker{
			APIReader: c,
			Client:    c,
			Config:    pagingConfig,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.BeEmpty())
	})

	t.Run("Delete failure - scan continues after error", func(g *testing.T) {
		gm := gomega.NewWithT(g)

		diagFail := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "fail-me", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-20 * time.Hour * 24))},
		}
		diagOk := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "delete-me", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-20 * time.Hour * 24))},
		}
		diagSafety := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diagFail, diagOk, diagSafety).Build()

		fc := &failingClient{
			Client:         c,
			failDeleteName: "fail-me",
		}

		worker := &GCWorker{
			APIReader: fc,
			Client:    fc,
			Config:    config,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		// fail-me should still exist, delete-me should be gone
		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(2))
		names := []string{list.Items[0].Name, list.Items[1].Name}
		gm.Expect(names).To(gomega.ContainElements("fail-me", "safety"))
	})

	t.Run("404 on delete is silently ignored", func(g *testing.T) {
		gm := gomega.NewWithT(g)

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-20 * time.Hour * 24))},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		fc := &failingClient{
			Client:         c,
			notFoundDelete: true,
		}

		worker := &GCWorker{
			APIReader: fc,
			Client:    fc,
			Config:    config,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())
	})

	t.Run("Safety valve - count exactly at threshold proceeds with GC", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		// SafetyMinCount = 2. Create exactly 2 objects past hard TTL.
		// Both should be deleted.
		diag1 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "expired-1", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-20 * 24 * time.Hour))},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "expired-2", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-20 * 24 * time.Hour))},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag1, diag2).Build()
		worker := &GCWorker{
			APIReader: c,
			Client:    c,
			Config:    config, // SafetyMinCount = 2
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.BeEmpty())
	})

	t.Run("Paging - unexpired objects on page 1, expired on page 2", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		// Page size = 2. The fake client returns objects in lexicographic name order.
		// "a-" prefix sorts before "z-", so:
		//   Page 1: a-unexpired-1, a-unexpired-2  (not expired → kept)
		//   Page 2: z-expired-1,   z-expired-2    (expired → deleted)
		//   Page 3: z-expired-3                   (expired → deleted)
		unexpired1 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "a-unexpired-1", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}
		unexpired2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "a-unexpired-2", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}
		expired1 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "z-expired-1", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-20 * 24 * time.Hour))},
		}
		expired2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "z-expired-2", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-20 * 24 * time.Hour))},
		}
		expired3 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "z-expired-3", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-20 * 24 * time.Hour))},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
			unexpired1, unexpired2, expired1, expired2, expired3,
		).Build()

		pagingConfig := config
		pagingConfig.PageSize = 2
		pagingConfig.SafetyMinCount = 1

		worker := &GCWorker{
			APIReader: c,
			Client:    c,
			Config:    pagingConfig,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(2))
		names := []string{list.Items[0].Name, list.Items[1].Name}
		gm.Expect(names).To(gomega.ContainElements("a-unexpired-1", "a-unexpired-2"))
	})

	t.Run("Rate limiter - token consumption is exactly N for N deletions", func(g *testing.T) {
		gm := gomega.NewWithT(g)

		var diags []runtime.Object
		for i := range 5 {
			diags = append(diags, &governancev1alpha1.AgentDiagnostic{
				ObjectMeta: metav1.ObjectMeta{
					Name:              fmt.Sprintf("expired-%d", i),
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(now.Add(-20 * 24 * time.Hour)),
				},
			})
		}
		// Add safety to avoid firing safety valve
		diags = append(diags, &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		})

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diags...).Build()

		limiter := &countingLimiter{Limiter: rate.NewLimiter(rate.Inf, 1)}
		worker := &GCWorker{
			APIReader: c,
			Client:    c,
			Config:    config,
			Now:       func() time.Time { return now },
			Limiter:   limiter,
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		gm.Expect(limiter.waitCount.Load()).To(gomega.Equal(int64(5)))
	})

	t.Run("Soft retention - object before retention window is NOT deleted", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		cfg := config
		cfg.DiagnosticRetentionTTL = 7 * 24 * time.Hour
		cfg.DiagnosticHardTTL = 14 * 24 * time.Hour

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "before-retention",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-5 * 24 * time.Hour)),
			},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		worker := NewGCWorker(c, c, cfg, func() time.Time { return now }, rate.NewLimiter(rate.Inf, 1), nil)

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(2))
	})

	t.Run("Soft retention - object past retention window (no pool) is deleted", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		cfg := config
		cfg.DiagnosticRetentionTTL = 7 * 24 * time.Hour
		cfg.DiagnosticHardTTL = 14 * 24 * time.Hour

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "past-retention",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-8 * 24 * time.Hour)),
			},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		worker := NewGCWorker(c, c, cfg, func() time.Time { return now }, rate.NewLimiter(rate.Inf, 1), nil)

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(1))
		gm.Expect(list.Items[0].Name).To(gomega.Equal("safety"))
	})

	t.Run("Hard TTL overrides export — object past hard TTL deleted unconditionally", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		cfg := config
		cfg.DiagnosticRetentionTTL = 7 * 24 * time.Hour
		cfg.ExportType = exportTypeOTLP
		cfg.Concurrency = 5

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "past-hard-ttl",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-15 * 24 * time.Hour)),
			},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		pool := NewExportPool(context.Background(), 1, &mockExporter{
			exportFn: func(ctx context.Context, obj *governancev1alpha1.AgentDiagnostic) error {
				return fmt.Errorf("fail")
			},
		})
		defer pool.Stop()
		worker := NewGCWorker(c, c, cfg, func() time.Time { return now }, rate.NewLimiter(rate.Inf, 1), pool)

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(1))
		gm.Expect(list.Items[0].Name).To(gomega.Equal("safety"))
	})

	t.Run("Export path - successful export causes deletion", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		cfg := config
		cfg.DiagnosticRetentionTTL = 7 * 24 * time.Hour
		cfg.ExportType = exportTypeOTLP

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "to-export",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-8 * 24 * time.Hour)),
			},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		pool := NewExportPool(context.Background(), 5, NoopExporter{})
		worker := NewGCWorker(c, c, cfg, func() time.Time { return now }, rate.NewLimiter(rate.Inf, 1), pool)

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())
		pool.Stop() // wait for async delete

		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(1))
		gm.Expect(list.Items[0].Name).To(gomega.Equal("safety"))
	})

	t.Run("Export failure - object skipped with export_pending, retryState populated", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		cfg := config
		cfg.DiagnosticRetentionTTL = 7 * 24 * time.Hour
		cfg.ExportType = exportTypeOTLP

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "fail-export",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-8 * 24 * time.Hour)),
			},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		pool := NewExportPool(context.Background(), 1, &mockExporter{
			exportFn: func(ctx context.Context, obj *governancev1alpha1.AgentDiagnostic) error {
				return fmt.Errorf("permanent fail")
			},
		})
		defer pool.Stop()
		worker := NewGCWorker(c, c, cfg, func() time.Time { return now }, rate.NewLimiter(rate.Inf, 1), pool)

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())
		pool.Stop() // wait for async failure callback

		// Object should still exist
		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(2))

		// retryState should have an entry
		key := "default/fail-export"
		gm.Expect(worker.retryState).To(gomega.HaveKey(key))
		gm.Expect(worker.retryState[key].attempts).To(gomega.Equal(1))
	})

	t.Run("Retry backoff - object is skipped when nextRetryAt is in the future", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		cfg := config
		cfg.DiagnosticRetentionTTL = 7 * 24 * time.Hour
		cfg.ExportType = exportTypeOTLP

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "retry-later",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-8 * 24 * time.Hour)),
			},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		pool := NewExportPool(context.Background(), 1, NoopExporter{})
		defer pool.Stop()
		worker := NewGCWorker(c, c, cfg, func() time.Time { return now }, rate.NewLimiter(rate.Inf, 1), pool)

		// Pre-populate retryState
		worker.retryState["default/retry-later"] = &retryRecord{
			attempts:    1,
			nextRetryAt: now.Add(time.Hour),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		// Object should still exist and NOT be submitted to pool
		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(2))
	})

	t.Run("retentionTTL=0 with export pool — objects NOT deleted (hard TTL only mode)", func(g *testing.T) {
		// Regression test: when DiagnosticRetentionTTL==0, the export path (step 4)
		// must be skipped even if Pool != nil and ExportType="otlp".
		// Without the guard, all objects under the hard TTL would be exported+deleted.
		gm := gomega.NewWithT(g)
		cfg := config
		cfg.DiagnosticRetentionTTL = 0 // disabled — hard TTL only
		cfg.ExportType = exportTypeOTLP
		cfg.Concurrency = 2

		// Object is 8 days old: past any soft retention, but not past 14d hard TTL.
		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "should-not-export",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-8 * 24 * time.Hour)),
			},
		}
		diag2 := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{Name: "safety", Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(diag, diag2).Build()
		pool := NewExportPool(context.Background(), cfg.Concurrency, NoopExporter{})
		worker := NewGCWorker(c, c, cfg, func() time.Time { return now }, rate.NewLimiter(rate.Inf, 1), pool)

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())
		pool.Stop()

		// Object must still exist — no soft retention configured, hard TTL not reached
		var list governancev1alpha1.AgentDiagnosticList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.HaveLen(2))
	})
}

func TestNextBackoff(t *testing.T) {
	gm := gomega.NewWithT(t)

	t.Run("nextBackoff respects max of 10 minutes", func(t *testing.T) {
		res := nextBackoff(100)
		gm.Expect(res).To(gomega.BeNumerically("<=", 10*time.Minute))
	})

	t.Run("nextBackoff jitter is within 20% of base", func(t *testing.T) {
		// attempts=0 -> base=5s. Jitter is +/- 20% -> [4s, 6s]
		for range 100 {
			res := nextBackoff(0)
			gm.Expect(res).To(gomega.BeNumerically(">=", 4*time.Second))
			gm.Expect(res).To(gomega.BeNumerically("<=", 6*time.Second))
		}
	})
}

type countingLimiter struct {
	*rate.Limiter
	waitCount atomic.Int64
}

func (l *countingLimiter) Wait(ctx context.Context) error {
	l.waitCount.Add(1)
	return l.Limiter.Wait(ctx)
}
