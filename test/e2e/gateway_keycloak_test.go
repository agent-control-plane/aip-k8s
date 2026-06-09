//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/credential"
	"github.com/agent-control-plane/aip-k8s/test/utils"
)

const (
	kcPort    = "18091"
	kcBase    = "https://127.0.0.1:" + kcPort
	kcRealm   = "aip"
	kc8GWPort = "18085"
	kcIssuer  = kcBase + "/realms/" + kcRealm

	// kcRegisteredAgentID is the Keycloak client_id for the registered agent.
	// Its azp claim matches the AllowedSubjects in its AgentRegistration.
	kcRegisteredAgentID     = "aip-registered-agent"
	kcRegisteredAgentSecret = "reg-agent-secret"

	// kcWrongSubjectID is a valid OIDC client whose azp is NOT listed in the
	// registered agent's AllowedSubjects. Used to exercise IDENTITY_MISMATCH.
	kcWrongSubjectID     = "aip-wrong-subject"
	kcWrongSubjectSecret = "wrong-subject-secret"

	// kcStaticUpstreamToken is the per-agent static bearer token stored in K8s
	// and forwarded to the fake MCP server. The test asserts the upstream receives
	// this token — not the shared MCPServer token (which is empty).
	kcStaticUpstreamToken = "kc-reg-static-upstream-token-e2e"
	kcStaticSecretName    = "kc-reg-agent-token"
	kcStaticSecretNS      = "default"

	// kcFakeMCPServerName matches the ExternalIdentityBinding.service field in
	// the AgentRegistration and the "name" key in MCP_REGISTRY.
	kcFakeMCPServerName = "kc-fake-mcp"

	githubOwner = "agent-control-plane"
	githubRepo  = "aip-demo-infra"

	// gatewayRestartGracePeriod is the time to wait after stopping the gateway
	// subprocess before starting it again with new flags.
	gatewayRestartGracePeriod = 2 * time.Second
)

