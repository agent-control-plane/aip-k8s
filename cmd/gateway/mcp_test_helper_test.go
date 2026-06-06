package main

import (
	"context"

	"github.com/agent-control-plane/aip-k8s/internal/credential"
)

type fakeProvider struct {
	token string
}

func (f fakeProvider) Token(ctx context.Context, rawOIDCToken string) (string, error) {
	return f.token, nil
}

func setupTestRegCache() *registrationCache {
	token := "test-token"
	return &registrationCache{
		providers: map[string]map[string]credential.Provider{
			"": {
				"github": fakeProvider{token: token},
				"k8s":    fakeProvider{token: token},
				"jira":   fakeProvider{token: token},
			},
			"agent-1": {
				"github":     fakeProvider{token: token},
				"k8s":        fakeProvider{token: token},
				"jira":       fakeProvider{token: token},
				"github-mcp": fakeProvider{token: token},
			},
		},
	}
}
