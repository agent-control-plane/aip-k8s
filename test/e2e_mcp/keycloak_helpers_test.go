//go:build mcp_e2e
// +build mcp_e2e

package e2e_mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/gomega"
)

// Keycloak connectivity constants. kcPort is the local port-forward target;
// kcInClusterIssuer is what the in-cluster gateway uses as --oidc-issuer-url.
const (
	kcPort              = "18091"
	kcBase              = "https://127.0.0.1:" + kcPort
	kcRealm             = "aip"
	kcInClusterIssuer   = "https://keycloak.keycloak.svc.cluster.local:8443/realms/" + kcRealm
	kcInClusterTokenURL = kcInClusterIssuer + "/protocol/openid-connect/token"

	// kc8bGWPort is the local port-forward for the Phase 8b in-cluster gateway.
	kc8bGWPort = "18085"
	// kc9GWPort is the local port-forward for the Phase 9 in-cluster gateway.
	kc9GWPort = "18088"

	// Phase 8b registration test identities — must match gateway-dev.yaml --agent-subjects.
	kcRegisteredAgentID     = "aip-registered-agent"
	kcRegisteredAgentSecret = "reg-agent-secret"
	kcWrongSubjectID        = "aip-wrong-subject"
	kcWrongSubjectSecret    = "wrong-subject-secret"
)

// ---------------------------------------------------------------------------
// Keycloak admin helpers
// ---------------------------------------------------------------------------

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
	kcDo("POST",
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients/%s/protocol-mappers/models",
			port, realm, clientInternalID),
		adminToken, mapper)
}

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

