//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

var _ = Describe("AgentRegistration", Ordered, func() {
	var ns string

	BeforeAll(func() {
		ns = "e2e-areg-" + randomSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterAll(func() {
		Expect(k8sClient.DeleteAllOf(ctx, &governancev1alpha1.AgentRegistration{},
			client.InNamespace(ns))).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &governancev1alpha1.AgentTrustProfile{},
			client.InNamespace(ns))).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &governancev1alpha1.AgentRequest{},
			client.InNamespace(ns))).To(Succeed())
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("controller creates ATP when AgentRegistration is applied", func() {
		agentID := "e2e-payment-bot"
		reg := &governancev1alpha1.AgentRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "payment-bot",
				Namespace: ns,
			},
			Spec: governancev1alpha1.AgentRegistrationSpec{
				AgentIdentity: agentID,
				OIDC: &governancev1alpha1.AgentRegistrationOIDC{
					Issuer:          "https://keycloak.example.com/realms/aip",
					SubjectClaim:    "azp",
					AllowedSubjects: []string{"payment-bot"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, reg)).To(Succeed())

		profileName := governancev1alpha1.ProfileNameForAgent(agentID)
		Eventually(func() error {
			var atp governancev1alpha1.AgentTrustProfile
			return k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)
		}, 30*time.Second, time.Second).Should(Succeed(), "ATP should be created by controller")

		var atp governancev1alpha1.AgentTrustProfile
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)).To(Succeed())
		Expect(atp.Spec.AgentIdentity).To(Equal(agentID))
	})

	It("ATP persists after AgentRegistration is deleted", func() {
		agentID := "e2e-payment-bot" // same agent from previous It
		profileName := governancev1alpha1.ProfileNameForAgent(agentID)

		// Delete the registration.
		reg := &governancev1alpha1.AgentRegistration{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "payment-bot", Namespace: ns}, reg)).To(Succeed())
		Expect(k8sClient.Delete(ctx, reg)).To(Succeed())

		// ATP must still exist after deletion.
		Consistently(func() error {
			var atp governancev1alpha1.AgentTrustProfile
			return k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)
		}, 5*time.Second, time.Second).Should(Succeed(), "ATP must not be deleted when Registration is removed")
	})

	It("ATP is still bootstrapped reactively from AgentRequest when no Registration exists", func() {
		agentID := "e2e-legacy-no-reg"
		profileName := governancev1alpha1.ProfileNameForAgent(agentID)

		// Create an AgentRequest with no Registration.
		ar := &governancev1alpha1.AgentRequest{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "ar-legacy-",
				Namespace:    ns,
			},
			Spec: governancev1alpha1.AgentRequestSpec{
				AgentIdentity: agentID,
				Target:        governancev1alpha1.Target{URI: "k8s://test/resource"},
				Action:        "read",
				Reason:        "e2e test",
			},
		}
		Expect(k8sClient.Create(ctx, ar)).To(Succeed())

		Eventually(func() error {
			var atp governancev1alpha1.AgentTrustProfile
			return k8sClient.Get(ctx, types.NamespacedName{Name: profileName, Namespace: ns}, &atp)
		}, 30*time.Second, time.Second).Should(Succeed(), "ATP should be bootstrapped reactively from AgentRequest")
	})
})

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
}
