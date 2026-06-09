//go:build ignore

package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	kcPort                  = "18091"
	kcBase                  = "https://127.0.0.1:" + kcPort
	kcRealm                 = "aip"
	kcRegisteredAgentID     = "aip-registered-agent"
	kcRegisteredAgentSecret = "reg-agent-secret"
	kcWrongSubjectID        = "aip-wrong-subject"
	kcWrongSubjectSecret    = "wrong-subject-secret"
)

func main() {
	// Disable TLS verification for self-signed certificates
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	// 1. Ensure Keycloak is accessible
	var pfCmd *exec.Cmd
	if !isPortOpen(kcPort) {
		fmt.Printf("Keycloak port %s is not open. Starting port-forward...\n", kcPort)
		// Clean up any stale port-forwards
		_ = exec.Command("pkill", "-f", "port-forward.*keycloak.*"+kcPort).Run()
		time.Sleep(500 * time.Millisecond)

		pfCmd = exec.Command("kubectl", "port-forward", "svc/keycloak", kcPort+":8443", "-n", "keycloak")
		if err := pfCmd.Start(); err != nil {
			fmt.Printf("Failed to start port-forward: %v\n", err)
			os.Exit(1)
		}
		defer func() {
			if pfCmd != nil && pfCmd.Process != nil {
				fmt.Println("Stopping port-forward...")
				_ = pfCmd.Process.Kill()
			}
		}()

		// Wait for port-forward to be ready
		ready := false
		for i := 0; i < 30; i++ {
			resp, err := http.Get(kcBase + "/realms/master/.well-known/openid-configuration")
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				ready = true
				break
			}
			if err == nil {
				resp.Body.Close()
			}
			time.Sleep(time.Second)
		}
		if !ready {
			fmt.Println("Timeout waiting for Keycloak port-forward to be ready")
			os.Exit(1)
		}
		fmt.Println("Port-forward started successfully.")
	} else {
		fmt.Printf("Keycloak port %s is already open, using existing connection.\n", kcPort)
	}

	// 2. Configure Keycloak
	fmt.Println("Configuring Keycloak realm and clients...")
	adminToken := getAdminToken()

	// Create realm aip
	fmt.Printf("Creating realm '%s'...\n", kcRealm)
	doRequest("POST", kcBase+"/admin/realms", adminToken, map[string]interface{}{
		"realm":   kcRealm,
		"enabled": true,
	})

	// Create core clients (aip-agent-1, aip-reviewer-1)
	for _, c := range []struct{ id, secret string }{
		{"aip-agent-1", "agent-1-secret"},
		{"aip-reviewer-1", "reviewer-1-secret"},
	} {
		fmt.Printf("Creating client '%s'...\n", c.id)
		internalID := createClient(adminToken, kcRealm, c.id, c.secret)
		addAudienceMapper(adminToken, kcRealm, internalID, "audience-aip-gateway", "aip-gateway")
	}

	// Create registration policy clients
	for _, c := range []struct{ id, secret string }{
		{kcRegisteredAgentID, kcRegisteredAgentSecret},
		{kcWrongSubjectID, kcWrongSubjectSecret},
	} {
		fmt.Printf("Creating client '%s'...\n", c.id)
		internalID := createClient(adminToken, kcRealm, c.id, c.secret)
		addAudienceMapper(adminToken, kcRealm, internalID, "audience-aip-gateway-"+c.id, "aip-gateway")
	}

	// Get aip-agent-1 internal ID
	aipAgent1InternalID := getClientInternalID(adminToken, kcRealm, "aip-agent-1")

	// Override sub claim for client credentials on aip-agent-1 to be "aip-agent-1"
	fmt.Println("Overriding sub claim on aip-agent-1 client...")
	addHardcodedSubMapper(adminToken, kcRealm, aipAgent1InternalID)

	// Enable token exchange on Keycloak realm
	fmt.Println("Enabling token exchange on realm...")
	enableTokenExchange(adminToken, kcRealm, aipAgent1InternalID)

	// Setup audience mapper for "kubernetes" audience on aip-agent-1 client
	fmt.Println("Setting up audience mapper for kubernetes on aip-agent-1...")
	addAudienceMapper(adminToken, kcRealm, aipAgent1InternalID, "audience-kubernetes-"+aipAgent1InternalID, "kubernetes")

	fmt.Println("Keycloak configuration completed successfully!")
}

