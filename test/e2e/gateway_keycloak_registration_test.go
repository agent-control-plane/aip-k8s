//go:build e2e

package e2e

// Phase 8: Gateway Keycloak OIDC + Registration Policy + Credential Brokering
//
// Test-layer contract (see also cmd/gateway/gateway_registration_test.go):
//   - Binary-subprocess tests (cmd/gateway/): cover strict-mode 403, IDENTITY_MISMATCH,
//     token injection logic with a fake OIDC server — fast, no cluster required.
//   - THIS FILE: verifies the full in-cluster path with a real Keycloak OIDC server
//     and a real MCPServer CR → controller → credential brokering round trip.
//
// The fake MCP upstream runs as an in-cluster Deployment rather than an httptest.Server
// on the host. This eliminates Docker Desktop's VM networking layer on macOS, where
// host.docker.internal resolves to a virtual IPv6 address (fdc4:.../254) that is not
// bound to any macOS network interface and therefore unreachable from Kind pods.
// The controller reaches the fake server via ClusterIP DNS; the test process reaches
// it via kubectl port-forward (same pattern as Keycloak and the gateway).

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/agent-control-plane/aip-k8s/test/utils"
)

const (
	kcStaticUpstreamToken = "kc-reg-static-upstream-token-e2e"
	kcStaticSecretName    = "kc-reg-agent-token"
	kcStaticSecretNS      = "default"

	kcFakeMCPServerName = "kc-fake-mcp"
	fakeMCPImageTag     = "example.com/fake-mcp:test"
	fakeMCPLocalPort    = "18089"
)

