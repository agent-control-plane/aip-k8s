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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	governancev1alpha1 "github.com/ravisantoshgudimetla/aip-k8s/api/v1alpha1"
	"github.com/ravisantoshgudimetla/aip-k8s/test/utils"
)

// namespace where the project is deployed in
const namespace = "aip-k8s-system"

// serviceAccountName created for the project
const serviceAccountName = "aip-k8s-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "aip-k8s-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "aip-k8s-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		// Use apply instead of create so this is idempotent: if Phase 6 BeforeAll
		// ran first (Ginkgo randomises top-level Describe order) it will have
		// already created the namespace via make deploy.
		cmd := exec.Command("kubectl", "create", "ns", namespace, "--dry-run=client", "-o", "yaml")
		nsYAML, err := cmd.Output()
		Expect(err).NotTo(HaveOccurred(), "Failed to render namespace manifest")
		applyCmd := exec.Command("kubectl", "apply", "-f", "-")
		applyCmd.Stdin = strings.NewReader(string(nsYAML))
		_, err = utils.Run(applyCmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager (skips if already running)")
		checkCmd := exec.Command("kubectl", "get", "deployment",
			"aip-k8s-controller-manager", "-n", namespace)
		if _, checkErr := utils.Run(checkCmd); checkErr != nil {
			cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
		}
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	// After all tests have been executed, clean up by deleting local test files.
	// We DO NOT uninstall CRDs or remove the manager namespace here because 
	// subsequent test specs (like gateway_test.go) require them.
	// The cluster is destroyed entirely by `make cleanup-test-e2e` later.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("cleaning up the metrics ClusterRoleBinding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			// Delete first to handle any leftover from a previously interrupted run.
			cmd := exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=aip-k8s-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})

	// Phase 2: AgentRequest lifecycle and AuditRecord generation
	Context("Phase 2: AgentRequest lifecycle", Ordered, func() {
		const (
			reqName      = "e2e-lifecycle-test"
			reqNS        = "default"
			agentReqJSON = `{
				"apiVersion": "governance.aip.io/v1alpha1",
				"kind": "AgentRequest",
				"metadata": {"name": "e2e-lifecycle-test", "namespace": "default"},
				"spec": {
					"agentIdentity": "e2e-test-agent",
					"action": "scale",
					"target": {"uri": "k8s://prod/default/deployment/test-app"},
					"reason": "e2e phase-2 lifecycle test"
				}
			}`
		)

		BeforeAll(func() {
			By("cleaning up any stale OpsLock leases from previous runs")
			cmd := exec.Command("kubectl", "delete", "leases", "-n", reqNS,
				"-l", "governance.aip.io/managed-by=aip-controller", "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		AfterAll(func() {
			By("cleaning up AgentRequest and its AuditRecords")
			cmd := exec.Command("kubectl", "delete", "agentrequest", reqName, "-n", reqNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)

			By("cleaning up OpsLock leases")
			cmd = exec.Command("kubectl", "delete", "leases", "-n", reqNS,
				"-l", "governance.aip.io/managed-by=aip-controller", "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should transition Pending -> Approved and emit AuditRecords", func() {
			By("creating the AgentRequest")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(agentReqJSON)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Phase=Pending (initial reconcile)")
			Eventually(func() string {
				return getAgentRequestPhase(reqName, reqNS)
			}).Should(Equal("Pending"))

			By("waiting for Phase=Approved (policy evaluation auto-approves in Phase 2)")
			Eventually(func() string {
				return getAgentRequestPhase(reqName, reqNS)
			}).Should(Equal("Approved"))

			By("asserting request.submitted AuditRecord exists")
			Eventually(func() bool {
				return auditRecordExists(reqName, reqNS, "request.submitted")
			}).Should(BeTrue())

			By("asserting request.approved AuditRecord exists")
			Eventually(func() bool {
				return auditRecordExists(reqName, reqNS, "request.approved")
			}).Should(BeTrue())
		})

		It("should transition Approved -> Executing when agent signals Executing condition", func() {
			By("patching Executing condition on the AgentRequest status")
			executingPatch := `{"status":{"conditions":[{"type":"Executing","status":"True","reason":"AgentStarted","message":"Agent is executing","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}`
			cmd := exec.Command("kubectl", "patch", "agentrequest", reqName, "-n", reqNS,
				"--subresource=status", "--type=merge", "-p", executingPatch)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Phase=Executing")
			Eventually(func() string {
				return getAgentRequestPhase(reqName, reqNS)
			}).Should(Equal("Executing"))

			By("asserting lock.acquired AuditRecord exists")
			Eventually(func() bool {
				return auditRecordExists(reqName, reqNS, "lock.acquired")
			}).Should(BeTrue())

			By("asserting request.executing AuditRecord exists")
			Eventually(func() bool {
				return auditRecordExists(reqName, reqNS, "request.executing")
			}).Should(BeTrue())
		})

		It("should transition Executing -> Completed when agent signals Completed condition", func() {
			By("patching Completed condition on the AgentRequest status")
			completedPatch := `{"status":{"conditions":[{"type":"Completed","status":"True","reason":"ActionSuccess","message":"Agent completed the action","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}`
			cmd := exec.Command("kubectl", "patch", "agentrequest", reqName, "-n", reqNS,
				"--subresource=status", "--type=merge", "-p", completedPatch)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Phase=Completed")
			Eventually(func() string {
				return getAgentRequestPhase(reqName, reqNS)
			}).Should(Equal("Completed"))

			By("asserting request.completed AuditRecord exists")
			Eventually(func() bool {
				return auditRecordExists(reqName, reqNS, "request.completed")
			}).Should(BeTrue())

			By("asserting lock.released AuditRecord exists")
			Eventually(func() bool {
				return auditRecordExists(reqName, reqNS, "lock.released")
			}).Should(BeTrue())
		})
	})

	// Phase 3: SafetyPolicy Evaluation
	Context("Phase 3: SafetyPolicy Evaluation", Ordered, func() {
		const (
			policyName = "deny-prod-scale"
			reqName    = "e2e-policy-test"
			ns         = "default"
			policyJSON = `{
				"apiVersion": "governance.aip.io/v1alpha1",
				"kind": "SafetyPolicy",
				"metadata": {"name": "deny-prod-scale", "namespace": "default"},
				"spec": {
					"targetSelector": {"matchActions": ["scale"]},
					"rules": [
						{
							"name": "block-scale",
							"type": "StateEvaluation",
							"action": "Deny",
							"expression": "request.spec.target.uri.startsWith('k8s://prod')"
						}
					],
					"failureMode": "FailClosed"
				}
			}`
			reqJSON = `{
				"apiVersion": "governance.aip.io/v1alpha1",
				"kind": "AgentRequest",
				"metadata": {"name": "e2e-policy-test", "namespace": "default"},
				"spec": {
					"agentIdentity": "e2e-test-agent",
					"action": "scale",
					"target": {"uri": "k8s://prod/default/deployment/backend"},
					"reason": "e2e policy test"
				}
			}`
		)

		AfterAll(func() {
			By("cleaning up SafetyPolicy and AgentRequest")
			cmd := exec.Command("kubectl", "delete", "safetypolicy", policyName, "-n", ns, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agentrequest", reqName, "-n", ns, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should transition to Denied with POLICY_VIOLATION when policy matches", func() {
			By("creating the SafetyPolicy")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(policyJSON)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for SafetyPolicy to be visible in the API server")
			Eventually(func(g Gomega) {
				var sp governancev1alpha1.SafetyPolicy
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ns}, &sp)).To(Succeed())
			}).Should(Succeed())

			By("creating the AgentRequest targeting prod")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(reqJSON)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Phase=Denied")
			Eventually(func() string {
				return getAgentRequestPhase(reqName, ns)
			}).Should(Equal("Denied"))

			By("checking that denial code is POLICY_VIOLATION and matched rule is block-scale")
			Eventually(func(g Gomega) {
				var ar governancev1alpha1.AgentRequest
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: reqName, Namespace: ns}, &ar)).To(Succeed())
				g.Expect(ar.Status.Denial).NotTo(BeNil())
				g.Expect(ar.Status.Denial.Code).To(Equal("POLICY_VIOLATION"))
				g.Expect(ar.Status.Denial.PolicyResults).NotTo(BeEmpty())
				g.Expect(ar.Status.Denial.PolicyResults[0].RuleName).To(Equal("block-scale"))
			}).Should(Succeed())

			By("asserting request.denied AuditRecord exists")
			Eventually(func() bool {
				return auditRecordExists(reqName, ns, "request.denied")
			}).Should(BeTrue())
		})
	})

	// Phase 4: OpsLock Contention
	Context("Phase 4: OpsLock Contention", Ordered, func() {
		const (
			req1Name        = "e2e-lock1"
			req2Name        = "e2e-lock2"
			ns              = "default"
			targetURI       = "k8s://dev/default/deployment/locked-app"
			reqJSONTemplate = `{
				"apiVersion": "governance.aip.io/v1alpha1",
				"kind": "AgentRequest",
				"metadata": {"name": "%s", "namespace": "default"},
				"spec": {
					"agentIdentity": "e2e-test-agent",
					"action": "update",
					"target": {"uri": "%s"},
					"reason": "e2e lock test"
				}
			}`
		)

		AfterAll(func() {
			By("cleaning up lock AgentRequests")
			cmd := exec.Command("kubectl", "delete", "agentrequest", req1Name, req2Name, "-n", ns, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should handle concurrent requests on the same target with lock contention", func() {
			req1JSON := fmt.Sprintf(reqJSONTemplate, req1Name, targetURI)
			req2JSON := fmt.Sprintf(reqJSONTemplate, req2Name, targetURI)

			// Note: "update" is intentionally used here — it matches no SafetyPolicy
			// matchActions, so requests skip policy evaluation and go directly to
			// OpsLock acquisition. This isolates the lock contention behaviour.

			By("creating AgentRequest 1")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(req1JSON)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for AgentRequest 1 to be Approved and hold the lock")
			Eventually(func() string {
				return getAgentRequestPhase(req1Name, ns)
			}).Should(Equal("Approved"))

			By("creating AgentRequest 2 on the same target")
			cmd2 := exec.Command("kubectl", "apply", "-f", "-")
			cmd2.Stdin = strings.NewReader(req2JSON)
			_, err = utils.Run(cmd2)
			Expect(err).NotTo(HaveOccurred())

			By("asserting AgentRequest 1 still holds the lock while AgentRequest 2 waits")
			Expect(getAgentRequestPhase(req1Name, ns)).To(Equal("Approved"))

			By("waiting for AgentRequest 2 to be Denied due to contention/timeout")
			Eventually(func() string {
				return getAgentRequestPhase(req2Name, ns)
			}, 3*time.Minute, 5*time.Second).Should(Equal("Denied"))

			By("checking that AgentRequest 2 denial code is LOCK_TIMEOUT or LOCK_CONTENTION")
			Eventually(func(g Gomega) {
				var ar governancev1alpha1.AgentRequest
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: req2Name, Namespace: ns}, &ar)).To(Succeed())
				g.Expect(ar.Status.Denial).NotTo(BeNil())
				g.Expect(ar.Status.Denial.Code).To(Or(Equal("LOCK_TIMEOUT"), Equal("LOCK_CONTENTION")))
			}).Should(Succeed())

			By("asserting AgentRequest 1 remained Approved for the duration — proving lock exclusivity")
			Expect(getAgentRequestPhase(req1Name, ns)).To(Equal("Approved"))
		})
	})

	// Phase 5: AgentDiagnostic CRD
	Context("Phase 5: AgentDiagnostic", Ordered, func() {
		const (
			diagNS            = "default"
			diagCorrelationID = "e2e-corr-abc123"
			diagJSON          = `{
				"apiVersion": "governance.aip.io/v1alpha1",
				"kind": "AgentDiagnostic",
				"metadata": {
					"name": "e2e-diag-test",
					"namespace": "default",
					"labels": {
						"aip.io/correlationID": "e2e-corr-abc123",
						"aip.io/agentIdentity": "e2e-diagnostic-agent",
						"aip.io/diagnosticType": "diagnosis"
					}
				},
				"spec": {
					"agentIdentity": "e2e-diagnostic-agent",
					"diagnosticType": "diagnosis",
					"correlationID": "e2e-corr-abc123",
					"summary": "e2e test: OOMKilled detected on test-app"
				}
			}`
		)

		BeforeAll(func() {
			By("cleaning up any stale AgentDiagnostic from previous runs")
			cmd := exec.Command("kubectl", "delete", "agentdiagnostic", "e2e-diag-test",
				"-n", diagNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		AfterAll(func() {
			By("cleaning up AgentDiagnostic")
			cmd := exec.Command("kubectl", "delete", "agentdiagnostic", "e2e-diag-test", "-n", diagNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should create an AgentDiagnostic and verify it is queryable by correlationID", func() {
			By("creating the AgentDiagnostic")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(diagJSON)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the AgentDiagnostic exists and has the correct spec")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "agentdiagnostic", "e2e-diag-test",
					"-n", diagNS, "-o", "jsonpath={.spec.correlationID}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal(diagCorrelationID))
			}).Should(Succeed())

			By("verifying the AgentDiagnostic is queryable by correlationID label")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "agentdiagnostics",
					"-n", diagNS, "-l", "aip.io/correlationID="+diagCorrelationID,
					"-o", "jsonpath={.items[0].metadata.name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("e2e-diag-test"))
			}).Should(Succeed())
		})

		It("should reject spec mutations after creation", func() {
			By("confirming the AgentDiagnostic exists before testing immutability")
			cmd := exec.Command("kubectl", "get", "agentdiagnostic", "e2e-diag-test", "-n", diagNS)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "AgentDiagnostic must exist before testing CEL immutability")

			By("attempting to mutate spec.summary — must be rejected by CEL")
			cmd = exec.Command("kubectl", "patch", "agentdiagnostic", "e2e-diag-test",
				"-n", diagNS, "--type=merge",
				"-p", `{"spec":{"summary":"mutated summary"}}`)
			out, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "spec mutation should be rejected by CEL immutability rule")
			Expect(out).To(ContainSubstring("immutable"), "rejection message should reference immutability")
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// getAgentRequestPhase returns the current phase of the named AgentRequest using client-go.
func getAgentRequestPhase(name, ns string) string {
	var ar governancev1alpha1.AgentRequest
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &ar); err != nil {
		return ""
	}
	return ar.Status.Phase
}

// auditRecordExists returns true if at least one AuditRecord in ns references reqName with the given event.
func auditRecordExists(reqName, ns, event string) bool {
	var list governancev1alpha1.AuditRecordList
	if err := k8sClient.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return false
	}
	for _, ar := range list.Items {
		if ar.Spec.AgentRequestRef == reqName && ar.Spec.Event == event {
			return true
		}
	}
	return false
}
