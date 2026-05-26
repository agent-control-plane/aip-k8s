package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
)

func setupWatchTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func newFakeClientWithScheme(s *runtime.Scheme, objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&v1alpha1.MCPServer{}).Build()
}

func TestMCPServerCache_UpsertGetRemove(t *testing.T) {
	g := gomega.NewWithT(t)
	cache := newMCPServerCache()

	// Upsert a server
	tools := []MCPTool{{Name: "tool1", ReadOnly: true}}
	cache.upsert("srv1", "http://srv1", "token1", tools)

	srv := cache.get("srv1")
	g.Expect(srv).NotTo(gomega.BeNil())
	g.Expect(srv.Name).To(gomega.Equal("srv1"))
	g.Expect(srv.URL).To(gomega.Equal("http://srv1"))
	g.Expect(srv.BearerToken).To(gomega.Equal("token1"))
	g.Expect(srv.Tools).To(gomega.HaveLen(1))

	// getAll returns a copy
	all := cache.getAll()
	g.Expect(all).To(gomega.HaveLen(1))

	// Remove it
	cache.remove("srv1")
	g.Expect(cache.get("srv1")).To(gomega.BeNil())
	g.Expect(cache.getAll()).To(gomega.BeEmpty())
}

func TestMCPServerCache_UpsertReplacesSnapshot(t *testing.T) {
	g := gomega.NewWithT(t)
	cache := newMCPServerCache()

	cache.upsert("srv1", "http://old", "old-token", []MCPTool{{Name: "t1"}})
	oldPtr := cache.get("srv1")
	g.Expect(oldPtr).NotTo(gomega.BeNil())

	cache.upsert("srv1", "http://new", "new-token", []MCPTool{{Name: "t2"}})
	newPtr := cache.get("srv1")
	g.Expect(newPtr).NotTo(gomega.BeNil())

	// The old pointer must remain stable (snapshot semantics).
	g.Expect(oldPtr.URL).To(gomega.Equal("http://old"))
	g.Expect(oldPtr.BearerToken).To(gomega.Equal("old-token"))
	g.Expect(oldPtr.Tools[0].Name).To(gomega.Equal("t1"))

	// The new pointer reflects the update.
	g.Expect(newPtr.URL).To(gomega.Equal("http://new"))
	g.Expect(newPtr.BearerToken).To(gomega.Equal("new-token"))
	g.Expect(newPtr.Tools[0].Name).To(gomega.Equal("t2"))
}

func TestMCPServerCache_SessionPreservedOnNoopUpdate(t *testing.T) {
	g := gomega.NewWithT(t)
	cache := newMCPServerCache()

	// Initial upsert: session is empty
	cache.upsert("srv1", "http://srv1", "token1", nil)
	// get() returns a copy, so mutate the internal entry directly to simulate
	// a successful upstream session (safe in this single-goroutine test).
	cache.servers["srv1"].SessionID = "sess-abc"
	cache.servers["srv1"].sessionReady = true

	// Re-upsert with identical URL and token — session should survive
	cache.upsert("srv1", "http://srv1", "token1", nil)
	srv := cache.get("srv1")
	g.Expect(srv.SessionID).To(gomega.Equal("sess-abc"))
	g.Expect(srv.sessionReady).To(gomega.BeTrue())

	// Re-upsert with different token — session should be invalidated
	cache.upsert("srv1", "http://srv1", "token2", nil)
	srv = cache.get("srv1")
	g.Expect(srv.SessionID).To(gomega.BeEmpty())
	g.Expect(srv.sessionReady).To(gomega.BeFalse())
}

func TestMCPServerCache_ConcurrentAccess(t *testing.T) {
	g := gomega.NewWithT(t)
	cache := newMCPServerCache()
	var wg sync.WaitGroup

	// Concurrent writes
	for range 10 {
		wg.Go(func() {
			cache.upsert("srv", "url", "tok", []MCPTool{{Name: "t"}})
		})
	}

	// Concurrent reads
	for range 10 {
		wg.Go(func() {
			_ = cache.get("srv")
			_ = cache.getAll()
		})
	}

	wg.Wait()
	g.Expect(cache.get("srv")).NotTo(gomega.BeNil())
}

func TestMCPServerCache_ListNames(t *testing.T) {
	g := gomega.NewWithT(t)
	cache := newMCPServerCache()

	g.Expect(cache.listNames()).To(gomega.BeEmpty())

	cache.upsert("alpha", "http://a", "", nil)
	cache.upsert("beta", "http://b", "", nil)

	names := cache.listNames()
	g.Expect(names).To(gomega.HaveLen(2))
	g.Expect(names).To(gomega.ContainElements("alpha", "beta"))
}