// Phase 8 extends the Keycloak OIDC integration with registration policy enforcement
// and credential brokering validation.
//
//   - Policy tests (strict/IDENTITY_MISMATCH/registered success) exercise the
//     K8s watch → registrationCache → gateway enforcement path with real Keycloak tokens.
//     The logic branches themselves are covered by unit-level integration tests;
//     what is unique here is the real K8s watch and real JWT azp claim extraction.
//
//   - The credential brokering test verifies that the gateway forwards the per-agent
//     StaticSecret credential (not the shared MCPServer token) to the upstream server,
//     proving the agentIdentity → ExternalIdentityBinding → provider → bearer-token chain.
//
// Phase 7 (mock OIDC) already covers: missing token → 401, expired → 401, wrong
// audience → 401, self-approval → 403, healthz → 200. Those tests are not repeated here.
var _ = Describe("Phase 8: Gateway Keycloak OIDC + Registration Policy + Credential Brokering", Ordered, func() {
	var gwProc *exec.Cmd
	var pfProc *exec.Cmd
	var fakeMCP *kcFakeMCPUpstream

	BeforeAll(func() {
		projDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())

		// Disable TLS verification for Keycloak HTTPS calls
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

		// 1. Deploy Keycloak (idempotent).
		By("deploying Keycloak dev instance")
		ensureKeycloakTLSSecret(projDir)
		applyCmd := exec.Command("kubectl", "apply", "-f",
			projDir+"/test/fixtures/keycloak-dev.yaml")
		out, err := applyCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "kubectl apply keycloak: %s", string(out))

		// 2. Wait for Keycloak pod.
		By("waiting for Keycloak pod to be ready")
		Eventually(func(g Gomega) {
			readyCmd := exec.Command("kubectl", "get", "pods",
				"-l", "app=keycloak", "-n", "keycloak",
				"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
			status, err := utils.Run(readyCmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"), "Keycloak pod not yet ready")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		// 3. Port-forward Keycloak; kill any stale forward first.
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

		// 4. Configure realm: existing clients (aip-agent-1, aip-reviewer-1) +
		// registration test clients (aip-registered-agent, aip-wrong-subject).
		By("configuring Keycloak realm and clients")
		kcSetup(kcPort, kcRealm)
		kcSetupRegistrationClients(kcPort, kcRealm)

		// 5. Start the fake upstream MCP server. It must be up before the gateway
		// subprocess starts so MCP_REGISTRY points to a live URL.
		By("starting fake upstream MCP server")
		fakeMCP = newKCFakeMCPUpstream()

		// 6. Create the K8s Secret holding the static per-agent upstream token.
		// Applied before the gateway starts so the StaticSecret provider can read it.
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

		// 7. Apply the AgentRegistration CR before the gateway starts so the initial
		// list+watch picks it up synchronously and the regCache is populated by the
		// time the first test request arrives.
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
		}`, kcRegisteredAgentID, kcIssuer, kcRegisteredAgentID,
			kcFakeMCPServerName, kcStaticSecretName, kcStaticSecretNS)
		applyReg := exec.Command("kubectl", "apply", "-f", "-")
		applyReg.Stdin = strings.NewReader(regJSON)
		out, err = applyReg.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "apply AgentRegistration: %s", string(out))

		// 8. Build gateway binary.
		By("building gateway binary")
		binPath := projDir + "/bin/gateway"
		buildCmd := exec.Command("go", "build", "-o", binPath, projDir+"/cmd/gateway")
		buildCmd.Dir = projDir
		buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err = buildCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "build gateway: %s", string(out))

		// 9. Start the gateway subprocess.
		//    --unregistered-agent-policy=strict exercises the strict enforcement path.
		//    MCP_REGISTRY wires in the fake upstream so no MCPServer CR watch is needed.
		//    The subprocess inherits KUBECONFIG from the test environment so the
		//    watchAgentRegistrations goroutine connects to the Kind cluster.
		By("starting gateway with strict policy and fake MCP server")
		mcpRegistry := fmt.Sprintf(
			`[{"name":%q,"url":%q,"tools":[{"name":"echo","read_only":true}]}]`,
			kcFakeMCPServerName, fakeMCP.url(),
		)
		// Explicitly restrict agent subjects so openMode() is false.
		// Without this every caller passes every role check (open mode),
		// which would cause test 4 to get 404 instead of 403 when an agent
		// token hits a reviewer endpoint (role check passes → K8s Get →
		// NotFound). The reviewer-subjects flag is intentionally omitted so
		// aip-registered-agent (agent only) fails the reviewer role check.
		agentSubjects := strings.Join([]string{
			kcRegisteredAgentID, // tests 3, 4, 5
			"aip-agent-1",       // test 1: unregistered agent strict rejection
			kcWrongSubjectID,    // test 2: IDENTITY_MISMATCH
		}, ",")
		gwProc = exec.Command(binPath,
			"--addr=:"+kc8GWPort,
			"--oidc-issuer-url="+kcIssuer,
			"--oidc-audience=aip-gateway",
			"--oidc-identity-claim=azp",
			"--agent-subjects="+agentSubjects,
			"--unregistered-agent-policy=strict",
		)
		gwProc.Dir = projDir
		gwProc.Env = append(os.Environ(),
			"MCP_REGISTRY="+mcpRegistry,
			"SSL_CERT_FILE="+filepath.Join(projDir, "test/fixtures/certs/ca.crt"),
		)
		gwProc.Stdout = GinkgoWriter
		gwProc.Stderr = GinkgoWriter
		Expect(gwProc.Start()).To(Succeed())

		Eventually(func() int {
			resp, err := http.Get("http://localhost:" + kc8GWPort + "/healthz") //nolint:noctx
			if err != nil {
				return 0
			}
			defer resp.Body.Close() //nolint:errcheck
			return resp.StatusCode
		}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

		// 10. Create a SafetyPolicy so POST /agent-requests returns 201 quickly
		// (RequiresApproval early-return) rather than blocking until the 90s timeout.
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
		if gwProc != nil && gwProc.Process != nil {
			_ = gwProc.Process.Kill()
		}
		if pfProc != nil && pfProc.Process != nil {
			_ = pfProc.Process.Kill()
		}
		if fakeMCP != nil {
			fakeMCP.close()
		}
		gwCleanup("default")
		// Delete all AgentRegistrations created by this suite.
		_ = exec.Command("kubectl", "delete", "agentregistration", "--all",
			"-n", "default", "--ignore-not-found").Run()
		// Delete the specific secret we created — deleting all secrets would
		// remove controller secrets and break unrelated tests.
		_ = exec.Command("kubectl", "delete", "secret", kcStaticSecretName,
			"-n", kcStaticSecretNS, "--ignore-not-found").Run()
	})

	It("unregistered agent + strict policy → 403 AGENT_NOT_REGISTERED", func() {
		// aip-agent-1 has no AgentRegistration; strict mode must block it.
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
		// aip-wrong-subject's azp is "aip-wrong-subject", which is not in the
		// AgentRegistration's AllowedSubjects for "aip-registered-agent".
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
		// aip-registered-agent's azp matches AllowedSubjects; request must succeed.
		// This validates the full path: real Keycloak token → azp extraction →
		// K8s-watch-backed registrationCache lookup → policy pass → 201.
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
		// This is the credential brokering test. It verifies that the gateway:
		//   1. Extracts agentIdentity from the azp claim of the Keycloak JWT.
		//   2. Resolves the StaticSecret provider via registrationCache.providerFor().
		//   3. Forwards the per-agent static token — not the shared MCPServer token
		//      (which is empty) — to the upstream server.
		//
		// If brokering is broken (e.g. cache miss, wrong agentIdentity extracted,
		// or fallback to shared token), the fake server receives an empty or wrong
		// Authorization header and the assertion fails.
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
		Expect(fakeMCP.lastAuth()).To(Equal("Bearer " + kcStaticUpstreamToken),
			"upstream should receive per-agent static credential, not shared MCPServer token")
	})

	Context("Phase 8b: per-agent AgentRegistration credentials", Ordered, func() {
		var mcpPfProc *exec.Cmd

		BeforeAll(func() {
			if os.Getenv("AIP_E2E_GITHUB_PAT_AGENT1") == "" ||
				os.Getenv("AIP_E2E_GITHUB_PAT_AGENT2") == "" {
				Skip("AIP_E2E_GITHUB_PAT_AGENT1 and AIP_E2E_GITHUB_PAT_AGENT2 required for Phase 8b")
			}

			// 1. Register aip-agent-2 as a new Keycloak client (reuse existing kcSetup helpers from Phase 8)
			kcSetupClient(kcPort, kcRealm, "aip-agent-2", "agent-2-secret")

			// 2. Create K8s Secrets in aip-k8s-system: github-pat-agent1 and github-pat-agent2, each with key token
			createSecretInNamespace("github-pat-agent1", "aip-k8s-system", os.Getenv("AIP_E2E_GITHUB_PAT_AGENT1"))
			createSecretInNamespace("github-pat-agent2", "aip-k8s-system", os.Getenv("AIP_E2E_GITHUB_PAT_AGENT2"))

			// 3. Deploy github-mcp-server to the cluster if it isn't deployed
			deployGitHubMCPServer()

			// 4. Port-forward github-mcp-server to localhost:18086
			_ = exec.Command("pkill", "-f", "port-forward.*github-mcp.*18086").Run()
			time.Sleep(time.Second)
			mcpPfProc = exec.Command("kubectl", "port-forward",
				"svc/github-mcp", "18086:80", "-n", "aip-k8s-system")
			mcpPfProc.Stdout = GinkgoWriter
			mcpPfProc.Stderr = GinkgoWriter
			Expect(mcpPfProc.Start()).To(Succeed())

			// Wait for the port-forward to be responsive
			Eventually(func() error {
				resp, err := http.Get("http://localhost:18086") //nolint:noctx
				if err != nil {
					return err
				}
				resp.Body.Close()
				return nil
			}, 30*time.Second, time.Second).Should(Succeed())

			// 5. kubectl apply AgentRegistration for aip-agent-1
			applyAgentRegistration("aip-agent-1", "github-pat-agent1")
			// Same for aip-agent-2 pointing at github-pat-agent2
			applyAgentRegistration("aip-agent-2", "github-pat-agent2")

			// 6. Eventually (30s, 2s interval): verify ATP exists for both agentIdentities via k8sClient.Get
			Eventually(func(g Gomega) {
				var atp1, atp2 governancev1alpha1.AgentTrustProfile
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "tp-aip-agent-1", Namespace: "default"}, &atp1)).To(Succeed())
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "tp-aip-agent-2", Namespace: "default"}, &atp2)).To(Succeed())
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			// 7. Restart the gateway subprocess adding --unregistered-agent-policy=warn
			By("stopping gateway subprocess")
			if gwProc != nil && gwProc.Process != nil {
				_ = gwProc.Process.Kill()
				_, _ = gwProc.Process.Wait()
			}
			time.Sleep(gatewayRestartGracePeriod)

			By("starting gateway with unregistered-agent-policy=warn")
			mcpRegistry := `[{"name":"github","url":"http://localhost:18086","tools":[{"name":"create_pull_request","read_only":false}]}]`
			agentSubjects := strings.Join([]string{
				kcRegisteredAgentID,
				"aip-agent-1",
				"aip-agent-2",
				kcWrongSubjectID,
				"completely-unknown-agent",
			}, ",")
			projDir, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			binPath := projDir + "/bin/gateway"

			gwProc = exec.Command(binPath,
				"--addr=:"+kc8GWPort,
				"--oidc-issuer-url="+kcIssuer,
				"--oidc-audience=aip-gateway",
				"--oidc-identity-claim=azp",
				"--agent-subjects="+agentSubjects,
				"--unregistered-agent-policy=warn",
			)
			gwProc.Dir = projDir
			gwProc.Env = append(os.Environ(), "MCP_REGISTRY="+mcpRegistry)
			gwProc.Stdout = GinkgoWriter
			gwProc.Stderr = GinkgoWriter
			Expect(gwProc.Start()).To(Succeed())

			// Wait for gateway healthz
			Eventually(func() int {
				resp, err := http.Get("http://localhost:" + kc8GWPort + "/healthz") //nolint:noctx
				if err != nil {
					return 0
				}
				defer resp.Body.Close() //nolint:errcheck
				return resp.StatusCode
			}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))
		})

		AfterAll(func() {
			if mcpPfProc != nil && mcpPfProc.Process != nil {
				_ = mcpPfProc.Process.Kill()
				_, _ = mcpPfProc.Process.Wait()
			}

			_ = exec.Command("kubectl", "delete", "agentregistration", "--all", "-n", "default", "--ignore-not-found").Run()
			_ = exec.Command("kubectl", "delete", "agentregistration", "--all", "-n", "aip-k8s-system", "--ignore-not-found").Run()
			_ = exec.Command("kubectl", "delete", "secret", "github-pat-agent1", "github-pat-agent2", "-n", "aip-k8s-system", "--ignore-not-found").Run()

			// Delete Keycloak clients created in this context (Phase 8b).
			kcDeleteClient(kcPort, kcRealm, "aip-agent-2")
			kcDeleteClient(kcPort, kcRealm, "completely-unknown-agent")
			// aip-agent-1 is created in the parent Phase 8 BeforeAll (kcSetupClient)
			// and is also deleted here because Phase 8b does not have its own
			// AfterAll in the parent context.
			kcDeleteClient(kcPort, kcRealm, "aip-agent-1")
		})

		// testPerAgentCredentialBrokering exercises the full flow for a single agent:
		// Keycloak token → gateway admission → MCP proxy call with per-agent PAT.
		testPerAgentCredentialBrokering := func(agentID, patEnvVar, kcClientID, kcClientSecret, prTitle string) {
			pat := os.Getenv(patEnvVar)
			expectedUser, err := getGitHubUser(pat)
			Expect(err).NotTo(HaveOccurred())

			branchName := fmt.Sprintf("phase8b-%s-%d", agentID, time.Now().UnixNano())
			createGitHubBranch(pat, branchName)
			defer deleteGitHubBranch(pat, branchName)

			token := kcFetchToken(kcPort, kcRealm, kcClientID, kcClientSecret)

			resp, err := gwPostWithToken(kc8GWPort, "/agent-requests", fmt.Sprintf(`{
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
			approveResp, err := gwPostWithToken(kc8GWPort, "/agent-requests/"+reqName+"/approve", fmt.Sprintf(`{"reason":"Phase 8b approval %s"}`, agentID), reviewerToken)
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
					"repo": "%s",
					"title": "%s",
					"body": "PR created during Keycloak per-agent credentials e2e test.",
					"head": "%s",
					"base": "main",
					"draft": true
				}
			}`, githubOwner, githubRepo, prTitle, branchName)

			proxyReq, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%s/mcp-proxy/github/create_pull_request", kc8GWPort), strings.NewReader(prBody))
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

			respPR, err := githubAPI("GET", fmt.Sprintf("/repos/%s/%s/pulls/%s", githubOwner, githubRepo, prNum), nil, pat)
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
				"aip-agent-1",
				"AIP_E2E_GITHUB_PAT_AGENT1",
				"aip-agent-1",
				"agent-1-secret",
				"[Phase 8b] E2E test PR agent-1",
			)
		})

		It("aip-agent-2 Keycloak token → gateway admits, MCP call uses agent-2 PAT", func() {
			testPerAgentCredentialBrokering(
				"aip-agent-2",
				"AIP_E2E_GITHUB_PAT_AGENT2",
				"aip-agent-2",
				"agent-2-secret",
				"[Phase 8b] E2E test PR agent-2",
			)
		})

		It("unregistered agent in warn mode → admitted with annotation", func() {
			kcSetupClient(kcPort, kcRealm, "completely-unknown-agent", "unknown-secret")
			token := kcFetchToken(kcPort, kcRealm, "completely-unknown-agent", "unknown-secret")

			resp, err := gwPostWithToken(kc8GWPort, "/agent-requests", `{
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
		})

		It("--unregistered-agent-policy=strict rejects unknown agent", func() {
			kc8GWPortStrict := "18087"
			mcpRegistry := `[{"name":"github","url":"http://localhost:18086","tools":[{"name":"create_pull_request","read_only":false}]}]`
			agentSubjects := strings.Join([]string{
				kcRegisteredAgentID,
				"aip-agent-1",
				"aip-agent-2",
				kcWrongSubjectID,
				"completely-unknown-agent",
			}, ",")
			projDir, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			binPath := projDir + "/bin/gateway"

			gwProcStrict := exec.Command(binPath,
				"--addr=:"+kc8GWPortStrict,
				"--oidc-issuer-url="+kcIssuer,
				"--oidc-audience=aip-gateway",
				"--oidc-identity-claim=azp",
				"--agent-subjects="+agentSubjects,
				"--unregistered-agent-policy=strict",
			)
			gwProcStrict.Dir = projDir
			gwProcStrict.Env = append(os.Environ(), "MCP_REGISTRY="+mcpRegistry)
			gwProcStrict.Stdout = GinkgoWriter
			gwProcStrict.Stderr = GinkgoWriter
			Expect(gwProcStrict.Start()).To(Succeed())

			defer func() {
				if gwProcStrict != nil && gwProcStrict.Process != nil {
					_ = gwProcStrict.Process.Kill()
					_, _ = gwProcStrict.Process.Wait()
				}
			}()

			Eventually(func() int {
				resp, err := http.Get("http://localhost:" + kc8GWPortStrict + "/healthz") //nolint:noctx
				if err != nil {
					return 0
				}
				defer resp.Body.Close() //nolint:errcheck
				return resp.StatusCode
			}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

			token := kcFetchToken(kcPort, kcRealm, "completely-unknown-agent", "unknown-secret")
			resp, err := gwPostWithToken(kc8GWPortStrict, "/agent-requests", `{
				"agentIdentity": "completely-unknown-agent",
				"action":        "test-action-strict",
				"targetURI":     "k8s://prod/default/deployment/strict-app",
				"reason":        "strict policy e2e"
			}`, token)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusForbidden))

			b, _ := io.ReadAll(resp.Body)
			Expect(string(b)).To(ContainSubstring("AGENT_NOT_REGISTERED"))
		})
	})
})

