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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func setupMCPServerScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	_ = governancev1alpha1.AddToScheme(s)
	return s
}

// newMCPServerReconciler creates an MCPServerReconciler backed by the given fake client.
func newMCPServerReconciler(fc client.WithWatch) *MCPServerReconciler {
	return &MCPServerReconciler{
		Client:    fc,
		APIReader: fc,
		Scheme:    fc.Scheme(),
	}
}

func TestMCPServerReconcile_UpstreamSuccess(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "sess-ok")
		_, _ = w.Write([]byte("data: " + `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"get_file_contents"},{"name":"create_pull_request"}]}}` + "\n\n"))
	}))
	defer upstream.Close()

	s := setupMCPServerScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("my-token")},
	}
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test-server"},
		Spec: governancev1alpha1.MCPServerSpec{
			URL:             upstream.URL,
			SecretNamespace: "default",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "test-token"},
				Key:                  "token",
			},
			ReadOnlyTools: []string{"get_file_contents"},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server, secret).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)
	r.HTTPClient = http.DefaultClient

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-server"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.MCPServer
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "test-server"}, &updated)).To(gomega.Succeed())

	g.Expect(updated.Status.DiscoveredToolCount).To(gomega.Equal(2))
	g.Expect(updated.Status.LastSyncTime).NotTo(gomega.BeNil())

	expectedTools := []governancev1alpha1.MCPServerTool{
		{Name: "get_file_contents", ReadOnly: true},
		{Name: "create_pull_request", ReadOnly: false},
	}
	g.Expect(updated.Status.Tools).To(gomega.Equal(expectedTools))

	g.Expect(updated.Status.Conditions).NotTo(gomega.BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(gomega.Equal("Synced"))
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(gomega.Equal("DiscoverySucceeded"))
}

func TestMCPServerReconcile_UpstreamError(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "sess-err")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"Internal error"}}` + "\n\n"))
	}))
	defer upstream.Close()

	s := setupMCPServerScheme()
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "error-server"},
		Spec: governancev1alpha1.MCPServerSpec{
			URL: upstream.URL,
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)
	r.HTTPClient = http.DefaultClient

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "error-server"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.MCPServer
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "error-server"}, &updated)).To(gomega.Succeed())

	g.Expect(updated.Status.DiscoveredToolCount).To(gomega.Equal(0))

	g.Expect(updated.Status.Conditions).NotTo(gomega.BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(gomega.Equal("Synced"))
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(gomega.Equal("DiscoveryFailed"))
}

func TestMCPServerReconcile_MissingSecret(t *testing.T) {
	g := gomega.NewWithT(t)

	s := setupMCPServerScheme()
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "no-secret"},
		Spec: governancev1alpha1.MCPServerSpec{
			URL:             "http://dummy:8080",
			SecretNamespace: "default",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "nonexistent"},
				Key:                  "token",
			},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)
	r.HTTPClient = http.DefaultClient

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "no-secret"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.MCPServer
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "no-secret"}, &updated)).To(gomega.Succeed())

	g.Expect(updated.Status.Conditions).NotTo(gomega.BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(gomega.Equal("DiscoveryFailed"))
	g.Expect(cond.Message).To(gomega.ContainSubstring("resolve token"))
}

func TestMCPServerReconcile_DeletedServer(t *testing.T) {
	g := gomega.NewWithT(t)

	s := setupMCPServerScheme()
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "does-not-exist"},
	})
	g.Expect(err).To(gomega.Succeed())
}

func TestMCPServerReconcile_NoBearerToken(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "sess-noauth")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"public_tool"}]}}` + "\n\n"))
	}))
	defer upstream.Close()

	s := setupMCPServerScheme()
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "public-server"},
		Spec: governancev1alpha1.MCPServerSpec{
			URL: upstream.URL,
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)
	r.HTTPClient = http.DefaultClient

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "public-server"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.MCPServer
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "public-server"}, &updated)).To(gomega.Succeed())

	g.Expect(updated.Status.DiscoveredToolCount).To(gomega.Equal(1))
	g.Expect(updated.Status.Tools).To(gomega.Equal([]governancev1alpha1.MCPServerTool{
		{Name: "public_tool", ReadOnly: false},
	}))
	g.Expect(updated.Status.Conditions).NotTo(gomega.BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(gomega.Equal("Synced"))
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue))
}

func TestMCPServerReconcile_RequeueInterval(t *testing.T) {
	g := gomega.NewWithT(t)

	s := setupMCPServerScheme()
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test-server"},
		Spec: governancev1alpha1.MCPServerSpec{
			URL: "http://dummy:8080",
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-server"},
	})
	g.Expect(err).To(gomega.Succeed())
	g.Expect(result.RequeueAfter).To(gomega.Equal(5 * time.Minute))
}

