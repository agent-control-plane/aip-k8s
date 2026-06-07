//go:build mcp_e2e
// +build mcp_e2e

package e2e_mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/jwt"
	"github.com/agent-control-plane/aip-k8s/test/utils"
)

const (
	namespace              = "aip-k8s-system"
	controllerDeployment   = "aip-k8s-controller"
	managerImage           = "example.com/aip-k8s:v0.0.1"
	gatewayPort            = "18080"
	mcpServerDeployment    = "github-mcp-server"
	mcpServerService       = "github-mcp"
	githubTokenSecret      = "aip-github-token"
	githubPATEnv           = "AIP_E2E_GITHUB_PAT"
	githubOwner            = "agent-control-plane"
	githubRepo             = "aip-demo-infra"
	githubConfigFileBranch = "main"
	githubConfigFilePath   = "infra/payment-service.json"
	e2eTestBranch          = "e2e-mcp-scale-17"
	e2eTestBranch2         = "e2e-mcp-scale-17-v2"
	e2eTestBranch3         = "e2e-mcp-scale-17-v3"
	mcpServerCRDName       = "github"
	mcpPort                = "18081"
)

var (
	k8sClient  client.Client
	ctx        = context.Background()
	gwURL      string
	gwCmd      *exec.Cmd
	mcpPFCmd   *exec.Cmd
	jwtKeyPath string
)

func TestMCPE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting AIP MCP e2e test suite\n")
	RunSpecs(t, "mcp e2e suite")
}

func getProjectDir() string {
	wd, err := os.Getwd()
	if err != nil {
		Fail(fmt.Sprintf("failed to get working directory: %v", err))
	}
	wd = strings.ReplaceAll(wd, "/test/e2e_mcp", "")
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd
}

func runCmd(cmd *exec.Cmd) (string, error) {
	projDir := getProjectDir()
	cmd.Dir = projDir
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", strings.Join(cmd.Args, " "))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed: %w\noutput: %s", strings.Join(cmd.Args, " "), err, string(output))
	}
	return string(output), nil
}

func kubectlApply(stdin string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(stdin)
	_, err := runCmd(cmd)
	return err
}

func kubectlDelete(manifest string) error {
	cmd := exec.Command("kubectl", "delete", "-f", "-", "--ignore-not-found", "--wait=false")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := runCmd(cmd)
	return err
}

// githubAPI performs a GitHub REST API call with the given method, path, and body.
func githubAPI(method, path string, body []byte, pat string) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func waitForDeploymentReady(ns, name string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "deployment", name, "-n", ns,
			"-o", "jsonpath={.status.conditions[?(@.type=='Available')].status}")
		out, err := runCmd(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("True"))
	}, timeout, 2*time.Second).Should(Succeed())
}

// ensureBranchLifecycle ensures a GitHub branch exists with a dummy commit and
// closes any open PRs from prior runs on that branch.
func ensureBranchLifecycle(branchName, dummyPrefix string) {
	By(fmt.Sprintf("ensuring branch %s exists in GitHub repo", branchName))
	cmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/%s/git/refs/heads/%s", githubOwner, githubRepo, branchName),
		"--jq", ".ref")
	if _, err := runCmd(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Branch %s not found, creating from %s\n", branchName, githubConfigFileBranch)
		shaOut, shaErr := runCmd(exec.Command("gh", "api",
			fmt.Sprintf("repos/%s/%s/git/refs/heads/%s", githubOwner, githubRepo, githubConfigFileBranch),
			"--jq", ".object.sha"))
		Expect(shaErr).NotTo(HaveOccurred(), "Failed to get SHA of base branch for %s", branchName)
		sha := strings.TrimSpace(shaOut)
		createCmd := exec.Command("gh", "api", "--method", "POST",
			fmt.Sprintf("repos/%s/%s/git/refs", githubOwner, githubRepo),
			"-f", fmt.Sprintf("ref=refs/heads/%s", branchName),
			"-f", fmt.Sprintf("sha=%s", sha))
		_, createErr := runCmd(createCmd)
		Expect(createErr).NotTo(HaveOccurred(), "Failed to create branch %s", branchName)
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "Branch %s already exists\n", branchName)
	}

	By(fmt.Sprintf("closing any open PRs from prior runs on %s", branchName))
	listPRs := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/%s/pulls?head=%s:%s&state=open", githubOwner, githubRepo, githubOwner, branchName),
		"--jq", ".[].number")
	if prOut, prErr := runCmd(listPRs); prErr == nil {
		for _, prNum := range strings.Fields(strings.TrimSpace(prOut)) {
			closePR := exec.Command("gh", "api", "--method", "PATCH",
				fmt.Sprintf("repos/%s/%s/pulls/%s", githubOwner, githubRepo, prNum),
				"-f", "state=closed")
			// Safe to ignore: best-effort cleanup of stale PRs from prior runs.
			// The e2e branches are deleted in AfterSuite regardless.
			_, _ = runCmd(closePR)
		}
	}

	By(fmt.Sprintf("creating a dummy commit on %s", branchName))
	dummyFile := fmt.Sprintf("%s-%d.txt", dummyPrefix, time.Now().Unix())
	dummyContent := base64.StdEncoding.EncodeToString([]byte("e2e test"))
	putCmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/%s/contents/%s", githubOwner, githubRepo, dummyFile),
		"--method", "PUT",
		"-f", "message=e2e dummy commit",
		"-f", fmt.Sprintf("content=%s", dummyContent),
		"-f", fmt.Sprintf("branch=%s", branchName))
	_, err := runCmd(putCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create dummy commit on %s", branchName)
}

