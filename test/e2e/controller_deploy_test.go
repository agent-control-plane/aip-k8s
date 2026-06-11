//go:build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/agent-control-plane/aip-k8s/test/utils"
)

var _ = Describe("Controller deployment", Ordered, func() {
	var controllerPodName string

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			}
		}
	})

	Context("Phase 5: Garbage Collection", Ordered, func() {
		It("should verify the controller has GC flags in the deployment", func() {
			By("fetching the controller deployment container args")
			cmd := exec.Command("kubectl", "get", "deployment",
				controllerDeploymentName, "-n", namespace,
				"-o", "jsonpath={.spec.template.spec.containers[0].args}")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("checking for GC-related flags")
			Expect(out).To(ContainSubstring("--gc-enabled="))
			Expect(out).To(ContainSubstring("--ops-lock-wait-timeout=20s"))
			Expect(out).NotTo(ContainSubstring("--gc-diagnostic"))
			Expect(out).NotTo(ContainSubstring("--gc-export-type"))
		})

		It("should verify the gc-healthz check is registered at /healthz/gc-healthz", func() {
			By("ensuring the controller pod name is known")
			if controllerPodName == "" {
				cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
					"-l", "control-plane=controller-manager",
					"-o", "jsonpath={.items[0].metadata.name}")
				out, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				controllerPodName = strings.TrimSpace(out)
			}
			Expect(controllerPodName).NotTo(BeEmpty())

			By("creating a temporary curl pod to verify GC health probe")
			const curlPodName = "curl-gc-test"
			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "pod", curlPodName, "-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			})

			By("getting the controller pod IP")
			var podIP string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.podIP}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podIP = strings.TrimSpace(out)
				g.Expect(podIP).NotTo(BeEmpty())
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("running curl pod and verifying /healthz/gc-healthz responds ok")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", curlPodName, "-n", namespace, "--ignore-not-found", "--wait=true"))

			curlCmd := fmt.Sprintf("until curl -sf -o /dev/null http://%s:8081/healthz/gc-healthz; do sleep 1; done; echo ok", podIP)
			_, err := utils.Run(exec.Command("kubectl", "run", curlPodName, "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"imagePullPolicy": "IfNotPresent",
							"command": ["/bin/sh", "-c"],
							"args": ["%s"],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {"drop": ["ALL"]},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {"type": "RuntimeDefault"}
							}
						}],
						"restartPolicy": "Never"
					}
				}`, curlCmd)))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				logCmd := exec.Command("kubectl", "logs", curlPodName, "-n", namespace)
				logs, err := utils.Run(logCmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(logs)).To(Equal("ok"))
			}, 90*time.Second, 2*time.Second).Should(Succeed())
		})

		It("should verify RBAC permissions for GC", func() {
			By("checking if the controller service account can delete agentrequests")
			cmd := exec.Command("kubectl", "auth", "can-i", "delete", "agentrequests",
				"--as", fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccountName),
				"-n", "default")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(out)).To(Equal("yes"), "Controller SA should have permission to delete agentrequests")

			By("checking if the controller service account can delete auditrecords")
			cmd = exec.Command("kubectl", "auth", "can-i", "delete", "auditrecords",
				"--as", fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccountName),
				"-n", "default")
			out, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(out)).To(Equal("yes"), "Controller SA should have permission to delete auditrecords")
		})
	})
})