// kcSetupRegistrationClients adds the aip-registered-agent and aip-wrong-subject
// Keycloak clients used by the registration policy and credential brokering tests.
// Each gets an audience mapper for "aip-gateway" so the gateway accepts their tokens.
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

// kcFakeMCPUpstream is a minimal in-process MCP server used to verify credential
// brokering. It handles the three MCP protocol methods the gateway calls during a
// tools/call request and captures the Authorization header on the tools/call leg.
//
// Keying by method is sufficient because the gateway calls these sequentially:
// initialize (ensureSession) → tools/list (fetchSchemas) → tools/call (forwardToolCall).
type kcFakeMCPUpstream struct {
	server       *httptest.Server
	capturedAuth atomic.Value // string: Authorization header value on last tools/call
}

func newKCFakeMCPUpstream() *kcFakeMCPUpstream {
	f := &kcFakeMCPUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		method, _ := req["method"].(string)
		id := req["id"]

		w.Header().Set("Content-Type", "text/event-stream")

		var result map[string]any
		switch method {
		case "initialize":
			// Return a session ID so initUpstreamSession succeeds.
			w.Header().Set("Mcp-Session-Id", "kc-fake-sess-1")
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"serverInfo":      map[string]any{"name": kcFakeMCPServerName},
				"capabilities":    map[string]any{},
			}
		case "tools/list":
			result = map[string]any{
				"tools": []map[string]any{
					{"name": "echo", "inputSchema": map[string]any{"type": "object"}},
				},
			}
		case "tools/call":
			// Capture the Authorization header before responding. This is the
			// assertion point for the credential brokering test.
			f.capturedAuth.Store(r.Header.Get("Authorization"))
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ok"}},
			}
		default:
			result = map[string]any{}
		}

		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		fmt.Fprintf(w, "data: %s\n\n", data) //nolint:errcheck
	})
	f.server = httptest.NewServer(mux)
	return f
}

func (f *kcFakeMCPUpstream) close()      { f.server.Close() }
func (f *kcFakeMCPUpstream) url() string { return f.server.URL }

