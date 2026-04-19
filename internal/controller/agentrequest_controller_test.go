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

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"fmt"
	"net/http"
	"net/http/httptest"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/evaluation"
	"github.com/agent-control-plane/aip-k8s/internal/evaluation/fetchers"
)

const (
	resourceName = "test-agent-request"
)

var _ = Describe("AgentRequest Controller", func() {
	Context("When reconciling a resource", func() {

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the GovernedResource")
			gr := &governancev1alpha1.GovernedResource{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-gr",
					Labels: map[string]string{
						"app": "test",
					},
				},
				Spec: governancev1alpha1.GovernedResourceSpec{
					URIPattern:       "k8s://prod/*",
					PermittedActions: []string{"create", "update", "delete"},
					ContextFetcher:   "none",
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-gr"}, gr)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, gr)).To(Succeed())
			}

			By("creating the custom resource for the Kind AgentRequest")
			agentReq := &governancev1alpha1.AgentRequest{}
			err = k8sClient.Get(ctx, typeNamespacedName, agentReq)
			if err != nil && errors.IsNotFound(err) {
				resource := &governancev1alpha1.AgentRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: governancev1alpha1.AgentRequestSpec{
						AgentIdentity: "test-agent",
						Action:        "create",
						Target: governancev1alpha1.Target{
							URI: "k8s://prod/default/pod/test-pod",
						},
						Reason: "test execution",
						GovernedResourceRef: &governancev1alpha1.GovernedResourceRef{
							Name:       "test-gr",
							Generation: gr.Generation,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &governancev1alpha1.AgentRequest{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance AgentRequest")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			// Clean up AuditRecords generated
			var auditList governancev1alpha1.AuditRecordList
			Expect(k8sClient.List(ctx, &auditList, client.InNamespace("default"))).To(Succeed())
			for _, audit := range auditList.Items {
				Expect(k8sClient.Delete(ctx, &audit)).To(Succeed())
			}

			// Clean up any Leases left behind
			var leaseList coordinationv1.LeaseList
			Expect(k8sClient.List(ctx, &leaseList, client.InNamespace("default"))).To(Succeed())
			for _, lease := range leaseList.Items {
				_ = k8sClient.Delete(ctx, &lease)
			}
		})

		It("should successfully transition through the lifecycle and generate AuditRecords", func() {
			eval, err := evaluation.NewEvaluator()
			Expect(err).NotTo(HaveOccurred())

			controllerReconciler := &AgentRequestReconciler{
				Client:          k8sClient,
				APIReader:       k8sClient,
				Scheme:          k8sClient.Scheme(),
				OpsLockDuration: testOpsLockDuration,
				Evaluator:       eval,
			}

			req := reconcile.Request{NamespacedName: typeNamespacedName}

			// STEP 1: Initial Reconcile (Sets Phase to Pending)
			_, err = controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			var fetchedReq governancev1alpha1.AgentRequest
			Expect(k8sClient.Get(ctx, typeNamespacedName, &fetchedReq)).To(Succeed())
			Expect(fetchedReq.Status.Phase).To(Equal(governancev1alpha1.PhasePending))

			Eventually(func() bool {
				return hasAuditRecord(ctx, resourceName, governancev1alpha1.AuditEventRequestSubmitted)
			}, time.Second*5, time.Millisecond*500).Should(BeTrue())

			// STEP 2: Reconcile Pending -> Approved
			_, err = controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, &fetchedReq)).To(Succeed())
			Expect(fetchedReq.Status.Phase).To(Equal(governancev1alpha1.PhaseApproved))

			Eventually(func() bool {
				return hasAuditRecord(ctx, resourceName, governancev1alpha1.AuditEventRequestApproved)
			}, time.Second*5, time.Millisecond*500).Should(BeTrue())

			// STEP 3: Reconcile Approved -> Agent signals Executing
			Expect(k8sClient.Get(ctx, typeNamespacedName, &fetchedReq)).To(Succeed())
			meta.SetStatusCondition(&fetchedReq.Status.Conditions, metav1.Condition{
				Type:    governancev1alpha1.ConditionExecuting,
				Status:  metav1.ConditionTrue,
				Reason:  "AgentStarted",
				Message: "Agent is now executing action",
			})
			Expect(k8sClient.Status().Update(ctx, &fetchedReq)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, &fetchedReq)).To(Succeed())
			Expect(fetchedReq.Status.Phase).To(Equal(governancev1alpha1.PhaseExecuting))

			Eventually(func() bool {
				return hasAuditRecord(ctx, resourceName, governancev1alpha1.AuditEventLockAcquired) &&
					hasAuditRecord(ctx, resourceName, governancev1alpha1.AuditEventRequestExecuting)
			}, time.Second*5, time.Millisecond*500).Should(BeTrue())

			// STEP 4: Agent signals Completed
			Expect(k8sClient.Get(ctx, typeNamespacedName, &fetchedReq)).To(Succeed())
			meta.SetStatusCondition(&fetchedReq.Status.Conditions, metav1.Condition{
				Type:    governancev1alpha1.ConditionCompleted,
				Status:  metav1.ConditionTrue,
				Reason:  "ActionSuccess",
				Message: "Agent completed action",
			})
			Expect(k8sClient.Status().Update(ctx, &fetchedReq)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, &fetchedReq)).To(Succeed())
			Expect(fetchedReq.Status.Phase).To(Equal(governancev1alpha1.PhaseCompleted))

			Eventually(func() bool {
				return hasAuditRecord(ctx, resourceName, governancev1alpha1.AuditEventRequestCompleted) &&
					hasAuditRecord(ctx, resourceName, governancev1alpha1.AuditEventLockReleased)
			}, time.Second*5, time.Millisecond*500).Should(BeTrue())
		})

		It("should handle execution timeout properly", func() {
			// Inject a clock that is 6 minutes in the future, making the resource appear timed out
			frozenFuture := time.Now().Add(6 * time.Minute)
			eval, err := evaluation.NewEvaluator()
			Expect(err).NotTo(HaveOccurred())

			controllerReconciler := &AgentRequestReconciler{
				Client:          k8sClient,
				APIReader:       k8sClient,
				Scheme:          k8sClient.Scheme(),
				OpsLockDuration: testOpsLockDuration,
				Evaluator:       eval,
				Clock:           func() time.Time { return frozenFuture },
			}

			// Force the AgentRequest into Executing phase
			var fetchedReq governancev1alpha1.AgentRequest
			Expect(k8sClient.Get(ctx, typeNamespacedName, &fetchedReq)).To(Succeed())
			fetchedReq.Status.Phase = governancev1alpha1.PhaseExecuting
			Expect(k8sClient.Status().Update(ctx, &fetchedReq)).To(Succeed())

			// Create a Lease so reconcileExecuting can find it and detect expiry.
			// RenewTime is set to now (real clock), so the frozen future clock sees it as expired.
			leaseName := generateLeaseName(fetchedReq.Spec.Target.URI)
			holderIdentity := fetchedReq.Spec.AgentIdentity + "/" + fetchedReq.Name
			lease := &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: "default"},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       ptr.To(holderIdentity),
					LeaseDurationSeconds: ptr.To(int32(300)),
					AcquireTime:          &metav1.MicroTime{Time: time.Now()},
					RenewTime:            &metav1.MicroTime{Time: time.Now()},
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())

			// Re-fetch so we have the latest resource version
			Expect(k8sClient.Get(ctx, typeNamespacedName, &fetchedReq)).To(Succeed())

			// Reconcile while Executing with a future clock triggers timeout
			_, err = controllerReconciler.reconcileExecuting(ctx, &fetchedReq)
			Expect(err).NotTo(HaveOccurred())

			Expect(fetchedReq.Status.Phase).To(Equal(governancev1alpha1.PhaseFailed))

			Eventually(func() bool {
				return hasAuditRecord(ctx, resourceName, governancev1alpha1.AuditEventLockExpired) &&
					hasAuditRecord(ctx, resourceName, governancev1alpha1.AuditEventRequestFailed)
			}, time.Second*5, time.Millisecond*500).Should(BeTrue())
		})
		It("should deny AgentRequest when a matching SafetyPolicy triggers Deny and emit policy.evaluated AuditRecord", func() {
			policyName := "deny-prod-delete"
			policy := &governancev1alpha1.SafetyPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      policyName,
					Namespace: "default",
				},
				Spec: governancev1alpha1.SafetyPolicySpec{
					GovernedResourceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "test",
						},
					},
					Rules: []governancev1alpha1.Rule{
						{
							Name:       "deny-delete",
							Type:       "StateEvaluation",
							Action:     "Deny",
							Expression: `request.spec.target.uri.startsWith("k8s://prod")`,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, policy)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, policy)
			}()

			var testGR governancev1alpha1.GovernedResource
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-gr"}, &testGR)).To(Succeed())

			reqName := "test-deny-request"
			reqNN := types.NamespacedName{Name: reqName, Namespace: "default"}
			agentReq := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      reqName,
					Namespace: "default",
				},
				Spec: governancev1alpha1.AgentRequestSpec{
					AgentIdentity: "test-agent",
					Action:        "delete",
					Target: governancev1alpha1.Target{
						URI: "k8s://prod/default/pod/critical-pod",
					},
					Reason: "maintenance",
					GovernedResourceRef: &governancev1alpha1.GovernedResourceRef{
						Name:       "test-gr",
						Generation: testGR.Generation,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentReq)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, agentReq)
			}()

			eval, err := evaluation.NewEvaluator()
			Expect(err).NotTo(HaveOccurred())

			controllerReconciler := &AgentRequestReconciler{
				Client:          k8sClient,
				APIReader:       k8sClient,
				Scheme:          k8sClient.Scheme(),
				OpsLockDuration: testOpsLockDuration,
				Evaluator:       eval,
			}

			// Reconcile step 1 -> sets phase to pending and emits request.submitted
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: reqNN})
			Expect(err).NotTo(HaveOccurred())

			// Reconcile pending -> evaluates policy and denies
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: reqNN})
			Expect(err).NotTo(HaveOccurred())

			var fetchedReq governancev1alpha1.AgentRequest
			Expect(k8sClient.Get(ctx, reqNN, &fetchedReq)).To(Succeed())
			Expect(fetchedReq.Status.Phase).To(Equal(governancev1alpha1.PhaseDenied))
			Expect(fetchedReq.Status.Denial.Code).To(Equal(governancev1alpha1.DenialCodePolicyViolation))

			Eventually(func() bool {
				return hasAuditRecord(ctx, reqName, governancev1alpha1.AuditEventPolicyEvaluated) &&
					hasAuditRecord(ctx, reqName, governancev1alpha1.AuditEventRequestDenied)
			}, time.Second*5, time.Millisecond*500).Should(BeTrue())
		})

		It("should deny a second AgentRequest for the same target due to LockTimeout", func() {
			var lockTestGR governancev1alpha1.GovernedResource
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-gr"}, &lockTestGR)).To(Succeed())

			// Request 1
			req1Name := "test-lock-1"
			req1NN := types.NamespacedName{Name: req1Name, Namespace: "default"}
			agentReq1 := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{Name: req1Name, Namespace: "default"},
				Spec: governancev1alpha1.AgentRequestSpec{
					AgentIdentity: "agent-1",
					Action:        "update",
					Target:        governancev1alpha1.Target{URI: "k8s://prod/default/deployment/backend"},
					Reason:        "scale up",
					GovernedResourceRef: &governancev1alpha1.GovernedResourceRef{
						Name:       "test-gr",
						Generation: lockTestGR.Generation,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentReq1)).To(Succeed())

			// Request 2
			req2Name := "test-lock-2"
			req2NN := types.NamespacedName{Name: req2Name, Namespace: "default"}
			agentReq2 := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{Name: req2Name, Namespace: "default"},
				Spec: governancev1alpha1.AgentRequestSpec{
					AgentIdentity: "agent-2",
					Action:        "update",
					Target:        governancev1alpha1.Target{URI: "k8s://prod/default/deployment/backend"},
					Reason:        "config change",
					GovernedResourceRef: &governancev1alpha1.GovernedResourceRef{
						Name:       "test-gr",
						Generation: lockTestGR.Generation,
					},
				},
			}
			// Set creation timestamp to 61 seconds ago to simulate timeout
			creationTime := metav1.NewTime(time.Now().Add(-61 * time.Second))
			agentReq2.CreationTimestamp = creationTime
			Expect(k8sClient.Create(ctx, agentReq2)).To(Succeed())

			eval, _ := evaluation.NewEvaluator()
			controllerReconciler := &AgentRequestReconciler{
				Client:          k8sClient,
				APIReader:       k8sClient,
				Scheme:          k8sClient.Scheme(),
				OpsLockDuration: testOpsLockDuration,
				Evaluator:       eval,
			}

			// Process Request 1 (Acquires Lock)
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: req1NN})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: req1NN})
			Expect(err).NotTo(HaveOccurred())

			var fetchedReq1 governancev1alpha1.AgentRequest
			Expect(k8sClient.Get(ctx, req1NN, &fetchedReq1)).To(Succeed())
			Expect(fetchedReq1.Status.Phase).To(Equal(governancev1alpha1.PhaseApproved))

			// Process Request 2 (Fails to Acquire Lock, times out)
			// Phase 1 -> Pending
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: req2NN})
			Expect(err).NotTo(HaveOccurred())
			// Override creation timestamp again just in case Create() reset it (which it does in real K8s, but envtest might behave slightly differently)
			Expect(k8sClient.Get(ctx, req2NN, agentReq2)).To(Succeed())

			controllerReconciler.Clock = func() time.Time { return time.Now().Add(62 * time.Second) }
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: req2NN})
			Expect(err).NotTo(HaveOccurred())

			var fetchedReq2 governancev1alpha1.AgentRequest
			Expect(k8sClient.Get(ctx, req2NN, &fetchedReq2)).To(Succeed())
			Expect(fetchedReq2.Status.Phase).To(Equal(governancev1alpha1.PhaseDenied))
			Expect(fetchedReq2.Status.Denial.Code).To(Equal(governancev1alpha1.DenialCodeLockTimeout))

			// Cleanup
			_ = k8sClient.Delete(ctx, agentReq1)
			_ = k8sClient.Delete(ctx, agentReq2)
		})

		It("should set FetcherSchemaViolation condition when fetcher output mismatches schema", func() {
			grName := "gr-with-schema"
			schemaJSON := `{"type":"object","properties":{"title":{"type":"integer"}},"required":["title"]}`
			gr := &governancev1alpha1.GovernedResource{
				ObjectMeta: metav1.ObjectMeta{Name: grName},
				Spec: governancev1alpha1.GovernedResourceSpec{
					URIPattern:       "github://*",
					ContextFetcher:   "github",
					ContextSchema:    &apiextensionsv1.JSON{Raw: []byte(schemaJSON)},
					PermittedActions: []string{"update"},
				},
			}
			Expect(k8sClient.Create(ctx, gr)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, gr) }()

			By("Creating aip-system namespace")
			aipNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "aip-system"}}
			_ = k8sClient.Create(ctx, aipNS)

			// Mock GitHub token secret
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "aip-github-token", Namespace: "aip-system"},
				Data:       map[string][]byte{"token": []byte("test")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			// Mock GitHub API server to return JSON that MISMATCHES schema
			// Schema requires "title", but we return "wrong_field"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintln(w, `{"wrong_field":"data"}`)
			}))
			defer server.Close()
			fetchers.GitHubBaseURL = server.URL
			defer func() { fetchers.GitHubBaseURL = "https://api.github.com" }()

			reqName := "schema-fail-req"
			reqNN := types.NamespacedName{Name: reqName, Namespace: "default"}
			agentReq := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{Name: reqName, Namespace: "default"},
				Spec: governancev1alpha1.AgentRequestSpec{
					AgentIdentity: "test-agent",
					Action:        "update",
					Target:        governancev1alpha1.Target{URI: "github://org/repo"},
					Reason:        "testing schema violation",
					GovernedResourceRef: &governancev1alpha1.GovernedResourceRef{
						Name:       grName,
						Generation: gr.Generation,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentReq)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, agentReq) }()

			eval, _ := evaluation.NewEvaluator()
			controllerReconciler := &AgentRequestReconciler{
				Client:          k8sClient,
				APIReader:       k8sClient, // Use direct client to avoid cache latency
				Scheme:          k8sClient.Scheme(),
				OpsLockDuration: testOpsLockDuration,
				Evaluator:       eval,
			}

			// Reconcile 1: Init -> Pending
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: reqNN})
			Expect(err).NotTo(HaveOccurred())

			var fetchedReq1 governancev1alpha1.AgentRequest
			Expect(k8sClient.Get(ctx, reqNN, &fetchedReq1)).To(Succeed())
			Expect(fetchedReq1.Status.Phase).To(Equal(governancev1alpha1.PhasePending))

			// Reconcile 2: Pending -> (fetch context)
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: reqNN})
			Expect(err).NotTo(HaveOccurred())

			var fetchedReq2 governancev1alpha1.AgentRequest
			Expect(k8sClient.Get(ctx, reqNN, &fetchedReq2)).To(Succeed())

			// Verify condition
			cond := meta.FindStatusCondition(fetchedReq2.Status.Conditions, "FetcherSchemaViolation")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("SchemaMismatch"))

			// ProviderContext should be nil
			Expect(fetchedReq2.Status.ProviderContext).To(BeNil())
		})

	})
})

func hasAuditRecord(ctx context.Context, reqName string, event string) bool {
	var auditList governancev1alpha1.AuditRecordList
	err := k8sClient.List(ctx, &auditList, client.InNamespace("default"))
	if err != nil {
		return false
	}

	for _, audit := range auditList.Items {
		if audit.Spec.AgentRequestRef == reqName && audit.Spec.Event == event {
			return true
		}
	}
	return false
}
