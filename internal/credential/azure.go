package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AzureWorkloadIdentityProvider exchanges the agent's OIDC token for an Azure Entra access token.
// Each distinct inbound OIDC token gets its own TokenCache entry so that concurrent
// callers with different identities cannot overwrite each other's token.
type AzureWorkloadIdentityProvider struct {
	tenantID string
	clientID string
	scope    string
	tokenURL string
	client   *http.Client

	// caches is keyed by rawOIDCToken; each entry is a *TokenCache.
	caches sync.Map
}

type azureTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   any    `json:"expires_in"` // Can be string or int
	TokenType   string `json:"token_type"`
}

// NewAzureWorkloadIdentityProvider creates a new AzureWorkloadIdentityProvider.
func NewAzureWorkloadIdentityProvider(tenantID, clientID, scope string) *AzureWorkloadIdentityProvider {
	return &AzureWorkloadIdentityProvider{
		tenantID: tenantID,
		clientID: clientID,
		scope:    scope,
		tokenURL: fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID),
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// WithTokenURL overrides the token endpoint URL (used for testing).
func (p *AzureWorkloadIdentityProvider) WithTokenURL(tokenURL string) *AzureWorkloadIdentityProvider {
	p.tokenURL = tokenURL
	return p
}

// WithClient overrides the HTTP client (used for testing).
func (p *AzureWorkloadIdentityProvider) WithClient(client *http.Client) *AzureWorkloadIdentityProvider {
	p.client = client
	return p
}

// doExchange performs the Azure federated credential exchange for the given assertion.
func (p *AzureWorkloadIdentityProvider) doExchange(ctx context.Context, assertion string) (string, time.Time, error) {
	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", p.clientID)
	data.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	data.Set("client_assertion", assertion)
	data.Set("scope", p.scope)

	req, err := http.NewRequestWithContext(ctx, "POST", p.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", time.Time{}, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var tokenResp azureTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to decode token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("token response did not contain access_token")
	}

	var expiresIn int64 = 3600
	if tokenResp.ExpiresIn != nil {
		switch v := tokenResp.ExpiresIn.(type) {
		case float64:
			expiresIn = int64(v)
		case string:
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
				expiresIn = parsed
			}
		}
	}

	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
	return tokenResp.AccessToken, expiresAt, nil
}

// Token returns the cached or freshly exchanged Azure access token.
// Each distinct rawOIDCToken gets its own TokenCache so concurrent callers with
// different inbound identities never share a single exchanged-token slot.
func (p *AzureWorkloadIdentityProvider) Token(ctx context.Context, rawOIDCToken string) (string, error) {
	if rawOIDCToken == "" {
		return "", fmt.Errorf("raw OIDC token is required for Azure exchange")
	}

	// Fast path: cache entry already exists for this token.
	if v, ok := p.caches.Load(rawOIDCToken); ok {
		return v.(*TokenCache).Get(ctx)
	}

	// Slow path: create a new TokenCache for this token and store it atomically.
	tok := rawOIDCToken // capture for the closure
	c := NewTokenCache(func(ctx context.Context) (string, time.Time, error) {
		return p.doExchange(ctx, tok)
	})
	actual, _ := p.caches.LoadOrStore(rawOIDCToken, c)
	return actual.(*TokenCache).Get(ctx)
}
