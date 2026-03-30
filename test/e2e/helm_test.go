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
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ravisantoshgudimetla/aip-k8s/test/utils"
)

// Chart e2e tests run only when GATEWAY_URL and DASHBOARD_URL are set.
// In the post-publish workflow these point to port-forwarded services.
// In the standard pre-merge e2e they are unset and the suite is skipped.

var _ = Describe("Chart", Ordered, func() {
	var (
		gatewayURL   string
		dashboardURL string
	)

	BeforeAll(func() {
		gatewayURL = os.Getenv("GATEWAY_URL")
		dashboardURL = os.Getenv("DASHBOARD_URL")
		if gatewayURL == "" || dashboardURL == "" {
			Skip("GATEWAY_URL and DASHBOARD_URL not set — skipping chart e2e")
		}
		if tag := os.Getenv("IMAGE_TAG"); tag != "" {
			GinkgoLogr.Info("chart e2e running against image tag", "tag", tag)
		}

		By("waiting for gateway /healthz to be reachable")
		Eventually(func() int {
			resp, err := http.Get(gatewayURL + "/healthz") //nolint:noctx
			if err != nil {
				return 0
			}
			defer resp.Body.Close() //nolint:errcheck
			return resp.StatusCode
		}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

		By("waiting for dashboard /healthz to be reachable")
		Eventually(func() int {
			resp, err := http.Get(dashboardURL + "/healthz") //nolint:noctx
			if err != nil {
				return 0
			}
			defer resp.Body.Close() //nolint:errcheck
			return resp.StatusCode
		}, 30*time.Second, time.Second).Should(Equal(http.StatusOK))

		By("cleaning up stale chart e2e resources from previous runs")
		chartCleanup()
	})

	AfterAll(func() {
		By("cleaning up chart e2e resources")
		chartCleanup()
	})

	Context("Gateway", func() {
		It("serves /healthz", func() {
			resp := httpGet(gatewayURL + "/healthz")
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(bodyString(resp)).To(Equal("ok"))
		})

		It("serves /readyz once K8s API is reachable", func() {
			Eventually(func() int {
				resp, err := http.Get(gatewayURL + "/readyz") //nolint:noctx
				if err != nil {
					return 0
				}
				defer resp.Body.Close() //nolint:errcheck
				return resp.StatusCode
			}, 30*time.Second, 2*time.Second).Should(Equal(http.StatusOK))
		})

		It("returns non-null list for GET /agent-requests", func() {
			resp := httpGet(gatewayURL + "/agent-requests")
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			var items []interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&items)).To(Succeed())
			Expect(items).NotTo(BeNil())
		})

		It("creates an AgentRequest via POST /agent-requests", func() {
			body := strings.NewReader(`{
				"agentIdentity": "chart-e2e-agent",
				"action":        "scale-down",
				"targetURI":     "k8s://default/deployments/smoke-app",
				"reason":        "chart e2e smoke test"
			}`)
			resp, err := http.Post(gatewayURL+"/agent-requests", "application/json", body) //nolint:noctx
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))
		})

		It("lists the created AgentRequest", func() {
			Eventually(func(g Gomega) {
				resp, err := http.Get(gatewayURL + "/agent-requests") //nolint:noctx
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
			}, 15*time.Second, 2*time.Second).Should(Succeed())
		})

		It("returns 409 on a duplicate POST /agent-requests", func() {
			// The dedup key is (agentIdentity, action, targetURI); reason is excluded by design.
			// Using a different reason here intentionally exercises that the gateway does not
			// treat reason as part of the dedup key (see checkDuplicate in cmd/gateway/main.go).
			body := strings.NewReader(`{
				"agentIdentity": "chart-e2e-agent",
				"action":        "scale-down",
				"targetURI":     "k8s://default/deployments/smoke-app",
				"reason":        "duplicate"
			}`)
			resp, err := http.Post(gatewayURL+"/agent-requests", "application/json", body) //nolint:noctx
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusConflict))
		})
	})

	// Governance is the canary for the full helm chart stack: it proves that the
	// gateway, CRDs, controller, policy evaluation, and OpsLock acquisition are all
	// wired correctly in the deployed chart — not just that the pods started.
	// The gateway polls synchronously and returns the terminal phase in the 201 body,
	// so a single POST is sufficient; no separate polling loop is needed here.
	Context("Governance", func() {
		It("controller approves an AgentRequest submitted via the gateway", func() {
			body := strings.NewReader(`{
				"agentIdentity": "chart-e2e-agent",
				"action":        "scale-down",
				"targetURI":     "k8s://default/deployments/governance-canary",
				"reason":        "chart e2e governance check"
			}`)
			resp, err := http.Post(gatewayURL+"/agent-requests", "application/json", body) //nolint:noctx
			Expect(err).NotTo(HaveOccurred())
			respBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusCreated),
				"gateway returned non-201; body: %s", string(respBody))
			var result map[string]interface{}
			Expect(json.Unmarshal(respBody, &result)).To(Succeed(),
				"failed to decode POST response; body: %s", string(respBody))
			Expect(result["name"]).NotTo(BeEmpty(), "response missing name field")
			Expect(result["phase"]).To(Equal("Approved"),
				"expected Approved but got %q — controller may not be reconciling (RBAC, image, webhook?)",
				result["phase"])
		})
	})

	Context("Dashboard", func() {
		It("serves /healthz", func() {
			resp := httpGet(dashboardURL + "/healthz")
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(bodyString(resp)).To(Equal("ok"))
		})

		It("serves the index page at /", func() {
			resp := httpGet(dashboardURL + "/")
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(bodyString(resp)).To(ContainSubstring("AIP Visual Audit"))
		})

		It("serves static assets", func() {
			for _, asset := range []string{"/app.js", "/styles.css"} {
				resp := httpGet(dashboardURL + asset)
				Expect(resp.StatusCode).To(Equal(http.StatusOK), "asset %s", asset)
				_ = resp.Body.Close()
			}
		})

		It("proxies GET /api/agent-requests to the gateway", func() {
			resp := httpGet(dashboardURL + "/api/agent-requests")
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			var items []interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&items)).To(Succeed())
			Expect(len(items)).To(BeNumerically(">=", 1))
		})
	})
})

const chartNS = "aip-k8s-system"

func httpGet(url string) *http.Response {
	resp, err := http.Get(url) //nolint:noctx
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "GET %s", url)
	return resp
}

func bodyString(resp *http.Response) string {
	defer resp.Body.Close() //nolint:errcheck
	b, err := io.ReadAll(resp.Body)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return strings.TrimSpace(string(b))
}

// chartCleanup removes AgentRequests, AuditRecords, and OpsLock Leases from the
// default namespace so each chart e2e run starts from a clean state.
func chartCleanup() {
	for _, res := range []string{"agentrequest", "auditrecord"} {
		cmd := exec.Command("kubectl", "delete", res, "--all", "-n", "default", "--ignore-not-found")
		_, _ = utils.Run(cmd)
	}
	// Remove OpsLock Leases so the next run is not blocked by stale locks.
	cmd := exec.Command("bash", "-c",
		`kubectl get lease -n default -o name 2>/dev/null | grep aip-lock- | xargs -r kubectl delete -n default`)
	_, _ = utils.Run(cmd)
}
