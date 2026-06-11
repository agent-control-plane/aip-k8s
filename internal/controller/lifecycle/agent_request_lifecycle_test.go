package lifecycle

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

var _ = Describe("AgentRequest lifecycle", Ordered, func() {
	var ns string

	createNS := func() string {
		name := fmt.Sprintf("lifecycle-%d", time.Now().UnixNano()%100000)
		Expect(lcycleClient.Create(lcycleCtx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		})).To(Succeed())
		return name
	}

	BeforeAll(func() {
		ns = createNS()
	})

	AfterAll(func() {
		_ = lcycleClient.DeleteAllOf(lcycleCtx, &governancev1alpha1.AgentRequest{},
			client.InNamespace(ns))
		_ = lcycleClient.DeleteAllOf(lcycleCtx, &governancev1alpha1.AuditRecord{},
			client.InNamespace(ns))
		_ = lcycleClient.DeleteAllOf(lcycleCtx, &coordinationv1.Lease{},
			client.InNamespace(ns))
		_ = lcycleClient.DeleteAllOf(lcycleCtx, &governancev1alpha1.GovernedResource{})
		_ = lcycleClient.Delete(lcycleCtx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
	})

	It("should transition Pending -> Approved and emit AuditRecords", func() {
		By("creating the GovernedResource")
		gr := &governancev1alpha1.GovernedResource{
			ObjectMeta: metav1.ObjectMeta{
				Name: "lifecycle-gr",
			},
			Spec: governancev1alpha1.GovernedResourceSpec{
				URIPattern:       "k8s://*/*",
				PermittedActions: []string{"scale"},
				PermittedAgents:  []string{"e2e-test-agent"},
			},
		}
		Expect(lcycleClient.Create(lcycleCtx, gr)).To(Succeed())

		By("creating the AgentRequest")
		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "lifecycle-test",
				Namespace: ns,
			},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: "e2e-test-agent",
				Action:        "scale",
				Target:        governancev1alpha1.Target{URI: "k8s://prod/default/deployment/test-app"},
				Reason:        "lifecycle test",
			},
		}
		Expect(lcycleClient.Create(lcycleCtx, ar)).To(Succeed())

		By("waiting for Phase=Approved")
		Eventually(func(g Gomega) {
			var current governancev1alpha1.AgentRequest
			g.Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: ar.Name, Namespace: ns}, &current)).To(Succeed())
			g.Expect(current.Status.Phase).To(Equal(governancev1alpha1.PhaseApproved))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("asserting request.submitted AuditRecord exists")
		Eventually(func() bool {
			return lifecycleHasAuditRecord(lcycleCtx, lcycleClient, ar.Name, ns, "request.submitted")
		}, 10*time.Second, time.Second).Should(BeTrue())

		By("asserting request.approved AuditRecord exists")
		Eventually(func() bool {
			return lifecycleHasAuditRecord(lcycleCtx, lcycleClient, ar.Name, ns, "request.approved")
		}, 10*time.Second, time.Second).Should(BeTrue())
	})

	It("should transition Approved -> Executing when agent signals Executing condition", func() {
		By("fetching the AgentRequest")
		var ar governancev1alpha1.AgentRequest
		Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: "lifecycle-test", Namespace: ns}, &ar)).To(Succeed())
		Expect(ar.Status.Phase).To(Equal(governancev1alpha1.PhaseApproved))

		By("patching Executing condition")
		base := ar.DeepCopy()
		ar.Status.Conditions = append(ar.Status.Conditions, metav1.Condition{
			Type:               governancev1alpha1.ConditionExecuting,
			Status:             metav1.ConditionTrue,
			Reason:             "AgentStarted",
			Message:            "Agent is executing",
			LastTransitionTime: metav1.Now(),
		})
		Expect(lcycleClient.Status().Patch(lcycleCtx, &ar, client.MergeFrom(base))).To(Succeed())

		By("waiting for Phase=Executing")
		Eventually(func(g Gomega) {
			var current governancev1alpha1.AgentRequest
			g.Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: ar.Name, Namespace: ns}, &current)).To(Succeed())
			g.Expect(current.Status.Phase).To(Equal(governancev1alpha1.PhaseExecuting))
		}, 15*time.Second, time.Second).Should(Succeed())

		By("asserting lock.acquired AuditRecord exists")
		Eventually(func() bool {
			return lifecycleHasAuditRecord(lcycleCtx, lcycleClient, ar.Name, ns, "lock.acquired")
		}, 10*time.Second, time.Second).Should(BeTrue())

		By("asserting request.executing AuditRecord exists")
		Eventually(func() bool {
			return lifecycleHasAuditRecord(lcycleCtx, lcycleClient, ar.Name, ns, "request.executing")
		}, 10*time.Second, time.Second).Should(BeTrue())
	})

	It("should transition Executing -> Completed when agent signals Completed condition", func() {
		By("patching Completed condition")
		var ar governancev1alpha1.AgentRequest
		Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: "lifecycle-test", Namespace: ns}, &ar)).To(Succeed())
		Expect(ar.Status.Phase).To(Equal(governancev1alpha1.PhaseExecuting))

		base := ar.DeepCopy()
		ar.Status.Conditions = append(ar.Status.Conditions, metav1.Condition{
			Type:               governancev1alpha1.ConditionCompleted,
			Status:             metav1.ConditionTrue,
			Reason:             "ActionSuccess",
			Message:            "Agent completed the action",
			LastTransitionTime: metav1.Now(),
		})
		Expect(lcycleClient.Status().Patch(lcycleCtx, &ar, client.MergeFrom(base))).To(Succeed())

		By("waiting for Phase=Completed")
		Eventually(func(g Gomega) {
			var current governancev1alpha1.AgentRequest
			g.Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: ar.Name, Namespace: ns}, &current)).To(Succeed())
			g.Expect(current.Status.Phase).To(Equal(governancev1alpha1.PhaseCompleted))
		}, 15*time.Second, time.Second).Should(Succeed())

		By("asserting request.completed AuditRecord exists")
		Eventually(func() bool {
			return lifecycleHasAuditRecord(lcycleCtx, lcycleClient, ar.Name, ns, "request.completed")
		}, 10*time.Second, time.Second).Should(BeTrue())

		By("asserting lock.released AuditRecord exists")
		Eventually(func() bool {
			return lifecycleHasAuditRecord(lcycleCtx, lcycleClient, ar.Name, ns, "lock.released")
		}, 10*time.Second, time.Second).Should(BeTrue())
	})
})

