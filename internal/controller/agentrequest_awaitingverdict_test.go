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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/evaluation"
)

var _ = Describe("AgentRequest AwaitingVerdict reconciliation", func() {
	ctx := context.Background()

	makeReconciler := func(clock func() time.Time) *AgentRequestReconciler {
		eval, err := evaluation.NewEvaluator()
		Expect(err).NotTo(HaveOccurred())
		r := &AgentRequestReconciler{
			Client:          k8sClient,
			APIReader:       k8sClient,
			Scheme:          k8sClient.Scheme(),
			OpsLockDuration: testOpsLockDuration,
			ApprovedTimeout: 5 * time.Minute,
			Evaluator:       eval,
		}
		if clock != nil {
			r.Clock = clock
		}
		return r
	}

	makeAR := func(name string) *governancev1alpha1.AgentRequest {
		return &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    map[string]string{"aip.io/agentIdentity": "test-agent"},
			},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: "test-agent",
				Action:        "restart",
				Target:        governancev1alpha1.Target{URI: "k8s://prod/ns/deploy/foo"},
				Reason:        "test",
			},
		}
	}

	cleanupAR := func(name string) {
		ar := &governancev1alpha1.AgentRequest{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, ar); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ar))).To(Succeed())
		}
		var auditList governancev1alpha1.AuditRecordList
		if k8sClient.List(ctx, &auditList, client.InNamespace("default")) == nil {
			for i := range auditList.Items {
				if auditList.Items[i].Spec.AgentRequestRef == name {
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &auditList.Items[i]))).To(Succeed())
				}
			}
		}
	}

	It("should transition AwaitingVerdict→Completed and emit verdict.submitted when verdict is set", func() {
		name := "av-verdict-test"
		defer cleanupAR(name)

		ar := makeAR(name)
		Expect(k8sClient.Create(ctx, ar)).To(Succeed())

		r := makeReconciler(nil)
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		req := reconcile.Request{NamespacedName: nn}

		// Reconcile 1: init → Pending (no GovernedResource)
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		// Force phase to AwaitingVerdict directly (simulates SoakMode admission)
		var current governancev1alpha1.AgentRequest
		Expect(k8sClient.Get(ctx, nn, &current)).To(Succeed())
		base := current.DeepCopy()
		current.Status.Phase = governancev1alpha1.PhaseAwaitingVerdict
		Expect(k8sClient.Status().Patch(ctx, &current, client.MergeFrom(base))).To(Succeed())

		// Set verdict fields (as the gateway verdict handler does)
		Expect(k8sClient.Get(ctx, nn, &current)).To(Succeed())
		base = current.DeepCopy()
		now := metav1.Now()
		current.Status.Verdict = verdictCorrect
		current.Status.VerdictBy = "reviewer@example.com"
		current.Status.VerdictAt = &now
		Expect(k8sClient.Status().Patch(ctx, &current, client.MergeFrom(base))).To(Succeed())

		// Reconcile: controller detects verdict and drives Completed
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, &current)).To(Succeed())
		Expect(current.Status.Phase).To(Equal(governancev1alpha1.PhaseCompleted))
		Expect(hasAuditRecord(ctx, name, governancev1alpha1.AuditEventVerdictSubmitted)).To(BeTrue())
	})

	It("should transition AwaitingVerdict→Expired and emit request.expired when TTL has passed", func() {
		name := "av-ttl-test"
		defer cleanupAR(name)

		ar := makeAR(name)
		Expect(k8sClient.Create(ctx, ar)).To(Succeed())

		// Use a clock set past the TTL so the expiry branch fires immediately.
		pastTTL := func() time.Time { return time.Now().Add(awaitingVerdictTTL + time.Hour) }
		r := makeReconciler(pastTTL)
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		req := reconcile.Request{NamespacedName: nn}

		// Force phase to AwaitingVerdict directly
		var current governancev1alpha1.AgentRequest
		Expect(k8sClient.Get(ctx, nn, &current)).To(Succeed())
		base := current.DeepCopy()
		current.Status.Phase = governancev1alpha1.PhaseAwaitingVerdict
		Expect(k8sClient.Status().Patch(ctx, &current, client.MergeFrom(base))).To(Succeed())

		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, &current)).To(Succeed())
		Expect(current.Status.Phase).To(Equal(governancev1alpha1.PhaseExpired))
		Expect(hasAuditRecord(ctx, name, governancev1alpha1.AuditEventRequestExpired)).To(BeTrue())
	})

	It("should include approvedBy annotation in request.approved AuditRecord", func() {
		name := "av-approvedby-test"
		defer cleanupAR(name)

		ar := makeAR(name)
		Expect(k8sClient.Create(ctx, ar)).To(Succeed())

		r := makeReconciler(nil)
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		req := reconcile.Request{NamespacedName: nn}

		// Reconcile 1: init → Pending
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		// Set HumanApproval with ApprovedBy (as gateway handleHumanDecision now does)
		var current governancev1alpha1.AgentRequest
		Expect(k8sClient.Get(ctx, nn, &current)).To(Succeed())
		base := current.DeepCopy()
		current.Spec.HumanApproval = &governancev1alpha1.HumanApproval{
			Decision:      "approved",
			Reason:        "looks good",
			ForGeneration: 0,
			ApprovedBy:    "reviewer@example.com",
		}
		Expect(k8sClient.Patch(ctx, &current, client.MergeFrom(base))).To(Succeed())

		// Reconcile: processes HumanApproval → Approved (or further)
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		// Verify request.approved AuditRecord carries approvedBy annotation
		var auditList governancev1alpha1.AuditRecordList
		Expect(k8sClient.List(ctx, &auditList, client.InNamespace("default"))).To(Succeed())
		var found bool
		for _, a := range auditList.Items {
			if a.Spec.AgentRequestRef == name && a.Spec.Event == governancev1alpha1.AuditEventRequestApproved {
				Expect(a.Spec.Annotations["approvedBy"]).To(Equal("reviewer@example.com"))
				found = true
			}
		}
		Expect(found).To(BeTrue(), "expected a request.approved AuditRecord with approvedBy annotation")
	})
})
