//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ravisantoshgudimetla/aip-k8s/test/utils"
)

const (
	kcPort     = "18091"
	kcBase     = "http://localhost:" + kcPort
	kcRealm    = "aip"
	kc8GWPort  = "18085"
	kcIssuer   = kcBase + "/realms/" + kcRealm
)

var _ = Describe("Phase 8: Gateway Keycloak OIDC Integration", Ordered, func() {
	var gwProc *exec.Cmd
	var pfProc *exec.Cmd

	BeforeAll(func() {
		projDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())

		// 1. Deploy Keycloak (idempotent — already-configured is fine)
		By("deploying Keycloak dev instance")
		applyCmd := exec.Command("kubectl", "apply", "-f",
			projDir+"/test/fixtures/keycloak-dev.yaml")
		out, err := applyCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "kubectl apply keycloak: %s", string(out))

		// 2. Wait for Keycloak to be ready
		By("waiting for Keycloak pod to be ready")
		Eventually(func(g Gomega) {
			readyCmd := exec.Command("kubectl", "get", "pods",
				"-l", "app=keycloak",
				"-n", "keycloak",
				"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
			status, err := utils.Run(readyCmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"), "Keycloak pod not yet ready")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		// 3. Port-forward Keycloak; kill any stale forward first
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

		// 4. Configure realm, clients, and protocol mappers (idempotent)
		By("configuring Keycloak realm and clients")
		kcSetup(kcPort, kcRealm)

		// 5. Ensure controller is deployed (idempotent — Phase 1 may have done this already,
		// but Ginkgo can randomize Describe block order so we cannot rely on it).
		By("ensuring controller-manager is deployed")
		checkCtrlCmd := exec.Command("kubectl", "get", "deployment",
			"aip-k8s-controller-manager", "-n", "aip-k8s-system")
		if _, checkErr := utils.Run(checkCtrlCmd); checkErr != nil {
			deployCmd := exec.Command("make", "deploy",
				fmt.Sprintf("IMG=%s", managerImage))
			deployCmd.Dir = projDir
			deployOut, deployErr := deployCmd.CombinedOutput()
			Expect(deployErr).NotTo(HaveOccurred(), "deploy controller: %s", string(deployOut))
		}

		By("waiting for controller to be ready")
		Eventually(func(g Gomega) {
			readyCmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-n", "aip-k8s-system",
				"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
			status, err := utils.Run(readyCmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"), "controller pod not yet ready")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		// 6. Build and start gateway subprocess pointing at Keycloak
		By("building gateway binary")
		binPath := projDir + "/bin/gateway"
		buildCmd := exec.Command("go", "build", "-o", binPath, projDir+"/cmd/gateway")
		buildCmd.Dir = projDir
		out, err = buildCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "build gateway: %s", string(out))

		By("starting gateway with Keycloak issuer")
		gwProc = exec.Command(binPath,
			"--addr=:"+kc8GWPort,
			"--oidc-issuer-url="+kcIssuer,
			"--oidc-audience=aip-gateway",
			"--oidc-identity-claim=azp",
			"--agent-subjects=aip-agent-1",
			"--reviewer-subjects=aip-reviewer-1",
		)
		gwProc.Dir = projDir
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

		// 7. SafetyPolicy so POST /agent-requests returns 201 quickly
		By("creating SafetyPolicy requiring human approval for kc-test-action")
		gwCleanup("default")
		policyJSON := `{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind": "SafetyPolicy",
			"metadata": {"name": "kc-require-human", "namespace": "default"},
			"spec": {
				"targetSelector": {"matchActions": ["kc-test-action"]},
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
		gwCleanup("default")
	})

	It("Missing token → 401", func() {
		resp, err := gwPostWithToken(kc8GWPort, "/agent-requests", "{}", "")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("Valid agent token — POST /agent-requests → 201", func() {
		token := kcFetchToken(kcPort, kcRealm, "aip-agent-1", "agent-1-secret")
		resp, err := gwPostWithToken(kc8GWPort, "/agent-requests", `{
			"agentIdentity": "aip-agent-1",
			"action":        "kc-test-action",
			"targetURI":     "k8s://prod/default/deployment/payment-api",
			"reason":        "keycloak e2e test"
		}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusCreated))
	})

	It("Agent token on /approve → 403 reviewer role required", func() {
		token := kcFetchToken(kcPort, kcRealm, "aip-agent-1", "agent-1-secret")
		resp, err := gwPostWithToken(kc8GWPort, "/agent-requests/nonexistent/approve",
			`{"reason":"test"}`, token)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		b, _ := io.ReadAll(resp.Body)
		Expect(string(b)).To(ContainSubstring("reviewer role required"))
	})

	It("Healthz unauthenticated → 200", func() {
		resp, err := http.Get("http://localhost:" + kc8GWPort + "/healthz") //nolint:noctx
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close() //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})
})

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
			"clientId":                clientID,
			"enabled":                 true,
			"publicClient":            false,
			"serviceAccountsEnabled":  true,
			"standardFlowEnabled":     false,
			"directAccessGrantsEnabled": false,
			"clientAuthenticatorType": "client-secret",
			"secret":                  secret,
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
