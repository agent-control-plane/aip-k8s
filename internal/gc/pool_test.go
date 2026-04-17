package gc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/onsi/gomega"
)

func TestExportPool(t *testing.T) {
	t.Run("Submit returns true and onSuccess is called for successful export", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		var counter int32
		exporter := &mockExporter{
			exportFn: func(ctx context.Context, obj *governancev1alpha1.AgentDiagnostic) error {
				return nil
			},
		}
		pool := NewExportPool(context.Background(), 2, exporter)
		success := pool.Submit(testDiag("diag1"), func() { atomic.AddInt32(&counter, 1) }, func(err error) {})
		gm.Expect(success).To(gomega.BeTrue())
		pool.Stop()
		gm.Expect(atomic.LoadInt32(&counter)).To(gomega.Equal(int32(1)))
	})

	t.Run("Submit returns true and onFailure is called for failed export", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		var counter int32
		exporter := &mockExporter{
			exportFn: func(ctx context.Context, obj *governancev1alpha1.AgentDiagnostic) error {
				return errors.New("fail")
			},
		}
		pool := NewExportPool(context.Background(), 2, exporter)
		success := pool.Submit(testDiag("diag1"), func() {}, func(err error) { atomic.AddInt32(&counter, 1) })
		gm.Expect(success).To(gomega.BeTrue())
		pool.Stop()
		gm.Expect(atomic.LoadInt32(&counter)).To(gomega.Equal(int32(1)))
	})

	t.Run("Pool is full — Submit returns false without blocking", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		var startedOnce sync.Once
		started := make(chan struct{})
		blocker := make(chan struct{})
		exporter := &mockExporter{
			exportFn: func(ctx context.Context, obj *governancev1alpha1.AgentDiagnostic) error {
				startedOnce.Do(func() { close(started) })
				<-blocker
				return nil
			},
		}
		// Concurrency 1, capacity 10
		pool := NewExportPool(context.Background(), 1, exporter)
		defer close(blocker)

		// Job 1: worker picks it up
		gm.Expect(pool.Submit(testDiag("diag-worker"), func() {}, func(err error) {})).To(gomega.BeTrue())

		select {
		case <-started:
		case <-time.After(1 * time.Second):
			t.Fatal("worker did not start")
		}

		// Jobs 2-11: fill the channel (capacity 10)
		for range 10 {
			gm.Expect(pool.Submit(testDiag("diag-chan"), func() {}, func(err error) {})).To(gomega.BeTrue())
		}

		// Job 12: should fail immediately
		start := time.Now()
		gm.Expect(pool.Submit(testDiag("diag-fail"), func() {}, func(err error) {})).To(gomega.BeFalse())
		gm.Expect(time.Since(start)).To(gomega.BeNumerically("<", 100*time.Millisecond))
	})

	t.Run("Stop drains in-flight work", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		var counter int32
		exporter := &mockExporter{
			exportFn: func(ctx context.Context, obj *governancev1alpha1.AgentDiagnostic) error {
				time.Sleep(10 * time.Millisecond)
				return nil
			},
		}
		pool := NewExportPool(context.Background(), 2, exporter)
		for range 5 {
			pool.Submit(testDiag("diag"), func() { atomic.AddInt32(&counter, 1) }, func(err error) {})
		}
		pool.Stop()
		gm.Expect(atomic.LoadInt32(&counter)).To(gomega.Equal(int32(5)))
	})

	t.Run("Context cancellation stops workers", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		ctx, cancel := context.WithCancel(context.Background())
		exporter := &mockExporter{
			exportFn: func(ctx context.Context, obj *governancev1alpha1.AgentDiagnostic) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}
		pool := NewExportPool(ctx, 2, exporter)
		pool.Submit(testDiag("diag"), func() {}, func(err error) {})

		cancel()
		start := time.Now()
		pool.Stop()
		gm.Expect(time.Since(start)).To(gomega.BeNumerically("<", 500*time.Millisecond))
	})
}
