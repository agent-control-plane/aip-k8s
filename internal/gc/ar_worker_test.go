package gc

import (
	"context"
	"fmt"
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

type arFailingClient struct {
	client.Client
	failDeleteName string
	notFoundDelete bool
}

func (f *arFailingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if f.notFoundDelete {
		return apierrors.NewNotFound(schema.GroupResource{Group: "governance.aip.io", Resource: "agentrequests"}, obj.GetName())
	}
	if f.failDeleteName != "" && obj.GetName() == f.failDeleteName {
		return fmt.Errorf("simulated error")
	}
	return f.Client.Delete(ctx, obj, opts...)
}

func TestARGCWorker_Run(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := governancev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to register scheme: %v", err)
	}

	now := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)
	arHardTTL := 1 * time.Hour
	config := GCConfig{
		Enabled:          true,
		DryRun:           false,
		HardTTL:          arHardTTL,
		SafetyMinCount:   1,
		PageSize:         10,
		DeleteRatePerSec: 100,
	}

	t.Run("Terminal AR past hard TTL is deleted with its AuditRecord", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		expiredTime := metav1.NewTime(now.Add(-arHardTTL))

		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "default", CreationTimestamp: expiredTime},
			Status:     governancev1alpha1.AgentRequestStatus{Phase: governancev1alpha1.PhaseCompleted},
		}
		audit := &governancev1alpha1.AuditRecord{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audit-1",
				Namespace: "default",
				Labels:    map[string]string{"aip.io/agentRequestRef": "expired"},
			},
			Spec: governancev1alpha1.AuditRecordSpec{
				AgentRequestRef: "expired",
				Timestamp:       metav1.NewTime(now),
				AgentIdentity:   "test-agent",
				Event:           governancev1alpha1.AuditEventRequestCompleted,
				Action:          "scale",
				TargetURI:       "k8s://default/deployment/test",
			},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ar, audit).Build()
		worker := &ARGCWorker{
			APIReader: c,
			Client:    c,
			Config:    config,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		gm.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ar), ar)).To(gomega.Satisfy(apierrors.IsNotFound))
		gm.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(audit), audit)).To(gomega.Satisfy(apierrors.IsNotFound))
	})

	t.Run("Active AR past hard TTL is NOT deleted", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		expiredTime := metav1.NewTime(now.Add(-arHardTTL))

		phases := []string{
			governancev1alpha1.PhaseExecuting,
			governancev1alpha1.PhaseApproved,
			governancev1alpha1.PhaseAwaitingVerdict,
			governancev1alpha1.PhasePending,
		}

		for _, phase := range phases {
			ar := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "active-" + phase, Namespace: "default", CreationTimestamp: expiredTime},
				Status:     governancev1alpha1.AgentRequestStatus{Phase: phase},
			}
			// Add a terminal AR to pass safety valve
			terminalAR := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "terminal-" + phase, Namespace: "default", CreationTimestamp: metav1.NewTime(now)},
				Status:     governancev1alpha1.AgentRequestStatus{Phase: governancev1alpha1.PhaseCompleted},
			}

			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ar, terminalAR).Build()
			worker := &ARGCWorker{
				APIReader: c,
				Client:    c,
				Config:    config,
				Now:       func() time.Time { return now },
				Limiter:   rate.NewLimiter(rate.Inf, 1),
			}

			err := worker.Run(context.Background())
			gm.Expect(err).NotTo(gomega.HaveOccurred())
			gm.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ar), ar)).To(gomega.Succeed())
		}
	})

	t.Run("AgentRequestHardTTL=0 disables deletion", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		disabledConfig := config
		disabledConfig.HardTTL = 0
		expiredTime := metav1.NewTime(now.Add(-arHardTTL))

		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "default", CreationTimestamp: expiredTime},
			Status:     governancev1alpha1.AgentRequestStatus{Phase: governancev1alpha1.PhaseCompleted},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ar).Build()
		worker := &ARGCWorker{
			APIReader: c,
			Client:    c,
			Config:    disabledConfig,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())
		gm.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ar), ar)).To(gomega.Succeed())
	})

	t.Run("Dry-run only logs", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		dryRunConfig := config
		dryRunConfig.DryRun = true
		expiredTime := metav1.NewTime(now.Add(-arHardTTL))

		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "default", CreationTimestamp: expiredTime},
			Status:     governancev1alpha1.AgentRequestStatus{Phase: governancev1alpha1.PhaseCompleted},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ar).Build()
		worker := &ARGCWorker{
			APIReader: c,
			Client:    c,
			Config:    dryRunConfig,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())
		gm.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ar), ar)).To(gomega.Succeed())
	})

	t.Run("Hard TTL boundary", func(g *testing.T) {
		gm := gomega.NewWithT(g)

		// Exactly at cutoff -> deleted
		ar1 := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "at-cutoff", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-arHardTTL))},
			Status:     governancev1alpha1.AgentRequestStatus{Phase: governancev1alpha1.PhaseCompleted},
		}
		// One second before cutoff -> NOT deleted
		ar2 := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "before-cutoff", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-arHardTTL).Add(time.Second))},
			Status:     governancev1alpha1.AgentRequestStatus{Phase: governancev1alpha1.PhaseCompleted},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ar1, ar2).Build()
		worker := &ARGCWorker{
			APIReader: c,
			Client:    c,
			Config:    config,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		gm.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ar1), ar1)).To(gomega.Satisfy(apierrors.IsNotFound))
		gm.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ar2), ar2)).To(gomega.Succeed())
	})

	t.Run("Safety valve: terminal count below threshold skips scan", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		safetyConfig := config
		safetyConfig.SafetyMinCount = 5

		expiredTime := metav1.NewTime(now.Add(-arHardTTL))
		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "default", CreationTimestamp: expiredTime},
			Status:     governancev1alpha1.AgentRequestStatus{Phase: governancev1alpha1.PhaseCompleted},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ar).Build()
		worker := &ARGCWorker{
			APIReader: c,
			Client:    c,
			Config:    safetyConfig,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())
		gm.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ar), ar)).To(gomega.Succeed())
	})

	t.Run("Paging: terminal ARs across multiple pages are deleted", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		pagingConfig := config
		pagingConfig.PageSize = 2
		pagingConfig.SafetyMinCount = 1

		var objects []runtime.Object
		for i := range 5 {
			objects = append(objects, &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("expired-%d", i), Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-arHardTTL))},
				Status:     governancev1alpha1.AgentRequestStatus{Phase: governancev1alpha1.PhaseCompleted},
			})
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
		worker := &ARGCWorker{
			APIReader: c,
			Client:    c,
			Config:    pagingConfig,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		var list governancev1alpha1.AgentRequestList
		gm.Expect(c.List(context.Background(), &list)).To(gomega.Succeed())
		gm.Expect(list.Items).To(gomega.BeEmpty())
	})

	t.Run("Delete failure on AuditRecord still deletes AR", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		expiredTime := metav1.NewTime(now.Add(-arHardTTL))

		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "default", CreationTimestamp: expiredTime},
			Status:     governancev1alpha1.AgentRequestStatus{Phase: governancev1alpha1.PhaseCompleted},
		}
		audit := &governancev1alpha1.AuditRecord{
			ObjectMeta: metav1.ObjectMeta{Name: "fail-audit", Namespace: "default"},
			Spec: governancev1alpha1.AuditRecordSpec{
				AgentRequestRef: "expired",
				Timestamp:       metav1.NewTime(now),
				AgentIdentity:   "test-agent",
				Event:           governancev1alpha1.AuditEventRequestCompleted,
				Action:          "scale",
				TargetURI:       "k8s://default/deployment/test",
			},
		}

		innerClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ar, audit).Build()
		c := &arFailingClient{Client: innerClient, failDeleteName: "fail-audit"}

		worker := &ARGCWorker{
			APIReader: c,
			Client:    c,
			Config:    config,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		gm.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ar), ar)).To(gomega.Satisfy(apierrors.IsNotFound))
		// AuditRecord still exists because deletion failed
		gm.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(audit), audit)).To(gomega.Succeed())
	})

	t.Run("404 on delete is ignored", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		expiredTime := metav1.NewTime(now.Add(-arHardTTL))

		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "default", CreationTimestamp: expiredTime},
			Status:     governancev1alpha1.AgentRequestStatus{Phase: governancev1alpha1.PhaseCompleted},
		}

		innerClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ar).Build()
		c := &arFailingClient{Client: innerClient, notFoundDelete: true}

		worker := &ARGCWorker{
			APIReader: c,
			Client:    c,
			Config:    config,
			Now:       func() time.Time { return now },
			Limiter:   rate.NewLimiter(rate.Inf, 1),
		}

		err := worker.Run(context.Background())
		gm.Expect(err).NotTo(gomega.HaveOccurred())
	})
}
