package soak

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/controller"
	"github.com/agent-control-plane/aip-k8s/internal/evaluation"
)

var (
	soakCtx       context.Context
	soakCancel    context.CancelFunc
	soakEnv       *envtest.Environment
	soakCfg       *rest.Config
	soakClient    client.Client
	soakMgrCancel context.CancelFunc
)

func TestSoak(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Soak Suite")
}

var _ = BeforeSuite(func() {
	soakCtx, soakCancel = context.WithCancel(context.TODO())

	var err error
	err = governancev1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = coordinationv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	By("bootstrapping soak test environment")
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(false)))
	soakEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	if dir := getSoakEnvTestBinaryDir(); dir != "" {
		soakEnv.BinaryAssetsDirectory = dir
	}

	soakCfg, err = soakEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(soakCfg).NotTo(BeNil())

	soakClient, err = client.New(soakCfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(soakClient).NotTo(BeNil())

	By("creating manager")
	mgrCtx, mgrCancel := context.WithCancel(soakCtx)
	soakMgrCancel = mgrCancel
	mgr, err := manager.New(soakCfg, manager.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())

	By("creating evaluator")
	eval, err := evaluation.NewEvaluator()
	Expect(err).NotTo(HaveOccurred())

	By("registering AgentRequest controller")
	arReconciler := &controller.AgentRequestReconciler{
		Client:          mgr.GetClient(),
		Scheme:          scheme.Scheme,
		OpsLockDuration: 15 * time.Second,
		ApprovedTimeout: 5 * time.Minute,
		Evaluator:       eval,
		Clock:           time.Now,
	}
	err = arReconciler.SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	By("registering DiagnosticAccuracy controller")
	accuracyReconciler := &controller.DiagnosticAccuracyReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    scheme.Scheme,
	}
	err = accuracyReconciler.SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	By("starting manager")
	go func() {
		defer GinkgoRecover()
		if err := mgr.Start(mgrCtx); err != nil {
			Fail("manager start failed: " + err.Error())
		}
	}()

	By("waiting for cache sync")
	mgr.GetCache().WaitForCacheSync(soakCtx)
})

var _ = AfterSuite(func() {
	By("stopping manager")
	soakMgrCancel()
	By("tearing down soak test environment")
	soakCancel()
	Eventually(func() error {
		return soakEnv.Stop()
	}, time.Minute, time.Second).Should(Succeed())
})

func getSoakEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", "bin", "k8s")
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