var _ = Describe("SafetyPolicy evaluation", Ordered, func() {
	var ns string

	BeforeAll(func() {
		ns = fmt.Sprintf("policy-%d", time.Now().UnixNano()%100000)
		Expect(lcycleClient.Create(lcycleCtx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterAll(func() {
		_ = lcycleClient.DeleteAllOf(lcycleCtx, &governancev1alpha1.SafetyPolicy{})
		_ = lcycleClient.DeleteAllOf(lcycleCtx, &governancev1alpha1.AgentRequest{},
			client.InNamespace(ns))
		_ = lcycleClient.DeleteAllOf(lcycleCtx, &governancev1alpha1.AuditRecord{},
			client.InNamespace(ns))
		_ = lcycleClient.Delete(lcycleCtx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
	})

	It("should transition to Denied with POLICY_VIOLATION when policy matches", func() {
		By("creating the SafetyPolicy that denies prod targets")
		failClosed := "FailClosed"
		sp := &governancev1alpha1.SafetyPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "deny-prod-scale",
				Namespace: ns,
			},
			Spec: governancev1alpha1.SafetyPolicySpec{
				GovernedResourceSelector: metav1.LabelSelector{},
				Rules: []governancev1alpha1.Rule{
					{
						Name:       "block-scale",
						Type:       "StateEvaluation",
						Action:     "Deny",
						Expression: `request.spec.target.uri.startsWith('k8s://prod')`,
					},
				},
				FailureMode: &failClosed,
			},
		}
		Expect(lcycleClient.Create(lcycleCtx, sp)).To(Succeed())

		By("creating the GovernedResource")
		gr := &governancev1alpha1.GovernedResource{
			ObjectMeta: metav1.ObjectMeta{
				Name: "policy-test-gr",
			},
			Spec: governancev1alpha1.GovernedResourceSpec{
				URIPattern:       "k8s://*/*",
				PermittedActions: []string{"scale"},
			},
		}
		Expect(lcycleClient.Create(lcycleCtx, gr)).To(Succeed())

		By("creating the AgentRequest targeting prod")
		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "policy-test",
				Namespace: ns,
			},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: "e2e-test-agent",
				Action:        "scale",
				Target:        governancev1alpha1.Target{URI: "k8s://prod/default/deployment/backend"},
				Reason:        "policy test",
			},
		}
		Expect(lcycleClient.Create(lcycleCtx, ar)).To(Succeed())

		By("waiting for Phase=Denied")
		Eventually(func(g Gomega) {
			var current governancev1alpha1.AgentRequest
			g.Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: ar.Name, Namespace: ns}, &current)).To(Succeed())
			g.Expect(current.Status.Phase).To(Equal(governancev1alpha1.PhaseDenied))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("checking denial code is POLICY_VIOLATION")
		var current governancev1alpha1.AgentRequest
		Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: ar.Name, Namespace: ns}, &current)).To(Succeed())
		Expect(current.Status.Denial).NotTo(BeNil())
		Expect(current.Status.Denial.Code).To(Equal(governancev1alpha1.DenialCodePolicyViolation))
		Expect(current.Status.Denial.PolicyResults).NotTo(BeEmpty())
		Expect(current.Status.Denial.PolicyResults[0].RuleName).To(Equal("block-scale"))

		By("asserting request.denied AuditRecord exists")
		Eventually(func() bool {
			return lifecycleHasAuditRecord(lcycleCtx, lcycleClient, ar.Name, ns, "request.denied")
		}, 10*time.Second, time.Second).Should(BeTrue())
	})
})

