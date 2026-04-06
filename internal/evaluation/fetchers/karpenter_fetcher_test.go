package fetchers

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestFetchKarpenter_Found(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()

	nodepool := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "karpenter.sh/v1beta1",
			"kind":       "NodePool",
			"metadata": map[string]any{
				"name": "test-np",
			},
			"spec": map[string]any{
				"limits": map[string]any{
					"cpu":    "100",
					"memory": "400Gi",
				},
			},
		},
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{"karpenter.sh/nodepool": "test-np"},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-pending",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodepool, node, pod).Build()

	uri := "k8s://prod/karpenter.sh/nodepool/test-np"
	json, err := FetchKarpenter(ctx, c, uri)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(json).NotTo(gomega.BeNil())

	g.Expect(string(json.Raw)).To(gomega.ContainSubstring(`"currentLimitCPU":"100"`))
	g.Expect(string(json.Raw)).To(gomega.ContainSubstring(`"currentNodeCount":1`))
	g.Expect(string(json.Raw)).To(gomega.ContainSubstring(`"pendingPods":1`))
}

func TestFetchKarpenter_NotFound(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	uri := "k8s://prod/karpenter.sh/nodepool/nonexistent"
	json, err := FetchKarpenter(ctx, c, uri)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(string(json.Raw)).To(gomega.ContainSubstring(`"currentNodeCount":0`))
}
