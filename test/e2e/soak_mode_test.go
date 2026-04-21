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
	"crypto/sha256"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/test/utils"
)

// soakSummaryName mirrors the naming logic in internal/controller and cmd/gateway
// so the e2e test can look up the correct DiagnosticAccuracySummary.
func soakSummaryName(agentIdentity string) string {
	h := sha256.Sum256([]byte(agentIdentity))
	suffix := fmt.Sprintf("%x", h[:4])
	re := regexp.MustCompile(`[^a-z0-9-]`)
	prefix := strings.ToLower(agentIdentity)
	prefix = re.ReplaceAllString(prefix, "-")
	if len(prefix) > 54 {
		prefix = prefix[:54]
	}
	prefix = strings.Trim(prefix, "-")
	if prefix == "" {
		prefix = "agent"
	}
	return prefix + "-" + suffix
}

var _ = Describe("SoakMode and Accuracy Tracking", Ordered, func() {
	const (
		grName        = "soak-mode-gr"
		reqName       = "soak-mode-req"
		ns            = "default"
		agentIdentity = "soak-mode-agent"
		targetURI     = "k8s://soak/resource"
	)

	var grGeneration int64

	BeforeAll(func() {
		By("creating a GovernedResource with soakMode: true")
		grJSON := fmt.Sprintf(`{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind": "GovernedResource",
			"metadata": {"name": "%s"},
			"spec": {
				"uriPattern": "k8s://soak/*",
				"permittedActions": ["test"],
				"contextFetcher": "none",
				"soakMode": true
			}
		}`, grName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(grJSON)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for GovernedResource and capturing its generation")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "governedresource", grName,
				"-o", "jsonpath={.metadata.generation}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			gen, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(gen).To(BeNumerically(">", 0))
			grGeneration = gen
		}, 10*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("cleaning up resources")
		cmd := exec.Command("kubectl", "delete", "governedresource", "--all", "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "agentrequest", "--all", "-n", ns, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "diagnosticaccuracysummary", "--all", "-n", ns, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	It("should route AgentRequest to AwaitingVerdict and update accuracy on verdict", func() {
		By("creating an AgentRequest with governedResourceRef pointing to the soak-mode GR")
		reqJSON := fmt.Sprintf(`{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind": "AgentRequest",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"agentIdentity": "%s",
				"action": "test",
				"target": {"uri": "%s"},
				"reason": "soak test",
				"governedResourceRef": {"name": "%s", "generation": %d}
			}
		}`, reqName, ns, agentIdentity, targetURI, grName, grGeneration)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(reqJSON)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for Phase=AwaitingVerdict")
		Eventually(func() string {
			return getAgentRequestPhase(reqName, ns)
		}, 30*time.Second).Should(Equal("AwaitingVerdict"))

		By("submitting a verdict via status patch")
		verdictAt := time.Now().Format(time.RFC3339)
		verdictPatch := fmt.Sprintf(`{
			"status": {
				"phase": "Completed",
				"verdict": "correct",
				"verdictBy": "e2e-tester",
				"verdictAt": "%s"
			}
		}`, verdictAt)
		cmd = exec.Command("kubectl", "patch", "agentrequest", reqName, "-n", ns,
			"--subresource=status", "--type=merge", "-p", verdictPatch)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for Phase=Completed")
		Eventually(func() string {
			return getAgentRequestPhase(reqName, ns)
		}, 30*time.Second).Should(Equal("Completed"))

		By("verifying DiagnosticAccuracySummary is updated")
		summaryName := soakSummaryName(agentIdentity)
		Eventually(func(g Gomega) {
			var summary governancev1alpha1.DiagnosticAccuracySummary
			err := k8sClient.Get(ctx, types.NamespacedName{Name: summaryName, Namespace: ns}, &summary)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(summary.Status.TotalReviewed).To(Equal(int64(1)))
			g.Expect(summary.Status.CorrectCount).To(Equal(int64(1)))
			g.Expect(summary.Status.DiagnosticAccuracy).NotTo(BeNil())
			g.Expect(*summary.Status.DiagnosticAccuracy).To(BeNumerically("~", 1.0, 0.001))
		}, 30*time.Second).Should(Succeed())
	})
})