func TestResolveBearerToken_Success(t *testing.T) {
	g := gomega.NewWithT(t)
	s := setupMCPServerScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "my-ns"},
		Data:       map[string][]byte{"token": []byte("my-token-value")},
	}
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	r := newMCPServerReconciler(fc)

	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: ""},
		Spec: governancev1alpha1.MCPServerSpec{
			SecretNamespace: "my-ns",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
				Key:                  "token",
			},
		},
	}

	token, err := r.resolveBearerToken(context.Background(), server)
	g.Expect(err).To(gomega.Succeed())
	g.Expect(token).To(gomega.Equal("my-token-value"))
}

func TestResolveBearerToken_NoRef(t *testing.T) {
	g := gomega.NewWithT(t)
	s := setupMCPServerScheme()

	fc := fake.NewClientBuilder().WithScheme(s).Build()
	r := newMCPServerReconciler(fc)

	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec:       governancev1alpha1.MCPServerSpec{},
	}

	token, err := r.resolveBearerToken(context.Background(), server)
	g.Expect(err).To(gomega.Succeed())
	g.Expect(token).To(gomega.Equal(""))
}

func TestResolveBearerToken_MissingSecret(t *testing.T) {
	g := gomega.NewWithT(t)
	s := setupMCPServerScheme()

	fc := fake.NewClientBuilder().WithScheme(s).Build()
	r := newMCPServerReconciler(fc)

	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: governancev1alpha1.MCPServerSpec{
			SecretNamespace: "some-ns",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "nonexistent"},
				Key:                  "token",
			},
		},
	}

	_, err := r.resolveBearerToken(context.Background(), server)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("nonexistent"))
}

func TestResolveBearerToken_DefaultKey(t *testing.T) {
	g := gomega.NewWithT(t)
	s := setupMCPServerScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("default-key-value")},
	}
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	r := newMCPServerReconciler(fc)

	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: governancev1alpha1.MCPServerSpec{
			SecretNamespace: "default",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
			},
		},
	}

	token, err := r.resolveBearerToken(context.Background(), server)
	g.Expect(err).To(gomega.Succeed())
	g.Expect(token).To(gomega.Equal("default-key-value"))
}

func TestResolveBearerToken_MissingKey(t *testing.T) {
	g := gomega.NewWithT(t)
	s := setupMCPServerScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
		Data:       map[string][]byte{"other-key": []byte("value")},
	}
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	r := newMCPServerReconciler(fc)

	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: governancev1alpha1.MCPServerSpec{
			SecretNamespace: "default",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
				Key:                  "nonexistent-key",
			},
		},
	}

	_, err := r.resolveBearerToken(context.Background(), server)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("nonexistent-key"))
}

func TestResolveBearerToken_ClusterScopedWithSecretNamespace(t *testing.T) {
	g := gomega.NewWithT(t)
	s := setupMCPServerScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "some-ns"},
		Data:       map[string][]byte{"token": []byte("cross-ns-token")},
	}
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	r := newMCPServerReconciler(fc)

	// Cluster-scoped MCPServer (Namespace == "") with explicit secretNamespace.
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-scoped"},
		Spec: governancev1alpha1.MCPServerSpec{
			SecretNamespace: "some-ns",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
				Key:                  "token",
			},
		},
	}

	token, err := r.resolveBearerToken(context.Background(), server)
	g.Expect(err).To(gomega.Succeed())
	g.Expect(token).To(gomega.Equal("cross-ns-token"))
}

func TestResolveBearerToken_ClusterScopedNoSecretNamespaceFails(t *testing.T) {
	g := gomega.NewWithT(t)
	s := setupMCPServerScheme()

	fc := fake.NewClientBuilder().WithScheme(s).Build()
	r := newMCPServerReconciler(fc)

	// Cluster-scoped with no secretNamespace — Secret lookup will use empty namespace.
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-scoped"},
		Spec: governancev1alpha1.MCPServerSpec{
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "any-secret"},
				Key:                  "token",
			},
		},
	}

	_, err := r.resolveBearerToken(context.Background(), server)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("secretNamespace is required"))
}

