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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Phase 6: Gateway v1alpha1 API", Ordered, func() {
	Context("v1alpha1 AgentDiagnostic API", Ordered, func() {
		const (
			v1DiagCorrID = "gw-e2e-v1a1-diag-corr-001"
			v1DiagAgent  = "gw-e2e-v1a1-agent"
			v1DiagType   = "observation"
		)
		var v1DiagName string

		It("creates an AgentDiagnostic via POST /v1alpha1/agent-diagnostics and returns 201", func() {
			resp, err := gwPost("/v1alpha1/agent-diagnostics", fmt.Sprintf(`{
				"agentIdentity":  %q,
				"diagnosticType": %q,
				"correlationID":  %q,
				"summary":        "gateway v1alpha1 e2e: scheduler pressure detected"
			}`, v1DiagAgent, v1DiagType, v1DiagCorrID))
			Expect(err).NotTo(HaveOccurred())
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusCreated), string(bodyBytes))

			var body map[string]interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			v1DiagName, _ = body["name"].(string)
			Expect(v1DiagName).NotTo(BeEmpty())
			Expect(body["agentIdentity"]).To(Equal(v1DiagAgent))
			Expect(body["diagnosticType"]).To(Equal(v1DiagType))
		})

		It("GET /v1alpha1/agent-diagnostics lists the created diagnostic", func() {
			Eventually(func(g Gomega) {
				resp, err := http.Get(gwBaseURL + "/v1alpha1/agent-diagnostics") //nolint:noctx
				g.Expect(err).NotTo(HaveOccurred())
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				g.Expect(resp.StatusCode).To(Equal(http.StatusOK),
					"gateway returned non-200; body: %s", string(body))
				var envelope struct {
					Items []interface{} `json:"items"`
				}
				g.Expect(json.Unmarshal(body, &envelope)).To(Succeed(),
					"failed to decode response envelope; body: %s", string(body))
				g.Expect(len(envelope.Items)).To(BeNumerically(">=", 1),
					"expected at least 1 item; body: %s", string(body))
			}, 15*time.Second, time.Second).Should(Succeed())
		})

		It("GET /v1alpha1/agent-diagnostics/{name} returns the diagnostic by name", func() {
			resp, err := http.Get(gwBaseURL + "/v1alpha1/agent-diagnostics/" + v1DiagName) //nolint:noctx
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			var body map[string]interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body["name"]).To(Equal(v1DiagName))
			Expect(body["agentIdentity"]).To(Equal(v1DiagAgent))
		})

		It("PATCH /v1alpha1/agent-diagnostics/{name}/status sets a verdict", func() {
			resp, err := gwPatch("/v1alpha1/agent-diagnostics/"+v1DiagName+"/status",
				`{"verdict":"correct"}`)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close() //nolint:errcheck
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			var body map[string]interface{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body["message"]).To(Equal("verdict saved"))
		})
	})
})