// lastAuth returns the Authorization header captured on the most recent tools/call
// request to this server. Returns "" if no tools/call has been received yet.
func (f *kcFakeMCPUpstream) lastAuth() string {
	v := f.capturedAuth.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

// kcSetup creates the realm, clients, and protocol mappers.
// Each step is idempotent: 409 Conflict means already exists and is treated as success.
func kcSetup(port, realm string) {
	adminToken := kcAdminToken(port)

	kcDo("POST", "https://127.0.0.1:"+port+"/admin/realms", adminToken,
		map[string]interface{}{"realm": realm, "enabled": true})

	for _, c := range []struct{ id, secret string }{
		{"aip-agent-1", "agent-1-secret"},
		{"aip-reviewer-1", "reviewer-1-secret"},
	} {
		internalID := kcCreateClient(port, adminToken, realm, c.id, c.secret)
		// Only the audience mapper is needed. The gateway reads azp (authorized
		// party), which Keycloak sets to the client_id automatically — no
		// per-client sub mapper required.
		kcAddMapper(port, adminToken, realm, internalID, map[string]interface{}{
			"name":           "audience-aip-gateway",
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

func kcAdminToken(port string) string {
	resp, err := http.PostForm( //nolint:noctx
		"https://127.0.0.1:"+port+"/realms/master/protocol/openid-connect/token",
		url.Values{
			"client_id":  {"admin-cli"},
			"username":   {"admin"},
			"password":   {"admin"},
			"grant_type": {"password"},
		})
	Expect(err).NotTo(HaveOccurred(), "get admin token")
	defer resp.Body.Close() //nolint:errcheck
	var result map[string]interface{}
	Expect(json.NewDecoder(resp.Body).Decode(&result)).To(Succeed())
	token, ok := result["access_token"].(string)
	Expect(ok).To(BeTrue(), "missing access_token in admin response")
	return token
}

func kcCreateClient(port, adminToken, realm, clientID, secret string) string {
	kcDo("POST",
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients", port, realm),
		adminToken, map[string]interface{}{
			"clientId":                  clientID,
			"enabled":                   true,
			"publicClient":              false,
			"serviceAccountsEnabled":    true,
			"standardFlowEnabled":       false,
			"directAccessGrantsEnabled": false,
			"clientAuthenticatorType":   "client-secret",
			"secret":                    secret,
		})

	// Fetch internal ID (needed for mapper endpoints)
	req, _ := http.NewRequest("GET", //nolint:noctx
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients?clientId=%s", port, realm, clientID), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close() //nolint:errcheck
	var clients []map[string]interface{}
	Expect(json.NewDecoder(resp.Body).Decode(&clients)).To(Succeed())
	Expect(clients).NotTo(BeEmpty(), "client %s not found after creation", clientID)
	return clients[0]["id"].(string)
}

func kcCreatePublicClient(port, adminToken, realm, clientID string) string {
	kcDo("POST",
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients", port, realm),
		adminToken, map[string]interface{}{
			"clientId":                  clientID,
			"enabled":                   true,
			"publicClient":              true,
			"standardFlowEnabled":       false,
			"directAccessGrantsEnabled": false,
			"clientAuthenticatorType":   "public",
		})

	req, _ := http.NewRequest("GET", //nolint:noctx
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients?clientId=%s", port, realm, clientID), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close() //nolint:errcheck
	var clients []map[string]interface{}
	Expect(json.NewDecoder(resp.Body).Decode(&clients)).To(Succeed())
	Expect(clients).NotTo(BeEmpty(), "client %s not found after creation", clientID)
	return clients[0]["id"].(string)
}

func kcAddMapper(port, adminToken, realm, clientInternalID string, mapper map[string]interface{}) {
	// Ignore 409: mapper with this name already exists from a previous run.
	kcDo("POST",
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients/%s/protocol-mappers/models",
			port, realm, clientInternalID),
		adminToken, mapper)
}

// kcDo executes an authenticated JSON request against the Keycloak admin API.
// 201 Created, 204 No Content, and 409 Conflict are all treated as success.
func kcDo(method, rawURL, token string, body interface{}) {
	b, err := json.Marshal(body)
	Expect(err).NotTo(HaveOccurred())
	req, err := http.NewRequest(method, rawURL, strings.NewReader(string(b))) //nolint:noctx
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close() //nolint:errcheck
	Expect(resp.StatusCode).To(BeElementOf(
		http.StatusCreated, http.StatusNoContent, http.StatusConflict),
		"unexpected status for %s %s", method, rawURL)
}

// kcFetchToken obtains an access_token from Keycloak using the client_credentials grant.
func kcFetchToken(port, realm, clientID, secret string) string {
	resp, err := http.PostForm( //nolint:noctx
		fmt.Sprintf("https://127.0.0.1:%s/realms/%s/protocol/openid-connect/token", port, realm),
		url.Values{
			"grant_type":    {"client_credentials"},
			"client_id":     {clientID},
			"client_secret": {secret},
			"scope":         {"openid"},
		})
	Expect(err).NotTo(HaveOccurred(), "fetch token for %s", clientID)
	defer resp.Body.Close() //nolint:errcheck
	var result map[string]interface{}
	Expect(json.NewDecoder(resp.Body).Decode(&result)).To(Succeed())
	token, ok := result["access_token"].(string)
	Expect(ok).To(BeTrue(), "missing access_token in Keycloak response for %s", clientID)
	return token
}

func kcSetupClient(port, realm, clientID, secret string) {
	adminToken := kcAdminToken(port)
	internalID := kcCreateClient(port, adminToken, realm, clientID, secret)
	kcAddMapper(port, adminToken, realm, internalID, map[string]interface{}{
		"name":           "audience-aip-gateway-" + clientID,
		"protocol":       "openid-connect",
		"protocolMapper": "oidc-audience-mapper",
		"config": map[string]string{
			"included.custom.audience": "aip-gateway",
			"id.token.claim":           "true",
			"access.token.claim":       "true",
		},
	})
}

func kcDeleteClient(port, realm, clientID string) {
	adminToken := kcAdminToken(port)
	req, _ := http.NewRequest("GET", //nolint:noctx
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients?clientId=%s", port, realm, clientID), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close() //nolint:errcheck
	var clients []map[string]interface{}
	if json.NewDecoder(resp.Body).Decode(&clients) != nil || len(clients) == 0 {
		return
	}
	internalID := clients[0]["id"].(string)

	reqDel, _ := http.NewRequest("DELETE", //nolint:noctx
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients/%s", port, realm, internalID), nil)
	reqDel.Header.Set("Authorization", "Bearer "+adminToken)
	respDel, err := http.DefaultClient.Do(reqDel)
	if err == nil {
		respDel.Body.Close()
	}
}

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

func getGitHubUser(pat string) (string, error) {
	resp, err := githubAPI("GET", "/user", nil, pat)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GitHub /user returned %d: %s", resp.StatusCode, string(b))
	}
	var res struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	return res.Login, nil
}

func createGitHubBranch(pat, branchName string) {
	resp, err := githubAPI("GET", fmt.Sprintf("/repos/%s/%s/git/refs/heads/main", githubOwner, githubRepo), nil, pat)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()
	Expect(resp.StatusCode).To(Equal(http.StatusOK))

	var refInfo struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	Expect(json.NewDecoder(resp.Body).Decode(&refInfo)).To(Succeed())
	sha := refInfo.Object.SHA

	bodyJSON := fmt.Sprintf(`{"ref":"refs/heads/%s","sha":"%s"}`, branchName, sha)
	resp2, err := githubAPI("POST", fmt.Sprintf("/repos/%s/%s/git/refs", githubOwner, githubRepo), []byte(bodyJSON), pat)
	Expect(err).NotTo(HaveOccurred())
	defer resp2.Body.Close()
	Expect(resp2.StatusCode).To(BeElementOf(http.StatusCreated, http.StatusUnprocessableEntity))
}

func deleteGitHubBranch(pat, branchName string) {
	resp, err := githubAPI("DELETE", fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", githubOwner, githubRepo, branchName), nil, pat)
	if err == nil {
		resp.Body.Close()
	}
}

func createSecretInNamespace(name, ns, token string) {
	secretJSON := fmt.Sprintf(`{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   {"name": %q, "namespace": %q},
		"stringData": {"token": %q}
	}`, name, ns, token)
	applySecret := exec.Command("kubectl", "apply", "-f", "-")
	applySecret.Stdin = strings.NewReader(secretJSON)
	out, err := applySecret.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "create Secret %s/%s: %s", ns, name, string(out))
}

func applyAgentRegistration(agentIdentity, secretName string) {
	regJSON := fmt.Sprintf(`{
		"apiVersion": "governance.aip.io/v1alpha1",
		"kind":       "AgentRegistration",
		"metadata":   {"name": %q, "namespace": "default"},
		"spec": {
			"agentIdentity": %q,
			"oidc": {
				"issuer":          %q,
				"subjectClaim":    "azp",
				"allowedSubjects": [%q]
			},
			"externalIdentities": [{
				"service": "github",
				"type":    "StaticSecret",
				"staticSecret": {
					"name":      %q,
					"namespace": "aip-k8s-system",
					"key":       "token"
				}
			}]
		}
	}`, "reg-"+agentIdentity, agentIdentity, kcIssuer, agentIdentity, secretName)
	applyReg := exec.Command("kubectl", "apply", "-f", "-")
	applyReg.Stdin = strings.NewReader(regJSON)
	out, err := applyReg.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "apply AgentRegistration for %s: %s", agentIdentity, string(out))
}

func deployGitHubMCPServer() {
	projDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	// Create aip-github-token secret in aip-k8s-system namespace.
	// This secret provides a default/fallback GitHub PAT for the MCP server
	// when per-agent credential brokering is not configured. During Phase 8b
	// tests, per-agent credential brokering (via AgentRegistration and
	// github-pat-agent1 / github-pat-agent2 secrets) is the primary path;
	// this shared secret is expected to be unused in those cases.
	secretJSON := fmt.Sprintf(`{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   {"name": "aip-github-token", "namespace": "aip-k8s-system"},
		"stringData": {"token": %q}
	}`, os.Getenv("AIP_E2E_GITHUB_PAT_AGENT1"))
	applySecret := exec.Command("kubectl", "apply", "-f", "-")
	applySecret.Stdin = strings.NewReader(secretJSON)
	out, err := applySecret.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "create aip-github-token: %s", string(out))

	// Apply github-mcp-server manifests
	applyCmd := exec.Command("kubectl", "apply", "-f", filepath.Join(projDir, "config", "mcp"))
	out, err = applyCmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "apply github-mcp-server: %s", string(out))

	// Wait for the deployment to be ready
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "deployment", "github-mcp-server", "-n", "aip-k8s-system",
			"-o", "jsonpath={.status.conditions[?(@.type=='Available')].status}")
		status, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(status)).To(Equal("True"))
	}, 3*time.Minute, 3*time.Second).Should(Succeed())

	// Wait for endpoints
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "endpoints", "github-mcp", "-n", "aip-k8s-system",
			"-o", "jsonpath={.subsets[0].addresses[0].ip}")
		ip, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(ip)).NotTo(BeEmpty())
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
}

// deployK8sMCPServer deploys the K8s MCP server (follows deployGitHubMCPServer exactly).
func deployK8sMCPServer() {
	projDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	// Apply k8s-mcp-server manifests
	applyCmd := exec.Command("kubectl", "apply", "-f", filepath.Join(projDir, "config", "mcp", "k8s-mcp-server.yaml"))
	out, err := applyCmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "apply k8s-mcp-server: %s", string(out))

	// Wait for the deployment to be ready
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "deployment", "k8s-mcp-server", "-n", "aip-k8s-system",
			"-o", "jsonpath={.status.conditions[?(@.type=='Available')].status}")
		status, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(status)).To(Equal("True"))
	}, 3*time.Minute, 3*time.Second).Should(Succeed())

	// Wait for endpoints
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "endpoints", "k8s-mcp", "-n", "aip-k8s-system",
			"-o", "jsonpath={.subsets[0].addresses[0].ip}")
		ip, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(ip)).NotTo(BeEmpty())
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
}