var _ = BeforeSuite(func() {
	projDir := getProjectDir()

	var cmd *exec.Cmd
	if os.Getenv("GATEWAY_URL") == "" && os.Getenv("SKIP_DEPLOY") == "" {
		By("building gateway binary")
		cmd = exec.Command("go", "build", "-o", filepath.Join(projDir, "bin", "gateway"), "./cmd/gateway")
		_, err := runCmd(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to build gateway binary")

		By("building controller image")
		cmd = exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
		_, err = runCmd(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to build controller image")

		By("loading controller image to Kind")
		err = utils.LoadImageToKindClusterWithName(managerImage)
		Expect(err).NotTo(HaveOccurred(), "Failed to load controller image to Kind")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = runCmd(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying controller")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = runCmd(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy controller")

		By("waiting for controller to be ready")
		waitForDeploymentReady(namespace, controllerDeployment, 3*time.Minute)
	}

	By("setting up k8s client")
	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(governancev1alpha1.AddToScheme(scheme)).To(Succeed())
	cfg, err := config.GetConfig()
	Expect(err).NotTo(HaveOccurred(), "Failed to get kubeconfig")
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred(), "Failed to create k8s client")

	githubPAT := os.Getenv(githubPATEnv)
	if githubPAT == "" {
		Skip(fmt.Sprintf("%s env var not set — skipping MCP e2e tests", githubPATEnv))
	}

	ensureBranchLifecycle(e2eTestBranch, "e2e-dummy")
	ensureBranchLifecycle(e2eTestBranch2, "e2e-dummy-c")
	ensureBranchLifecycle(e2eTestBranch3, "e2e-dummy-d")

	By("removing pod security enforcement on namespace for mcp server (image runs as root)")
	cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
		"pod-security.kubernetes.io/enforce-")
	_, _ = runCmd(cmd)

	By("creating aip-github-token Secret")
	tokenSecret := fmt.Sprintf(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"%s","namespace":"%s"},"type":"Opaque","stringData":{"token":"%s"}}`, githubTokenSecret, namespace, githubPAT)
	Expect(kubectlApply(tokenSecret)).To(Succeed(), "Failed to create github token Secret")

	By("creating AgentRegistrations for MCP e2e agents")
	// These agents authenticate via X-Remote-User proxy header (no OIDC tokens).
	// Omit the oidc field entirely: an empty oidc: {} is non-nil and activates
	// AllowedSubjects enforcement with an empty list, which has subtly different
	// semantics. Omitting it is the explicit "no OIDC validation" signal.
	agentRegs := fmt.Sprintf(`
apiVersion: governance.aip.io/v1alpha1
kind: AgentRegistration
metadata:
  name: mcp-e2e-agent
  namespace: %s
spec:
  agentIdentity: "e2e-mcp-agent"
  externalIdentities:
    - service: "github"
      type: StaticSecret
      staticSecret:
        name: %s
        namespace: %s
        key: token
---
apiVersion: governance.aip.io/v1alpha1
kind: AgentRegistration
metadata:
  name: mcp-e2e-agent-c
  namespace: %s
spec:
  agentIdentity: "e2e-mcp-agent-c"
  externalIdentities:
    - service: "github"
      type: StaticSecret
      staticSecret:
        name: %s
        namespace: %s
        key: token
---
apiVersion: governance.aip.io/v1alpha1
kind: AgentRegistration
metadata:
  name: mcp-e2e-agent-d
  namespace: %s
spec:
  agentIdentity: "e2e-mcp-agent-d"
  externalIdentities:
    - service: "github"
      type: StaticSecret
      staticSecret:
        name: %s
        namespace: %s
        key: token
`, namespace, githubTokenSecret, namespace, namespace, githubTokenSecret, namespace, namespace, githubTokenSecret, namespace)
	Expect(kubectlApply(agentRegs)).To(Succeed(), "Failed to create AgentRegistration CRs")

	By("deploying github-mcp-server into aip-k8s-system namespace")
	cmd = exec.Command("kubectl", "apply", "-f", filepath.Join(projDir, "config", "mcp"))
	_, err = runCmd(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy github-mcp-server manifests")

	By("waiting for github-mcp-server deployment to be ready")
	waitForDeploymentReady(namespace, mcpServerDeployment, 3*time.Minute)

	By("waiting for github-mcp Service endpoints")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "endpoints", mcpServerService, "-n", namespace, "-o", "jsonpath={.subsets[0].addresses[0].ip}")
		out, err := runCmd(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty())
	}, 2*time.Minute, 2*time.Second).Should(Succeed())

	By("generating Ed25519 JWT signing key")
	jwtKeyFile := filepath.Join(projDir, "bin", "mcp-e2e-jwt.key")
	err = jwt.GenerateEd25519Key(jwtKeyFile)
	Expect(err).NotTo(HaveOccurred(), "Failed to generate JWT signing key")
	jwtKeyPath = jwtKeyFile

	By("port-forwarding github-mcp service for local gateway access")
	mcpPFCmd = exec.Command("kubectl", "port-forward", "-n", namespace,
		fmt.Sprintf("svc/%s", mcpServerService), fmt.Sprintf("%s:80", mcpPort))
	mcpPFCmd.Stdout = GinkgoWriter
	mcpPFCmd.Stderr = GinkgoWriter
	err = mcpPFCmd.Start()
	Expect(err).NotTo(HaveOccurred(), "Failed to start kubectl port-forward for MCP")
	time.Sleep(2 * time.Second)

	By("creating MCPServer CR for the github-mcp server")
	mcpServerCR := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name: mcpServerCRDName,
		},
		Spec: governancev1alpha1.MCPServerSpec{
			URL:             fmt.Sprintf("http://localhost:%s", mcpPort),
			SecretNamespace: namespace,
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: githubTokenSecret},
				Key:                  "token",
			},
		},
	}
	Expect(k8sClient.Create(ctx, mcpServerCR)).To(Succeed(), "Failed to create MCPServer CR")

	// The gateway runs locally (not in-cluster) so it uses the port-forwarded URL.
	// The controller (in-cluster) cannot reach localhost, so we manually populate
	// status.tools here so the gateway's CRD watch sees a fully configured server.
	By("patching MCPServer status with discovered tools")
	mcpServerCR.Status.Tools = []governancev1alpha1.MCPServerTool{
		{Name: "create_pull_request", ReadOnly: false},
		{Name: "get_file_contents", ReadOnly: true},
		{Name: "list_pull_requests", ReadOnly: true},
	}
	mcpServerCR.Status.DiscoveredToolCount = 3
	Expect(k8sClient.Status().Update(ctx, mcpServerCR)).To(Succeed(), "Failed to patch MCPServer status")

	if os.Getenv("GATEWAY_URL") != "" {
		gwURL = os.Getenv("GATEWAY_URL")
		By(fmt.Sprintf("using pre-existing gateway at %s", gwURL))
	} else {
		By("starting gateway binary")
		gwCmd = exec.Command(filepath.Join(projDir, "bin", "gateway"),
			"--jwt-key-path", jwtKeyFile,
			"--addr", fmt.Sprintf(":%s", gatewayPort),
			"--wait-timeout", "90s",
			"--dedup-window", "5s",
		)
		gwCmd.Stdout = GinkgoWriter
		gwCmd.Stderr = GinkgoWriter
		err = gwCmd.Start()
		Expect(err).NotTo(HaveOccurred(), "Failed to start gateway binary")
		_, _ = fmt.Fprintf(GinkgoWriter, "Gateway started on port %s (PID %d)\n", gatewayPort, gwCmd.Process.Pid)

		gwURL = fmt.Sprintf("http://localhost:%s", gatewayPort)

		By("waiting for gateway to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("curl", "-sf", fmt.Sprintf("%s/healthz", gwURL))
			out, err := runCmd(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("ok"))
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	}

	By("waiting for gateway to cache MCPServer 'github' with tools")
	// The gateway watches MCPServer CRDs and populates its tool cache asynchronously.
	// Poll /mcp-registry until the 'github' server appears with the expected tools so
	// that Scenario B's /mcp-proxy call does not race against an empty cache.
	Eventually(func(g Gomega) {
		cmd := exec.Command("curl", "-sf", fmt.Sprintf("%s/mcp-registry", gwURL))
		out, err := runCmd(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(ContainSubstring(`"create_pull_request"`))
	}, 30*time.Second, 1*time.Second).Should(Succeed(), "gateway did not cache MCPServer 'github' with tools within 30s")
})

var _ = AfterSuite(func() {
	By("cleaning up github-mcp-server resources")
	cmd := exec.Command("kubectl", "delete", "-f", filepath.Join(getProjectDir(), "config", "mcp"), "--ignore-not-found", "--wait=false")
	_, _ = runCmd(cmd)

	By("deleting MCPServer CRs")
	err := k8sClient.Delete(ctx, &governancev1alpha1.MCPServer{ObjectMeta: metav1.ObjectMeta{Name: mcpServerCRDName}})
	Expect(client.IgnoreNotFound(err)).To(Succeed(), "deleting MCPServer %s", mcpServerCRDName)
	// Defensive cleanup for Scenario E's MCPServer; its AfterAll deletes it too,
	// but if that AfterAll was skipped or panicked this ensures no leak between runs.
	_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &governancev1alpha1.MCPServer{ObjectMeta: metav1.ObjectMeta{Name: "github-scenario-e"}}))

	By("deleting AgentRegistration CRs")
	cmd = exec.Command("kubectl", "delete", "agentregistration", "--all", "-n", namespace, "--ignore-not-found")
	_, _ = runCmd(cmd)

	By("deleting aip-github-token Secret")
	cmd = exec.Command("kubectl", "delete", "secret", githubTokenSecret, "-n", namespace, "--ignore-not-found", "--wait=false")
	_, _ = runCmd(cmd)

	if mcpPFCmd != nil && mcpPFCmd.Process != nil {
		By("stopping MCP port-forward")
		_ = mcpPFCmd.Process.Kill()
		_ = mcpPFCmd.Wait()
	}

	if gwCmd != nil && gwCmd.Process != nil {
		By("stopping gateway process")
		_ = gwCmd.Process.Kill()
		_ = gwCmd.Wait()
	}

	if os.Getenv(githubPATEnv) != "" {
		By("deleting e2e test branches from GitHub (auto-closes any open PRs)")
		// Safe to ignore: test infra cleanup; failures don't affect test results.
		cmd = exec.Command("gh", "api", "--method", "DELETE",
			fmt.Sprintf("repos/%s/%s/git/refs/heads/%s", githubOwner, githubRepo, e2eTestBranch))
		_, _ = runCmd(cmd)
		cmd = exec.Command("gh", "api", "--method", "DELETE",
			fmt.Sprintf("repos/%s/%s/git/refs/heads/%s", githubOwner, githubRepo, e2eTestBranch2))
		_, _ = runCmd(cmd)
		cmd = exec.Command("gh", "api", "--method", "DELETE",
			fmt.Sprintf("repos/%s/%s/git/refs/heads/%s", githubOwner, githubRepo, e2eTestBranch3))
		_, _ = runCmd(cmd)
	}
})