func TestMCPServerCache_StaleEviction(t *testing.T) {
	g := gomega.NewWithT(t)
	cache := newMCPServerCache()

	cache.upsert("keep", "http://keep", "", nil)
	cache.upsert("evict", "http://evict", "", nil)

	present := map[string]struct{}{"keep": {}}
	for _, name := range cache.listNames() {
		if _, ok := present[name]; !ok {
			cache.remove(name)
		}
	}

	g.Expect(cache.get("keep")).NotTo(gomega.BeNil())
	g.Expect(cache.get("evict")).To(gomega.BeNil())
}

func TestUpsertMCPServerFromCRD_ResolvesBearerToken(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s := setupWatchTestScheme()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-secret", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("shh")},
	}
	cl := newFakeClientWithScheme(s, secret)
	cache := newMCPServerCache()

	crd := &v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github"},
		Spec: v1alpha1.MCPServerSpec{
			URL:             "http://github-mcp",
			SecretNamespace: "default",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "mcp-secret"},
				Key:                  "token",
			},
		},
		Status: v1alpha1.MCPServerStatus{
			Tools: []v1alpha1.MCPServerTool{
				{Name: "create_pull_request", ReadOnly: false},
				{Name: "list_pull_requests", ReadOnly: true},
			},
		},
	}

	upsertMCPServerFromCRD(ctx, crd, cl, cache)

	srv := cache.get("github")
	g.Expect(srv).NotTo(gomega.BeNil())
	g.Expect(srv.URL).To(gomega.Equal("http://github-mcp"))
	g.Expect(srv.BearerToken).To(gomega.Equal("shh"))
	g.Expect(srv.Tools).To(gomega.HaveLen(2))
	g.Expect(srv.Tools[0].Name).To(gomega.Equal("create_pull_request"))
	g.Expect(srv.Tools[0].ReadOnly).To(gomega.BeFalse())
	g.Expect(srv.Tools[1].ReadOnly).To(gomega.BeTrue())
}

// TestUpsertMCPServerFromCRD_EmptySecretNamespace verifies that a CRD with
// bearerTokenSecretRef set but an empty secretNamespace causes an early return
// (the Secret cannot be looked up without a namespace), leaving the cache empty.
func TestUpsertMCPServerFromCRD_EmptySecretNamespace(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s := setupWatchTestScheme()
	cl := newFakeClientWithScheme(s)
	cache := newMCPServerCache()

	crd := &v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github"},
		Spec: v1alpha1.MCPServerSpec{
			URL: "http://github-mcp",
			// SecretNamespace intentionally omitted — simulates a CRD that slipped
			// past CEL validation with bearerTokenSecretRef set but no namespace.
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "mcp-secret"},
				Key:                  "token",
			},
		},
	}

	upsertMCPServerFromCRD(ctx, crd, cl, cache)
	// Early return: no cache entry is created because the namespace is missing.
	g.Expect(cache.get("github")).To(gomega.BeNil())
}

// TestUpsertMCPServerFromCRD_MissingSecretKey verifies that when the referenced
// Secret exists but does not contain the expected key, upsertMCPServerFromCRD
// still creates a cache entry with an empty bearer token (auth removed).
func TestUpsertMCPServerFromCRD_MissingSecretKey(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s := setupWatchTestScheme()
	// Secret exists but only has an unrelated key — the requested "token" key is absent.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-secret", Namespace: "default"},
		Data:       map[string][]byte{"other": []byte("val")},
	}
	cl := newFakeClientWithScheme(s, secret)
	cache := newMCPServerCache()

	crd := &v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github"},
		Spec: v1alpha1.MCPServerSpec{
			URL:             "http://github-mcp",
			SecretNamespace: "default",
			BearerTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "mcp-secret"},
				Key:                  "token",
			},
		},
	}

	upsertMCPServerFromCRD(ctx, crd, cl, cache)
	// The server is still cached, but with an empty bearer token because the
	// key was missing — this matches the "key missing → proceed with empty token" path.
	srv := cache.get("github")
	g.Expect(srv).NotTo(gomega.BeNil())
	g.Expect(srv.BearerToken).To(gomega.BeEmpty())
}

func TestUpsertMCPServerFromCRD_NoSecretRef(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s := setupWatchTestScheme()
	cl := newFakeClientWithScheme(s)
	cache := newMCPServerCache()

	crd := &v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github"},
		Spec:       v1alpha1.MCPServerSpec{URL: "http://github-mcp"},
	}

	upsertMCPServerFromCRD(ctx, crd, cl, cache)
	g.Expect(cache.get("github").BearerToken).To(gomega.BeEmpty())
	g.Expect(cache.get("github").URL).To(gomega.Equal("http://github-mcp"))
}
