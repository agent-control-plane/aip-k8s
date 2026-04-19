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
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agent-control-plane/aip-k8s/test/utils"
)

const (
	defaultEventuallyTimeout = 30 * time.Second
	longEventuallyTimeout    = 60 * time.Second
	shortEventuallyPolling   = 2 * time.Second
)

var _ = Describe("OpsLock Renewal", Ordered, func() {
	const (
		grName    = "opslock-renewal-gr"
		reqName   = "opslock-renewal-req"
		ns        = "default"
		targetURI = "github://testorg/testrepo/files/main/config.yaml"
	)

	BeforeAll(func() {
		By("creating a GovernedResource")
		grJSON := fmt.Sprintf(`{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind": "GovernedResource",
			"metadata": {"name": "%s"},
			"spec": {
				"uriPattern": "github://testorg/testrepo/files/main/**",
				"permittedActions": ["update"]
			}
		}`, grName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(grJSON)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("cleaning up resources")
		// GovernedResource is cluster-scoped — no -n flag
		cmd := exec.Command("kubectl", "delete", "governedresource", "--all", "--ignore-not-found")
		// Errors are intentionally ignored because this is best-effort cleanup in test teardown
		// and failures are non-fatal (e.g. resources may already be absent).
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "agentrequest", "--all", "-n", ns, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	It("should renew the OpsLock lease when the AgentRequest is in Executing phase", func() {
		By("creating an AgentRequest")
		reqJSON := fmt.Sprintf(`{
			"apiVersion": "governance.aip.io/v1alpha1",
			"kind": "AgentRequest",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"agentIdentity": "e2e-renewal-agent",
				"action": "update",
				"target": {"uri": "%s"},
				"reason": "e2e renewal test"
			}
		}`, reqName, ns, targetURI)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(reqJSON)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for Phase=Approved")
		Eventually(func() string {
			return getAgentRequestPhase(reqName, ns)
		}, defaultEventuallyTimeout).Should(Equal("Approved"))

		By("patching Executing condition on the AgentRequest status")
		executingPatch := `{"status":{"conditions":[{"type":"Executing","status":"True","reason":"AgentStarted","message":"Agent is executing","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}`
		cmd = exec.Command("kubectl", "patch", "agentrequest", reqName, "-n", ns,
			"--subresource=status", "--type=merge", "-p", executingPatch)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for Phase=Executing")
		Eventually(func() string {
			return getAgentRequestPhase(reqName, ns)
		}, defaultEventuallyTimeout).Should(Equal("Executing"))

		By("fetching the OpsLock lease")
		var lease coordinationv1.Lease
		Eventually(func(g Gomega) {
			var list coordinationv1.LeaseList
			g.Expect(k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{"governance.aip.io/managed-by": "aip-controller"})).To(Succeed())

			found := false
			for _, l := range list.Items {
				if l.Spec.HolderIdentity != nil && strings.Contains(*l.Spec.HolderIdentity, reqName) {
					lease = l
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "Lease not found for request %s", reqName)
			g.Expect(lease.Spec.RenewTime).NotTo(BeNil(), "Lease RenewTime should be set")
		}, defaultEventuallyTimeout).Should(Succeed())

		initialRenewTime := lease.Spec.RenewTime.DeepCopy()
		By(fmt.Sprintf("Initial RenewTime: %v", initialRenewTime))

		By("waiting for the lease to be renewed (TTL is 15s, should renew around 7.5s)")
		// Wait up to 30s and assert RenewTime has advanced
		Eventually(func(g Gomega) {
			var currentLease coordinationv1.Lease
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lease.Name, Namespace: ns}, &currentLease)).To(Succeed())
			g.Expect(currentLease.Spec.RenewTime).NotTo(BeNil())
			g.Expect(currentLease.Spec.RenewTime.After(initialRenewTime.Time)).To(BeTrue(),
				"RenewTime should have advanced. Initial: %v, Current: %v", initialRenewTime, currentLease.Spec.RenewTime)
		}, longEventuallyTimeout, shortEventuallyPolling).Should(Succeed())

		By("asserting the AgentRequest is still in Executing phase")
		Expect(getAgentRequestPhase(reqName, ns)).To(Equal("Executing"))
	})
})
