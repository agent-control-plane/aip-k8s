//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/agent-control-plane/aip-k8s/test/utils"
)

var _ = Describe("AgentRequest GC", Ordered, func() {
	const (
		ns     = "default"
		grName = "gc-test-gr"
	)

	BeforeAll(func() {
		projDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred(), "failed to get project dir")

		By("ensuring governance CRDs are installed")
		if os.Getenv("HELM_DEPLOYED") != "true" {
			cmd := exec.Command("make", "install")
			cmd.Dir = projDir
			out, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "failed to install CRDs: %s", string(out))
		}

		By("waiting for controller-manager to be ready")
		Eventually(func(g Gomega) {
			readyCmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-n", "aip-k8s-system",
				"-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
			status, err := utils.Run(readyCmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("creating a GovernedResource")
		grJSON := fmt.Sprintf(`{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind": "GovernedResource",
			"metadata": {"name": "%s"},
			"spec": {
				"uriPattern": "k8s://default/deployment/*",
				"permittedActions": ["scale"],
				"contextFetcher": "none"
			}
		}`, grName)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(grJSON)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("cleaning up resources")
		cmd := exec.Command("kubectl", "delete", "agentrequest", "--all", "-n", ns, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "auditrecord", "--all", "-n", ns, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "governedresource", "--all", "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	It("should delete a terminal AgentRequest and its AuditRecords", func() {
		arName := "gc-terminal-ar"

		By("creating an AgentRequest")
		arJSON := fmt.Sprintf(`{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind": "AgentRequest",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"agentIdentity": "gc-e2e-agent",
				"action": "scale",
				"target": {"uri": "k8s://default/deployment/gc-target"},
				"reason": "testing GC"
			}
		}`, arName, ns)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(arJSON)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for Phase=Approved")
		Eventually(func() string {
			out, _ := utils.Run(exec.Command("kubectl", "get", "agentrequest", arName,
				"-n", ns, "-o", "jsonpath={.status.phase}"))
			return out
		}, 30*time.Second, time.Second).Should(Equal("Approved"))

		By("patching status to Phase=Completed")
		cmd = exec.Command("kubectl", "patch", "agentrequest", arName,
			"-n", ns, "--subresource=status", "--type=merge",
			"-p", `{"status":{"phase":"Completed"}}`)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the AgentRequest to be deleted by GC")
		// GC interval=1m, hardTTL=1m. Worst case: object created just after a tick →
		// waits up to 2 cycles (2m) plus slow-CI overhead. 6 minutes gives headroom.
		Eventually(func() string {
			out, _ := utils.Run(exec.Command("kubectl", "get", "agentrequest", arName,
				"-n", ns, "--ignore-not-found", "-o", "name"))
			return out
		}, 6*time.Minute, 10*time.Second).Should(BeEmpty())

		By("asserting AuditRecords for this AR are also gone")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "auditrecord",
				"-n", ns, "-o", "jsonpath={.items[*].spec.agentRequestRef}", "--ignore-not-found"))
			g.Expect(err).NotTo(HaveOccurred(), "kubectl get auditrecord failed")
			g.Expect(out).NotTo(ContainSubstring(arName))
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})

	It("should NOT delete an active AgentRequest", func() {
		arName := "gc-active-ar"

		By("creating an AgentRequest")
		arJSON := fmt.Sprintf(`{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind": "AgentRequest",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"agentIdentity": "gc-e2e-agent",
				"action": "scale",
				"target": {"uri": "k8s://default/deployment/active-target"},
				"reason": "testing GC active"
			}
		}`, arName, ns)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(arJSON)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for Phase=Approved")
		Eventually(func() string {
			out, _ := utils.Run(exec.Command("kubectl", "get", "agentrequest", arName,
				"-n", ns, "-o", "jsonpath={.status.phase}"))
			return out
		}, 30*time.Second, time.Second).Should(Equal("Approved"))

		By("consistently asserting it still exists for 2 minutes")
		// 2 minutes > 1 GC cycle (interval=1m), so if GC were to touch it we'd catch it.
		Consistently(func() string {
			out, _ := utils.Run(exec.Command("kubectl", "get", "agentrequest", arName,
				"-n", ns, "--ignore-not-found", "-o", "name"))
			return out
		}, 2*time.Minute, 10*time.Second).Should(Not(BeEmpty()))
	})
})