func isPortOpen(port string) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func getAdminToken() string {
	resp, err := http.PostForm(
		kcBase+"/realms/master/protocol/openid-connect/token",
		url.Values{
			"client_id":  {"admin-cli"},
			"username":   {"admin"},
			"password":   {"admin"},
			"grant_type": {"password"},
		})
	if err != nil {
		fmt.Printf("failed to get admin token: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("failed to get admin token (status %d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("failed to decode admin token: %v\n", err)
		os.Exit(1)
	}

	token, ok := result["access_token"].(string)
	if !ok {
		fmt.Println("missing access_token in admin response")
		os.Exit(1)
	}
	return token
}

func createClient(adminToken, realm, clientID, secret string) string {
	doRequest("POST",
		fmt.Sprintf("%s/admin/realms/%s/clients", kcBase, realm),
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

	return getClientInternalID(adminToken, realm, clientID)
}

func createPublicClient(adminToken, realm, clientID string) string {
	doRequest("POST",
		fmt.Sprintf("%s/admin/realms/%s/clients", kcBase, realm),
		adminToken, map[string]interface{}{
			"clientId":                  clientID,
			"enabled":                   true,
			"publicClient":              true,
			"standardFlowEnabled":       false,
			"directAccessGrantsEnabled": false,
			"clientAuthenticatorType":   "public",
		})

	return getClientInternalID(adminToken, realm, clientID)
}

func getClientInternalID(adminToken, realm, clientID string) string {
	req, err := http.NewRequest("GET",
		fmt.Sprintf("%s/admin/realms/%s/clients?clientId=%s", kcBase, realm, clientID), nil)
	if err != nil {
		fmt.Printf("failed to create GET client request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("failed to get client details: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("failed to get client (status %d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	var clients []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		fmt.Printf("failed to decode clients response: %v\n", err)
		os.Exit(1)
	}

	if len(clients) == 0 {
		fmt.Printf("client %s not found\n", clientID)
		os.Exit(1)
	}

	return clients[0]["id"].(string)
}

func addAudienceMapper(adminToken, realm, clientInternalID, mapperName, audience string) {
	doRequest("POST",
		fmt.Sprintf("%s/admin/realms/%s/clients/%s/protocol-mappers/models", kcBase, realm, clientInternalID),
		adminToken, map[string]interface{}{
			"name":           mapperName,
			"protocol":       "openid-connect",
			"protocolMapper": "oidc-audience-mapper",
			"config": map[string]string{
				"included.custom.audience": audience,
				"id.token.claim":           "true",
				"access.token.claim":       "true",
			},
		})
}

func addHardcodedSubMapper(adminToken, realm, clientInternalID string) {
	doRequest("POST",
		fmt.Sprintf("%s/admin/realms/%s/clients/%s/protocol-mappers/models", kcBase, realm, clientInternalID),
		adminToken, map[string]interface{}{
			"name":           "override-sub-claim",
			"protocol":       "openid-connect",
			"protocolMapper": "oidc-hardcoded-claim-mapper",
			"config": map[string]string{
				"claim.name":           "sub",
				"claim.value":          "aip-agent-1",
				"jsonType.label":       "String",
				"id.token.claim":       "true",
				"access.token.claim":   "true",
				"userinfo.token.claim": "true",
			},
		})
}

func enableTokenExchange(adminToken, realm, aipAgent1InternalID string) {
	kcRequest := func(method, path string, body interface{}) []byte {
		var bodyReader io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				fmt.Printf("failed to marshal body: %v\n", err)
				os.Exit(1)
			}
			bodyReader = bytes.NewReader(b)
		}
		urlStr := fmt.Sprintf("%s%s", kcBase, path)
		req, err := http.NewRequest(method, urlStr, bodyReader)
		if err != nil {
			fmt.Printf("failed to create request: %v\n", err)
			os.Exit(1)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Printf("failed to execute request %s %s: %v\n", method, urlStr, err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated &&
			resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusConflict {
			bodyBytes, _ := io.ReadAll(resp.Body)
			fmt.Printf("unexpected status %d for %s %s: %s\n", resp.StatusCode, method, urlStr, string(bodyBytes))
			os.Exit(1)
		}

		out, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("failed to read response body: %v\n", err)
			os.Exit(1)
		}
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
	kubernetesId := createClient(adminToken, realm, "kubernetes", "kubernetes-client-secret")
	addAudienceMapper(adminToken, realm, kubernetesId, "audience-kubernetes-target", "kubernetes")

	// Create requesting client "aip-gateway"
	gatewayId := createClient(adminToken, realm, "aip-gateway", "gateway-secret")

	// Get realm-management client internal ID
	var realmManagementClients []map[string]interface{}
	rmBytes := kcRequest("GET", "/admin/realms/"+realm+"/clients?clientId=realm-management", nil)
	if err := json.Unmarshal(rmBytes, &realmManagementClients); err != nil {
		fmt.Printf("failed to unmarshal realm-management response: %v\n", err)
		os.Exit(1)
	}
	if len(realmManagementClients) == 0 {
		fmt.Println("realm-management client not found")
		os.Exit(1)
	}
	realmManagementInternalID := realmManagementClients[0]["id"].(string)

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
		if err := json.Unmarshal(policiesBytes, &policies); err != nil {
			fmt.Printf("failed to unmarshal policies: %v\n", err)
			os.Exit(1)
		}
		if len(policies) == 0 {
			fmt.Println("failed to find created policy")
			os.Exit(1)
		}
		policyID = policies[0]["id"].(string)
	}

	// 6. Get the token-exchange permission ID for kubernetes (target client)
	var permsMap map[string]interface{}
	var tokenExchangePermID string
	var ok bool
	for i := 0; i < 10; i++ {
		var permObj map[string]interface{}
		permBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/management/permissions", realm, kubernetesId), nil)
		if err := json.Unmarshal(permBytes, &permObj); err == nil {
			pm, exists := permObj["permissions"].(map[string]interface{})
			if !exists {
				pm, exists = permObj["scopePermissions"].(map[string]interface{})
			}
			if exists {
				permsMap = pm
				if tepID, present := permsMap["token-exchange"].(string); present {
					tokenExchangePermID = tepID
					ok = true
					break
				}
			}
		}
		time.Sleep(time.Second)
	}
	if !ok {
		fmt.Println("failed to retrieve token-exchange permission ID for kubernetes after retries")
		os.Exit(1)
	}

	// Update the token-exchange permission of kubernetes
	var permissionDetail map[string]interface{}
	detailBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/permission/scope/%s", realm, realmManagementInternalID, tokenExchangePermID), nil)
	if err := json.Unmarshal(detailBytes, &permissionDetail); err != nil {
		fmt.Printf("failed to unmarshal permission detail: %v\n", err)
		os.Exit(1)
	}
	permissionDetail["policies"] = []string{policyID}
	kcRequest("PUT", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/permission/scope/%s", realm, realmManagementInternalID, tokenExchangePermID), permissionDetail)

	// 7. Get the token-exchange permission ID for aip-agent-1 (source client)
	var agentPermsMap map[string]interface{}
	var agentTokenExchangePermID string
	var agentOk bool
	for i := 0; i < 10; i++ {
		var permObj map[string]interface{}
		permBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/management/permissions", realm, aipAgent1InternalID), nil)
		if err := json.Unmarshal(permBytes, &permObj); err == nil {
			pm, exists := permObj["permissions"].(map[string]interface{})
			if !exists {
				pm, exists = permObj["scopePermissions"].(map[string]interface{})
			}
			if exists {
				agentPermsMap = pm
				if tepID, present := agentPermsMap["token-exchange"].(string); present {
					agentTokenExchangePermID = tepID
					agentOk = true
					break
				}
			}
		}
		time.Sleep(time.Second)
	}
	if !agentOk {
		fmt.Println("failed to retrieve token-exchange permission ID for aip-agent-1 after retries")
		os.Exit(1)
	}

	// Update the token-exchange permission of aip-agent-1
	var agentPermissionDetail map[string]interface{}
	agentDetailBytes := kcRequest("GET", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/permission/scope/%s", realm, realmManagementInternalID, agentTokenExchangePermID), nil)
	if err := json.Unmarshal(agentDetailBytes, &agentPermissionDetail); err != nil {
		fmt.Printf("failed to unmarshal agent permission detail: %v\n", err)
		os.Exit(1)
	}
	agentPermissionDetail["policies"] = []string{policyID}
	kcRequest("PUT", fmt.Sprintf("/admin/realms/%s/clients/%s/authz/resource-server/permission/scope/%s", realm, realmManagementInternalID, agentTokenExchangePermID), agentPermissionDetail)
}

func doRequest(method, urlStr, token string, body interface{}) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			fmt.Printf("failed to marshal body: %v\n", err)
			os.Exit(1)
		}
		bodyReader = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, urlStr, bodyReader)
	if err != nil {
		fmt.Printf("failed to create request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("request failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated &&
		resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusConflict {
		bodyBytes, _ := io.ReadAll(resp.Body)
		fmt.Printf("unexpected status %d for %s %s: %s\n", resp.StatusCode, method, urlStr, string(bodyBytes))
		os.Exit(1)
	}
}