var _ = Describe("Phase 8: Gateway Keycloak OIDC + Registration Policy + Credential Brokering", Ordered, func() {
	var gwPFProc *exec.Cmd
	var pfProc *exec.Cmd
	var fakeMCP *kcFakeMCPUpstream

	BeforeAll(func() {
		projDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())

		clusterName := os.Getenv("KIND_CLUSTER_NAME")
		if clusterName == "" {
			clusterName = "aip-k8s-test-e2e"
		}
		kindBinary := os.Getenv("KIND")
		if kindBinary == "" {
			kindBinary = "kind"
		}

		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec

		By("deploying Keycloak dev instance")
		ensureKeycloakTLSSecret(projDir)
		applyCmd := exec.Command("kubectl", "apply", "-f",
			projDir+"/test/fixtures/keycloak-dev.yaml")
		out, err := applyCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "kubectl apply keycloak: %s", string(out))

		// Build images while Keycloak is starting up to reduce total wall-clock time.
		By("building fake MCP server Docker image")
		buildFake := exec.Command("docker", "build", "-f", "Dockerfile.fake-mcp",
			"-t", fakeMCPImageTag, ".")
		buildFake.Dir = projDir
		out, err = buildFake.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "build fake-mcp image: %s", string(out))

		By("loading fake MCP server image into Kind cluster")
		loadFake := exec.Command(kindBinary, "load", "docker-image", fakeMCPImageTag, "--name", clusterName)
		out, err = loadFake.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "kind load fake-mcp image: %s", string(out))

		By("building gateway Docker image")
		buildImg := exec.Command("docker", "build", "-f", "Dockerfile.gateway",
			"-t", "example.com/aip-gateway:v0.0.1", ".")
		buildImg.Dir = projDir
		out, err = buildImg.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "build gateway image: %s", string(out))

		By("loading gateway image into Kind cluster")
		loadImg := exec.Command(kindBinary, "load", "docker-image", "example.com/aip-gateway:v0.0.1", "--name", clusterName)
		out, err = loadImg.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "kind load gateway image: %s", string(out))

		By("waiting for Keycloak pod to be ready")
		Eventually(func(g Gomega) {
			readyCmd := exec.Command("kubectl", "get", "pods",
				"-l", "app=keycloak", "-n", "keycloak",
				"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
			status, err := utils.Run(readyCmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"), "Keycloak pod not yet ready")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("port-forwarding Keycloak to localhost:" + kcPort)
		_ = exec.Command("pkill", "-f", "port-forward.*keycloak.*"+kcPort).Run()
		time.Sleep(time.Second)
		pfProc = exec.Command("kubectl", "port-forward",
			"svc/keycloak", kcPort+":8443", "-n", "keycloak")
		pfProc.Stdout = GinkgoWriter
		pfProc.Stderr = GinkgoWriter
		Expect(pfProc.Start()).To(Succeed())
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
		kcSetupRegistrationClients(kcPort, kcRealm)

		By("deploying fake MCP server Deployment + Service")
		fakeMCPDeployJSON := fmt.Sprintf(`{
			"apiVersion": "apps/v1",
			"kind": "Deployment",
			"metadata": {"name": "fake-mcp-server", "namespace": "default"},
			"spec": {
				"replicas": 1,
				"selector": {"matchLabels": {"app": "fake-mcp-server"}},
				"template": {
					"metadata": {"labels": {"app": "fake-mcp-server"}},
					"spec": {
						"containers": [{
							"name": "fake-mcp",
							"image": %q,
							"imagePullPolicy": "Never",
							"ports": [{"containerPort": 8080}],
							"resources": {
								"requests": {"cpu": "10m", "memory": "32Mi"},
								"limits":   {"cpu": "100m", "memory": "64Mi"}
							}
						}]
					}
				}
			}
		}`, fakeMCPImageTag)
		applyFakeDeploy := exec.Command("kubectl", "apply", "-f", "-")
		applyFakeDeploy.Stdin = strings.NewReader(fakeMCPDeployJSON)
		out, err = applyFakeDeploy.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "deploy fake-mcp-server: %s", string(out))

		fakeMCPSvcJSON := `{
			"apiVersion": "v1",
			"kind": "Service",
			"metadata": {"name": "fake-mcp-server", "namespace": "default"},
			"spec": {
				"selector": {"app": "fake-mcp-server"},
				"ports": [{"port": 8080, "targetPort": 8080}]
			}
		}`
		applyFakeSvc := exec.Command("kubectl", "apply", "-f", "-")
		applyFakeSvc.Stdin = strings.NewReader(fakeMCPSvcJSON)
		out, err = applyFakeSvc.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "create fake-mcp-server service: %s", string(out))

		By("waiting for fake MCP server pod to be ready")
		Eventually(func(g Gomega) {
			readyCmd := exec.Command("kubectl", "get", "pods",
				"-l", "app=fake-mcp-server", "-n", "default",
				"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
			status, err := utils.Run(readyCmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"), "fake-mcp-server pod not yet ready")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("port-forwarding fake MCP server to localhost:" + fakeMCPLocalPort)
		_ = exec.Command("pkill", "-f", "port-forward.*fake-mcp-server.*"+fakeMCPLocalPort).Run()
		time.Sleep(500 * time.Millisecond)
		fakePF := exec.Command("kubectl", "port-forward",
			"svc/fake-mcp-server", fakeMCPLocalPort+":8080", "-n", "default")
		fakePF.Stdout = GinkgoWriter
		fakePF.Stderr = GinkgoWriter
		Expect(fakePF.Start()).To(Succeed())
		fakeMCP = &kcFakeMCPUpstream{pfProc: fakePF}

		By("creating K8s Secret for per-agent static credential")
		secretJSON := fmt.Sprintf(`{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   {"name": %q, "namespace": %q},
			"stringData": {"token": %q}
		}`, kcStaticSecretName, kcStaticSecretNS, kcStaticUpstreamToken)
		applySecret := exec.Command("kubectl", "apply", "-f", "-")
		applySecret.Stdin = strings.NewReader(secretJSON)
		out, err = applySecret.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "create Secret: %s", string(out))

		By("applying AgentRegistration CR")
		regJSON := fmt.Sprintf(`{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind":       "AgentRegistration",
			"metadata":   {"name": "kc-registered-agent", "namespace": "default"},
			"spec": {
				"agentIdentity": %q,
				"oidc": {
					"issuer":          %q,
					"subjectClaim":    "azp",
					"allowedSubjects": [%q]
				},
				"externalIdentities": [{
					"service": %q,
					"type":    "StaticSecret",
					"staticSecret": {
						"name":      %q,
						"namespace": %q,
						"key":       "token"
					}
				}]
			}
		}`, kcRegisteredAgentID, kcInClusterIssuer, kcRegisteredAgentID,
			kcFakeMCPServerName, kcStaticSecretName, kcStaticSecretNS)
		applyReg := exec.Command("kubectl", "apply", "-f", "-")
		applyReg.Stdin = strings.NewReader(regJSON)
		out, err = applyReg.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "apply AgentRegistration: %s", string(out))

		By("creating MCPServer CR pointing at in-cluster fake upstream")
		mcpServerJSON := fmt.Sprintf(`{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind":       "MCPServer",
			"metadata":   {"name": %q},
			"spec": {
				"url":           %q,
				"readOnlyTools": ["echo"]
			}
		}`, kcFakeMCPServerName, fakeMCP.clusterURLStr())
		applyMCPServer := exec.Command("kubectl", "apply", "-f", "-")
		applyMCPServer.Stdin = strings.NewReader(mcpServerJSON)
		out, err = applyMCPServer.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "apply MCPServer CR: %s", string(out))

		By("creating gateway client credentials secret (placeholder for Phase 8)")
		gwSecretJSON := `{
			"apiVersion": "v1",
			"kind": "Secret",
			"metadata": {"name": "aip-gateway-secret", "namespace": "aip-k8s-system"},
			"stringData": {"client-secret": "not-used-in-phase8"}
		}`
		applyGwSecret := exec.Command("kubectl", "apply", "-f", "-")
		applyGwSecret.Stdin = strings.NewReader(gwSecretJSON)
		out, err = applyGwSecret.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "create aip-gateway-secret: %s", string(out))

		By("creating gateway CA cert secret")
		ensureGatewayCASecret(projDir)

		By("deploying gateway to cluster")
		_ = exec.Command("kubectl", "delete", "deployment", "aip-gateway", "-n", "aip-k8s-system", "--ignore-not-found").Run()
		_ = exec.Command("kubectl", "wait", "--for=delete", "pod", "-l", "app=aip-gateway", "-n", "aip-k8s-system", "--timeout=30s").Run()
		applyGw := exec.Command("kubectl", "apply", "-f", projDir+"/test/fixtures/gateway-dev.yaml")
		out, err = applyGw.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "apply gateway-dev.yaml: %s", string(out))

		By("waiting for gateway pod to be ready")
		Eventually(func(g Gomega) {
			readyCmd := exec.Command("kubectl", "get", "pods",
				"-l", "app=aip-gateway", "-n", "aip-k8s-system",
				"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
			status, err := utils.Run(readyCmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"), "Gateway pod not yet ready")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("port-forwarding gateway to localhost:" + kc8GWPort)
		_ = exec.Command("pkill", "-f", "port-forward.*aip-gateway.*"+kc8GWPort).Run()
		time.Sleep(500 * time.Millisecond)
		gwPF := exec.Command("kubectl", "port-forward",
			"svc/aip-gateway", kc8GWPort+":8080", "-n", "aip-k8s-system")
		gwPF.Stdout = GinkgoWriter
		gwPF.Stderr = GinkgoWriter
		Expect(gwPF.Start()).To(Succeed())
		gwPFProc = gwPF

		Eventually(func() int {
			resp, err := http.Get("http://localhost:" + kc8GWPort + "/healthz") //nolint:noctx
			if err != nil {
				return 0
			}
			defer resp.Body.Close() //nolint:errcheck
			return resp.StatusCode
		}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

		By("waiting for MCPServer controller to discover tools from fake upstream")
		Eventually(func() string {
			out, err := exec.Command("kubectl", "get", "mcpserver", kcFakeMCPServerName,
				"-o", `jsonpath={.status.discoveredToolCount}`).CombinedOutput()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(out))
		}, 60*time.Second, 2*time.Second).Should(Equal("1"), "MCPServer controller did not discover echo tool in time")

		By("creating SafetyPolicy requiring human approval")
		gwCleanup("default")
		policyJSON := `{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind":       "SafetyPolicy",
			"metadata":   {"name": "kc-require-human", "namespace": "default"},
			"spec": {
				"governedResourceSelector": {},
				"rules": [{"name": "require-human", "type": "StateEvaluation",
				           "action": "RequireApproval", "expression": "true"}],
				"failureMode": "FailClosed"
			}
		}`
		policyCmd := exec.Command("kubectl", "apply", "-f", "-")
		policyCmd.Stdin = strings.NewReader(policyJSON)
		policyOut, policyErr := policyCmd.CombinedOutput()
		Expect(policyErr).NotTo(HaveOccurred(), "create SafetyPolicy: %s", string(policyOut))
		Eventually(func() error {
			return exec.Command("kubectl", "get", "safetypolicy",
				"kc-require-human", "-n", "default").Run()
		}, 15*time.Second, time.Second).Should(Succeed())
	})

	AfterAll(func() {
		if gwPFProc != nil && gwPFProc.Process != nil {
			_ = gwPFProc.Process.Kill()
		}
		if pfProc != nil && pfProc.Process != nil {
			_ = pfProc.Process.Kill()
		}
		if fakeMCP != nil {
			fakeMCP.close()
		}
		gwCleanup("default")
		_ = exec.Command("kubectl", "delete", "agentregistration", "--all",
			"-n", "default", "--ignore-not-found").Run()
		_ = exec.Command("kubectl", "delete", "mcpserver", "--all", "--ignore-not-found").Run()
		_ = exec.Command("kubectl", "delete", "secret", "--all",
			"-n", kcStaticSecretNS, "--ignore-not-found").Run()
	})

	It("unregistered agent + strict policy → 403 AGENT_NOT_REGISTERED", func() {
		token := kcFetchToken(kcPort, kcRealm, "aip-agent-1", "agent-1-secret")
		resp, err := gwPostWithToken(kc8GWPort, "/agent-requests", `{
			"agentIdentity": "aip-agent-1",
			"action":        "test-action",
			"targetURI":     "k8s://prod/default/deployment/app",
			"reason":        "strict policy e2e"
		}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		b, _ := io.ReadAll(resp.Body)
		Expect(string(b)).To(ContainSubstring("AGENT_NOT_REGISTERED"))
	})

	It("wrong OIDC subject claiming registered identity → 403 IDENTITY_MISMATCH", func() {
		token := kcFetchToken(kcPort, kcRealm, kcWrongSubjectID, kcWrongSubjectSecret)
		resp, err := gwPostWithToken(kc8GWPort, "/agent-requests", fmt.Sprintf(`{
			"agentIdentity": %q,
			"action":        "test-action",
			"targetURI":     "k8s://prod/default/deployment/app",
			"reason":        "identity mismatch e2e"
		}`, kcRegisteredAgentID), token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		b, _ := io.ReadAll(resp.Body)
		Expect(string(b)).To(ContainSubstring("IDENTITY_MISMATCH"))
	})

	It("registered agent with matching OIDC subject → 201", func() {
		token := kcFetchToken(kcPort, kcRealm, kcRegisteredAgentID, kcRegisteredAgentSecret)
		resp, err := gwPostWithToken(kc8GWPort, "/agent-requests", fmt.Sprintf(`{
			"agentIdentity": %q,
			"action":        "kc-reg-action",
			"targetURI":     "k8s://prod/default/deployment/payment-api",
			"reason":        "registration e2e"
		}`, kcRegisteredAgentID), token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))
	})

	It("agent token on reviewer endpoint → 403 reviewer role required", func() {
		token := kcFetchToken(kcPort, kcRealm, kcRegisteredAgentID, kcRegisteredAgentSecret)
		resp, err := gwPostWithToken(kc8GWPort, "/agent-requests/nonexistent/approve",
			`{"reason": "test"}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		b, _ := io.ReadAll(resp.Body)
		Expect(string(b)).To(ContainSubstring("reviewer role required"))
	})

	It("registered agent tool call → upstream receives per-agent static credential", func() {
		token := kcFetchToken(kcPort, kcRealm, kcRegisteredAgentID, kcRegisteredAgentSecret)
		mcpBody := fmt.Sprintf(`{
			"jsonrpc": "2.0", "id": 1,
			"method": "tools/call",
			"params": {"name": %q, "arguments": {}}
		}`, kcFakeMCPServerName+"/echo")
		resp, err := gwPostWithToken(kc8GWPort, "/mcp", mcpBody, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(fakeMCP.lastAuth()).To(Equal("Bearer "+kcStaticUpstreamToken),
			"upstream should receive per-agent static credential, not shared MCPServer token")
	})
})

func kcSetupRegistrationClients(port, realm string) {
	adminToken := kcAdminToken(port)
	for _, c := range []struct{ id, secret string }{
		{kcRegisteredAgentID, kcRegisteredAgentSecret},
		{kcWrongSubjectID, kcWrongSubjectSecret},
	} {
		internalID := kcCreateClient(port, adminToken, realm, c.id, c.secret)
		kcAddMapper(port, adminToken, realm, internalID, map[string]interface{}{
			"name":           "audience-aip-gateway-" + c.id,
			"protocol":       "openid-connect",
			"protocolMapper": "oidc-audience-mapper",
			"config": map[string]string{
				"included.custom.audience": "aip-gateway",
				"id.token.claim":           "true",
				"access.token.claim":       "true",
			},
		})
	}
}

// kcFakeMCPUpstream manages a fake MCP server running as an in-cluster Deployment.
// The controller reaches it via ClusterIP DNS (no Docker networking required).
// The test process reaches it via kubectl port-forward for lastAuth() queries.
type kcFakeMCPUpstream struct {
	pfProc *exec.Cmd
}

// clusterURLStr returns the in-cluster Service URL for the MCPServer controller.
func (f *kcFakeMCPUpstream) clusterURLStr() string {
	return "http://fake-mcp-server.default.svc.cluster.local:8080"
}

// lastAuth queries the port-forwarded fake server for the last captured Authorization header.
func (f *kcFakeMCPUpstream) lastAuth() string {
	resp, err := http.Get("http://localhost:" + fakeMCPLocalPort + "/_last-auth") //nolint:noctx
	if err != nil {
		return ""
	}
	defer resp.Body.Close() //nolint:errcheck
	var result map[string]string
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return ""
	}
	return result["auth"]
}

func (f *kcFakeMCPUpstream) close() {
	if f.pfProc != nil && f.pfProc.Process != nil {
		_ = f.pfProc.Process.Kill()
	}
	_ = exec.Command("kubectl", "delete", "deployment", "fake-mcp-server",
		"-n", "default", "--ignore-not-found").Run()
	_ = exec.Command("kubectl", "delete", "service", "fake-mcp-server",
		"-n", "default", "--ignore-not-found").Run()
}
