package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/controller"
	"github.com/agent-control-plane/aip-k8s/internal/evaluation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	testEnv        *envtest.Environment
	cfg            *rest.Config
	k8sClient      client.Client
	mgrClient      client.Client
	ctx            context.Context
	cancel         context.CancelFunc
	projDir        string
	kubeconfigPath string
	mgrErrCh       chan error
)

func TestGatewayE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway E2E Suite")
}

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.Background())
	var err error
	projDir, err = os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	projDir = strings.ReplaceAll(projDir, "/cmd/gateway", "")

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())

	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
	Expect(coordinationv1.AddToScheme(scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())

	mgrClient = startTestManagerForGinkgo(cfg, scheme)

	Eventually(func() error {
		var list v1alpha1.AgentRequestList
		return mgrClient.List(ctx, &list)
	}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

	kubeconfigPath, err = writeKubeconfig(cfg)
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	if cancel != nil {
		cancel()
	}
	if mgrErrCh != nil {
		select {
		case err := <-mgrErrCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				GinkgoT().Errorf("manager exited with unexpected error: %v", err)
			}
		case <-time.After(15 * time.Second):
			GinkgoT().Error("manager did not stop within expected time")
		}
	}
	if kubeconfigPath != "" {
		_ = os.Remove(kubeconfigPath)
	}
	if testEnv != nil {
		Expect(testEnv.Stop()).To(Succeed())
	}
})

func startTestManagerForGinkgo(cfg *rest.Config, scheme *runtime.Scheme) client.Client {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{
			SkipNameValidation: func() *bool { b := true; return &b }(),
		},
	})
	Expect(err).NotTo(HaveOccurred())

	eval, err := evaluation.NewEvaluator()
	Expect(err).NotTo(HaveOccurred())

	Expect((&controller.AgentRequestReconciler{
		Client:               mgr.GetClient(),
		APIReader:            mgr.GetAPIReader(),
		Scheme:               mgr.GetScheme(),
		OpsLockDuration:      5 * time.Minute,
		ApprovedTimeout:      5 * time.Minute,
		Evaluator:            eval,
		TargetContextFetcher: &evaluation.KubernetesTargetContextFetcher{Client: mgr.GetAPIReader()},
	}).SetupWithManager(mgr)).To(Succeed())

	Expect((&controller.GovernedResourceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)).To(Succeed())

	Expect((&controller.DiagnosticAccuracyReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
	}).SetupWithManager(mgr)).To(Succeed())

	Expect((&controller.AgentTrustProfileReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
	}).SetupWithManager(context.Background(), mgr)).To(Succeed())

	mgrErrCh = make(chan error, 1)
	go func() { mgrErrCh <- mgr.Start(ctx) }()

	return mgr.GetClient()
}

func getAgentRequestPhase(name, ns string) string {
	var ar v1alpha1.AgentRequest
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &ar); err != nil {
		return ""
	}
	return ar.Status.Phase
}

func auditRecordExists(reqName, ns, event string) bool {
	var list v1alpha1.AuditRecordList
	if err := k8sClient.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return false
	}
	for _, ar := range list.Items {
		if ar.Spec.AgentRequestRef == reqName && ar.Spec.Event == event {
			return true
		}
	}
	return false
}

func gwCleanup(ns string) {
	_ = k8sClient.DeleteAllOf(ctx, &v1alpha1.AgentRequest{}, client.InNamespace(ns))
	_ = k8sClient.DeleteAllOf(ctx, &coordinationv1.Lease{}, client.InNamespace(ns))
	_ = k8sClient.DeleteAllOf(ctx, &v1alpha1.SafetyPolicy{}, client.InNamespace(ns))
	_ = k8sClient.DeleteAllOf(ctx, &v1alpha1.AuditRecord{}, client.InNamespace(ns))
}

func strPtr(s string) *string { return &s }
