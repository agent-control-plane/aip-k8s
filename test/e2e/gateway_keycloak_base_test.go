//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/gomega"

	"github.com/agent-control-plane/aip-k8s/test/utils"
)

const (
	kcPort    = "18091"
	kcBase    = "https://127.0.0.1:" + kcPort
	kcRealm   = "aip"
	kcIssuer  = kcBase + "/realms/" + kcRealm
	kcInClusterIssuer = "https://keycloak.keycloak.svc.cluster.local:8443/realms/" + kcRealm

	kc8GWPort = "18085"

	kcRegisteredAgentID     = "aip-registered-agent"
	kcRegisteredAgentSecret = "reg-agent-secret"
	kcWrongSubjectID        = "aip-wrong-subject"
	kcWrongSubjectSecret    = "wrong-subject-secret"
)

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

func gwPostWithToken(port, path, body, token string) (*http.Response, error) {
	url := "http://localhost:" + port + path
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 90 * time.Second}
	return client.Do(req)
}

func gwGetWithToken(port, path, token string) (*http.Response, error) {
	url := "http://localhost:" + port + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 90 * time.Second}
	return client.Do(req)
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

func gwCleanup(ns string) {
	cmd := exec.Command("kubectl", "delete", "agentrequest", "--all", "-n", ns, "--ignore-not-found")
	_, _ = utils.Run(cmd)
	cmd = exec.Command("kubectl", "delete", "lease", "-l", "governance.aip.io/managed-by=aip-controller", "-n", ns, "--ignore-not-found")
	_, _ = utils.Run(cmd)
	cmd = exec.Command("kubectl", "delete", "safetypolicy", "--all", "-n", ns, "--ignore-not-found")
	_, _ = utils.Run(cmd)
}

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