// kcSetup creates the realm and the base set of clients used by all OIDC phases.
func kcSetup(port, realm string) {
	adminToken := kcAdminToken(port)
	kcDo("POST", "https://127.0.0.1:"+port+"/admin/realms", adminToken,
		map[string]interface{}{"realm": realm, "enabled": true})

	for _, c := range []struct{ id, secret string }{
		{"aip-agent-1", "agent-1-secret"},
		{"aip-reviewer-1", "reviewer-1-secret"},
	} {
		internalID := kcCreateClient(port, adminToken, realm, c.id, c.secret)
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
	if respDel, err := http.DefaultClient.Do(reqDel); err == nil {
		respDel.Body.Close()
	}
}

func kcGetClientSecret(port, adminToken, realm, clientInternalID string) string {
	rawURL := fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients/%s/client-secret", port, realm, clientInternalID)
	req, err := http.NewRequest("GET", rawURL, nil) //nolint:noctx
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

func kcAddServiceAccountClientIDMapper(port, adminToken, realm, targetClientInternalID, claimName string) {
	kcAddMapper(port, adminToken, realm, targetClientInternalID, map[string]interface{}{
		"name":           "service-account-client-id-as-" + claimName,
		"protocol":       "openid-connect",
		"protocolMapper": "oidc-usermodel-attribute-mapper",
		"config": map[string]string{
			"user.attribute":       "serviceAccountClientId",
			"claim.name":           claimName,
			"jsonType.label":       "String",
			"id.token.claim":       "false",
			"access.token.claim":   "true",
			"userinfo.token.claim": "false",
		},
	})
}

func kcSetupGatewaySecret(port, realm string) string {
	adminToken := kcAdminToken(port)
	req, _ := http.NewRequest("GET", //nolint:noctx
		fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients?clientId=aip-gateway", port, realm), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()
	var gwClients []map[string]interface{}
	Expect(json.NewDecoder(resp.Body).Decode(&gwClients)).To(Succeed())
	Expect(gwClients).NotTo(BeEmpty())
	return kcGetClientSecret(port, adminToken, realm, gwClients[0]["id"].(string))
}

func kcEnableTokenExchange(port, realm string) {
	adminToken := kcAdminToken(port)

	kcRequest := func(method, path string, body interface{}) []byte {
		var bodyReader io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			Expect(err).NotTo(HaveOccurred())
			bodyReader = strings.NewReader(string(b))
		}
		rawURL := fmt.Sprintf("https://127.0.0.1:%s%s", port, path)
		req, err := http.NewRequest(method, rawURL, bodyReader) //nolint:noctx
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(BeElementOf(http.StatusOK, http.StatusCreated, http.StatusNoContent, http.StatusConflict),
			"unexpected status %d for %s %s", resp.StatusCode, method, rawURL)
		out, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		return out
	}

	scopeBody := map[string]interface{}{
		"name":     "kubernetes",
		"protocol": "openid-connect",
		"protocolMappers": []map[string]interface{}{{
			"name":           "audience-mapper",
			"protocol":       "openid-connect",
			"protocolMapper": "oidc-audience-mapper",
			"config": map[string]string{
				"included.custom.audience": "kubernetes",
				"id.token.claim":           "true",
				"access.token.claim":       "true",
			},
		}},
	}
	kcRequest("POST", "/admin/realms/"+realm+"/client-scopes", scopeBody)

	kubernetesID := kcCreateClient(port, adminToken, realm, "kubernetes", "kubernetes-client-secret")
	kcSetupAudienceMapper(port, realm, kubernetesID, "kubernetes")
	gatewayID := kcCreateClient(port, adminToken, realm, "aip-gateway", "gateway-secret")

	var realmMgmtClients []map[string]interface{}
	rmBytes := kcRequest("GET", "/admin/realms/"+realm+"/clients?clientId=realm-management", nil)
	Expect(json.Unmarshal(rmBytes, &realmMgmtClients)).To(Succeed())
	Expect(realmMgmtClients).NotTo(BeEmpty())
	realmMgmtID := realmMgmtClients[0]["id"].(string)

	var agent1Clients []map[string]interface{}
	Expect(json.Unmarshal(kcRequest("GET", "/admin/realms/"+realm+"/clients?clientId=aip-agent-1", nil), &agent1Clients)).To(Succeed())
	Expect(agent1Clients).NotTo(BeEmpty())
	aipAgent1ID := agent1Clients[0]["id"].(string)

	kcRequest("PUT", fmt.Sprintf("/admin/realms/%s/clients/%s/management/permissions", realm, aipAgent1ID), map[string]interface{}{"enabled": true})
	kcRequest("PUT", fmt.Sprintf("/admin/realms/%s/clients/%s/management/permissions", realm, kubernetesID), map[string]interface{}{"enabled": true})

	policyBody := map[string]interface{}{
		"name":    "aip-gateway-policy",
		"type":    "client",
		"logic":   "POSITIVE",
		"clients": []string{gatewayID},
	}
	kcRequest("POST", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/policy/client", realm, realmMgmtID), policyBody)
	var policies []map[string]interface{}
	Expect(json.Unmarshal(kcRequest("GET",
		fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/policy/client?name=aip-gateway-policy", realm, realmMgmtID), nil),
		&policies)).To(Succeed())
	Expect(policies).NotTo(BeEmpty())
	policyID := policies[0]["id"].(string)

	for _, targetID := range []string{kubernetesID, aipAgent1ID} {
		var permsMap map[string]interface{}
		Eventually(func(g Gomega) {
			var permObj map[string]interface{}
			permBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/management/permissions", realm, targetID), nil)
			g.Expect(json.Unmarshal(permBytes, &permObj)).To(Succeed())
			pm, ok := permObj["permissions"].(map[string]interface{})
			if !ok {
				pm, ok = permObj["scopePermissions"].(map[string]interface{})
			}
			g.Expect(ok).To(BeTrue())
			permsMap = pm
		}, 10*time.Second, time.Second).Should(Succeed())

		permID, ok := permsMap["token-exchange"].(string)
		Expect(ok).To(BeTrue(), "missing token-exchange permission ID for %s", targetID)

		var permDetail map[string]interface{}
		detailBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/permission/scope/%s", realm, realmMgmtID, permID), nil)
		Expect(json.Unmarshal(detailBytes, &permDetail)).To(Succeed())
		permDetail["policies"] = []string{policyID}
		kcRequest("PUT", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/permission/scope/%s", realm, realmMgmtID, permID), permDetail)
	}
}

// ---------------------------------------------------------------------------
// Gateway HTTP helpers
// ---------------------------------------------------------------------------

func gwPostWithToken(port, path, body, token string) (*http.Response, error) {
	req, err := http.NewRequest("POST", "http://localhost:"+port+path, strings.NewReader(body)) //nolint:noctx
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return (&http.Client{Timeout: 90 * time.Second}).Do(req)
}

func gwGetWithToken(port, path, token string) (*http.Response, error) {
	req, err := http.NewRequest("GET", "http://localhost:"+port+path, nil) //nolint:noctx
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return (&http.Client{Timeout: 90 * time.Second}).Do(req)
}

func gwDeleteWithToken(port, path, token string) (*http.Response, error) {
	req, err := http.NewRequest("DELETE", "http://localhost:"+port+path, nil) //nolint:noctx
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return (&http.Client{Timeout: 90 * time.Second}).Do(req)
}

func gwCleanup(ns string) {
	_, _ = runCmd(exec.Command("kubectl", "delete", "agentrequest", "--all", "-n", ns, "--ignore-not-found"))
	_, _ = runCmd(exec.Command("kubectl", "delete", "lease", "-l", "governance.aip.io/managed-by=aip-controller", "-n", ns, "--ignore-not-found"))
	_, _ = runCmd(exec.Command("kubectl", "delete", "safetypolicy", "--all", "-n", ns, "--ignore-not-found"))
}

// ---------------------------------------------------------------------------
// Cluster secret / TLS helpers
// ---------------------------------------------------------------------------

func ensureKeycloakTLSSecret(projDir string) {
	_, _ = runCmd(exec.Command("kubectl", "create", "ns", "keycloak", "--dry-run=client", "-o", "yaml"))
	_ = exec.Command("kubectl", "create", "ns", "keycloak").Run()

	certPEM, err := os.ReadFile(filepath.Join(projDir, "test/fixtures/certs/keycloak.crt"))
	Expect(err).NotTo(HaveOccurred())
	keyPEM, err := os.ReadFile(filepath.Join(projDir, "test/fixtures/certs/keycloak.key"))
	Expect(err).NotTo(HaveOccurred())

	secretJSON := fmt.Sprintf(`{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": {"name": "keycloak-tls", "namespace": "keycloak"},
		"type": "kubernetes.io/tls",
		"stringData": {"tls.crt": %q, "tls.key": %q}
	}`, string(certPEM), string(keyPEM))
	Expect(kubectlApply(secretJSON)).To(Succeed())
}

func ensureGatewayCASecret(projDir string) {
	caPEM, err := os.ReadFile(filepath.Join(projDir, "test/fixtures/certs/ca.crt"))
	Expect(err).NotTo(HaveOccurred())

	secretJSON := fmt.Sprintf(`{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": {"name": "gateway-ca-cert", "namespace": "aip-k8s-system"},
		"stringData": {"ca.crt": %q}
	}`, string(caPEM))
	Expect(kubectlApply(secretJSON)).To(Succeed())
}

func createSecretInNamespace(name, ns, token string) {
	secretJSON := fmt.Sprintf(`{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": {"name": %q, "namespace": %q},
		"stringData": {"token": %q}
	}`, name, ns, token)
	Expect(kubectlApply(secretJSON)).To(Succeed())
}

// ---------------------------------------------------------------------------
// Downstream service deployment helpers
// ---------------------------------------------------------------------------

// deployGitHubMCPServer deploys the github-mcp-server using pat as the shared token.
func deployGitHubMCPServer(pat string) {
	projDir := getProjectDir()
	secretJSON := fmt.Sprintf(`{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": {"name": "aip-github-token", "namespace": "aip-k8s-system"},
		"stringData": {"token": %q}
	}`, pat)
	Expect(kubectlApply(secretJSON)).To(Succeed())

	_, err := runCmd(exec.Command("kubectl", "apply", "-f", filepath.Join(projDir, "config", "mcp")))
	Expect(err).NotTo(HaveOccurred(), "apply github-mcp-server")

	waitForDeploymentReady("aip-k8s-system", "github-mcp-server", 3*time.Minute)
	Eventually(func(g Gomega) {
		out, err := runCmd(exec.Command("kubectl", "get", "endpoints", "github-mcp", "-n", "aip-k8s-system",
			"-o", "jsonpath={.subsets[0].addresses[0].ip}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty())
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
}

func deployK8sMCPServer() {
	projDir := getProjectDir()
	clusterName := os.Getenv("KIND_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = "aip-k8s-test-e2e"
	}
	kindNodeName := clusterName + "-control-plane"
	ctrOut, ctrErr := exec.Command("docker", "exec", kindNodeName,
		"ctr", "-n=k8s.io", "images", "pull", "ghcr.io/containers/kubernetes-mcp-server:latest",
	).CombinedOutput()
	Expect(ctrErr).NotTo(HaveOccurred(), "ctr pull k8s-mcp-server: %s", string(ctrOut))

	_, err := runCmd(exec.Command("kubectl", "apply", "-f", filepath.Join(projDir, "config", "mcp", "k8s-mcp-server.yaml")))
	Expect(err).NotTo(HaveOccurred(), "apply k8s-mcp-server.yaml")

	waitForDeploymentReady("aip-k8s-system", "k8s-mcp-server", 3*time.Minute)
	Eventually(func(g Gomega) {
		out, err := runCmd(exec.Command("kubectl", "get", "endpoints", "k8s-mcp", "-n", "aip-k8s-system",
			"-o", "jsonpath={.subsets[0].addresses[0].ip}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty())
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
}

// buildLoadGatewayImage builds the gateway Docker image and loads it into the Kind cluster.
func buildLoadGatewayImage(projDir string) {
	clusterName := os.Getenv("KIND_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = "aip-k8s-test-e2e"
	}
	kindBinary := os.Getenv("KIND")
	if kindBinary == "" {
		kindBinary = "kind"
	}
	buildImg := exec.Command("docker", "build", "-f", "Dockerfile.gateway", "-t", "example.com/aip-gateway:v0.0.1", ".")
	buildImg.Dir = projDir
	out, err := buildImg.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "build gateway image: %s", string(out))

	loadImg := exec.Command(kindBinary, "load", "docker-image", "example.com/aip-gateway:v0.0.1", "--name", clusterName)
	out, err = loadImg.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "load gateway image: %s", string(out))
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
				"staticSecret": {"name": %q, "namespace": "aip-k8s-system", "key": "token"}
			}]
		}
	}`, "reg-"+agentIdentity, agentIdentity, kcInClusterIssuer, agentIdentity, secretName)
	Expect(kubectlApply(regJSON)).To(Succeed())
}

// ---------------------------------------------------------------------------
// GitHub API helpers (complements githubAPI already in mcp_suite_test.go)
// ---------------------------------------------------------------------------

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
		Object struct{ SHA string `json:"sha"` } `json:"object"`
	}
	Expect(json.NewDecoder(resp.Body).Decode(&refInfo)).To(Succeed())

	bodyJSON := fmt.Sprintf(`{"ref":"refs/heads/%s","sha":"%s"}`, branchName, refInfo.Object.SHA)
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

// ---------------------------------------------------------------------------
// Audit log helper
// ---------------------------------------------------------------------------

func readKindAuditLog(kindNodeName string) []map[string]interface{} {
	out, err := exec.Command("docker", "exec", kindNodeName, "tail", "-n", "5000", "/var/log/kubernetes/audit/audit.log").CombinedOutput()
	if err != nil {
		return nil
	}
	var entries []map[string]interface{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if json.Unmarshal([]byte(line), &entry) == nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

