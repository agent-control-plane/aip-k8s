package fetchers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var GitHubBaseURL = "https://api.github.com"

// FetchGitHub fetches metadata for a GitHub repository.
// targetURI format: github://<org>/<repo>
func FetchGitHub(ctx context.Context, c client.Client, targetURI string) (*apiextensionsv1.JSON, error) {
	u, err := url.Parse(targetURI)
	if err != nil || u.Scheme != "github" {
		return nil, fmt.Errorf("invalid GitHub URI: %s", targetURI)
	}

	org := u.Host
	repo := strings.TrimPrefix(u.Path, "/")
	if org == "" || repo == "" {
		return nil, fmt.Errorf("invalid GitHub URI: %s (missing org or repo)", targetURI)
	}

	// 1. Fetch token
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Name: "aip-github-token", Namespace: "aip-system"}, &secret); err != nil {
		return nil, fmt.Errorf("failed to fetch GitHub token from Secret aip-github-token: %w", err)
	}
	tokenBytes, ok := secret.Data["token"]
	if !ok {
		return nil, fmt.Errorf("GitHub token not found in Secret aip-github-token (missing 'token' key)")
	}
	token := string(tokenBytes)

	// 2. Call GitHub API
	httpClient := &http.Client{Timeout: 10 * time.Second}

	// Fetch Repo Info
	repoURL := fmt.Sprintf("%s/repos/%s/%s", GitHubBaseURL, org, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build GitHub repo request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call GitHub API for repo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d for repo", resp.StatusCode)
	}

	var ghRepo struct {
		Name          string `json:"name"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ghRepo); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub repo response: %w", err)
	}

	// Fetch PR Count
	prURL := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open", GitHubBaseURL, org, repo)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, prURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build GitHub PR request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)

	respPR, err := httpClient.Do(req)
	var openPRCount int
	if err == nil {
		defer func() { _ = respPR.Body.Close() }()
		if respPR.StatusCode == http.StatusOK {
			var prs []any
			if err := json.NewDecoder(respPR.Body).Decode(&prs); err == nil {
				openPRCount = len(prs)
			}
		}
	}

	// Result
	result := struct {
		Title         string `json:"title"`
		DefaultBranch string `json:"defaultBranch"`
		OpenPRCount   int    `json:"openPRCount"`
		CIStatus      string `json:"ciStatus"`
	}{
		Title:         ghRepo.Name,
		DefaultBranch: ghRepo.DefaultBranch,
		OpenPRCount:   openPRCount,
		CIStatus:      "unknown", // No real CI integration yet; avoid false-positive signals
	}

	raw, _ := json.Marshal(result)
	return &apiextensionsv1.JSON{Raw: raw}, nil
}
