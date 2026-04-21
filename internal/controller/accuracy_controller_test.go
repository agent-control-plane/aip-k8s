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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

var _ = Describe("DiagnosticAccuracy Controller", func() {
	Context("When reconciling AgentRequest verdicts", func() {
		const namespace = "default"

		It("should increment CorrectCount for correct verdict", func() {
			const agentIdentity = "acc-agent-correct"
			reqName := "req-correct"
			agentReq := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      reqName,
					Namespace: namespace,
				},
				Spec: governancev1alpha1.AgentRequestSpec{
					AgentIdentity: agentIdentity,
					Action:        "scale-up",
					Target:        governancev1alpha1.Target{URI: "k8s://cluster/deploy/app"},
					Reason:        "test",
				},
			}
			Expect(k8sClient.Create(context.Background(), agentReq)).To(Succeed())

			// Update status
			base := agentReq.DeepCopy()
			agentReq.Status.Phase = governancev1alpha1.PhaseCompleted
			agentReq.Status.Verdict = "correct"
			now := metav1.Now()
			agentReq.Status.VerdictAt = &now
			Expect(k8sClient.Status().Patch(context.Background(), agentReq, client.MergeFrom(base))).To(Succeed())

			reconciler := &DiagnosticAccuracyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: reqName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			summary := &governancev1alpha1.DiagnosticAccuracySummary{}
			summaryName := summaryNameForAgent(agentIdentity)
			Eventually(func() error {
				return k8sClient.Get(context.Background(), types.NamespacedName{Name: summaryName, Namespace: namespace}, summary)
			}, 5*time.Second).Should(Succeed())

			Expect(summary.Status.CorrectCount).To(Equal(int64(1)))
			Expect(summary.Status.TotalReviewed).To(Equal(int64(1)))
			Expect(*summary.Status.DiagnosticAccuracy).To(Equal(1.0))
		})

		It("should increment IncorrectCount for wrong_diagnosis", func() {
			const agentIdentity = "acc-agent-incorrect"
			reqName := "req-incorrect"
			agentReq := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      reqName,
					Namespace: namespace,
				},
				Spec: governancev1alpha1.AgentRequestSpec{
					AgentIdentity: agentIdentity,
					Action:        "scale-up",
					Target:        governancev1alpha1.Target{URI: "k8s://cluster/deploy/app"},
					Reason:        "test",
				},
			}
			Expect(k8sClient.Create(context.Background(), agentReq)).To(Succeed())

			base := agentReq.DeepCopy()
			agentReq.Status.Phase = governancev1alpha1.PhaseCompleted
			agentReq.Status.Verdict = "incorrect"
			agentReq.Status.VerdictReasonCode = "wrong_diagnosis"
			now := metav1.Now()
			agentReq.Status.VerdictAt = &now
			Expect(k8sClient.Status().Patch(context.Background(), agentReq, client.MergeFrom(base))).To(Succeed())

			reconciler := &DiagnosticAccuracyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: reqName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			summary := &governancev1alpha1.DiagnosticAccuracySummary{}
			summaryName := summaryNameForAgent(agentIdentity)
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: summaryName, Namespace: namespace}, summary)).To(Succeed())
				g.Expect(summary.Status.TotalReviewed).To(Equal(int64(1)))
			}, 5*time.Second).Should(Succeed())

			Expect(summary.Status.CorrectCount).To(Equal(int64(0)))
			Expect(summary.Status.IncorrectCount).To(Equal(int64(1)))
			Expect(*summary.Status.DiagnosticAccuracy).To(Equal(0.0))
		})

		It("should NOT affect accuracy for bad_timing", func() {
			const agentIdentity = "acc-agent-bad-timing"
			reqName := "req-bad-timing"
			agentReq := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      reqName,
					Namespace: namespace,
				},
				Spec: governancev1alpha1.AgentRequestSpec{
					AgentIdentity: agentIdentity,
					Action:        "scale-up",
					Target:        governancev1alpha1.Target{URI: "k8s://cluster/deploy/app"},
					Reason:        "test",
				},
			}
			Expect(k8sClient.Create(context.Background(), agentReq)).To(Succeed())

			base := agentReq.DeepCopy()
			agentReq.Status.Phase = governancev1alpha1.PhaseCompleted
			agentReq.Status.Verdict = "incorrect"
			agentReq.Status.VerdictReasonCode = "bad_timing"
			now := metav1.Now()
			agentReq.Status.VerdictAt = &now
			Expect(k8sClient.Status().Patch(context.Background(), agentReq, client.MergeFrom(base))).To(Succeed())

			reconciler := &DiagnosticAccuracyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: reqName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// bad_timing is excluded from accuracy; the summary should never be created.
			summaryName := summaryNameForAgent(agentIdentity)
			Consistently(func() bool {
				var summary governancev1alpha1.DiagnosticAccuracySummary
				err := k8sClient.Get(context.Background(),
					types.NamespacedName{Name: summaryName, Namespace: namespace}, &summary)
				// Either not found (never created) or TotalReviewed still 0 — both are correct.
				if apierrors.IsNotFound(err) {
					return true
				}
				return err == nil && summary.Status.TotalReviewed == 0
			}, 2*time.Second, 200*time.Millisecond).Should(BeTrue())
		})

		It("should use half weight for partial verdict", func() {
			const agentIdentity = "acc-agent-partial"
			reqName := "req-partial"
			agentReq := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      reqName,
					Namespace: namespace,
				},
				Spec: governancev1alpha1.AgentRequestSpec{
					AgentIdentity: agentIdentity,
					Action:        "scale-up",
					Target:        governancev1alpha1.Target{URI: "k8s://cluster/deploy/app"},
					Reason:        "test",
				},
			}
			Expect(k8sClient.Create(context.Background(), agentReq)).To(Succeed())

			base := agentReq.DeepCopy()
			agentReq.Status.Phase = governancev1alpha1.PhaseCompleted
			agentReq.Status.Verdict = "partial"
			agentReq.Status.VerdictReasonCode = "wrong_diagnosis"
			now := metav1.Now()
			agentReq.Status.VerdictAt = &now
			Expect(k8sClient.Status().Patch(context.Background(), agentReq, client.MergeFrom(base))).To(Succeed())

			reconciler := &DiagnosticAccuracyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: reqName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			summary := &governancev1alpha1.DiagnosticAccuracySummary{}
			summaryName := summaryNameForAgent(agentIdentity)
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: summaryName, Namespace: namespace}, summary)).To(Succeed())
				g.Expect(summary.Status.TotalReviewed).To(Equal(int64(1)))
			}, 5*time.Second).Should(Succeed())

			// (0 correct + 0.5 * 1 partial) / 1 total = 0.5
			Expect(*summary.Status.DiagnosticAccuracy).To(Equal(0.5))
		})

		It("should correctly recompute formula for complex set", func() {
			agentIdComplex := "complex-agent"
			summaryName := summaryNameForAgent(agentIdComplex)

			summary := &governancev1alpha1.DiagnosticAccuracySummary{
				ObjectMeta: metav1.ObjectMeta{
					Name:      summaryName,
					Namespace: namespace,
				},
				Spec: governancev1alpha1.DiagnosticAccuracySummarySpec{
					AgentIdentity: agentIdComplex,
				},
			}
			Expect(k8sClient.Create(context.Background(), summary)).To(Succeed())

			// Initialize status
			baseSummary := summary.DeepCopy()
			summary.Status.CorrectCount = 7
			summary.Status.PartialCount = 2
			summary.Status.IncorrectCount = 1
			summary.Status.TotalReviewed = 10
			Expect(k8sClient.Status().Patch(context.Background(), summary, client.MergeFrom(baseSummary))).To(Succeed())

			// Add one more correct verdict
			reqName := "req-final-correct"
			agentReq := &governancev1alpha1.AgentRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      reqName,
					Namespace: namespace,
				},
				Spec: governancev1alpha1.AgentRequestSpec{
					AgentIdentity: agentIdComplex,
					Action:        "scale-up",
					Target:        governancev1alpha1.Target{URI: "k8s://cluster/deploy/app"},
					Reason:        "test",
				},
			}
			Expect(k8sClient.Create(context.Background(), agentReq)).To(Succeed())

			baseReq := agentReq.DeepCopy()
			agentReq.Status.Phase = governancev1alpha1.PhaseCompleted
			agentReq.Status.Verdict = "correct"
			now := metav1.Now()
			agentReq.Status.VerdictAt = &now
			Expect(k8sClient.Status().Patch(context.Background(), agentReq, client.MergeFrom(baseReq))).To(Succeed())

			reconciler := &DiagnosticAccuracyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: reqName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: summaryName, Namespace: namespace}, summary)).To(Succeed())
				g.Expect(summary.Status.TotalReviewed).To(Equal(int64(11)))
			}, 5*time.Second).Should(Succeed())

			Expect(*summary.Status.DiagnosticAccuracy).To(BeNumerically("~", 0.818, 0.001))
		})

	})
})
