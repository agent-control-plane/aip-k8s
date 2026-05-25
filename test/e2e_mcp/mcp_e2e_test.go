//go:build mcp_e2e
// +build mcp_e2e

package e2e_mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

const (
	govResourceName = "github-infra-resource"
	policyName      = "replica-cap-guard"
	reqNamespace    = "default"
	testBranch      = "main"
)

var (
	govResourceJSON = fmt.Sprintf(`{
	"apiVersion": "governance.aip.io/v1alpha1",
	"kind": "GovernedResource",
	"metadata": {"name": "%s"},
	"spec": {
		"uriPattern": "github://%s/%s/**",
		"permittedActions": ["github/create_pull_request"],
		"contextFetcher": "github"
	}
}`, govResourceName, githubOwner, githubRepo)

	policyJSON = fmt.Sprintf(`{
	"apiVersion": "governance.aip.io/v1alpha1",
	"kind": "SafetyPolicy",
	"metadata": {"name": "%s", "namespace": "%s"},
	"spec": {
		"governedResourceSelector": {},
		"rules": [
			{
				"name": "replica-cap-guard",
				"type": "StateEvaluation",
				"action": "Deny",
				"expression": "has(request.spec.parameters) && has(request.spec.parameters.proposedMaxReplicas) && target != null && has(target.fileContent) && has(target.fileContent.absoluteMax) && double(request.spec.parameters.proposedMaxReplicas) / double(target.fileContent.absoluteMax) > 0.9",
				"message": "Proposed maxReplicas exceeds 90%% of absoluteMax. Reduce the request."
			},
			{
				"name": "require-human-approval",
				"type": "StateEvaluation",
				"action": "RequireApproval",
				"expression": "has(request.spec.parameters) && has(request.spec.parameters.proposedMaxReplicas)",
				"message": "Human approval required for infrastructure config changes."
			}
		],
		"failureMode": "FailClosed"
	}
}`, policyName, reqNamespace)
)

// gwRequestResponse is the subset of the gateway's POST /agent-requests response
// that the e2e tests need to inspect.
type gwRequestResponse struct {
	Name   string `json:"name"`
	Phase  string `json:"phase"`
	Denial *struct {
		Code          string `json:"code"`
		PolicyResults []struct {
			RuleName string `json:"ruleName"`
		} `json:"policyResults"`
	} `json:"denial"`
	Conditions []struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	} `json:"conditions"`
}

// submitToGateway POSTs an AgentRequest to the gateway and blocks until
// the gateway returns a resolved phase. The gateway matches the target URI
// against GovernedResources and sets GovernedResourceRef automatically,
// which triggers provider context fetching in the controller.
func submitToGateway(replicas int) gwRequestResponse {
	return submitToGatewayAs("e2e-mcp-agent", replicas)
}

// submitToGatewayAs submits an AgentRequest with a custom agent identity,
// used when a test needs to avoid the dedup key (agentIdentity, action, targetURI).
func submitToGatewayAs(agentIdentity string, replicas int) gwRequestResponse {
	body := fmt.Sprintf(`{
		"agentIdentity": "%s",
		"action": "github/create_pull_request",
		"targetURI": "github://%s/%s/files/%s/%s",
		"reason": "e2e mcp test",
		"namespace": "%s",
		"parameters": {"proposedMaxReplicas": %d}
	}`, agentIdentity, githubOwner, githubRepo, testBranch, githubConfigFilePath, reqNamespace, replicas)

	req, err := http.NewRequest("POST", gwURL+"/agent-requests", strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")

	// Client timeout must exceed the gateway's --wait-timeout (90s).
	gwClient := &http.Client{Timeout: 3 * time.Minute}
	resp, err := gwClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusCreated), "gateway returned non-201: %s", string(bodyBytes))

	var result gwRequestResponse
	Expect(json.Unmarshal(bodyBytes, &result)).To(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "gateway response: name=%s phase=%s\n", result.Name, result.Phase)
	return result
}

