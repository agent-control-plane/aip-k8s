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

	t.Run("AR GC deletes terminal AgentRequest and its AuditRecords", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		ctx, cancel := context.WithCancel(g.Context())
		defer cancel()

		mgr, err := manager.New(cfg, manager.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		config := GCConfig{
			Enabled:          true,
			DryRun:           false,
			Interval:         100 * time.Millisecond,
			HardTTL:          1 * time.Hour,
			SafetyMinCount:   1,
			PageSize:         10,
			DeleteRatePerSec: 100,
		}

		getFutureNow := func() time.Time { return time.Now().Add(2 * time.Hour) }

		err = mgr.Add(&GCManager{
			APIReader: mgr.GetAPIReader(),
			Client:    mgr.GetClient(),
			Config:    config,
			Now:       getFutureNow,
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

		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "terminal-ar", Namespace: "default"},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: "test-agent",
				Action:        "scale",
				Target:        governancev1alpha1.Target{URI: "k8s://default/deployment/test"},
				Reason:        "test",
			},
		}
		gm.Expect(k8sClient.Create(ctx, ar)).To(gomega.Succeed())
		ar.Status.Phase = governancev1alpha1.PhaseCompleted
		gm.Expect(k8sClient.Status().Update(ctx, ar)).To(gomega.Succeed())

		audit := &governancev1alpha1.AuditRecord{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audit-for-ar",
				Namespace: "default",
				Labels:    map[string]string{"aip.io/agentRequestRef": "terminal-ar"},
			},
			Spec: governancev1alpha1.AuditRecordSpec{
				Timestamp:       metav1.Now(),
				AgentRequestRef: "terminal-ar",
				AgentIdentity:   "test-agent",
				Event:           governancev1alpha1.AuditEventRequestCompleted,
				Action:          "scale",
				TargetURI:       "k8s://default/deployment/test",
			},
		}
		gm.Expect(k8sClient.Create(ctx, audit)).To(gomega.Succeed())

		gm.Eventually(func() error {
			var fetched governancev1alpha1.AgentRequest
			return k8sClient.Get(ctx, client.ObjectKey{Name: "terminal-ar", Namespace: "default"}, &fetched)
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Satisfy(apierrors.IsNotFound))

		gm.Eventually(func() error {
			var fetched governancev1alpha1.AuditRecord
			return k8sClient.Get(ctx, client.ObjectKey{Name: "audit-for-ar", Namespace: "default"}, &fetched)
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Satisfy(apierrors.IsNotFound))

		mgrCancel()
		gm.Eventually(startErrCh, 5*time.Second).Should(gomega.Receive(gomega.Or(gomega.BeNil(), gomega.Equal(context.Canceled))))
	})

	t.Run("AR GC does NOT delete active AgentRequest", func(g *testing.T) {
		gm := gomega.NewWithT(g)
		ctx, cancel := context.WithCancel(g.Context())
		defer cancel()

		mgr, err := manager.New(cfg, manager.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		gm.Expect(err).NotTo(gomega.HaveOccurred())

		config := GCConfig{
			Enabled:          true,
			DryRun:           false,
			Interval:         100 * time.Millisecond,
			HardTTL:          1 * time.Hour,
			SafetyMinCount:   1,
			PageSize:         10,
			DeleteRatePerSec: 100,
		}

		getFutureNow := func() time.Time { return time.Now().Add(2 * time.Hour) }

		err = mgr.Add(&GCManager{
			APIReader: mgr.GetAPIReader(),
			Client:    mgr.GetClient(),
			Config:    config,
			Now:       getFutureNow,
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

		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "active-ar", Namespace: "default"},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: "test-agent",
				Action:        "scale",
				Target:        governancev1alpha1.Target{URI: "k8s://default/deployment/test"},
				Reason:        "test",
			},
		}
		gm.Expect(k8sClient.Create(ctx, ar)).To(gomega.Succeed())
		ar.Status.Phase = governancev1alpha1.PhaseExecuting
		gm.Expect(k8sClient.Status().Update(ctx, ar)).To(gomega.Succeed())

		gm.Consistently(func() error {
			var fetched governancev1alpha1.AgentRequest
			return k8sClient.Get(ctx, client.ObjectKey{Name: "active-ar", Namespace: "default"}, &fetched)
		}, 1*time.Second, 200*time.Millisecond).Should(gomega.Succeed())

		mgrCancel()
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
