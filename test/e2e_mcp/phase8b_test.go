//go:build mcp_e2e
// +build mcp_e2e

// Phase 8b: per-agent AgentRegistration credentials with real Keycloak + GitHub MCP.
//
// Two-layer test coverage:
//   - cmd/gateway/gateway_registration_test.go — fake OIDC, makes test (strict mode, identity checks)
//   - THIS FILE — real Keycloak + real GitHub API, make test-e2e (PAT brokering per agent)
//
// Requires: AIP_E2E_GITHUB_PAT_AGENT1, AIP_E2E_GITHUB_PAT_AGENT2, Kind cluster.

package e2e_mcp

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

var _ = Describe("Phase 8b: per-agent AgentRegistration credentials (Keycloak)", Ordered, func() {
	var gwPFProc *exec.Cmd
	var kcPFProc *exec.Cmd

	BeforeAll(func() {
		if os.Getenv("AIP_E2E_GITHUB_PAT_AGENT1") == "" ||
			os.Getenv("AIP_E2E_GITHUB_PAT_AGENT2") == "" {
			Skip("AIP_E2E_GITHUB_PAT_AGENT1 and AIP_E2E_GITHUB_PAT_AGENT2 required for Phase 8b")
		}

		projDir := getProjectDir()
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec

		By("deploying Keycloak dev instance")
		ensureKeycloakTLSSecret(projDir)
		_, err := runCmd(exec.Command("kubectl", "apply", "-f",
			filepath.Join(projDir, "test/fixtures/keycloak-dev.yaml")))
		Expect(err).NotTo(HaveOccurred(), "apply keycloak-dev.yaml")

		By("waiting for Keycloak pod to be ready")
		Eventually(func(g Gomega) {
			out, err := runCmd(exec.Command("kubectl", "get", "pods",
				"-l", "app=keycloak", "-n", "keycloak",
				"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("True"))
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("port-forwarding Keycloak to localhost:" + kcPort)
		_ = exec.Command("pkill", "-f", "port-forward.*keycloak.*"+kcPort).Run()
		time.Sleep(time.Second)
		kcPFProc = exec.Command("kubectl", "port-forward",
			"svc/keycloak", kcPort+":8443", "-n", "keycloak")
		kcPFProc.Stdout = GinkgoWriter
		kcPFProc.Stderr = GinkgoWriter
		Expect(kcPFProc.Start()).To(Succeed())
		Eventually(func() int {
			resp, err := http.Get(kcBase + "/realms/master/.well-known/openid-configuration") //nolint:noctx
			if err != nil {
				return 0
			}
			defer resp.Body.Close() //nolint:errcheck
			return resp.StatusCode
		}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

		By("configuring Keycloak realm and clients")
		kcSetup(kcPort, kcRealm)
		kcSetupClient(kcPort, kcRealm, "aip-agent-2", "agent-2-secret")

		By("creating K8s Secrets for per-agent GitHub PATs")
		createSecretInNamespace("github-pat-agent1", "aip-k8s-system", os.Getenv("AIP_E2E_GITHUB_PAT_AGENT1"))
		createSecretInNamespace("github-pat-agent2", "aip-k8s-system", os.Getenv("AIP_E2E_GITHUB_PAT_AGENT2"))

		By("deploying github-mcp-server")
		deployGitHubMCPServer(os.Getenv("AIP_E2E_GITHUB_PAT_AGENT1"))

		By("creating github MCPServer CR")
		githubMCPServerJSON := `{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind":       "MCPServer",
			"metadata":   {"name": "github"},
			"spec": {
				"url": "http://github-mcp.aip-k8s-system.svc.cluster.local:80"
			}
		}`
		Expect(kubectlApply(githubMCPServerJSON)).To(Succeed())

		By("applying AgentRegistrations")
		applyAgentRegistration("aip-agent-1", "github-pat-agent1")
		applyAgentRegistration("aip-agent-2", "github-pat-agent2")

		By("waiting for AgentTrustProfiles")
		Eventually(func(g Gomega) {
			var atp1, atp2 governancev1alpha1.AgentTrustProfile
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "tp-aip-agent-1", Namespace: "default"}, &atp1)).To(Succeed())
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "tp-aip-agent-2", Namespace: "default"}, &atp2)).To(Succeed())
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("building and loading gateway Docker image")
		buildLoadGatewayImage(projDir)

		By("deploying gateway (gateway-dev.yaml) and patching for warn policy")
		ensureGatewayCASecret(projDir)
		// Ensure any previous gateway is gone so apply starts fresh.
		_ = exec.Command("kubectl", "delete", "deployment", "aip-gateway", "-n", "aip-k8s-system", "--ignore-not-found").Run()
		_ = exec.Command("kubectl", "wait", "--for=delete", "pod", "-l", "app=aip-gateway", "-n", "aip-k8s-system", "--timeout=30s").Run()

		// gateway-dev.yaml already has oidc-issuer-url, audience, identity-claim, addr=:18088.
		// We need to create the gateway-secret (required by gateway-dev.yaml volume mount).
		gwSecretJSON := `{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": {"name": "aip-gateway-secret", "namespace": "aip-k8s-system"},
			"stringData": {"client-secret": "placeholder-not-used-for-phase8b"}
		}`
		Expect(kubectlApply(gwSecretJSON)).To(Succeed())

		_, err = runCmd(exec.Command("kubectl", "apply", "-f",
			filepath.Join(projDir, "test/fixtures/gateway-dev.yaml")))
		Expect(err).NotTo(HaveOccurred(), "apply gateway-dev.yaml")

		agentSubjects := strings.Join([]string{
			kcRegisteredAgentID,
			"aip-agent-1",
			"aip-agent-2",
			kcWrongSubjectID,
			"completely-unknown-agent",
		}, ",")
		patchJSON := fmt.Sprintf(
			`{"spec":{"template":{"spec":{"containers":[{"name":"gateway","args":[
				"--addr=:18088",
				"--oidc-issuer-url=%s",
				"--oidc-audience=aip-gateway",
				"--oidc-identity-claim=azp",
				"--agent-subjects=%s",
				"--reviewer-subjects=aip-reviewer-1",
				"--admin-subjects=aip-reviewer-1",
				"--unregistered-agent-policy=warn"
			]}]}}}}`,
			kcInClusterIssuer, agentSubjects)
		_, err = runCmd(exec.Command("kubectl", "patch", "deployment", "aip-gateway",
			"-n", "aip-k8s-system", "--type=strategic", "--patch", patchJSON))
		Expect(err).NotTo(HaveOccurred(), "patch gateway deployment")

		_, err = runCmd(exec.Command("kubectl", "rollout", "status", "deployment/aip-gateway",
			"-n", "aip-k8s-system", "--timeout=3m"))
		Expect(err).NotTo(HaveOccurred(), "gateway rollout status")

		By("port-forwarding gateway to localhost:" + kc8bGWPort)
		_ = exec.Command("pkill", "-f", "port-forward.*aip-gateway.*"+kc8bGWPort).Run()
		time.Sleep(500 * time.Millisecond)
		gwPFProc = exec.Command("kubectl", "port-forward", "svc/aip-gateway", kc8bGWPort+":8080", "-n", "aip-k8s-system")
		gwPFProc.Stdout = GinkgoWriter
		gwPFProc.Stderr = GinkgoWriter
		Expect(gwPFProc.Start()).To(Succeed())
		Eventually(func() int {
			resp, err := http.Get("http://localhost:" + kc8bGWPort + "/healthz") //nolint:noctx
			if err != nil {
				return 0
			}
			defer resp.Body.Close() //nolint:errcheck
			return resp.StatusCode
		}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))
	})

	AfterAll(func() {
		_ = exec.Command("kubectl", "delete", "agentregistration", "--all", "-n", "default", "--ignore-not-found").Run()
		_ = exec.Command("kubectl", "delete", "agentregistration", "--all", "-n", "aip-k8s-system", "--ignore-not-found").Run()
		_ = exec.Command("kubectl", "delete", "secret", "--all", "-n", "aip-k8s-system", "--ignore-not-found").Run()
		_ = exec.Command("kubectl", "delete", "mcpserver", "--all", "--ignore-not-found").Run()
		_ = exec.Command("kubectl", "delete", "deployment", "aip-gateway", "-n", "aip-k8s-system", "--ignore-not-found").Run()

		if gwPFProc != nil && gwPFProc.Process != nil {
			_ = gwPFProc.Process.Kill()
		}
		if kcPFProc != nil && kcPFProc.Process != nil {
			_ = kcPFProc.Process.Kill()
		}
		gwCleanup("default")
	})

	testPerAgentCredentialBrokering := func(agentID, patEnvVar, kcClientID, kcClientSecret, prTitle string) {
		pat := os.Getenv(patEnvVar)
		expectedUser, err := getGitHubUser(pat)
		Expect(err).NotTo(HaveOccurred())

		branchName := fmt.Sprintf("phase8b-%s-%d", agentID, time.Now().UnixNano())
		createGitHubBranch(pat, branchName)
		defer deleteGitHubBranch(pat, branchName)

		token := kcFetchToken(kcPort, kcRealm, kcClientID, kcClientSecret)

		resp, err := gwPostWithToken(kc8bGWPort, "/agent-requests", fmt.Sprintf(`{
			"agentIdentity": "%s",
			"action":        "github/create_pull_request",
			"targetURI":     "github://%s/%s",
			"reason":        "Phase 8b e2e test %s"
		}`, agentID, githubOwner, githubRepo, agentID), token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))

		var createResp struct {
			Name string `json:"name"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&createResp)).To(Succeed())
		reqName := createResp.Name
		Expect(reqName).NotTo(BeEmpty())

		reviewerToken := kcFetchToken(kcPort, kcRealm, "aip-reviewer-1", "reviewer-1-secret")
		approveResp, err := gwPostWithToken(kc8bGWPort,
			"/agent-requests/"+reqName+"/approve",
			fmt.Sprintf(`{"reason":"Phase 8b approval %s"}`, agentID),
			reviewerToken)
		Expect(err).NotTo(HaveOccurred())
		defer approveResp.Body.Close()
		Expect(approveResp.StatusCode).To(Equal(http.StatusOK))

		var aprBody struct {
			Token string `json:"token"`
		}
		Expect(json.NewDecoder(approveResp.Body).Decode(&aprBody)).To(Succeed())
		aipJWT := aprBody.Token
		Expect(aipJWT).NotTo(BeEmpty())

		prBody := fmt.Sprintf(`{
			"name": "create_pull_request",
			"arguments": {
				"owner": "%s",
				"repo":  "%s",
				"title": "%s",
				"body":  "PR created during Keycloak per-agent credentials e2e test.",
				"head":  "%s",
				"base":  "main",
				"draft": true
			}
		}`, githubOwner, githubRepo, prTitle, branchName)

		proxyReq, err := http.NewRequest("POST", //nolint:noctx
			fmt.Sprintf("http://localhost:%s/mcp-proxy/github/create_pull_request", kc8bGWPort),
			strings.NewReader(prBody))
		Expect(err).NotTo(HaveOccurred())
		proxyReq.Header.Set("Content-Type", "application/json")
		proxyReq.Header.Set("X-AIP-Authorization", "Bearer "+aipJWT)
		proxyResp, err := http.DefaultClient.Do(proxyReq)
		Expect(err).NotTo(HaveOccurred())
		defer proxyResp.Body.Close()
		Expect(proxyResp.StatusCode).To(Equal(http.StatusOK))

		var proxyResult struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		Expect(json.NewDecoder(proxyResp.Body).Decode(&proxyResult)).To(Succeed())
		Expect(proxyResult.Content).NotTo(BeEmpty())
		prURL := proxyResult.Content[0].Text
		Expect(prURL).To(ContainSubstring("/pull/"))

		parts := strings.Split(prURL, "/")
		prNum := parts[len(parts)-1]

		respPR, err := githubAPI("GET",
			fmt.Sprintf("/repos/%s/%s/pulls/%s", githubOwner, githubRepo, prNum),
			nil, pat)
		Expect(err).NotTo(HaveOccurred())
		defer respPR.Body.Close()
		Expect(respPR.StatusCode).To(Equal(http.StatusOK))

		var prInfo struct {
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		Expect(json.NewDecoder(respPR.Body).Decode(&prInfo)).To(Succeed())
		Expect(prInfo.User.Login).To(Equal(expectedUser))
	}

	It("aip-agent-1 Keycloak token → gateway admits, MCP call uses agent-1 PAT", func() {
		testPerAgentCredentialBrokering(
			"aip-agent-1", "AIP_E2E_GITHUB_PAT_AGENT1",
			"aip-agent-1", "agent-1-secret",
			"[Phase 8b] E2E test PR agent-1",
		)
	})

	It("aip-agent-2 Keycloak token → gateway admits, MCP call uses agent-2 PAT", func() {
		testPerAgentCredentialBrokering(
			"aip-agent-2", "AIP_E2E_GITHUB_PAT_AGENT2",
			"aip-agent-2", "agent-2-secret",
			"[Phase 8b] E2E test PR agent-2",
		)
	})

	It("unregistered agent in warn mode → admitted with annotation", func() {
		kcSetupClient(kcPort, kcRealm, "completely-unknown-agent", "unknown-secret")
		token := kcFetchToken(kcPort, kcRealm, "completely-unknown-agent", "unknown-secret")

		resp, err := gwPostWithToken(kc8bGWPort, "/agent-requests", `{
			"agentIdentity": "completely-unknown-agent",
			"action":        "test-action-warn",
			"targetURI":     "k8s://prod/default/deployment/warn-app",
			"reason":        "warn policy e2e"
		}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))

		var body map[string]interface{}
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		reqName, _ := body["name"].(string)
		Expect(reqName).NotTo(BeEmpty())

		Eventually(func(g Gomega) {
			var ar governancev1alpha1.AgentRequest
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: reqName, Namespace: "default"}, &ar)).To(Succeed())
			g.Expect(ar.Annotations).NotTo(BeNil())
			g.Expect(ar.Annotations["governance.aip.io/unregistered"]).To(Equal("true"))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		// Clean up the unknown-agent Keycloak client created only for this spec.
		kcDeleteClient(kcPort, kcRealm, "completely-unknown-agent")
	})
})