func TestMapSecretToMCPServer(t *testing.T) {
	g := gomega.NewWithT(t)
	s := setupMCPServerScheme()

	server1 := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "server1", Namespace: ""},
		Spec: governancev1alpha1.MCPServerSpec{
			URL:             "http://s1:8080",
			SecretNamespace: "default",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "shared-secret"},
				Key:                  "token",
			},
		},
	}
	server2 := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "server2", Namespace: ""},
		Spec: governancev1alpha1.MCPServerSpec{
			URL:             "http://s2:8080",
			SecretNamespace: "default",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "shared-secret"},
				Key:                  "token",
			},
		},
	}
	// No SecretRef — should never match.
	server3 := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "server3", Namespace: ""},
		Spec: governancev1alpha1.MCPServerSpec{
			URL: "http://s3:8080",
		},
	}
	// Different secretNamespace — should not match.
	server4 := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "server4", Namespace: ""},
		Spec: governancev1alpha1.MCPServerSpec{
			URL:             "http://s4:8080",
			SecretNamespace: "other-ns",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "shared-secret"},
				Key:                  "token",
			},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server1, server2, server3, server4).
		Build()

	r := newMCPServerReconciler(fc)

	// Secret "shared-secret" in "default" namespace triggers servers 1 and 2.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-secret", Namespace: "default"},
	}

	reqs := r.mapSecretToMCPServer(context.Background(), secret)
	g.Expect(reqs).To(gomega.HaveLen(2))

	names := make([]string, len(reqs))
	for i, req := range reqs {
		names[i] = req.Name
	}
	g.Expect(names).To(gomega.ConsistOf("server1", "server2"))
}

func TestMapSecretToMCPServer_NonSecretObject(t *testing.T) {
	g := gomega.NewWithT(t)

	fc := fake.NewClientBuilder().WithScheme(setupMCPServerScheme()).Build()
	r := newMCPServerReconciler(fc)

	reqs := r.mapSecretToMCPServer(context.Background(), &governancev1alpha1.MCPServer{})
	g.Expect(reqs).To(gomega.BeNil())
}

func TestMCPServerReconcile_PhaseTransition(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "sess-empty")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}` + "\n\n"))
	}))
	defer upstream.Close()

	s := setupMCPServerScheme()
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-server"},
		Spec: governancev1alpha1.MCPServerSpec{
			URL: upstream.URL,
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)
	r.HTTPClient = http.DefaultClient

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "empty-server"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.MCPServer
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "empty-server"}, &updated)).To(gomega.Succeed())

	g.Expect(updated.Status.DiscoveredToolCount).To(gomega.Equal(0))
	g.Expect(updated.Status.Tools).To(gomega.BeEmpty())
	g.Expect(updated.Status.Conditions).NotTo(gomega.BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(gomega.Equal("Synced"))
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue))
}

func TestMCPServerReconcile_ReadOnlyToolsMerge(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "sess-merge")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"tool_a"},{"name":"tool_b"},{"name":"tool_c"}]}}` + "\n\n"))
	}))
	defer upstream.Close()

	s := setupMCPServerScheme()
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "merge-server"},
		Spec: governancev1alpha1.MCPServerSpec{
			URL:           upstream.URL,
			ReadOnlyTools: []string{"tool_a", "tool_c"},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)
	r.HTTPClient = http.DefaultClient

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "merge-server"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.MCPServer
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "merge-server"}, &updated)).To(gomega.Succeed())

	g.Expect(updated.Status.Conditions).NotTo(gomega.BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(gomega.Equal("Synced"))
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue))

	expectedTools := []governancev1alpha1.MCPServerTool{
		{Name: "tool_a", ReadOnly: true},
		{Name: "tool_b", ReadOnly: false},
		{Name: "tool_c", ReadOnly: true},
	}

	less := func(a, b governancev1alpha1.MCPServerTool) bool { return a.Name < b.Name }
	if diff := cmp.Diff(expectedTools, updated.Status.Tools, cmpopts.SortSlices(less)); diff != "" {
		t.Errorf("Tools mismatch (-want +got):\n%s", diff)
	}
}

