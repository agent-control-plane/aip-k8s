package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/controller"
	"github.com/agent-control-plane/aip-k8s/internal/evaluation"
	"github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	testAgentSub          = "agent-sub"
	testReviewerSub       = "reviewer-sub"
	testDefaultNS         = "default"
	eventuallyTimeout     = 10 * time.Second
	eventuallyLongTimeout = 15 * time.Second
	eventuallyInterval    = 100 * time.Millisecond
	serverStartupTimeout  = 5 * time.Second
	serverWaitTimeout     = 10 * time.Second
)

func TestGatewayIntegration(t *testing.T) {
	g := gomega.NewWithT(t)

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("Failed to stop testEnv: %v", err)
		}
	})

	err = v1alpha1.AddToScheme(scheme.Scheme)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	err = coordinationv1.AddToScheme(scheme.Scheme)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	directClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	mgrClient := startTestManager(t, cfg)
	ctx := context.Background()

	// Wait for cache to be ready
	g.Eventually(func() error {
		var list v1alpha1.AgentRequestList
		return mgrClient.List(ctx, &list)
	}, serverStartupTimeout, eventuallyInterval).Should(gomega.Succeed())

	runRequestLifecycleTests(t, mgrClient, directClient, ctx)
	runAuthAndApprovalTests(t, mgrClient, directClient, ctx)
	runSoakModeAndVerdictTests(t, mgrClient, directClient, ctx)
}

func startTestManager(t *testing.T, cfg *rest.Config) client.Client {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	eval, err := evaluation.NewEvaluator()
	if err != nil {
		t.Fatalf("Failed to create evaluator: %v", err)
	}

	err = (&controller.AgentRequestReconciler{
		Client:               mgr.GetClient(),
		APIReader:            mgr.GetAPIReader(),
		Scheme:               mgr.GetScheme(),
		OpsLockDuration:      5 * time.Minute,
		Evaluator:            eval,
		TargetContextFetcher: &evaluation.KubernetesTargetContextFetcher{Client: mgr.GetAPIReader()},
	}).SetupWithManager(mgr)
	if err != nil {
		t.Fatalf("Failed to setup AgentRequestReconciler: %v", err)
	}

	err = (&controller.GovernedResourceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	if err != nil {
		t.Fatalf("Failed to setup GovernedResourceReconciler: %v", err)
	}

	err = (&controller.DiagnosticAccuracyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	if err != nil {
		t.Fatalf("Failed to setup DiagnosticAccuracyReconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("manager exited with unexpected error: %v", err)
			}
		case <-time.After(eventuallyTimeout):
			t.Error("manager did not stop within expected time")
		}
	})

	return mgr.GetClient()
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

func cleanup(ctx context.Context, gm *gomega.WithT, c client.Client) {
	gm.Expect(c.DeleteAllOf(ctx, &v1alpha1.AgentRequest{}, client.InNamespace(testDefaultNS))).To(gomega.Succeed())
	gm.Expect(c.DeleteAllOf(ctx, &v1alpha1.AuditRecord{}, client.InNamespace(testDefaultNS))).To(gomega.Succeed())
	gm.Expect(c.DeleteAllOf(ctx, &coordinationv1.Lease{}, client.InNamespace(testDefaultNS))).To(gomega.Succeed())
}
