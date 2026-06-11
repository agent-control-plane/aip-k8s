package soak

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

var _ = Describe("SoakMode and Accuracy Tracking", Ordered, func() {
	const (
		grName        = "soak-mode-gr"
		reqName       = "soak-mode-req"
		agentIdentity = "soak-mode-agent"
		targetURI     = "k8s://soak/resource"
	)

	AfterAll(func() {
		By("cleaning up resources")
		_ = soakClient.DeleteAllOf(soakCtx, &governancev1alpha1.GovernedResource{},
			client.MatchingFields{"metadata.name": grName})
		_ = soakClient.DeleteAllOf(soakCtx, &governancev1alpha1.AgentRequest{},
			client.InNamespace("default"))
		_ = soakClient.DeleteAllOf(soakCtx, &governancev1alpha1.DiagnosticAccuracySummary{},
			client.InNamespace("default"))
	})

	It("should route AgentRequest to AwaitingVerdict and update accuracy on verdict", func() {
		By("creating a GovernedResource with soakMode: true")
		gr := &governancev1alpha1.GovernedResource{
			ObjectMeta: metav1.ObjectMeta{
				Name: grName,
			},
			Spec: governancev1alpha1.GovernedResourceSpec{
				URIPattern:       "k8s://soak/*",
				PermittedActions: []string{"test"},
				ContextFetcher:   "none",
				SoakMode:         true,
			},
		}
		Expect(soakClient.Create(soakCtx, gr)).To(Succeed())

		By("waiting for GovernedResource and capturing its generation")
		var grCurrent governancev1alpha1.GovernedResource
		Eventually(func(g Gomega) {
			g.Expect(soakClient.Get(soakCtx, types.NamespacedName{Name: grName}, &grCurrent)).To(Succeed())
			g.Expect(grCurrent.Generation).To(BeNumerically(">", 0))
		}, 10*time.Second, time.Second).Should(Succeed())

		By("creating an AgentRequest with governedResourceRef")
		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      reqName,
				Namespace: "default",
			},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: agentIdentity,
				Action:        "test",
				Target:        governancev1alpha1.Target{URI: targetURI},
				Reason:        "soak test",
				GovernedResourceRef: &governancev1alpha1.GovernedResourceRef{
					Name:       grName,
					Generation: grCurrent.Generation,
				},
			},
		}
		Expect(soakClient.Create(soakCtx, ar)).To(Succeed())

		By("waiting for Phase=AwaitingVerdict")
		Eventually(func(g Gomega) {
			var current governancev1alpha1.AgentRequest
			g.Expect(soakClient.Get(soakCtx, types.NamespacedName{Name: reqName, Namespace: "default"}, &current)).To(Succeed())
			g.Expect(current.Status.Phase).To(Equal(governancev1alpha1.PhaseAwaitingVerdict))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("submitting a verdict via status patch")
		var arCurrent governancev1alpha1.AgentRequest
		Expect(soakClient.Get(soakCtx, types.NamespacedName{Name: reqName, Namespace: "default"}, &arCurrent)).To(Succeed())
		base := arCurrent.DeepCopy()
		arCurrent.Status.Phase = governancev1alpha1.PhaseCompleted
		arCurrent.Status.Verdict = "correct"
		now := metav1.Now()
		arCurrent.Status.VerdictAt = &now
		Expect(soakClient.Status().Patch(soakCtx, &arCurrent, client.MergeFrom(base))).To(Succeed())

		By("waiting for Phase=Completed")
		Eventually(func(g Gomega) {
			var current governancev1alpha1.AgentRequest
			g.Expect(soakClient.Get(soakCtx, types.NamespacedName{Name: reqName, Namespace: "default"}, &current)).To(Succeed())
			g.Expect(current.Status.Phase).To(Equal(governancev1alpha1.PhaseCompleted))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("verifying DiagnosticAccuracySummary is updated")
		summaryName := governancev1alpha1.ProfileNameForAgent(agentIdentity)
		Eventually(func(g Gomega) {
			var summary governancev1alpha1.DiagnosticAccuracySummary
			g.Expect(soakClient.Get(soakCtx, types.NamespacedName{Name: summaryName, Namespace: "default"}, &summary)).To(Succeed())
			g.Expect(summary.Status.TotalReviewed).To(Equal(int64(1)))
			g.Expect(summary.Status.CorrectCount).To(Equal(int64(1)))
			g.Expect(summary.Status.DiagnosticAccuracy).NotTo(BeNil())
			g.Expect(*summary.Status.DiagnosticAccuracy).To(BeNumerically("~", 1.0, 0.001))
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})
