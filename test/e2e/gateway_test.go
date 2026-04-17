//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
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

	"k8s.io/apimachinery/pkg/types"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/test/utils"
)

// Phase 6: Gateway API — exercises the full agent path through the gateway
// HTTP API. The gateway binary is built from source and started as a subprocess
// pointing at the Kind cluster, so these tests run automatically as part of
// make test-e2e with no extra configuration.

const (
	gwAddr    = ":18081"
	gwBaseURL = "http://localhost:18081"
	gwNS      = "default"
)

var _ = Describe("Phase 6: Gateway API", Ordered, func() {
	var gwProc *exec.Cmd

	BeforeAll(func() {
		projDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred(), "failed to get project dir")
		binPath := projDir + "/bin/gateway"
		cmdPath := projDir + "/cmd/gateway"

		var cmd *exec.Cmd
		var out []byte

		By("ensuring governance CRDs are installed")
		if os.Getenv("HELM_DEPLOYED") != "true" {
			cmd = exec.Command("make", "install")
			cmd.Dir = projDir
			out, err = cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "failed to install CRDs: %s", string(out))
		} else {
			By("skipping make install; HELM_DEPLOYED=true")
		}

		// Deploy the controller only when it is not already running.
		// In the full suite (make test-e2e) the Manager BeforeAll deploys it first;
		// calling make deploy again would re-create the aip-k8s-system namespace
		// and conflict with Manager's own kubectl create ns. In the chart-e2e
		// workflow the images are loaded under ghcr.io tags, not managerImage, so
		// make deploy would pull a non-existent image and fail. Skip make deploy
		// when HELM_DEPLOYED=true (chart already deployed the controller).
		By("ensuring controller is deployed (skips if already running)")
		if os.Getenv("HELM_DEPLOYED") != "true" {
			checkCmd := exec.Command("kubectl", "get", "deployment",
				"aip-k8s-controller", "-n", "aip-k8s-system")
			if _, checkErr := utils.Run(checkCmd); checkErr != nil {
				cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
				cmd.Dir = projDir
				out, err = cmd.CombinedOutput()
				Expect(err).NotTo(HaveOccurred(), "failed to deploy controller: %s", string(out))
			}
		} else {
			By("skipping make deploy; HELM_DEPLOYED=true")
		}

		By("waiting for controller-manager to be ready")
		Eventually(func(g Gomega) {
			readyCmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-n", "aip-k8s-system",
				"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
			status, err := utils.Run(readyCmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"), "controller-manager pod not yet ready")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("building the gateway binary")
		cmd = exec.Command("go", "build", "-o", binPath, cmdPath)
		cmd.Dir = projDir
		out, err = cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "failed to build gateway: %s", string(out))

		By("starting the gateway subprocess")
		// The gateway does not support the --namespace flag
		gwProc = exec.Command(binPath, "--addr="+gwAddr)
		gwProc.Dir = projDir
		gwProc.Stdout = GinkgoWriter
		gwProc.Stderr = GinkgoWriter
		Expect(gwProc.Start()).To(Succeed(), "failed to start gateway")

		By("waiting for gateway /healthz to be ready")
		Eventually(func() int {
			resp, err := http.Get(gwBaseURL + "/healthz") //nolint:noctx
			if err != nil {
				return 0
			}
			defer resp.Body.Close() //nolint:errcheck
			return resp.StatusCode
		}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

		By("cleaning up any stale resources from previous runs")
		gwCleanup(gwNS)
	})

	AfterAll(func() {
		if gwProc != nil && gwProc.Process != nil {
			_ = gwProc.Process.Kill()
		}
		By("cleaning up gateway e2e resources")
		gwCleanup(gwNS)
	})

	Context("AgentRequest CRUD", func() {
		var createdName string

		It("creates an AgentRequest via POST /agent-requests and returns 201", func() {
			resp, err := gwPost("/agent-requests", `{
				"agentIdentity": "gw-e2e-agent",
				"action":        "gw-e2e-action",
				"targetURI":     "k8s://dev/default/deployment/gw-app",
				"reason":        "gateway e2e smoke test",
				"correlationID": "gw-e2e-corr-001"
			}`)
			Expect(err).NotTo(HaveOccurred())
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusCreated), string(bodyBytes))

			var body map[string]interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			createdName, _ = body["name"].(string)
			Expect(createdName).NotTo(BeEmpty())
		})

		It("CRD is visible in the cluster after creation", func() {
			Eventually(func(g Gomega) {
				var ar governancev1alpha1.AgentRequest
				g.Expect(k8sClient.Get(ctx,
					types.NamespacedName{Name: createdName, Namespace: gwNS}, &ar),
				).To(Succeed())
			}, 15*time.Second, time.Second).Should(Succeed())
		})

		It("GET /agent-requests lists the created request", func() {
			Eventually(func(g Gomega) {
				resp, err := http.Get(gwBaseURL + "/agent-requests") //nolint:noctx
				g.Expect(err).NotTo(HaveOccurred())
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				g.Expect(resp.StatusCode).To(Equal(http.StatusOK),
					"gateway returned non-200; body: %s", string(body))
				var items []interface{}
				g.Expect(json.Unmarshal(body, &items)).To(Succeed(),
					"failed to decode response as JSON array; body: %s", string(body))
				g.Expect(len(items)).To(BeNumerically(">=", 1),
					"expected at least 1 item; body: %s", string(body))
			}, 15*time.Second, time.Second).Should(Succeed())
		})

		It("GET /agent-requests/{name} returns the specific request", func() {
			resp, err := http.Get(gwBaseURL + "/agent-requests/" + createdName) //nolint:noctx
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			var body map[string]interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			// The gateway returns a flat object with "name" at the top level, not nested under "metadata".
			Expect(body["name"]).To(Equal(createdName))
		})

		It("controller reconciles the request to Approved", func() {
			Eventually(func() string {
				return getAgentRequestPhase(createdName, gwNS)
			}, 2*time.Minute, 2*time.Second).Should(Equal("Approved"))
		})

		It("returns 200 OK on a duplicate POST /agent-requests (idempotent)", func() {
			// The dedup key is (agentIdentity, action, targetURI); reason is excluded by design.
			// Using a different reason here intentionally exercises that the gateway does not
			// treat reason as part of the dedup key (see checkDuplicate in cmd/gateway/main.go).
			resp, err := gwPost("/agent-requests", `{
				"agentIdentity": "gw-e2e-agent",
				"action":        "gw-e2e-action",
				"targetURI":     "k8s://dev/default/deployment/gw-app",
				"reason":        "duplicate"
			}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
		})
	})

	Context("Human decision flow", Ordered, func() {
		const policyName = "gw-require-human"
		var pendingName string

		BeforeAll(func() {
			By("creating SafetyPolicy that requires human approval")
			policyJSON := fmt.Sprintf(`{
				"apiVersion": "governance.aip.io/v1alpha1",
				"kind": "SafetyPolicy",
				"metadata": {"name": %q, "namespace": %q},
				"spec": {
					"governedResourceSelector": {},
					"rules": [{"name": "require-human", "type": "StateEvaluation", "action": "RequireApproval", "expression": "true"}],
					"failureMode": "FailClosed"
				}
			}`, policyName, gwNS)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(policyJSON)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for SafetyPolicy to be visible")
			Eventually(func(g Gomega) {
				var sp governancev1alpha1.SafetyPolicy
				g.Expect(k8sClient.Get(ctx,
					types.NamespacedName{Name: policyName, Namespace: gwNS}, &sp),
				).To(Succeed())
			}, 15*time.Second, time.Second).Should(Succeed())
		})

		It("POST /agent-requests creates a request held for human approval", func() {
			resp, err := gwPost("/agent-requests", `{
				"agentIdentity": "gw-e2e-agent",
				"action":        "gw-human-action",
				"targetURI":     "k8s://dev/default/deployment/human-app",
				"reason":        "gateway human decision e2e"
			}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))
			var body map[string]interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			pendingName, _ = body["name"].(string)
			Expect(pendingName).NotTo(BeEmpty())
		})

		It("controller sets RequiresApproval condition and holds at Pending", func() {
			Eventually(func(g Gomega) {
				var ar governancev1alpha1.AgentRequest
				g.Expect(k8sClient.Get(ctx,
					types.NamespacedName{Name: pendingName, Namespace: gwNS}, &ar),
				).To(Succeed())
				g.Expect(ar.Status.Phase).To(Equal(governancev1alpha1.PhasePending))
				var hasCondition bool
				for _, c := range ar.Status.Conditions {
					if c.Type == governancev1alpha1.ConditionRequiresApproval && c.Status == "True" {
						hasCondition = true
					}
				}
				g.Expect(hasCondition).To(BeTrue(), "expected RequiresApproval=True condition")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("POST /agent-requests/{name}/approve transitions to Approved", func() {
			resp, err := gwPost("/agent-requests/"+pendingName+"/approve", `{"reason":"e2e human approval"}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			Eventually(func() string {
				return getAgentRequestPhase(pendingName, gwNS)
			}, 30*time.Second, time.Second).Should(Equal("Approved"))
		})

		It("AuditRecord for request.approved is emitted", func() {
			Eventually(func() bool {
				return auditRecordExists(pendingName, gwNS, "request.approved")
			}, 30*time.Second, time.Second).Should(BeTrue())
		})

		It("POST /agent-requests/{name}/deny transitions a pending request to Denied", func() {
			By("creating a second request held for approval")
			resp, err := gwPost("/agent-requests", `{
				"agentIdentity": "gw-e2e-agent",
				"action":        "gw-human-action",
				"targetURI":     "k8s://dev/default/deployment/deny-app",
				"reason":        "gateway human deny e2e"
			}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))
			var body map[string]interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			denyName, _ := body["name"].(string)
			Expect(denyName).NotTo(BeEmpty())

			By("waiting for RequiresApproval condition")
			Eventually(func() string {
				return getAgentRequestPhase(denyName, gwNS)
			}, 2*time.Minute, 2*time.Second).Should(Equal("Pending"))

			By("denying via gateway")
			resp2, err := gwPost("/agent-requests/"+denyName+"/deny", `{"reason":"e2e human deny"}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp2.Body.Close() //nolint:errcheck
			Expect(resp2.StatusCode).To(Equal(http.StatusOK))

			Eventually(func() string {
				return getAgentRequestPhase(denyName, gwNS)
			}, 30*time.Second, time.Second).Should(Equal("Denied"))
		})
	})

	Context("AgentDiagnostic", func() {
		const (
			diagCorrID = "gw-e2e-diag-corr-001"
			diagAgent  = "gw-e2e-diag-agent"
			diagType   = "observation"
		)

		It("creates an AgentDiagnostic via POST /agent-diagnostics and returns 201", func() {
			resp, err := gwPost("/agent-diagnostics", fmt.Sprintf(`{
				"agentIdentity":  %q,
				"diagnosticType": %q,
				"correlationID":  %q,
				"summary":        "gateway e2e: scheduler pressure detected"
			}`, diagAgent, diagType, diagCorrID))
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))
		})

		It("GET /agent-diagnostics lists the created diagnostic", func() {
			Eventually(func(g Gomega) {
				resp, err := http.Get(gwBaseURL + "/agent-diagnostics") //nolint:noctx
				g.Expect(err).NotTo(HaveOccurred())
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				g.Expect(resp.StatusCode).To(Equal(http.StatusOK),
					"gateway returned non-200; body: %s", string(body))
				var items []interface{}
				g.Expect(json.Unmarshal(body, &items)).To(Succeed(),
					"failed to decode response as JSON array; body: %s", string(body))
				g.Expect(len(items)).To(BeNumerically(">=", 1),
					"expected at least 1 item; body: %s", string(body))
			}, 15*time.Second, time.Second).Should(Succeed())
		})

		It("GET /agent-diagnostics?correlationID filters correctly", func() {
			Eventually(func(g Gomega) {
				resp, err := http.Get(gwBaseURL + "/agent-diagnostics?correlationID=" + diagCorrID) //nolint:noctx
				g.Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close() //nolint:errcheck
				var items []map[string]interface{}
				g.Expect(json.NewDecoder(resp.Body).Decode(&items)).To(Succeed())
				g.Expect(items).To(HaveLen(1))
				spec, _ := items[0]["spec"].(map[string]interface{})
				g.Expect(spec["correlationID"]).To(Equal(diagCorrID))
			}, 15*time.Second, time.Second).Should(Succeed())
		})

		It("returns 200 OK on a duplicate POST /agent-diagnostics (idempotent)", func() {
			resp, err := gwPost("/agent-diagnostics", fmt.Sprintf(`{
				"agentIdentity":  %q,
				"diagnosticType": %q,
				"correlationID":  %q,
				"summary":        "duplicate diagnostic"
			}`, diagAgent, diagType, diagCorrID))
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
		})
	})
})

// gwPost posts JSON to the gateway and returns the response.
func gwPost(path, body string) (*http.Response, error) {
	return http.Post(gwBaseURL+path, "application/json", strings.NewReader(body)) //nolint:noctx
}

// gwCleanup removes all AgentRequests, AgentDiagnostics, OpsLock Leases, and
// the human-approval SafetyPolicy in ns. The gateway does not stamp a
// test-specific label on resources it creates, so we delete all rather than
// relying on a label selector.
// OpsLock Leases must be deleted explicitly: deleting the AgentRequest does not
// synchronously release the Lease, so a stale Lease from a previous run blocks
// the next request with "Lock contention" until LOCK_TIMEOUT expires.
func gwCleanup(ns string) {
	for _, res := range []string{"agentrequest", "agentdiagnostic"} {
		cmd := exec.Command("kubectl", "delete", res, "--all", "-n", ns, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	}
	// Delete OpsLock Leases (named aip-lock-<hash>).
	cmd := exec.Command("bash", "-c",
		"kubectl get lease -n "+ns+" -o name 2>/dev/null | grep aip-lock- | xargs -r kubectl delete -n "+ns)
	_, _ = utils.Run(cmd)
	cmd = exec.Command("kubectl", "delete", "safetypolicy", "--all", "-n", ns, "--ignore-not-found")
	_, _ = utils.Run(cmd)
}

// gwReadBody reads and closes the response body.
func gwReadBody(resp *http.Response) string {
	defer resp.Body.Close() //nolint:errcheck
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