var _ = Describe("OpsLock contention", Ordered, func() {
	var ns string

	BeforeAll(func() {
		ns = fmt.Sprintf("opslock-%d", time.Now().UnixNano()%100000)
		Expect(lcycleClient.Create(lcycleCtx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterAll(func() {
		_ = lcycleClient.DeleteAllOf(lcycleCtx, &governancev1alpha1.AgentRequest{},
			client.InNamespace(ns))
		_ = lcycleClient.DeleteAllOf(lcycleCtx, &governancev1alpha1.GovernedResource{})
		_ = lcycleClient.DeleteAllOf(lcycleCtx, &coordinationv1.Lease{},
			client.InNamespace(ns))
		_ = lcycleClient.Delete(lcycleCtx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
	})

	It("should handle concurrent requests on the same target with lock contention", func() {
		targetURI := "k8s://dev/default/deployment/locked-app"

		By("creating the GovernedResource")
		gr := &governancev1alpha1.GovernedResource{
			ObjectMeta: metav1.ObjectMeta{
				Name: "opslock-gr",
			},
			Spec: governancev1alpha1.GovernedResourceSpec{
				URIPattern:       "k8s://*/*",
				PermittedActions: []string{"update"},
			},
		}
		Expect(lcycleClient.Create(lcycleCtx, gr)).To(Succeed())

		By("creating AgentRequest 1")
		ar1 := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "opslock-1",
				Namespace: ns,
			},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: "e2e-test-agent",
				Action:        "update",
				Target:        governancev1alpha1.Target{URI: targetURI},
				Reason:        "lock test 1",
			},
		}
		Expect(lcycleClient.Create(lcycleCtx, ar1)).To(Succeed())

		By("waiting for AgentRequest 1 to be Approved")
		Eventually(func(g Gomega) {
			var current governancev1alpha1.AgentRequest
			g.Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: ar1.Name, Namespace: ns}, &current)).To(Succeed())
			g.Expect(current.Status.Phase).To(Equal(governancev1alpha1.PhaseApproved))
		}, 60*time.Second, time.Second).Should(Succeed())

		By("creating AgentRequest 2 on the same target")
		ar2 := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "opslock-2",
				Namespace: ns,
			},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: "e2e-test-agent",
				Action:        "update",
				Target:        governancev1alpha1.Target{URI: targetURI},
				Reason:        "lock test 2",
			},
		}
		Expect(lcycleClient.Create(lcycleCtx, ar2)).To(Succeed())

		By("asserting AgentRequest 1 still holds the lock")
		var ar1Current governancev1alpha1.AgentRequest
		Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: ar1.Name, Namespace: ns}, &ar1Current)).To(Succeed())
		Expect(ar1Current.Status.Phase).To(Equal(governancev1alpha1.PhaseApproved))

		By("waiting for AgentRequest 2 to be Denied due to contention")
		Eventually(func(g Gomega) {
			var current governancev1alpha1.AgentRequest
			g.Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: ar2.Name, Namespace: ns}, &current)).To(Succeed())
			g.Expect(current.Status.Phase).To(Equal(governancev1alpha1.PhaseDenied))
		}, 90*time.Second, time.Second).Should(Succeed())

		By("checking denial code is LOCK_TIMEOUT")
		var ar2Current governancev1alpha1.AgentRequest
		Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: ar2.Name, Namespace: ns}, &ar2Current)).To(Succeed())
		Expect(ar2Current.Status.Denial).NotTo(BeNil())
		Expect(ar2Current.Status.Denial.Code).To(BeElementOf(
			governancev1alpha1.DenialCodeLockTimeout,
			governancev1alpha1.DenialCodeLockContention,
		))

		By("asserting AgentRequest 1 remained Approved")
		Expect(lcycleClient.Get(lcycleCtx, types.NamespacedName{Name: ar1.Name, Namespace: ns}, &ar1Current)).To(Succeed())
		Expect(ar1Current.Status.Phase).To(Equal(governancev1alpha1.PhaseApproved))
	})
})

func lifecycleHasAuditRecord(ctx context.Context, cl client.Client, reqName, ns, event string) bool {
	var list governancev1alpha1.AuditRecordList
	if err := cl.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return false
	}
	for _, ar := range list.Items {
		if ar.Spec.AgentRequestRef == reqName && ar.Spec.Event == event {
			return true
		}
	}
	return false
}
