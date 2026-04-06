package fetchers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestFetchGitHub_NoSecret(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	uri := "github://org/repo"
	jsonRes, err := FetchGitHub(ctx, c, uri)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("aip-github-token"))
	g.Expect(jsonRes).To(gomega.BeNil())
}

func TestFetchGitHub_Success(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()

	// Mock GitHub API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintln(w, `{"name":"repo","default_branch":"main"}`)
		case "/repos/org/repo/pulls":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintln(w, `[{"id":1}, {"id":2}, {"id":3}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// 1. Create Secret with token
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aip-github-token",
			Namespace: "aip-system",
		},
		Data: map[string][]byte{
			"token": []byte("test-token"),
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = "https://api.github.com" }()

	uri := "github://org/repo"
	jsonRes, err := FetchGitHub(ctx, c, uri)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(jsonRes).NotTo(gomega.BeNil())

	g.Expect(string(jsonRes.Raw)).To(gomega.ContainSubstring(`"title":"repo"`))
	g.Expect(string(jsonRes.Raw)).To(gomega.ContainSubstring(`"openPRCount":3`))
}
