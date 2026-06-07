//go:build e2e

package e2e

import (
	"bytes"
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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/test/utils"
)

const (
	kcPort    = "18091"
	kcBase    = "http://localhost:" + kcPort
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

		// 1. Deploy Keycloak (idempotent).
		By("deploying Keycloak dev instance")
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
			"svc/keycloak", kcPort+":8080", "-n", "keycloak")
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
		gwProc.Env = append(os.Environ(), "MCP_REGISTRY="+mcpRegistry)
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
			_ = exec.Command("pkill", "-f", "port-forward.*github-mcp-server.*18086").Run()
			time.Sleep(time.Second)
			mcpPfProc = exec.Command("kubectl", "port-forward",
				"svc/github-mcp-server", "18086:8080", "-n", "aip-k8s-system")
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
			time.Sleep(2 * time.Second)

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

			kcDeleteClient(kcPort, kcRealm, "aip-agent-2")
		})

		It("aip-agent-1 Keycloak token → gateway admits, MCP call uses agent-1 PAT", func() {
			pat := os.Getenv("AIP_E2E_GITHUB_PAT_AGENT1")
			expectedUser, err := getGitHubUser(pat)
			Expect(err).NotTo(HaveOccurred())

			branchName := fmt.Sprintf("phase8b-agent1-%d", time.Now().UnixNano())
			createGitHubBranch(pat, branchName)
			defer deleteGitHubBranch(pat, branchName)

			token := kcFetchToken(kcPort, kcRealm, "aip-agent-1", "agent-1-secret")

			resp, err := gwPostWithToken(kc8GWPort, "/agent-requests", fmt.Sprintf(`{
				"agentIdentity": "aip-agent-1",
				"action":        "github/create_pull_request",
				"targetURI":     "github://%s/%s",
				"reason":        "Phase 8b e2e test agent-1"
			}`, githubOwner, githubRepo), token)
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
			approveResp, err := gwPostWithToken(kc8GWPort, "/agent-requests/"+reqName+"/approve", `{"reason":"Phase 8b approval agent-1"}`, reviewerToken)
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
					"title": "[Phase 8b] E2E test PR agent-1",
					"body": "PR created during Keycloak per-agent credentials e2e test.",
					"head": "%s",
					"base": "main",
					"draft": true
				}
			}`, githubOwner, githubRepo, branchName)

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
		})

		It("aip-agent-2 Keycloak token → gateway admits, MCP call uses agent-2 PAT", func() {
			pat := os.Getenv("AIP_E2E_GITHUB_PAT_AGENT2")
			expectedUser, err := getGitHubUser(pat)
			Expect(err).NotTo(HaveOccurred())

			branchName := fmt.Sprintf("phase8b-agent2-%d", time.Now().UnixNano())
			createGitHubBranch(pat, branchName)
			defer deleteGitHubBranch(pat, branchName)

			token := kcFetchToken(kcPort, kcRealm, "aip-agent-2", "agent-2-secret")

			resp, err := gwPostWithToken(kc8GWPort, "/agent-requests", fmt.Sprintf(`{
				"agentIdentity": "aip-agent-2",
				"action":        "github/create_pull_request",
				"targetURI":     "github://%s/%s",
				"reason":        "Phase 8b e2e test agent-2"
			}`, githubOwner, githubRepo), token)
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
			approveResp, err := gwPostWithToken(kc8GWPort, "/agent-requests/"+reqName+"/approve", `{"reason":"Phase 8b approval agent-2"}`, reviewerToken)
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
					"title": "[Phase 8b] E2E test PR agent-2",
					"body": "PR created during Keycloak per-agent credentials e2e test.",
					"head": "%s",
					"base": "main",
					"draft": true
				}
			}`, githubOwner, githubRepo, branchName)

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

			var ar governancev1alpha1.AgentRequest
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: reqName, Namespace: "default"}, &ar)).To(Succeed())
			Expect(ar.Annotations).NotTo(BeNil())
			Expect(ar.Annotations["governance.aip.io/unregistered"]).To(Equal("true"))
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

	kcDo("POST", "http://localhost:"+port+"/admin/realms", adminToken,
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
		"http://localhost:"+port+"/realms/master/protocol/openid-connect/token",
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
		fmt.Sprintf("http://localhost:%s/admin/realms/%s/clients", port, realm),
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
		fmt.Sprintf("http://localhost:%s/admin/realms/%s/clients?clientId=%s", port, realm, clientID), nil)
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
		fmt.Sprintf("http://localhost:%s/admin/realms/%s/clients/%s/protocol-mappers/models",
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
		fmt.Sprintf("http://localhost:%s/realms/%s/protocol/openid-connect/token", port, realm),
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
		fmt.Sprintf("http://localhost:%s/admin/realms/%s/clients?clientId=%s", port, realm, clientID), nil)
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
		fmt.Sprintf("http://localhost:%s/admin/realms/%s/clients/%s", port, realm, internalID), nil)
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

	// Create aip-github-token secret in aip-k8s-system namespace
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
