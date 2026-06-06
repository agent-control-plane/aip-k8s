package main

import (
	"context"

	"github.com/agent-control-plane/aip-k8s/api/v1alpha1"
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
		// byAgent must be initialized (not nil) so that any future call to
		// upsert() or remove() — which write to this map — does not panic.
		byAgent: map[string]*v1alpha1.AgentRegistration{},
		providers: map[string]map[string]credential.Provider{
			// "" matches callers with no OIDC identity (unit tests without middleware).
			"": {
				"github": fakeProvider{token: token},
				"k8s":    fakeProvider{token: token},
				"jira":   fakeProvider{token: token},
			},
			// "agent-1" matches write-tool tests whose JWT subject is "agent-1".
			"agent-1": {
				"github":     fakeProvider{token: token},
				"k8s":        fakeProvider{token: token},
				"jira":       fakeProvider{token: token},
				"github-mcp": fakeProvider{token: token},
			},
		},
	}
}
