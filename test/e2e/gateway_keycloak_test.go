//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
		gwProc = exec.Command(binPath,
			"--addr=:"+kc8GWPort,
			"--oidc-issuer-url="+kcIssuer,
			"--oidc-audience=aip-gateway",
			"--oidc-identity-claim=azp",
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
