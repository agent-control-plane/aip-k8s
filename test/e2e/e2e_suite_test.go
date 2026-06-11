//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/test/utils"
)

const (
	namespace                = "aip-k8s-system"
	serviceAccountName       = "aip-k8s-controller"
	controllerDeploymentName = "aip-k8s-controller"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "example.com/aip-k8s:v0.0.1"
	// shouldCleanupCertManager tracks whether CertManager was installed by this suite.
	shouldCleanupCertManager = false

	// k8sClient is a typed client-go client used for all assertion and status checks in tests.
	// Resource creation still uses kubectl apply so the wire format is exercised end-to-end.
	k8sClient client.Client

	// ctx is the shared context for all client-go calls in the suite.
	ctx = context.Background()
)

// crdsInstalled returns true if all governance CRDs in config/crd/bases/
// are already present in the cluster. New CRDs are automatically detected.
func crdsInstalled() bool {
	projDir, err := utils.GetProjectDir()
	if err != nil {
		return false
	}
	basesDir := filepath.Join(projDir, "config", "crd", "bases")
	entries, err := os.ReadDir(basesDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		cmd := exec.Command("kubectl", "get", "-f", filepath.Join(basesDir, entry.Name()))
		if _, err := utils.Run(cmd); err != nil {
			return false
		}
	}
	return true
}

// controllerDeployed returns true if the controller-manager deployment exists.
func controllerDeployed() bool {
	cmd := exec.Command("kubectl", "get", "deployment",
		controllerDeploymentName, "-n", namespace)
	_, err := utils.Run(cmd)
	return err == nil
}

// helmReleaseExists returns true if the aip-k8s Helm release is present.
func helmReleaseExists() bool {
	cmd := exec.Command("helm", "list", "-n", namespace,
		"-q", "-f", "aip-k8s")
	out, err := utils.Run(cmd)
	return err == nil && strings.TrimSpace(out) == "aip-k8s"
}

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind and CertManager.
//
// To skip CertManager installation, set: CERT_MANAGER_INSTALL_SKIP=true
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting aip-k8s e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	// When GATEWAY_URL is set the suite is running against an already-deployed
	// release (Helm or otherwise). Skip image build/load and CertManager.
	if os.Getenv("GATEWAY_URL") == "" {
		By("building the manager image")
		cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

		By("loading the manager image on Kind")
		err = utils.LoadImageToKindClusterWithName(managerImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

		setupCertManager()
	}

	// Install CRDs if they are not already present. Works regardless of whether
	// the cluster was prepared via make install, Helm, or any other method.
	if !crdsInstalled() {
		By("installing CRDs")
		cmd := exec.Command("make", "install")
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install CRDs")
	}

	// Create namespace and deploy controller-manager if not already present.
	// Works regardless of deployment method (Kustomize, Helm, etc.).
	if !controllerDeployed() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace, "--dry-run=client", "-o", "yaml")
		nsYAML, err := cmd.Output()
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to render namespace manifest")
		applyCmd := exec.Command("kubectl", "apply", "-f", "-")
		applyCmd.Stdin = strings.NewReader(string(nsYAML))
		_, err = utils.Run(applyCmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	}

	// Always wait for controller readiness — even if it was already deployed,
	// we need to be sure it is Ready before any test runs.
	By("waiting for controller to be ready")
	Eventually(func(g Gomega) {
		readyCmd := exec.Command("kubectl", "get", "pods",
			"-l", "control-plane=controller-manager",
			"-n", namespace,
			"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
		status, err := utils.Run(readyCmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(status).To(Equal("True"), "controller pod not yet ready")
	}, 2*time.Minute, 2*time.Second).Should(Succeed())

	By("setting up typed Kubernetes client for assertions")
	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(governancev1alpha1.AddToScheme(scheme)).To(Succeed())
	cfg, err := config.GetConfig()
	Expect(err).NotTo(HaveOccurred(), "Failed to get kubeconfig")
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred(), "Failed to create Kubernetes client")
})

var _ = AfterSuite(func() {
	teardownCertManager()
})

// setupCertManager installs CertManager if needed for webhook tests.
// Skips installation if CERT_MANAGER_INSTALL_SKIP=true or if already present.
func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}

	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	shouldCleanupCertManager = true

	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// teardownCertManager uninstalls CertManager if it was installed by setupCertManager.
func teardownCertManager() {
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	utils.UninstallCertManager()
}