func TestMCPServerReconcile_ConditionObservedGeneration(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "sess-gen")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"t1"}]}}` + "\n\n"))
	}))
	defer upstream.Close()

	s := setupMCPServerScheme()
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gen-server"},
		Spec: governancev1alpha1.MCPServerSpec{
			URL: upstream.URL,
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)
	r.HTTPClient = http.DefaultClient

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gen-server"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.MCPServer
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "gen-server"}, &updated)).To(gomega.Succeed())

	for _, cond := range updated.Status.Conditions {
		g.Expect(cond.ObservedGeneration).To(gomega.Equal(updated.Status.ObservedGeneration))
	}
}

func TestReadSSEDataLine_Success(t *testing.T) {
	g := gomega.NewWithT(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"result\":{\"tools\":[{\"name\":\"test\"}]}}\n\n"))
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	g.Expect(err).To(gomega.Succeed())

	data, err := readSSEDataLine(resp)
	g.Expect(err).To(gomega.Succeed())
	g.Expect(data).To(gomega.Equal(`{"result":{"tools":[{"name":"test"}]}}`))
}

func TestReadSSEDataLine_MultipleLines(t *testing.T) {
	g := gomega.NewWithT(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: ping\n\ndata: {\"result\":{\"tools\":[]}}\n\n"))
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	g.Expect(err).To(gomega.Succeed())

	data, err := readSSEDataLine(resp)
	g.Expect(err).To(gomega.Succeed())
	g.Expect(data).To(gomega.Equal(`{"result":{"tools":[]}}`))
}

func TestReadSSEDataLine_NoData(t *testing.T) {
	g := gomega.NewWithT(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: ping\n\n"))
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	g.Expect(err).To(gomega.Succeed())

	_, err = readSSEDataLine(resp)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no SSE data line"))
}

func TestMapSecretToMCPServer_EmptyList(t *testing.T) {
	g := gomega.NewWithT(t)
	s := setupMCPServerScheme()

	fc := fake.NewClientBuilder().WithScheme(s).Build()
	r := newMCPServerReconciler(fc)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "some-secret", Namespace: "default"},
	}

	reqs := r.mapSecretToMCPServer(context.Background(), secret)
	g.Expect(reqs).To(gomega.BeNil())
}

func TestMCPServerReconcile_HTTPClientDefault(t *testing.T) {
	g := gomega.NewWithT(t)

	s := setupMCPServerScheme()
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "nil-client"},
		Spec: governancev1alpha1.MCPServerSpec{
			URL: "http://127.0.0.1:1",
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)
	r.HTTPClient = nil

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nil-client"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.MCPServer
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "nil-client"}, &updated)).To(gomega.Succeed())
	g.Expect(updated.Status.Conditions).NotTo(gomega.BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(gomega.Equal("Synced"))
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
}

func TestTruncateMessage(t *testing.T) {
	g := gomega.NewWithT(t)

	short := "short message"
	g.Expect(truncateMessage(short)).To(gomega.Equal(short))

	long := ""
	for range 300 {
		long += "x"
	}
	truncated := truncateMessage(long)
	g.Expect(truncated).To(gomega.HaveLen(256))
	g.Expect(truncated).To(gomega.Equal(long[:256]))

	// UTF-8 multi-byte: build a string where byte 256 falls inside a multi-byte rune.
	prefix := ""
	for range 255 {
		prefix += "a"
	}
	utf8long := prefix + "日本語" // 255 + 9 = 264 bytes
	truncUTF8 := truncateMessage(utf8long)
	// Should drop the partial multi-byte rune, landing at 255 bytes.
	g.Expect(truncUTF8).To(gomega.HaveLen(255))
	g.Expect(truncUTF8).To(gomega.Equal(prefix))
}

func TestInitSession_HTTPError(t *testing.T) {
	g := gomega.NewWithT(t)

	// Upstream returns 500.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	r := newMCPServerReconciler(fake.NewClientBuilder().WithScheme(setupMCPServerScheme()).Build())

	_, err := r.initSession(context.Background(), http.DefaultClient, upstream.URL, "")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("HTTP 500"))
}

func TestInitSession_EmptySessionID(t *testing.T) {
	g := gomega.NewWithT(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	r := newMCPServerReconciler(fake.NewClientBuilder().WithScheme(setupMCPServerScheme()).Build())

	_, err := r.initSession(context.Background(), http.DefaultClient, upstream.URL, "")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("empty Mcp-Session-Id"))
}

func TestToolsList_HTTPError(t *testing.T) {
	g := gomega.NewWithT(t)

	// Initialize succeeds but tools/list returns 500.
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Mcp-Session-Id", "sess-ok")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	s := setupMCPServerScheme()
	server := &governancev1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "http-err"},
		Spec: governancev1alpha1.MCPServerSpec{
			URL: upstream.URL,
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(server).
		WithStatusSubresource(&governancev1alpha1.MCPServer{}).
		Build()

	r := newMCPServerReconciler(fc)
	r.HTTPClient = http.DefaultClient

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "http-err"},
	})
	g.Expect(err).To(gomega.Succeed())

	var updated governancev1alpha1.MCPServer
	g.Expect(fc.Get(context.Background(), types.NamespacedName{Name: "http-err"}, &updated)).To(gomega.Succeed())

	cond := updated.Status.Conditions[0]
	g.Expect(cond.Reason).To(gomega.Equal("DiscoveryFailed"))
	g.Expect(cond.Message).To(gomega.ContainSubstring("503"))
}

func TestMapSecretToMCPServer_ErrorLogged(t *testing.T) {
	g := gomega.NewWithT(t)

	// Create a fake client that returns a scheme with NO MCPServer type registered.
	// This will cause List to fail, testing the error logging path.
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	fc := fake.NewClientBuilder().WithScheme(s).Build()

	r := newMCPServerReconciler(fc)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "some-secret", Namespace: "default"},
	}

	// Should not panic — the error is logged and nil is returned.
	reqs := r.mapSecretToMCPServer(context.Background(), secret)
	g.Expect(reqs).To(gomega.BeNil())
}