// kcEnableTokenExchange enables RFC 8693 token exchange in Keycloak for the given realm
// and creates a "kubernetes" client that agents can exchange into.
func kcEnableTokenExchange(port, realm string) {
	adminToken := kcAdminToken(port)

	kcRequest := func(method, path string, body interface{}) []byte {
		var bodyReader io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			Expect(err).NotTo(HaveOccurred())
			bodyReader = strings.NewReader(string(b))
		}
		url := fmt.Sprintf("https://127.0.0.1:%s%s", port, path)
		req, err := http.NewRequest(method, url, bodyReader)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(BeElementOf(http.StatusOK, http.StatusCreated, http.StatusNoContent, http.StatusConflict),
			"unexpected status %d for %s %s", resp.StatusCode, method, url)

		out, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		return out
	}

	// 1. Update realm - skipped as it is not needed and Keycloak 26 rejects invalid realm fields.

	// 2. Create client scope
	scopeBody := map[string]interface{}{
		"name":     "kubernetes",
		"protocol": "openid-connect",
		"protocolMappers": []map[string]interface{}{
			{
				"name":           "audience-mapper",
				"protocol":       "openid-connect",
				"protocolMapper": "oidc-audience-mapper",
				"config": map[string]string{
					"included.custom.audience": "kubernetes",
					"id.token.claim":           "true",
					"access.token.claim":       "true",
				},
			},
		},
	}
	kcRequest("POST", "/admin/realms/"+realm+"/client-scopes", scopeBody)

	// 3. Create target client "kubernetes"
	kubernetesId := kcCreateClient(port, adminToken, realm, "kubernetes", "kubernetes-client-secret")
	kcSetupAudienceMapper(port, realm, kubernetesId, "kubernetes")

	// Create requesting client "aip-gateway"
	gatewayId := kcCreateClient(port, adminToken, realm, "aip-gateway", "gateway-secret")

	// Get realm-management client internal ID
	var realmManagementClients []map[string]interface{}
	rmBytes := kcRequest("GET", "/admin/realms/"+realm+"/clients?clientId=realm-management", nil)
	Expect(json.Unmarshal(rmBytes, &realmManagementClients)).To(Succeed())
	Expect(realmManagementClients).NotTo(BeEmpty())
	realmManagementInternalID := realmManagementClients[0]["id"].(string)

	// Get aip-agent-1 client internal ID
	var clients []map[string]interface{}
	clientsBytes := kcRequest("GET", "/admin/realms/"+realm+"/clients?clientId=aip-agent-1", nil)
	Expect(json.Unmarshal(clientsBytes, &clients)).To(Succeed())
	Expect(clients).NotTo(BeEmpty())
	aipAgent1InternalID := clients[0]["id"].(string)

	// 4. Enable permissions
	kcRequest("PUT", fmt.Sprintf("/admin/realms/%s/clients/%s/management/permissions", realm, aipAgent1InternalID), map[string]interface{}{
		"enabled": true,
	})
	kcRequest("PUT", fmt.Sprintf("/admin/realms/%s/clients/%s/management/permissions", realm, kubernetesId), map[string]interface{}{
		"enabled": true,
	})

	// 5. Create client policy "aip-gateway-policy"
	var policyID string
	var existingPolicies []map[string]interface{}
	policiesBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/policy/client?name=%s", realm, realmManagementInternalID, "aip-gateway-policy"), nil)
	if json.Unmarshal(policiesBytes, &existingPolicies) == nil && len(existingPolicies) > 0 {
		policyID = existingPolicies[0]["id"].(string)
	} else {
		policyBody := map[string]interface{}{
			"name":    "aip-gateway-policy",
			"type":    "client",
			"logic":   "POSITIVE",
			"clients": []string{gatewayId},
		}
		kcRequest("POST", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/policy/client", realm, realmManagementInternalID), policyBody)

		var policies []map[string]interface{}
		policiesBytes = kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/policy/client?name=%s", realm, realmManagementInternalID, "aip-gateway-policy"), nil)
		Expect(json.Unmarshal(policiesBytes, &policies)).To(Succeed())
		Expect(policies).NotTo(BeEmpty())
		policyID = policies[0]["id"].(string)
	}

	// 6. Get the token-exchange permission ID for kubernetes (target client)
	var permsMap map[string]interface{}
	Eventually(func(g Gomega) {
		var permObj map[string]interface{}
		permBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/management/permissions", realm, kubernetesId), nil)
		g.Expect(json.Unmarshal(permBytes, &permObj)).To(Succeed())
		var ok bool
		permsMap, ok = permObj["permissions"].(map[string]interface{})
		if !ok {
			permsMap, ok = permObj["scopePermissions"].(map[string]interface{})
		}
		g.Expect(ok).To(BeTrue(), "missing permissions or scopePermissions map")
	}, 10*time.Second, time.Second).Should(Succeed())
	tokenExchangePermID, ok := permsMap["token-exchange"].(string)
	Expect(ok).To(BeTrue(), "missing token-exchange permission ID for kubernetes")

	// Update the token-exchange permission of kubernetes
	var permissionDetail map[string]interface{}
	detailBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/permission/scope/%s", realm, realmManagementInternalID, tokenExchangePermID), nil)
	Expect(json.Unmarshal(detailBytes, &permissionDetail)).To(Succeed())
	permissionDetail["policies"] = []string{policyID}
	kcRequest("PUT", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/permission/scope/%s", realm, realmManagementInternalID, tokenExchangePermID), permissionDetail)

	// 7. Get the token-exchange permission ID for aip-agent-1 (source client)
	var agentPermsMap map[string]interface{}
	Eventually(func(g Gomega) {
		var permObj map[string]interface{}
		permBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/management/permissions", realm, aipAgent1InternalID), nil)
		g.Expect(json.Unmarshal(permBytes, &permObj)).To(Succeed())
		var ok bool
		agentPermsMap, ok = permObj["permissions"].(map[string]interface{})
		if !ok {
			agentPermsMap, ok = permObj["scopePermissions"].(map[string]interface{})
		}
		g.Expect(ok).To(BeTrue(), "missing permissions or scopePermissions map")
	}, 10*time.Second, time.Second).Should(Succeed())
	agentTokenExchangePermID, ok := agentPermsMap["token-exchange"].(string)
	Expect(ok).To(BeTrue(), "missing token-exchange permission ID for aip-agent-1")

	// Update the token-exchange permission of aip-agent-1
	var agentPermissionDetail map[string]interface{}
	agentDetailBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/permission/scope/%s", realm, realmManagementInternalID, agentTokenExchangePermID), nil)
	Expect(json.Unmarshal(agentDetailBytes, &agentPermissionDetail)).To(Succeed())
	agentPermissionDetail["policies"] = []string{policyID}
	kcRequest("PUT", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/permission/scope/%s", realm, realmManagementInternalID, agentTokenExchangePermID), agentPermissionDetail)
}

// kcRenameServiceAccountUser renames the Keycloak service account user for a client so
// that preferred_username in issued tokens equals desiredUsername. This is necessary
// because Keycloak auto-creates service account users as "service-account-{clientId}",
// but the K8s OIDC integration is configured with oidc-username-claim=preferred_username,
// so the username in K8s audit logs reflects the Keycloak username, not the sub UUID.
// Renaming the service account user is the correct way to control the agent's K8s identity.
func kcRenameServiceAccountUser(port, adminToken, realm, clientInternalID, desiredUsername string) {
	// Fetch the service account user for the client
	req, _ := http.NewRequest("GET", //nolint:noctx
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients/%s/service-account-user", port, realm, clientInternalID), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred(), "fetch service account user for client %s", clientInternalID)
	defer resp.Body.Close() //nolint:errcheck
	Expect(resp.StatusCode).To(Equal(http.StatusOK), "service account user fetch returned %d", resp.StatusCode)

	var user map[string]interface{}
	Expect(json.NewDecoder(resp.Body).Decode(&user)).To(Succeed())
	userID, ok := user["id"].(string)
	Expect(ok).To(BeTrue(), "service account user missing id field")

	// Update the username (and email to avoid uniqueness conflicts)
	user["username"] = desiredUsername
	user["email"] = desiredUsername + "@aip.local"
	body, _ := json.Marshal(user)

	putReq, _ := http.NewRequest("PUT", //nolint:noctx
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/users/%s", port, realm, userID),
		strings.NewReader(string(body)))
	putReq.Header.Set("Content-Type", "application/json")
	putReq.Header.Set("Authorization", "Bearer "+adminToken)
	resp2, err := http.DefaultClient.Do(putReq)
	Expect(err).NotTo(HaveOccurred(), "rename service account user %s → %s", userID, desiredUsername)
	defer resp2.Body.Close() //nolint:errcheck
	Expect(resp2.StatusCode).To(BeElementOf(http.StatusOK, http.StatusNoContent),
		"unexpected status %d renaming service account user for client %s", resp2.StatusCode, clientInternalID)
}

// readKindAuditLog returns the raw audit log NDJSON from the Kind control-plane node.
func readKindAuditLog(kindNodeName string) []map[string]interface{} {
	cmd := exec.Command("docker", "exec", kindNodeName, "cat", "/var/log/kubernetes/audit/audit.log")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil
	}

	var entries []map[string]interface{}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err == nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

func kcSetupAudienceMapper(port, realm, clientInternalID, audience string) {
	adminToken := kcAdminToken(port)
	kcAddMapper(port, adminToken, realm, clientInternalID, map[string]interface{}{
		"name":           "audience-" + audience + "-" + clientInternalID,
		"protocol":       "openid-connect",
		"protocolMapper": "oidc-audience-mapper",
		"config": map[string]string{
			"included.custom.audience": audience,
			"id.token.claim":           "true",
			"access.token.claim":       "true",
		},
	})
}

func gwDeleteWithToken(port, path, token string) (*http.Response, error) {
	url := "http://localhost:" + port + path
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 90 * time.Second}
	return client.Do(req)
}

var _ = Describe("Phase 9: End-to-End Keycloak Identity Flow and Auditing", Ordered, func() {
	Context("9a: real cluster e2e tests", Ordered, func() {
		var gwProc *exec.Cmd
		var pfProc *exec.Cmd
		var gwPFProc *exec.Cmd
		const (
			kc9GWPort = "18088"
		)

		BeforeAll(func() {
			if os.Getenv("OIDC_KIND_CLUSTER") != "true" {
				Skip("OIDC_KIND_CLUSTER=true is required for Phase 9 real cluster e2e tests")
			}

			projDir, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())

			// Disable TLS verification for Keycloak HTTPS calls
			http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

			// Restart kube-apiserver to ensure it reloads the current OIDC CA certificate
			clusterName := os.Getenv("KIND_CLUSTER_NAME")
			if clusterName == "" {
				clusterName = "aip-k8s-test-e2e"
			}
			kindNodeName := clusterName + "-control-plane"
			_ = exec.Command("docker", "exec", kindNodeName, "mv", "/etc/kubernetes/manifests/kube-apiserver.yaml", "/tmp/").Run()
			time.Sleep(3 * time.Second)
			_ = exec.Command("docker", "exec", kindNodeName, "mv", "/tmp/kube-apiserver.yaml", "/etc/kubernetes/manifests/").Run()
			Eventually(func() error {
				return exec.Command("kubectl", "get", "nodes").Run()
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			// Clean up any stale resources from previous runs
			_ = exec.Command("kubectl", "delete", "agentregistration", "reg-aip-agent-1", "-n", "default", "--ignore-not-found").Run()
			_ = exec.Command("kubectl", "delete", "safetypolicy", "kc-require-human", "-n", "default", "--ignore-not-found").Run()
			_ = exec.Command("kubectl", "delete", "agentrequest", "--all", "-n", "default", "--ignore-not-found").Run()

			// 2. Deploy Keycloak (idempotent)
			By("deploying Keycloak dev instance")
			ensureKeycloakTLSSecret(projDir)
			applyCmd := exec.Command("kubectl", "apply", "-f",
				projDir+"/test/fixtures/keycloak-dev.yaml")
			out, err := applyCmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "kubectl apply keycloak: %s", string(out))

			// Wait for Keycloak pod
			By("waiting for Keycloak pod to be ready")
			Eventually(func(g Gomega) {
				readyCmd := exec.Command("kubectl", "get", "pods",
					"-l", "app=keycloak", "-n", "keycloak",
					"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
				status, err := utils.Run(readyCmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("True"), "Keycloak pod not yet ready")
			}, 3*time.Minute, 3*time.Second).Should(Succeed())

			// 3. Port-forward Keycloak
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
				defer resp.Body.Close()
				return resp.StatusCode
			}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

			// Setup in-node loopback redirect for API server
			clusterName = os.Getenv("KIND_CLUSTER_NAME")
			if clusterName == "" {
				clusterName = "aip-k8s-test-e2e"
			}
			kindNodeName = clusterName + "-control-plane"

			By("installing socat inside Kind control plane node")
			installSocat := exec.Command("docker", "exec", kindNodeName, "sh", "-c", "apt-get update && apt-get install -y socat")
			out, err = installSocat.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "install socat: %s", string(out))

			By("getting Keycloak service ClusterIP")
			getIPCmd := exec.Command("kubectl", "get", "svc", "keycloak", "-n", "keycloak", "-o", "jsonpath={.spec.clusterIP}")
			kcClusterIPBytes, err := getIPCmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "get keycloak cluster IP: %s", string(kcClusterIPBytes))
			kcClusterIP := strings.TrimSpace(string(kcClusterIPBytes))

			By("starting socat port redirection in control plane node container")
			_ = exec.Command("docker", "exec", kindNodeName, "pkill", "-f", "socat").Run()
			socatCmd := exec.Command("docker", "exec", "-d", kindNodeName, "socat",
				"TCP-LISTEN:18091,fork,reuseaddr", "TCP:"+kcClusterIP+":8443")
			Expect(socatCmd.Run()).To(Succeed())

			// 4. Configure realm: creates realm + aip-agent-1 + reviewer clients
			By("configuring Keycloak realm and clients")
			kcSetup(kcPort, kcRealm)

			// Get aip-agent-1 client internal ID
			adminToken := kcAdminToken(kcPort)
			var clients []map[string]interface{}
			req, _ := http.NewRequest("GET", fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients?clientId=aip-agent-1", kcPort, kcRealm), nil)
			req.Header.Set("Authorization", "Bearer "+adminToken)
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(json.NewDecoder(resp.Body).Decode(&clients)).To(Succeed())
			Expect(clients).NotTo(BeEmpty())
			aipAgent1InternalID := clients[0]["id"].(string)

			// Rename the Keycloak service account user so preferred_username = "aip-agent-1"
			// in all tokens (both direct client_credentials tokens and RFC 8693 exchanged tokens).
			// K8s is configured with oidc-username-claim=preferred_username and
			// oidc-username-prefix="-", so the agent will appear as "aip-agent-1" in K8s
			// RBAC and audit logs.
			//
			// Note: overriding the "sub" claim via a hardcoded mapper does NOT work in
			// Keycloak 26+ which protects the sub claim from being overridden by mappers.
			By("renaming Keycloak service account user for aip-agent-1 to match K8s RBAC subject")
			kcRenameServiceAccountUser(kcPort, adminToken, kcRealm, aipAgent1InternalID, "aip-agent-1")

			// 5. Enable token exchange on Keycloak realm
			By("enabling token exchange on realm")
			kcEnableTokenExchange(kcPort, kcRealm)

			// 6. Setup audience mapper for "kubernetes" audience on aip-agent-1 client
			By("setting up audience mapper for kubernetes on aip-agent-1")
			kcSetupAudienceMapper(kcPort, kcRealm, aipAgent1InternalID, "kubernetes")

			// 7. Deploy K8s MCP server
			By("deploying K8s MCP server")
			deployK8sMCPServer()

			// 8. Create ClusterRole + ClusterRoleBinding for aip-agent-1 identity
			By("creating ClusterRole and ClusterRoleBinding for agent identity")
			rbacYAML := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: aip-agent-1-k8s-mcp
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aip-agent-1-k8s-mcp
subjects:
- kind: User
  name: aip-agent-1
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: aip-agent-1-k8s-mcp
  apiGroup: rbac.authorization.k8s.io
`
			applyRBAC := exec.Command("kubectl", "apply", "-f", "-")
			applyRBAC.Stdin = strings.NewReader(rbacYAML)
			out, err = applyRBAC.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "apply RBAC: %s", string(out))

			// Apply the MCPServer CR to the cluster
			applyCR := exec.Command("kubectl", "apply", "-f", projDir+"/config/mcp/k8s-mcp-server-cr.yaml")
			out, err = applyCR.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "apply MCPServer CR: %s", string(out))

			// Apply SafetyPolicy requiring human approval so requests stay Pending for approval testing
			By("creating SafetyPolicy requiring human approval")
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

			// 9. Deploy gateway to Kubernetes
			By("building gateway Docker image")
			kindBinary := os.Getenv("KIND")
			if kindBinary == "" {
				kindBinary = "kind"
			}
			buildImg := exec.Command("docker", "build", "-f", "Dockerfile.gateway", "-t", "example.com/aip-gateway:v0.0.1", ".")
			buildImg.Dir = projDir
			out, err = buildImg.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "build gateway image: %s", string(out))

			By("loading gateway image into Kind cluster")
			loadImg := exec.Command(kindBinary, "load", "docker-image", "example.com/aip-gateway:v0.0.1", "--name", clusterName)
			out, err = loadImg.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "load gateway image: %s", string(out))

			// Get aip-gateway client internal ID
			adminToken = kcAdminToken(kcPort)
			var gwClients []map[string]interface{}
			gwReq, _ := http.NewRequest("GET", fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients?clientId=aip-gateway", kcPort, kcRealm), nil)
			gwReq.Header.Set("Authorization", "Bearer "+adminToken)
			gwResp, err := http.DefaultClient.Do(gwReq)
			Expect(err).NotTo(HaveOccurred())
			Expect(json.NewDecoder(gwResp.Body).Decode(&gwClients)).To(Succeed())
			gwResp.Body.Close()
			Expect(gwClients).NotTo(BeEmpty())
			aipGatewayInternalID := gwClients[0]["id"].(string)

			// Get actual gateway secret
			gatewaySecret := kcGetClientSecret(kcPort, adminToken, kcRealm, aipGatewayInternalID)

			By("creating gateway client credentials secret")
			gatewaySecretJSON := fmt.Sprintf(`{
				"apiVersion": "v1",
				"kind": "Secret",
				"metadata": {
					"name": "aip-gateway-secret",
					"namespace": "aip-k8s-system"
				},
				"stringData": {
					"client-secret": %q
				}
			}`, gatewaySecret)

			applyGwSecret := exec.Command("kubectl", "apply", "-f", "-")
			applyGwSecret.Stdin = strings.NewReader(gatewaySecretJSON)
			out, err = applyGwSecret.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "failed to create aip-gateway-secret: %s", string(out))

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

			By("port-forwarding gateway to localhost:" + kc9GWPort)
			_ = exec.Command("pkill", "-f", "port-forward.*aip-gateway.*"+kc9GWPort).Run()
			time.Sleep(500 * time.Millisecond)
			gwPF := exec.Command("kubectl", "port-forward",
				"svc/aip-gateway", kc9GWPort+":8080", "-n", "aip-k8s-system")
			gwPF.Stdout = GinkgoWriter
			gwPF.Stderr = GinkgoWriter
			Expect(gwPF.Start()).To(Succeed())
			gwPFProc = gwPF

			Eventually(func() int {
				resp, err := http.Get("http://localhost:" + kc9GWPort + "/healthz") //nolint:noctx
				if err != nil {
					return 0
				}
				defer resp.Body.Close()
				return resp.StatusCode
			}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

			// 10. Admin JWT → POST /agent-registrations
			By("registering agent via gateway POST /agent-registrations")
			reviewerToken := kcFetchToken(kcPort, kcRealm, "aip-reviewer-1", "reviewer-1-secret")
			regJSON := fmt.Sprintf(`{
				"metadata": {"name": "reg-aip-agent-1", "namespace": "default"},
				"spec": {
					"agentIdentity": "aip-agent-1",
					"oidc": {
						"issuer": %q,
						"allowedSubjects": ["aip-agent-1"]
					},
					"externalIdentities": [{
						"service": "k8s-mcp",
						"type": "KubernetesOIDC",
						"kubernetesOIDC": {
							"tokenExchangeURL": %q,
							"audience": "kubernetes"
						}
					}]
				}
			}`, kcIssuer, fmt.Sprintf("https://127.0.0.1:%s/realms/%s/protocol/openid-connect/token", kcPort, kcRealm))
			respReg, err := gwPostWithToken(kc9GWPort, "/agent-registrations", regJSON, reviewerToken)
			Expect(err).NotTo(HaveOccurred())
			defer respReg.Body.Close()
			Expect(respReg.StatusCode).To(Equal(http.StatusCreated))
		})

		AfterAll(func() {
			if gwProc != nil && gwProc.Process != nil {
				_ = gwProc.Process.Kill()
			}
			if pfProc != nil && pfProc.Process != nil {
				_ = pfProc.Process.Kill()
			}
			if gwPFProc != nil && gwPFProc.Process != nil {
				_ = gwPFProc.Process.Kill()
			}

			if os.Getenv("OIDC_KIND_CLUSTER") == "true" {
				reviewerToken := kcFetchToken(kcPort, kcRealm, "aip-reviewer-1", "reviewer-1-secret")
				_, _ = gwDeleteWithToken(kc9GWPort, "/agent-registrations/reg-aip-agent-1", reviewerToken)

				_ = exec.Command("kubectl", "delete", "clusterrolebinding", "aip-agent-1-k8s-mcp", "--ignore-not-found").Run()
				_ = exec.Command("kubectl", "delete", "clusterrole", "aip-agent-1-k8s-mcp", "--ignore-not-found").Run()
				_ = exec.Command("kubectl", "delete", "safetypolicy", "--all", "-n", "default", "--ignore-not-found").Run()
				_ = exec.Command("kubectl", "delete", "agentrequest", "--all", "-n", "default", "--ignore-not-found").Run()
				_ = exec.Command("kubectl", "delete", "-f", "config/mcp/k8s-mcp-server-cr.yaml", "--ignore-not-found").Run()
				_ = exec.Command("kubectl", "delete", "-f", "config/mcp/k8s-mcp-server.yaml", "--ignore-not-found").Run()
				_ = exec.Command("kubectl", "delete", "-f", "test/fixtures/gateway-dev.yaml", "--ignore-not-found").Run()
				_ = exec.Command("kubectl", "delete", "secret", "gateway-ca-cert", "-n", "aip-k8s-system", "--ignore-not-found").Run()
				_ = exec.Command("kubectl", "delete", "secret", "aip-gateway-secret", "-n", "aip-k8s-system", "--ignore-not-found").Run()
			}
		})

		It("Keycloak JWT → KubernetesOIDC exchange → K8s audit shows agent identity", func() {
			// 1. Fetch token for agent
			token := kcFetchToken(kcPort, kcRealm, "aip-agent-1", "agent-1-secret")

			// 2. Submit AgentRequest (requires OIDC exchange)
			resp, err := gwPostWithToken(kc9GWPort, "/agent-requests", `{
				"agentIdentity": "aip-agent-1",
				"targetURI":     "k8s://prod/default/configmap/test",
				"action":        "list",
				"reason":        "OIDC kind audit test"
			}`, token)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))

			var createResp struct {
				Name string `json:"name"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&createResp)).To(Succeed())
			reqName := createResp.Name
			Expect(reqName).NotTo(BeEmpty())

			// 3. Admin approves
			reviewerToken := kcFetchToken(kcPort, kcRealm, "aip-reviewer-1", "reviewer-1-secret")
			approveResp, err := gwPostWithToken(kc9GWPort, "/agent-requests/"+reqName+"/approve", `{"reason":"Approved for auditing test"}`, reviewerToken)
			Expect(err).NotTo(HaveOccurred())
			defer approveResp.Body.Close()
			Expect(approveResp.StatusCode).To(Equal(http.StatusOK))

			// 4. Agent executes MCP tool (read-only tool resources_list)
			mcpBody := `{
				"jsonrpc": "2.0", "id": 1,
				"method": "tools/call",
				"params": {
					"name": "k8s-mcp/resources_list",
					"arguments": {
						"apiVersion": "v1",
						"kind": "ConfigMap",
						"namespace": "default"
					}
				}
			}`
			// Note: since resources_list is read-only, we call it directly with the agent's OIDC token in Authorization
			mcpResp, err := gwPostWithToken(kc9GWPort, "/mcp", mcpBody, token)
			Expect(err).NotTo(HaveOccurred())
			defer mcpResp.Body.Close()
			Expect(mcpResp.StatusCode).To(Equal(http.StatusOK))

			// 5. Read audit log from Kind control-plane node
			clusterName := os.Getenv("KIND_CLUSTER_NAME")
			if clusterName == "" {
				clusterName = "aip-k8s-test-e2e"
			}
			kindNodeName := clusterName + "-control-plane"

			Eventually(func(g Gomega) {
				auditLogs := readKindAuditLog(kindNodeName)
				g.Expect(auditLogs).NotTo(BeEmpty(), "audit log should not be empty")

				foundAgent := false
				foundSA := false

				for _, entry := range auditLogs {
					// Filter by verb=list AND resource=configmaps
					verb, _ := entry["verb"].(string)
					objectRef, _ := entry["objectRef"].(map[string]interface{})
					var resource string
					if objectRef != nil {
						resource, _ = objectRef["resource"].(string)
					}

					if verb == "list" && resource == "configmaps" {
						user, _ := entry["user"].(map[string]interface{})
						if user != nil {
							username, _ := user["username"].(string)
							if username == "aip-agent-1" {
								foundAgent = true
							}
							if strings.HasPrefix(username, "system:serviceaccount:") {
								foundSA = true
							}
						}
					}
				}

				g.Expect(foundAgent).To(BeTrue(), "should find at least one list configmaps entry where username == aip-agent-1")
				g.Expect(foundSA).To(BeFalse(), "should NOT find any list configmaps entry where username starts with system:serviceaccount:")
			}, 30*time.Second, time.Second).Should(Succeed())
		})

		It("token cache: second MCP call within TTL skips re-exchange", func() {
			// Clean Keycloak event log first to get a clean count
			adminToken := kcAdminToken(kcPort)
			clearEventsReq, _ := http.NewRequest("DELETE", fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/events", kcPort, kcRealm), nil)
			clearEventsReq.Header.Set("Authorization", "Bearer "+adminToken)
			respDel, err := http.DefaultClient.Do(clearEventsReq)
			Expect(err).NotTo(HaveOccurred())
			respDel.Body.Close()

			token := kcFetchToken(kcPort, kcRealm, "aip-agent-1", "agent-1-secret")

			// First MCP tool call
			mcpBody := `{
				"jsonrpc": "2.0", "id": 1,
				"method": "tools/call",
				"params": {
					"name": "k8s-mcp/resources_list",
					"arguments": {
						"apiVersion": "v1",
						"kind": "ConfigMap",
						"namespace": "default"
					}
				}
			}`
			mcpResp1, err := gwPostWithToken(kc9GWPort, "/mcp", mcpBody, token)
			Expect(err).NotTo(HaveOccurred())
			mcpResp1.Body.Close()
			Expect(mcpResp1.StatusCode).To(Equal(http.StatusOK))

			// Second MCP tool call (same agent, same token)
			mcpResp2, err := gwPostWithToken(kc9GWPort, "/mcp", mcpBody, token)
			Expect(err).NotTo(HaveOccurred())
			mcpResp2.Body.Close()
			Expect(mcpResp2.StatusCode).To(Equal(http.StatusOK))

			// Count Keycloak token exchange calls by scraping Keycloak event log
			getEventsReq, _ := http.NewRequest("GET", fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/events?type=TOKEN_EXCHANGE", kcPort, kcRealm), nil)
			getEventsReq.Header.Set("Authorization", "Bearer "+adminToken)
			respEvents, err := http.DefaultClient.Do(getEventsReq)
			Expect(err).NotTo(HaveOccurred())
			defer respEvents.Body.Close()
			Expect(respEvents.StatusCode).To(Equal(http.StatusOK))

			var events []interface{}
			Expect(json.NewDecoder(respEvents.Body).Decode(&events)).To(Succeed())
			// Assert exchange endpoint was called exactly once between the two calls
			Expect(len(events)).To(Equal(1))
		})
	})

	Context("9b: provider stub tests", func() {
		It("AzureWorkloadIdentity: client_credentials + WIF grant, exchanged token forwarded to upstream", func() {
			var capturedGrantType, capturedAssertionType, capturedAssertion string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				capturedGrantType = r.Form.Get("grant_type")
				capturedAssertionType = r.Form.Get("client_assertion_type")
				capturedAssertion = r.Form.Get("client_assertion")
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"access_token":"azure-access-token","expires_in":3600}`))
			}))
			defer server.Close()

			provider := credential.NewAzureWorkloadIdentityProvider("tenant-1", "client-1", "scope-1").
				WithTokenURL(server.URL)

			ctx := context.Background()
			tok, err := provider.Token(ctx, "synthetic-agent-oidc-token")
			Expect(err).NotTo(HaveOccurred())
			Expect(tok).To(Equal("azure-access-token"))
			Expect(capturedGrantType).To(Equal("client_credentials"))
			Expect(capturedAssertionType).To(Equal("urn:ietf:params:oauth:client-assertion-type:jwt-bearer"))
			Expect(capturedAssertion).To(Equal("synthetic-agent-oidc-token"))
		})

		It("AWSWebIdentity: STS AssumeRoleWithWebIdentity called, session token forwarded", func() {
			var capturedAction, capturedToken string
			var callCount int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&callCount, 1)
				_ = r.ParseForm()
				capturedAction = r.Form.Get("Action")
				capturedToken = r.Form.Get("WebIdentityToken")
				w.Header().Set("Content-Type", "application/xml")
				w.Write([]byte(`<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleWithWebIdentityResult>
    <Credentials>
      <AccessKeyId>ASIAIOSFODNN7EXAMPLE</AccessKeyId>
      <SecretAccessKey>wJalrXUtnFEMI/K7MDENG/bPxRfiCYzEXAMPLEKEY</SecretAccessKey>
      <SessionToken>synthetic-sts-session-token</SessionToken>
      <Expiration>2030-11-09T13:00:00Z</Expiration>
    </Credentials>
  </AssumeRoleWithWebIdentityResult>
</AssumeRoleWithWebIdentityResponse>`))
			}))
			defer server.Close()

			provider := credential.NewAWSWebIdentityProvider("arn:aws:iam::123456789012:role/test-role", "session-1", "us-east-1", nil, server.URL)

			ctx := context.Background()
			tok, err := provider.Token(ctx, "synthetic-agent-oidc-token")
			Expect(err).NotTo(HaveOccurred())
			Expect(tok).To(ContainSubstring("synthetic-sts-session-token"))
			Expect(capturedAction).To(Equal("AssumeRoleWithWebIdentity"))
			Expect(capturedToken).To(Equal("synthetic-agent-oidc-token"))
			Expect(atomic.LoadInt32(&callCount)).To(Equal(int32(1)))
		})

		It("TokenCache: 10 concurrent calls to same provider deduplicate to 1 exchange", func() {
			var callCount int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt64(&callCount, 1)
				time.Sleep(50 * time.Millisecond) // simulate delay
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"access_token":"exchanged-token","expires_in":3600}`))
			}))
			defer server.Close()

			provider := credential.NewKubernetesOIDCProvider(server.URL, "kubernetes")

			ctx := context.Background()
			var wg sync.WaitGroup
			var errorsList []error
			var mu sync.Mutex
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					tok, err := provider.Token(ctx, "same-raw-token")
					if err != nil {
						mu.Lock()
						errorsList = append(errorsList, err)
						mu.Unlock()
					} else {
						Expect(tok).To(Equal("exchanged-token"))
					}
				}()
			}
			wg.Wait()
			Expect(errorsList).To(BeEmpty())
			Expect(atomic.LoadInt64(&callCount)).To(Equal(int64(1)))
		})
	})
})

