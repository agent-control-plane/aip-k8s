package fetchers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var defaultGitHubMCPURL = "http://github-mcp.aip-k8s-system.svc"

func TestParseGitHubURI_Basic(t *testing.T) {
	g := gomega.NewWithT(t)

	org, repo, branch, filePath, err := parseGitHubURI("github://myorg/myrepo")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(org).To(gomega.Equal("myorg"))
	g.Expect(repo).To(gomega.Equal("myrepo"))
	g.Expect(branch).To(gomega.Equal(""))
	g.Expect(filePath).To(gomega.Equal(""))
}

func TestParseGitHubURI_WithFile(t *testing.T) {
	g := gomega.NewWithT(t)

	org, repo, branch, filePath, err := parseGitHubURI("github://myorg/myrepo/files/main/config/app.yaml")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(org).To(gomega.Equal("myorg"))
	g.Expect(repo).To(gomega.Equal("myrepo"))
	g.Expect(branch).To(gomega.Equal("main"))
	g.Expect(filePath).To(gomega.Equal("config/app.yaml"))
}

func TestParseGitHubURI_WithFileNoPath(t *testing.T) {
	g := gomega.NewWithT(t)

	org, repo, branch, filePath, err := parseGitHubURI("github://myorg/myrepo/files/feature-branch")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(org).To(gomega.Equal("myorg"))
	g.Expect(repo).To(gomega.Equal("myrepo"))
	g.Expect(branch).To(gomega.Equal("feature-branch"))
	g.Expect(filePath).To(gomega.Equal(""))
}

func TestParseGitHubURI_Invalid(t *testing.T) {
	g := gomega.NewWithT(t)

	_, _, _, _, err := parseGitHubURI("https://github.com/org/repo")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("invalid GitHub URI"))
}

func TestParseGitHubURI_NoOrg(t *testing.T) {
	g := gomega.NewWithT(t)

	_, _, _, _, err := parseGitHubURI("github://")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("missing org or repo"))
}

func TestFetchGitHubMCP_FileContents(t *testing.T) {
	g := gomega.NewWithT(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.Expect(r.URL.Path).To(gomega.Equal("/tools/call"))
		g.Expect(r.Method).To(gomega.Equal("POST"))

		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		g.Expect(json.NewDecoder(r.Body).Decode(&call)).To(gomega.Succeed())

		if call.Name == "get_file_contents" {
			g.Expect(call.Arguments["owner"]).To(gomega.Equal("myorg"))
			g.Expect(call.Arguments["repo"]).To(gomega.Equal("myrepo"))
			g.Expect(call.Arguments["path"]).To(gomega.Equal("config.yaml"))
			g.Expect(call.Arguments["branch"]).To(gomega.Equal("main"))

			w.Header().Set("Content-Type", "application/json")
			// writing to in-memory test recorder cannot fail
			_, _ = fmt.Fprintln(w, `{"content":[{"type":"text","text":"replicas: 5\nmaxNodes: 10"}],"isError":false}`)
			return
		}
		if call.Name == "list_pull_requests" {
			w.Header().Set("Content-Type", "application/json")
			// writing to in-memory test recorder cannot fail
			_, _ = fmt.Fprintln(w, `{"content":[{"type":"text","text":"[{\"id\":1},{\"id\":2}]"}],"isError":false}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		// writing to in-memory test recorder cannot fail
		_, _ = fmt.Fprintln(w, `{"message":"unknown tool"}`)
	}))
	defer server.Close()

	GitHubMCPURL = server.URL
	defer func() { GitHubMCPURL = defaultGitHubMCPURL }()

	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	uri := "github://myorg/myrepo/files/main/config.yaml"
	jsonRes, err := FetchGitHubMCP(context.Background(), c, uri)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(jsonRes).NotTo(gomega.BeNil())

	var result map[string]any
	g.Expect(json.Unmarshal(jsonRes.Raw, &result)).To(gomega.Succeed())
	g.Expect(result["owner"]).To(gomega.Equal("myorg"))
	g.Expect(result["repo"]).To(gomega.Equal("myrepo"))
	g.Expect(result["branch"]).To(gomega.Equal("main"))
	g.Expect(result["filePath"]).To(gomega.Equal("config.yaml"))
	g.Expect(result["fileContent"]).NotTo(gomega.BeNil())
	g.Expect(result["openPRCount"]).To(gomega.Equal(float64(2)))
}

func TestFetchGitHubMCP_RepoOnly(t *testing.T) {
	g := gomega.NewWithT(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.Expect(r.URL.Path).To(gomega.Equal("/tools/call"))

		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		g.Expect(json.NewDecoder(r.Body).Decode(&call)).To(gomega.Succeed())

		if call.Name == "list_pull_requests" {
			g.Expect(call.Arguments["owner"]).To(gomega.Equal("myorg"))
			g.Expect(call.Arguments["repo"]).To(gomega.Equal("myrepo"))
			g.Expect(call.Arguments["state"]).To(gomega.Equal("open"))

			w.Header().Set("Content-Type", "application/json")
			// writing to in-memory test recorder cannot fail
			_, _ = fmt.Fprintln(w, `{"content":[{"type":"text","text":"[{\"id\":1},{\"id\":2},{\"id\":3}]"}],"isError":false}`)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	GitHubMCPURL = server.URL
	defer func() { GitHubMCPURL = defaultGitHubMCPURL }()

	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	uri := "github://myorg/myrepo"
	jsonRes, err := FetchGitHubMCP(context.Background(), c, uri)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(jsonRes).NotTo(gomega.BeNil())

	var result map[string]any
	g.Expect(json.Unmarshal(jsonRes.Raw, &result)).To(gomega.Succeed())
	g.Expect(result["owner"]).To(gomega.Equal("myorg"))
	g.Expect(result["repo"]).To(gomega.Equal("myrepo"))
	g.Expect(result["ciStatus"]).To(gomega.Equal("unknown"))
	g.Expect(result["openPRCount"]).To(gomega.Equal(float64(3)))
}

func TestFetchGitHubMCP_ServerUnreachable(t *testing.T) {
	g := gomega.NewWithT(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()

	GitHubMCPURL = server.URL
	defer func() { GitHubMCPURL = defaultGitHubMCPURL }()

	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := FetchGitHubMCP(context.Background(), c, "github://myorg/myrepo/files/main/config.yaml")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("MCP server unreachable"))
}

func TestFetchGitHubMCP_InvalidURI(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := FetchGitHubMCP(context.Background(), c, "not-a-github-uri")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("invalid GitHub URI"))
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		panic(fmt.Sprintf("failed to add corev1 to scheme: %v", err))
	}
	return s
}
