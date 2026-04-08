package fetchers

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestFetchK8sDeployment_Found(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()

	podLabels := map[string]string{"app": "test-dep"}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-dep",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(3)),
			Selector: &metav1.LabelSelector{
				MatchLabels: podLabels,
			},
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 2,
		},
	}

	// Service that selects the Deployment's pods (same labels as Deployment selector).
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-dep-svc",
			Namespace: "default",
			Labels:    podLabels,
		},
		Spec: corev1.ServiceSpec{
			Selector: podLabels,
		},
	}

	eps := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-dep-eps",
			Namespace: "default",
			Labels:    map[string]string{"kubernetes.io/service-name": "test-dep-svc"},
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses: []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{
					Ready: ptr.To(true),
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, svc, eps).Build()

	uri := "k8s://prod/default/deployment/test-dep"
	json, err := FetchK8sDeployment(ctx, c, uri)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(json).NotTo(gomega.BeNil())

	g.Expect(string(json.Raw)).To(gomega.ContainSubstring(`"readyReplicas":2`))
	g.Expect(string(json.Raw)).To(gomega.ContainSubstring(`"specReplicas":3`))
	g.Expect(string(json.Raw)).To(gomega.ContainSubstring(`"hasActiveEndpoints":true`))
}

func TestFetchK8sDeployment_NotFound(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	uri := "k8s://prod/default/deployment/nonexistent"
	json, err := FetchK8sDeployment(ctx, c, uri)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(string(json.Raw)).To(gomega.ContainSubstring(`"targetExists":false`))
}