var _ = Describe("MCP E2E: GitHub PR Governance", Ordered, func() {
	BeforeAll(func() {
		By("creating GovernedResource")
		Expect(kubectlApply(govResourceJSON)).To(Succeed())

		By("waiting for GovernedResource to be visible")
		Eventually(func(g Gomega) {
			var gr governancev1alpha1.GovernedResource
			err := k8sClient.Get(ctx, types.NamespacedName{Name: govResourceName}, &gr)
			g.Expect(err).NotTo(HaveOccurred())
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("creating SafetyPolicy")
		Expect(kubectlApply(policyJSON)).To(Succeed())

		By("waiting for SafetyPolicy to be visible")
		Eventually(func(g Gomega) {
			var sp governancev1alpha1.SafetyPolicy
			err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: reqNamespace}, &sp)
			g.Expect(err).NotTo(HaveOccurred())
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("cleaning up resources")
		_ = kubectlDelete(policyJSON)
		_ = kubectlDelete(govResourceJSON)
		cmd := exec.Command("kubectl", "delete", "agentrequest", "--all", "-n", reqNamespace, "--ignore-not-found")
		_, _ = runCmd(cmd)
		cmd = exec.Command("kubectl", "delete", "lease", "-l", "governance.aip.io/managed-by=aip-controller", "-n", reqNamespace, "--ignore-not-found")
		_, _ = runCmd(cmd)
	})

	Context("Scenario A: Denied — agent proposes 19 replicas (95% of absoluteMax)", func() {
		It("should evaluate safety policy and deny the request", func() {
			By("submitting AgentRequest with proposedMaxReplicas=19 through gateway")
			resp := submitToGateway(19)

			By("verifying phase=Denied with rule replica-cap-guard")
			Expect(resp.Phase).To(Equal("Denied"))
			Expect(resp.Denial).NotTo(BeNil())
			Expect(resp.Denial.Code).To(Equal("POLICY_VIOLATION"))
			Expect(resp.Denial.PolicyResults).NotTo(BeEmpty())
			Expect(resp.Denial.PolicyResults[0].RuleName).To(Equal("replica-cap-guard"))
		})
	})

	Context("Scenario B: Approved — agent proposes 17 replicas (85% of absoluteMax)", func() {
		var arName string

		It("should evaluate safety policy and require human approval (no Deny match)", func() {
			By("submitting AgentRequest with proposedMaxReplicas=17 through gateway")
			resp := submitToGateway(17)
			arName = resp.Name

			By("verifying phase=Pending with RequiresApproval condition")
			Expect(resp.Phase).To(Equal("Pending"))

			var found bool
			for _, c := range resp.Conditions {
				if c.Type == "RequiresApproval" {
					found = c.Status == "True"
					break
				}
			}
			Expect(found).To(BeTrue(), "RequiresApproval condition should be True")
		})

		It("should mint a scoped JWT via gateway approve endpoint and create a PR via MCP proxy", func() {
			By("calling POST /agent-requests/{name}/approve on gateway to mint JWT")
			approveURL := fmt.Sprintf("%s/agent-requests/%s/approve?namespace=%s", gwURL, arName, reqNamespace)
			req, err := http.NewRequest("POST", approveURL, bytes.NewReader([]byte(`{"reason":"e2e test approval"}`)))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Remote-User", "e2e-reviewer")
			req.Header.Set("X-Remote-Groups", "reviewers")

			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred(), "Failed to call approve endpoint")
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK), "approve endpoint returned non-200: %s", string(body))

			var approveResp struct {
				Token          string `json:"token"`
				TokenExpiresAt string `json:"token_expires_at"`
			}
			Expect(json.Unmarshal(body, &approveResp)).To(Succeed())
			Expect(approveResp.Token).NotTo(BeEmpty(), "approve response should contain a token")
			_, _ = fmt.Fprintf(GinkgoWriter, "Received JWT token (expires: %s)\n", approveResp.TokenExpiresAt)

			By("calling POST /mcp-proxy/github/create_pull_request with the JWT")
			proxyURL := fmt.Sprintf("%s/mcp-proxy/github/create_pull_request", gwURL)

			prBody := fmt.Sprintf(`{
				"name": "create_pull_request",
				"arguments": {
					"owner": "%s",
					"repo": "%s",
					"title": "[e2e-test] Scale payment-api maxReplicas to 17",
					"body": "Auto-generated by AIP MCP e2e test.\n\nProposed change: increase maxReplicas from 5 to 17 (85%% of absoluteMax 20).\n\nPolicy evaluation passed: replica-cap-guard ratio 0.85 <= 0.9.",
					"head": "%s",
					"base": "%s",
					"draft": true
				}
			}`, githubOwner, githubRepo, e2eTestBranch, testBranch)

			proxyReq, err := http.NewRequest("POST", proxyURL, strings.NewReader(prBody))
			Expect(err).NotTo(HaveOccurred())
			proxyReq.Header.Set("Content-Type", "application/json")
			proxyReq.Header.Set("X-AIP-Authorization", fmt.Sprintf("Bearer %s", approveResp.Token))

			proxyResp, err := http.DefaultClient.Do(proxyReq)
			Expect(err).NotTo(HaveOccurred(), "Failed to call MCP proxy")
			defer proxyResp.Body.Close()

			proxyBody, err := io.ReadAll(proxyResp.Body)
			Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "MCP proxy response status: %d\n", proxyResp.StatusCode)
			_, _ = fmt.Fprintf(GinkgoWriter, "MCP proxy response body: %s\n", string(proxyBody))

			Expect(proxyResp.StatusCode).To(Equal(http.StatusOK), "MCP proxy returned non-200: %s", string(proxyBody))

			By("verifying the PR was created on GitHub")
			var proxyResult struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			Expect(json.Unmarshal(proxyBody, &proxyResult)).To(Succeed())
			Expect(proxyResult.Content).NotTo(BeEmpty())
			// github-mcp-server v1.0.0 create_pull_request returns {id, url} not html_url
			Expect(proxyResult.Content[0].Text).To(ContainSubstring("/pull/"))
			_, _ = fmt.Fprintf(GinkgoWriter, "PR created successfully: %s\n", proxyResult.Content[0].Text)
		})
	})

	Context("Scenario C: PR creation via POST /mcp JSON-RPC", func() {
		It("should submit, approve, and create a PR via POST /mcp with JSON-RPC", func() {
			By("submitting AgentRequest with unique identity (avoids dedup from Scenario B)")
			resp := submitToGatewayAs("e2e-mcp-agent-c", 17)

			By("verifying phase=Pending with RequiresApproval condition")
			Expect(resp.Phase).To(Equal("Pending"))
			var found bool
			for _, c := range resp.Conditions {
				if c.Type == "RequiresApproval" {
					found = c.Status == "True"
					break
				}
			}
			Expect(found).To(BeTrue(), "RequiresApproval condition should be True")

			By("approving through gateway to mint JWT")
			approveURL := fmt.Sprintf("%s/agent-requests/%s/approve?namespace=%s", gwURL, resp.Name, reqNamespace)
			req, err := http.NewRequest("POST", approveURL, bytes.NewReader([]byte(`{"reason":"e2e test approval"}`)))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Remote-User", "e2e-reviewer")
			req.Header.Set("X-Remote-Groups", "reviewers")

			approveResp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred(), "Failed to call approve endpoint")
			defer approveResp.Body.Close()

			body, err := io.ReadAll(approveResp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(approveResp.StatusCode).To(Equal(http.StatusOK), "approve endpoint returned non-200: %s", string(body))

			var approveResult struct {
				Token          string `json:"token"`
				TokenExpiresAt string `json:"token_expires_at"`
			}
			Expect(json.Unmarshal(body, &approveResult)).To(Succeed())
			Expect(approveResult.Token).NotTo(BeEmpty(), "approve response should contain a token")

			By("calling POST /mcp with tools/call for github/create_pull_request")
			mcpURL := fmt.Sprintf("%s/mcp", gwURL)
			rpcBody := fmt.Sprintf(`{
				"jsonrpc": "2.0",
				"id": 1,
				"method": "tools/call",
				"params": {
					"name": "github/create_pull_request",
					"arguments": {
						"owner": "%s",
						"repo": "%s",
						"title": "[e2e-test] Scale payment-api maxReplicas to 17 (MCP protocol)",
						"body": "Auto-generated by AIP MCP e2e test (POST /mcp).\n\nProposed change: increase maxReplicas from 5 to 17 (85%% of absoluteMax 20).\n\nPolicy evaluation passed: replica-cap-guard ratio 0.85 <= 0.9.",
						"head": "%s",
						"base": "%s",
						"draft": true
					}
				}
			}`, githubOwner, githubRepo, e2eTestBranch2, testBranch)

			mcpReq, err := http.NewRequest("POST", mcpURL, strings.NewReader(rpcBody))
			Expect(err).NotTo(HaveOccurred())
			mcpReq.Header.Set("Content-Type", "application/json")
			mcpReq.Header.Set("X-AIP-Authorization", fmt.Sprintf("Bearer %s", approveResult.Token))

			mcpResp, err := http.DefaultClient.Do(mcpReq)
			Expect(err).NotTo(HaveOccurred(), "Failed to call POST /mcp")
			defer mcpResp.Body.Close()

			mcpBody, err := io.ReadAll(mcpResp.Body)
			Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "POST /mcp response status: %d\n", mcpResp.StatusCode)
			_, _ = fmt.Fprintf(GinkgoWriter, "POST /mcp response body: %s\n", string(mcpBody))

			Expect(mcpResp.StatusCode).To(Equal(http.StatusOK), "POST /mcp returned non-200: %s", string(mcpBody))

			By("verifying JSON-RPC response contains PR URL")
			var rpcResp struct {
				JSONRPC string `json:"jsonrpc"`
				ID      int    `json:"id"`
				Result  *struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result,omitempty"`
				Error *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error,omitempty"`
			}
			Expect(json.Unmarshal(mcpBody, &rpcResp)).To(Succeed())
			Expect(rpcResp.Error).To(BeNil(), "JSON-RPC error: %+v", rpcResp.Error)
			Expect(rpcResp.Result).NotTo(BeNil())
			Expect(rpcResp.Result.Content).NotTo(BeEmpty())
			Expect(rpcResp.Result.Content[0].Text).To(ContainSubstring("/pull/"))
			_, _ = fmt.Fprintf(GinkgoWriter, "PR created via POST /mcp: %s\n", rpcResp.Result.Content[0].Text)
		})
	})

	Context("Scenario D: Full MCP-native await_approval flow", func() {
		It("should return pending_approval on a write tool call, block in aip/await_approval, and execute after human approval", func() {
			mcpURL := fmt.Sprintf("%s/mcp", gwURL)

			By("calling tools/call for github/create_pull_request without an AIP JWT — expect pending_approval")
			// proposedMaxReplicas=17 triggers the require-human-approval SafetyPolicy rule
			// (ratio 0.85 < 0.9, so the deny rule does NOT fire). The AR goes to Pending,
			// requiring a human reviewer to approve before aip/await_approval can unblock.
			submitRPC := fmt.Sprintf(`{
				"jsonrpc": "2.0",
				"id": 20,
				"method": "tools/call",
				"params": {
					"name": "github/create_pull_request",
					"arguments": {
						"_aip_target_uri": "github://%s/%s/files/%s/%s",
						"owner": "%s",
						"repo": "%s",
						"title": "[e2e-test] await_approval flow",
						"body": "MCP-native await_approval e2e test.",
						"head": "%s",
						"base": "%s",
						"draft": true,
						"proposedMaxReplicas": 17
					}
				}
			}`, githubOwner, githubRepo, githubConfigFileBranch, githubConfigFilePath, githubOwner, githubRepo, e2eTestBranch3, testBranch)

			submitReq, err := http.NewRequest("POST", mcpURL, strings.NewReader(submitRPC))
			Expect(err).NotTo(HaveOccurred())
			submitReq.Header.Set("Content-Type", "application/json")
			submitReq.Header.Set("X-Remote-User", "e2e-mcp-agent-d")

			submitResp, err := http.DefaultClient.Do(submitReq)
			Expect(err).NotTo(HaveOccurred())
			defer submitResp.Body.Close()
			submitBodyBytes, err := io.ReadAll(submitResp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(submitResp.StatusCode).To(Equal(http.StatusOK), "tools/call: %s", string(submitBodyBytes))

			By("verifying the response is pending_approval with a requestId")
			var submitRPCResp struct {
				Result *struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result,omitempty"`
			}
			Expect(json.Unmarshal(submitBodyBytes, &submitRPCResp)).To(Succeed())
			Expect(submitRPCResp.Result).NotTo(BeNil())
			Expect(submitRPCResp.Result.Content).NotTo(BeEmpty())

			var pendingPayload struct {
				Status    string `json:"status"`
				RequestID string `json:"requestId"`
			}
			Expect(json.Unmarshal([]byte(submitRPCResp.Result.Content[0].Text), &pendingPayload)).To(Succeed())
			Expect(pendingPayload.Status).To(Equal("pending_approval"))
			Expect(pendingPayload.RequestID).NotTo(BeEmpty())
			requestID := pendingPayload.RequestID
			_, _ = fmt.Fprintf(GinkgoWriter, "AgentRequest created by tools/call: %s\n", requestID)

			By("starting aip/await_approval in background — will block until the AR is approved")
			type awaitResult struct {
				jwt string
				err error
			}
			awaitCh := make(chan awaitResult, 1)

			go func() {
				awaitRPC := fmt.Sprintf(`{
					"jsonrpc": "2.0",
					"id": 21,
					"method": "tools/call",
					"params": {
						"name": "aip/await_approval",
						"arguments": {"requestId": "%s"}
					}
				}`, requestID)

				awaitReq, reqErr := http.NewRequest("POST", mcpURL, strings.NewReader(awaitRPC))
				if reqErr != nil {
					awaitCh <- awaitResult{err: reqErr}
					return
				}
				awaitReq.Header.Set("Content-Type", "application/json")

				// Client timeout must exceed the gateway --wait-timeout (90s in BeforeSuite).
				awaitClient := &http.Client{Timeout: 3 * time.Minute}
				awaitResp, respErr := awaitClient.Do(awaitReq)
				if respErr != nil {
					awaitCh <- awaitResult{err: respErr}
					return
				}
				defer awaitResp.Body.Close()
				awaitBodyBytes, readErr := io.ReadAll(awaitResp.Body)
				if readErr != nil {
					awaitCh <- awaitResult{err: readErr}
					return
				}

				var awaitRPCResp struct {
					Result *struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result,omitempty"`
					Error *struct {
						Message string `json:"message"`
					} `json:"error,omitempty"`
				}
				if jsonErr := json.Unmarshal(awaitBodyBytes, &awaitRPCResp); jsonErr != nil {
					awaitCh <- awaitResult{err: fmt.Errorf("unmarshal await_approval response: %w; body: %s", jsonErr, string(awaitBodyBytes))}
					return
				}
				if awaitRPCResp.Error != nil {
					awaitCh <- awaitResult{err: fmt.Errorf("await_approval JSON-RPC error: %s", awaitRPCResp.Error.Message)}
					return
				}
				if awaitRPCResp.Result == nil || len(awaitRPCResp.Result.Content) == 0 {
					awaitCh <- awaitResult{err: fmt.Errorf("empty await_approval result: %s", string(awaitBodyBytes))}
					return
				}

				var approvedPayload struct {
					Status string `json:"status"`
					JWT    string `json:"jwt"`
				}
				if jsonErr := json.Unmarshal([]byte(awaitRPCResp.Result.Content[0].Text), &approvedPayload); jsonErr != nil {
					awaitCh <- awaitResult{err: fmt.Errorf("unmarshal approved payload: %w", jsonErr)}
					return
				}
				if approvedPayload.Status != "approved" {
					awaitCh <- awaitResult{err: fmt.Errorf("unexpected await_approval status: %s (full: %s)", approvedPayload.Status, awaitRPCResp.Result.Content[0].Text)}
					return
				}
				awaitCh <- awaitResult{jwt: approvedPayload.JWT}
			}()

			By("waiting briefly for await_approval to be blocking, then approving via gateway REST API")
			time.Sleep(2 * time.Second)

			approveURL := fmt.Sprintf("%s/agent-requests/%s/approve?namespace=%s", gwURL, requestID, reqNamespace)
			approveReq, err := http.NewRequest("POST", approveURL, bytes.NewReader([]byte(`{"reason":"e2e await_approval test"}`)))
			Expect(err).NotTo(HaveOccurred())
			approveReq.Header.Set("Content-Type", "application/json")
			approveReq.Header.Set("X-Remote-User", "e2e-reviewer")
			approveReq.Header.Set("X-Remote-Groups", "reviewers")

			approveResp, err := http.DefaultClient.Do(approveReq)
			Expect(err).NotTo(HaveOccurred())
			defer approveResp.Body.Close()
			approveBodyBytes, err := io.ReadAll(approveResp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(approveResp.StatusCode).To(Equal(http.StatusOK), "approve endpoint: %s", string(approveBodyBytes))
			_, _ = fmt.Fprintf(GinkgoWriter, "Approved AgentRequest %s\n", requestID)

			By("receiving JWT from aip/await_approval")
			var res awaitResult
			Eventually(awaitCh, 2*time.Minute).Should(Receive(&res))
			if res.err != nil {
				// Capture AR status for diagnosis before failing.
				diagCmd := exec.Command("kubectl", "get", "agentrequest", requestID, "-n", reqNamespace, "-o", "json")
				diagOut, _ := runCmd(diagCmd)
				_, _ = fmt.Fprintf(GinkgoWriter, "DIAG AR status: %s\n", diagOut)
			}
			Expect(res.err).NotTo(HaveOccurred())
			Expect(res.jwt).NotTo(BeEmpty())
			_, _ = fmt.Fprintf(GinkgoWriter, "aip/await_approval returned JWT (len=%d)\n", len(res.jwt))

			By("re-calling github/create_pull_request via POST /mcp with _aip_authorization from aip/await_approval")
			execRPC := fmt.Sprintf(`{
				"jsonrpc": "2.0",
				"id": 22,
				"method": "tools/call",
				"params": {
					"name": "github/create_pull_request",
					"arguments": {
						"_aip_authorization": "%s",
						"owner": "%s",
						"repo": "%s",
						"title": "[e2e-test] await_approval flow",
						"body": "MCP-native await_approval e2e test.\n\nApproved via aip/await_approval.",
						"head": "%s",
						"base": "%s",
						"draft": true,
						"proposedMaxReplicas": 17
					}
				}
			}`, res.jwt, githubOwner, githubRepo, e2eTestBranch3, testBranch)

			execReq, err := http.NewRequest("POST", mcpURL, strings.NewReader(execRPC))
			Expect(err).NotTo(HaveOccurred())
			execReq.Header.Set("Content-Type", "application/json")
			execReq.Header.Set("X-Remote-User", "e2e-mcp-agent-d")

			execResp, err := http.DefaultClient.Do(execReq)
			Expect(err).NotTo(HaveOccurred())
			defer execResp.Body.Close()
			execBodyBytes, err := io.ReadAll(execResp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(execResp.StatusCode).To(Equal(http.StatusOK), "POST /mcp exec: %s", string(execBodyBytes))

			By("verifying the JSON-RPC response contains a GitHub PR URL")
			var execRPCResp struct {
				Result *struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result,omitempty"`
				Error *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error,omitempty"`
			}
			Expect(json.Unmarshal(execBodyBytes, &execRPCResp)).To(Succeed())
			Expect(execRPCResp.Error).To(BeNil(), "JSON-RPC error: %+v", execRPCResp.Error)
			Expect(execRPCResp.Result).NotTo(BeNil())
			Expect(execRPCResp.Result.Content).NotTo(BeEmpty())
			Expect(execRPCResp.Result.Content[0].Text).To(ContainSubstring("/pull/"))
			_, _ = fmt.Fprintf(GinkgoWriter, "PR created via aip/await_approval flow: %s\n", execRPCResp.Result.Content[0].Text)
		})
	})
})
