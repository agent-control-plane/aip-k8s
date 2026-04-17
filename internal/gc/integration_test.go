package gc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func TestGCIntegration(t *testing.T) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))
	gm := gomega.NewWithT(t)

	// 1. Setup envtest
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}

	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	gm.Expect(err).NotTo(gomega.HaveOccurred())
	defer func() {
		_ = testEnv.Stop()
	}()

	err = governancev1alpha1.AddToScheme(scheme.Scheme)
	gm.Expect(err).NotTo(gomega.HaveOccurred())

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	gm.Expect(err).NotTo(gomega.HaveOccurred())

	t.Run("Manager runs GC cycle and deletes expired objects", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		ctx, cancel := context.WithCancel(g.Context())
		defer cancel()

		// 2. Create a manager
		mgr, err := manager.New(cfg, manager.Options{
			Scheme: scheme.Scheme,
			Metrics: metricsserver.Options{
				BindAddress: "0",
			},
		})
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		// 3. Configure GC with a very short interval for testing
		config := GCConfig{
			Enabled:           true,
			DryRun:            false,
			Interval:          100 * time.Millisecond,
			DiagnosticHardTTL: 1 * time.Hour,
			PageSize:          10,
			DeleteRatePerSec:  100,
			SafetyMinCount:    1, // Allow testing with just one object
		}

		// Mock time: Always return 2 hours in the future from current real time.
		// This ensures that any object created NOW with a standard timestamp
		// looks 2 hours old to the GC worker (since TTL is 1 hour).
		getFutureNow := func() time.Time { return time.Now().Add(2 * time.Hour) }

		// 4. Register GC Manager
		err = mgr.Add(&GCManager{
			APIReader: mgr.GetAPIReader(),
			Client:    mgr.GetClient(),
			Config:    config,
			Now:       getFutureNow,
		})
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		// 5. Start manager in background
		startErrCh := make(chan error, 1)
		mgrCtx, mgrCancel := context.WithCancel(g.Context())
		go func() {
			startErrCh <- mgr.Start(mgrCtx)
		}()

		// Wait for manager to start by checking if we can get the client
		gm.Eventually(func() bool {
			return mgr.GetCache().WaitForCacheSync(ctx)
		}, 5*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

		// 6. Create an AgentDiagnostic
		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "integration-test-diag",
				Namespace: "default",
			},
			Spec: governancev1alpha1.AgentDiagnosticSpec{
				AgentIdentity:  "test-agent",
				DiagnosticType: "test",
				CorrelationID:  "test-correlation-id",
				Summary:        "test summary",
			},
		}
		gm.Expect(k8sClient.Create(ctx, diag)).To(gomega.Succeed())
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), diag)
		})

		// 7. Verify deletion. Eventually it should be gone.
		gm.Eventually(func() error {
			var fetched governancev1alpha1.AgentDiagnostic
			return k8sClient.Get(ctx, client.ObjectKey{Name: "integration-test-diag", Namespace: "default"}, &fetched)
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Satisfy(apierrors.IsNotFound))

		mgrCancel()
		// Capture the error from mgr.Start(mgrCtx). At the end of each subtest, cancel the manager context,
		// and assert that the only permitted non-nil error is context.Canceled (or nil).
		// Ignoring other errors is not allowed per guidelines.
		gm.Eventually(startErrCh, 5*time.Second).Should(gomega.Receive(gomega.Or(gomega.BeNil(), gomega.Equal(context.Canceled))))
	})

	t.Run("Manager does NOT delete non-expired objects", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		ctx, cancel := context.WithCancel(g.Context())
		defer cancel()

		mgr, err := manager.New(cfg, manager.Options{
			Scheme: scheme.Scheme,
			Metrics: metricsserver.Options{
				BindAddress: "0",
			},
		})
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		config := GCConfig{
			Enabled:           true,
			DryRun:            false,
			Interval:          100 * time.Millisecond,
			DiagnosticHardTTL: 1 * time.Hour,
			PageSize:          10,
			DeleteRatePerSec:  100,
			SafetyMinCount:    1,
		}

		// Mock time: current time is exactly now, so newly created objects
		// are definitely NOT expired.
		currentNow := func() time.Time { return time.Now() }

		err = mgr.Add(&GCManager{
			APIReader: mgr.GetAPIReader(),
			Client:    mgr.GetClient(),
			Config:    config,
			Now:       currentNow,
		})
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		startErrCh := make(chan error, 1)
		mgrCtx, mgrCancel := context.WithCancel(g.Context())
		go func() {
			startErrCh <- mgr.Start(mgrCtx)
		}()

		gm.Eventually(func() bool {
			return mgr.GetCache().WaitForCacheSync(ctx)
		}, 5*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "keep-me",
				Namespace: "default",
			},
			Spec: governancev1alpha1.AgentDiagnosticSpec{
				AgentIdentity:  "test-agent",
				DiagnosticType: "test",
				CorrelationID:  "test-correlation-id",
				Summary:        "test summary",
			},
		}
		gm.Expect(k8sClient.Create(ctx, diag)).To(gomega.Succeed())
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), diag)
		})

		// Wait for a few GC cycles to pass and verify it still exists.
		gm.Consistently(func() error {
			var fetched governancev1alpha1.AgentDiagnostic
			return k8sClient.Get(ctx, client.ObjectKey{Name: "keep-me", Namespace: "default"}, &fetched)
		}, 500*time.Millisecond, 100*time.Millisecond).Should(gomega.Succeed())

		mgrCancel()
		// Capture the error from mgr.Start(mgrCtx). At the end of each subtest, cancel the manager context,
		// and assert that the only permitted non-nil error is context.Canceled (or nil).
		// Ignoring other errors is not allowed per guidelines.
		gm.Eventually(startErrCh, 5*time.Second).Should(gomega.Receive(gomega.Or(gomega.BeNil(), gomega.Equal(context.Canceled))))
	})

	t.Run("Export pool with noop exporter deletes soft-retention expired objects", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		ctx, cancel := context.WithCancel(g.Context())
		defer cancel()

		mgr, err := manager.New(cfg, manager.Options{
			Scheme: scheme.Scheme,
			Metrics: metricsserver.Options{
				BindAddress: "0",
			},
		})
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		config := GCConfig{
			Enabled:                true,
			DryRun:                 false,
			Interval:               100 * time.Millisecond,
			DiagnosticRetentionTTL: 1 * time.Hour,
			DiagnosticHardTTL:      24 * time.Hour,
			ExportType:             "otlp",
			Concurrency:            2,
			PageSize:               10,
			DeleteRatePerSec:       100,
			SafetyMinCount:         1,
		}

		getFutureNow := func() time.Time { return time.Now().Add(2 * time.Hour) }

		err = mgr.Add(&GCManager{
			APIReader: mgr.GetAPIReader(),
			Client:    mgr.GetClient(),
			Config:    config,
			Now:       getFutureNow,
			Exporter:  NoopExporter{},
		})
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		startErrCh := make(chan error, 1)
		mgrCtx, mgrCancel := context.WithCancel(g.Context())
		go func() {
			startErrCh <- mgr.Start(mgrCtx)
		}()

		gm.Eventually(func() bool {
			return mgr.GetCache().WaitForCacheSync(ctx)
		}, 5*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

		diag := &governancev1alpha1.AgentDiagnostic{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "soft-delete",
				Namespace: "default",
			},
			Spec: governancev1alpha1.AgentDiagnosticSpec{
				AgentIdentity:  "test-agent",
				DiagnosticType: "test",
				CorrelationID:  "test-correlation-id",
				Summary:        "test summary",
			},
		}
		gm.Expect(k8sClient.Create(ctx, diag)).To(gomega.Succeed())
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), diag)
		})

		gm.Eventually(func() error {
			var fetched governancev1alpha1.AgentDiagnostic
			return k8sClient.Get(ctx, client.ObjectKey{Name: "soft-delete", Namespace: "default"}, &fetched)
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Satisfy(apierrors.IsNotFound))

		mgrCancel()
		// Capture the error from mgr.Start(mgrCtx). At the end of each subtest, cancel the manager context,
		// and assert that the only permitted non-nil error is context.Canceled (or nil).
		// Ignoring other errors is not allowed per guidelines.
		gm.Eventually(startErrCh, 5*time.Second).Should(gomega.Receive(gomega.Or(gomega.BeNil(), gomega.Equal(context.Canceled))))
	})
}

func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
