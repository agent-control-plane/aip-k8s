//go:build mcp_e2e
// +build mcp_e2e

// Phase 9: end-to-end Keycloak identity flow with K8s audit verification.
//
// Requires OIDC_KIND_CLUSTER=true: the test restarts kube-apiserver for audit log
// capture, deploys Keycloak in-cluster, and configures token exchange so the gateway
// can impersonate the agent identity in K8s API calls. Verifies that the audit log
// shows username == "aip-agent-1" (not the gateway service account).

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
)

var _ = Describe("Phase 9: Keycloak Identity Flow and K8s Audit", Ordered, func() {
	Context("9a: real cluster OIDC e2e tests", Ordered, func() {
		var gwPFProc *exec.Cmd
		var kcPFProc *exec.Cmd

		BeforeAll(func() {
			if os.Getenv("OIDC_KIND_CLUSTER") != "true" {
				Skip("OIDC_KIND_CLUSTER=true is required for Phase 9 real cluster e2e tests")
			}

			projDir := getProjectDir()
			http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec

			clusterName := os.Getenv("KIND_CLUSTER_NAME")
			if clusterName == "" {
				clusterName = "aip-k8s-test-e2e"
			}
			kindNodeName := clusterName + "-control-plane"

			// Restart kube-apiserver by bouncing its static pod manifest so audit logging
			// is guaranteed to flush from the start of this test's requests.
			_ = exec.Command("docker", "exec", kindNodeName, "mv", "/etc/kubernetes/manifests/kube-apiserver.yaml", "/tmp/").Run()
			time.Sleep(3 * time.Second)
			_ = exec.Command("docker", "exec", kindNodeName, "mv", "/tmp/kube-apiserver.yaml", "/etc/kubernetes/manifests/").Run()
			Eventually(func() error {
				return exec.Command("kubectl", "get", "nodes").Run()
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			// Best-effort cleanup of stale resources from prior runs.
			_ = exec.Command("kubectl", "delete", "agentregistration", "--all", "-n", "default", "--ignore-not-found").Run()
			_ = exec.Command("kubectl", "delete", "safetypolicy", "--all", "-n", "default", "--ignore-not-found").Run()
			_ = exec.Command("kubectl", "delete", "agentrequest", "--all", "-n", "default", "--ignore-not-found").Run()

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
				defer resp.Body.Close()
				return resp.StatusCode
			}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

			By("adding Keycloak hostname to Kind node /etc/hosts")
			kcClusterIPBytes, err := exec.Command("kubectl", "get", "svc", "keycloak", "-n", "keycloak",
				"-o", "jsonpath={.spec.clusterIP}").CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "get keycloak cluster IP: %s", string(kcClusterIPBytes))
			kcClusterIP := strings.TrimSpace(string(kcClusterIPBytes))
			hostsEntry := fmt.Sprintf("%s keycloak.keycloak.svc.cluster.local", kcClusterIP)
			addHostsCmd := exec.Command("docker", "exec", kindNodeName, "sh", "-c",
				fmt.Sprintf(`grep -qF '%s' /etc/hosts || echo '%s' >> /etc/hosts`, hostsEntry, hostsEntry))
			out, err := addHostsCmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "add /etc/hosts entry: %s", string(out))

			By("configuring Keycloak realm and clients")
			kcSetup(kcPort, kcRealm)

			adminToken := kcAdminToken(kcPort)
			var agent1Clients []map[string]interface{}
			req, _ := http.NewRequest("GET", //nolint:noctx
				fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients?clientId=aip-agent-1", kcPort, kcRealm), nil)
			req.Header.Set("Authorization", "Bearer "+adminToken)
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(json.NewDecoder(resp.Body).Decode(&agent1Clients)).To(Succeed())
			Expect(agent1Clients).NotTo(BeEmpty())
			aipAgent1InternalID := agent1Clients[0]["id"].(string)

			By("enabling token exchange on realm")
			kcEnableTokenExchange(kcPort, kcRealm)

			By("setting up audience mapper for kubernetes on aip-agent-1")
			kcSetupAudienceMapper(kcPort, kcRealm, aipAgent1InternalID, "kubernetes")

			By("adding preferred_username mapper to kubernetes client for agent identity")
			adminToken = kcAdminToken(kcPort)
			var k8sClients []map[string]interface{}
			k8sReq, _ := http.NewRequest("GET", //nolint:noctx
				fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients?clientId=kubernetes", kcPort, kcRealm), nil)
			k8sReq.Header.Set("Authorization", "Bearer "+adminToken)
			k8sResp, err := http.DefaultClient.Do(k8sReq)
			Expect(err).NotTo(HaveOccurred())
			defer k8sResp.Body.Close()
			Expect(json.NewDecoder(k8sResp.Body).Decode(&k8sClients)).To(Succeed())
			Expect(k8sClients).NotTo(BeEmpty())
			kubernetesClientID := k8sClients[0]["id"].(string)

			// Remove the default "profile" scope so preferred_username doesn't
			// default to the service account name and collide with our hardcoded claim.
			scopeListReq, _ := http.NewRequest("GET", //nolint:noctx
				fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients/%s/default-client-scopes",
					kcPort, kcRealm, kubernetesClientID), nil)
			scopeListReq.Header.Set("Authorization", "Bearer "+adminToken)
			scopeListResp, err := http.DefaultClient.Do(scopeListReq)
			Expect(err).NotTo(HaveOccurred())
			var defaultScopes []map[string]interface{}
			Expect(json.NewDecoder(scopeListResp.Body).Decode(&defaultScopes)).To(Succeed())
			scopeListResp.Body.Close()
			for _, scope := range defaultScopes {
				if scope["name"] == "profile" {
					profileScopeID, _ := scope["id"].(string)
					delScopeReq, _ := http.NewRequest("DELETE", //nolint:noctx
						fmt.Sprintf("https://127.0.0.1:%s/admin/realms/%s/clients/%s/default-client-scopes/%s",
							kcPort, kcRealm, kubernetesClientID, profileScopeID), nil)
					delScopeReq.Header.Set("Authorization", "Bearer "+adminToken)
					delScopeResp, err := http.DefaultClient.Do(delScopeReq)
					Expect(err).NotTo(HaveOccurred())
					delScopeResp.Body.Close()
					Expect(delScopeResp.StatusCode).To(BeElementOf(200, 204),
						"failed to remove profile scope from kubernetes client")
					break
				}
			}

			kcAddMapper(kcPort, adminToken, kcRealm, kubernetesClientID, map[string]interface{}{
				"name":           "agent-preferred-username",
				"protocol":       "openid-connect",
				"protocolMapper": "oidc-hardcoded-claim-mapper",
				"config": map[string]string{
					"claim.name":           "preferred_username",
					"claim.value":          "aip-agent-1",
					"jsonType.label":       "String",
					"id.token.claim":       "false",
					"access.token.claim":   "true",
					"userinfo.token.claim": "false",
				},
			})

			By("deploying K8s MCP server")
			deployK8sMCPServer()

			By("creating ClusterRole and ClusterRoleBinding for aip-agent-1")
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
			Expect(kubectlApply(rbacYAML)).To(Succeed())

			_, err = runCmd(exec.Command("kubectl", "apply", "-f",
				filepath.Join(projDir, "config/mcp/k8s-mcp-server-cr.yaml")))
			Expect(err).NotTo(HaveOccurred(), "apply MCPServer CR")

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
			Expect(kubectlApply(policyJSON)).To(Succeed())
			Eventually(func() error {
				return exec.Command("kubectl", "get", "safetypolicy", "kc-require-human", "-n", "default").Run()
			}, 15*time.Second, time.Second).Should(Succeed())

			By("building and loading gateway Docker image")
			buildLoadGatewayImage(projDir)

			By("creating gateway client credentials secret")
			gatewaySecret := kcSetupGatewaySecret(kcPort, kcRealm)
			gwSecretJSON := fmt.Sprintf(`{
				"apiVersion": "v1", "kind": "Secret",
				"metadata": {"name": "aip-gateway-secret", "namespace": "aip-k8s-system"},
				"stringData": {"client-secret": %q}
			}`, gatewaySecret)
			Expect(kubectlApply(gwSecretJSON)).To(Succeed())

			By("creating gateway CA cert secret")
			ensureGatewayCASecret(projDir)

			By("deploying gateway to cluster (gateway-dev.yaml)")
			_ = exec.Command("kubectl", "delete", "deployment", "aip-gateway", "-n", "aip-k8s-system", "--ignore-not-found").Run()
			_ = exec.Command("kubectl", "wait", "--for=delete", "pod", "-l", "app=aip-gateway", "-n", "aip-k8s-system", "--timeout=30s").Run()
			_, err = runCmd(exec.Command("kubectl", "apply", "-f",
				filepath.Join(projDir, "test/fixtures/gateway-dev.yaml")))
			Expect(err).NotTo(HaveOccurred(), "apply gateway-dev.yaml")

			By("waiting for gateway pod to be ready")
			Eventually(func(g Gomega) {
				out, err := runCmd(exec.Command("kubectl", "get", "pods",
					"-l", "app=aip-gateway", "-n", "aip-k8s-system",
					"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("True"))
			}, 3*time.Minute, 3*time.Second).Should(Succeed())

			By("port-forwarding gateway to localhost:" + kc9GWPort)
			_ = exec.Command("pkill", "-f", "port-forward.*aip-gateway.*"+kc9GWPort).Run()
			time.Sleep(500 * time.Millisecond)
			gwPFProc = exec.Command("kubectl", "port-forward",
				"svc/aip-gateway", kc9GWPort+":8080", "-n", "aip-k8s-system")
			gwPFProc.Stdout = GinkgoWriter
			gwPFProc.Stderr = GinkgoWriter
			Expect(gwPFProc.Start()).To(Succeed())
			Eventually(func() int {
				resp, err := http.Get("http://localhost:" + kc9GWPort + "/healthz") //nolint:noctx
				if err != nil {
					return 0
				}
				defer resp.Body.Close()
				return resp.StatusCode
			}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

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
			}`, kcInClusterIssuer, kcInClusterTokenURL)
			respReg, err := gwPostWithToken(kc9GWPort, "/agent-registrations", regJSON, reviewerToken)
			Expect(err).NotTo(HaveOccurred())
			defer respReg.Body.Close()
			Expect(respReg.StatusCode).To(Equal(http.StatusCreated))
		})

		AfterAll(func() {
			if os.Getenv("OIDC_KIND_CLUSTER") != "true" {
				return
			}
			reviewerToken := kcFetchToken(kcPort, kcRealm, "aip-reviewer-1", "reviewer-1-secret")
			_, _ = gwDeleteWithToken(kc9GWPort, "/agent-registrations/reg-aip-agent-1", reviewerToken)

			if gwPFProc != nil && gwPFProc.Process != nil {
				_ = gwPFProc.Process.Kill()
			}
			if kcPFProc != nil && kcPFProc.Process != nil {
				_ = kcPFProc.Process.Kill()
			}

			_ = exec.Command("kubectl", "delete", "clusterrolebinding", "aip-agent-1-k8s-mcp", "--ignore-not-found").Run()
			_ = exec.Command("kubectl", "delete", "clusterrole", "aip-agent-1-k8s-mcp", "--ignore-not-found").Run()
			_ = exec.Command("kubectl", "delete", "safetypolicy", "--all", "-n", "default", "--ignore-not-found").Run()
			_ = exec.Command("kubectl", "delete", "agentrequest", "--all", "-n", "default", "--ignore-not-found").Run()
			_, _ = runCmd(exec.Command("kubectl", "delete", "-f", "config/mcp/k8s-mcp-server-cr.yaml", "--ignore-not-found"))
			_, _ = runCmd(exec.Command("kubectl", "delete", "-f", "config/mcp/k8s-mcp-server.yaml", "--ignore-not-found"))
			_, _ = runCmd(exec.Command("kubectl", "delete", "-f", "test/fixtures/gateway-dev.yaml", "--ignore-not-found"))
			_ = exec.Command("kubectl", "delete", "secret", "--all", "-n", "aip-k8s-system", "--ignore-not-found").Run()
		})

		It("Keycloak JWT → KubernetesOIDC exchange → K8s audit shows agent identity", func() {
			token := kcFetchToken(kcPort, kcRealm, "aip-agent-1", "agent-1-secret")

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

			reviewerToken := kcFetchToken(kcPort, kcRealm, "aip-reviewer-1", "reviewer-1-secret")
			approveResp, err := gwPostWithToken(kc9GWPort,
				"/agent-requests/"+reqName+"/approve",
				`{"reason":"Approved for auditing test"}`,
				reviewerToken)
			Expect(err).NotTo(HaveOccurred())
			defer approveResp.Body.Close()
			Expect(approveResp.StatusCode).To(Equal(http.StatusOK))

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
			callTime := time.Now().UTC()

			mcpResp, err := gwPostWithToken(kc9GWPort, "/mcp", mcpBody, token)
			Expect(err).NotTo(HaveOccurred())
			defer mcpResp.Body.Close()
			Expect(mcpResp.StatusCode).To(Equal(http.StatusOK))

			time.Sleep(3 * time.Second)

			clusterName := os.Getenv("KIND_CLUSTER_NAME")
			if clusterName == "" {
				clusterName = "aip-k8s-test-e2e"
			}
			auditLogs := readKindAuditLog(clusterName + "-control-plane")
			Expect(auditLogs).NotTo(BeEmpty(), "audit log should not be empty")

			foundAgent := false
			foundSA := false
			for _, entry := range auditLogs {
				if ts, _ := entry["requestReceivedTimestamp"].(string); ts != "" {
					if t, err := time.Parse(time.RFC3339Nano, ts); err != nil || t.Before(callTime) {
						continue
					}
				}
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

			Expect(foundAgent).To(BeTrue(), "should find list configmaps entry where username == aip-agent-1")
			Expect(foundSA).To(BeFalse(), "should NOT find list configmaps entry where username starts with system:serviceaccount:")
		})
	})
})