func ensureKeycloakTLSSecret(projDir string) {
	_ = exec.Command("kubectl", "create", "ns", "keycloak").Run()

	certPEM, err := os.ReadFile(filepath.Join(projDir, "test/fixtures/certs/keycloak.crt"))
	Expect(err).NotTo(HaveOccurred(), "failed to read keycloak.crt cert file")
	keyPEM, err := os.ReadFile(filepath.Join(projDir, "test/fixtures/certs/keycloak.key"))
	Expect(err).NotTo(HaveOccurred(), "failed to read keycloak.key key file")

	secretJSON := fmt.Sprintf(`{
		"apiVersion": "v1",
		"kind": "Secret",
		"metadata": {
			"name": "keycloak-tls",
			"namespace": "keycloak"
		},
		"type": "kubernetes.io/tls",
		"stringData": {
			"tls.crt": %q,
			"tls.key": %q
		}
	}`, string(certPEM), string(keyPEM))

	applySecret := exec.Command("kubectl", "apply", "-f", "-")
	applySecret.Stdin = strings.NewReader(secretJSON)
	out, err := applySecret.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "failed to create keycloak-tls secret: %s", string(out))
}

func ensureGatewayCASecret(projDir string) {
	caPEM, err := os.ReadFile(filepath.Join(projDir, "test/fixtures/certs/ca.crt"))
	Expect(err).NotTo(HaveOccurred(), "failed to read ca.crt file")

	secretJSON := fmt.Sprintf(`{
		"apiVersion": "v1",
		"kind": "Secret",
		"metadata": {
			"name": "gateway-ca-cert",
			"namespace": "aip-k8s-system"
		},
		"stringData": {
			"ca.crt": %q
		}
	}`, string(caPEM))

	applySecret := exec.Command("kubectl", "apply", "-f", "-")
	applySecret.Stdin = strings.NewReader(secretJSON)
	out, err := applySecret.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "failed to create gateway-ca-cert secret: %s", string(out))
}

func kcGetClientSecret(port, adminToken, realm, clientInternalID string) string {
	url := fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients/%s/client-secret", port, realm, clientInternalID)
	req, err := http.NewRequest("GET", url, nil)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()
	var result map[string]interface{}
	Expect(json.NewDecoder(resp.Body).Decode(&result)).To(Succeed())
	secret, ok := result["value"].(string)
	Expect(ok).To(BeTrue(), "missing value in client-secret response")
	return secret
}

