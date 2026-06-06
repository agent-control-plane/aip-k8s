package credential

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubernetesOIDCProvider exchanges an agent OIDC token for a target-audience token
// via RFC 8693 token exchange. Passthrough mode (no exchange URL) is not supported:
// forwarding the gateway-audience token to upstream servers allows compromised servers
// to replay it against the gateway.
//
// Each distinct inbound OIDC token gets its own TokenCache entry (keyed by the token's
// SHA-256 digest) to prevent cross-identity credential contamination under concurrent load.
type KubernetesOIDCProvider struct {
	tokenExchangeURL string
	audience         string
	client           *http.Client

	// caches is keyed by sha256(rawOIDCToken) hex; each value is a *TokenCache.
	// Keying by digest avoids retaining raw JWT strings (which contain sensitive claims)
	// as map keys in memory. Entries are evicted amortized on each Token() call.
	caches sync.Map
}

type oidcExchangeResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   any    `json:"expires_in"` // Can be string or int
}

// NewKubernetesOIDCProvider creates a new KubernetesOIDCProvider.
// tokenExchangeURL must be non-empty; passthrough mode is intentionally unsupported.
func NewKubernetesOIDCProvider(tokenExchangeURL, audience string) *KubernetesOIDCProvider {
	return &KubernetesOIDCProvider{
		tokenExchangeURL: tokenExchangeURL,
		audience:         audience,
		client:           &http.Client{Timeout: 15 * time.Second},
	}
}

// WithClient overrides the HTTP client (used for testing).
func (p *KubernetesOIDCProvider) WithClient(httpClient *http.Client) *KubernetesOIDCProvider {
	p.client = httpClient
	return p
}

// tokenKey returns a safe map key for rawOIDCToken: the hex-encoded SHA-256 digest.
// This avoids retaining full JWT strings (including header/payload) as map keys.
func tokenKey(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// doExchange performs the RFC 8693 token exchange for the given assertion.
func (p *KubernetesOIDCProvider) doExchange(ctx context.Context, assertion string) (string, time.Time, error) {
	data := url.Values{}
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	data.Set("subject_token", assertion)
	data.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")
	if p.audience != "" {
		data.Set("audience", p.audience)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.tokenExchangeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create token exchange request: %w", err)
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

	var tokenResp oidcExchangeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&tokenResp); err != nil {
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

// Token returns the exchanged token for the given inbound OIDC token.
// Each distinct rawOIDCToken gets its own TokenCache keyed by sha256(token), so
// concurrent callers with different identities cannot share or overwrite each other's
// cached credential. Expired cache entries are evicted amortized on each call.
func (p *KubernetesOIDCProvider) Token(ctx context.Context, rawOIDCToken string) (string, error) {
	if p.tokenExchangeURL == "" {
		// Passthrough mode is not supported: the inbound OIDC token is scoped to the
		// gateway audience. Forwarding it to upstream MCP servers allows a compromised
		// server to replay the token against the gateway.
		return "", fmt.Errorf("KubernetesOIDC: tokenExchangeURL is required; passthrough is not supported")
	}

	if rawOIDCToken == "" {
		return "", fmt.Errorf("raw OIDC token is required for Kubernetes token exchange")
	}

	key := tokenKey(rawOIDCToken)

	// Fast path: cache entry already exists for this token digest.
	if v, ok := p.caches.Load(key); ok {
		return v.(*TokenCache).Get(ctx)
	}

	// Slow path: create a new TokenCache for this token and store it atomically.
	tok := rawOIDCToken // capture for the closure
	c := NewTokenCache(func(ctx context.Context) (string, time.Time, error) {
		return p.doExchange(ctx, tok)
	})
	actual, _ := p.caches.LoadOrStore(key, c)
	result, err := actual.(*TokenCache).Get(ctx)
	if err != nil && actual == c {
		// First fetch for this new entry failed. Evict it so the closure's
		// reference to the raw OIDC token is released. IsExpired() returns false
		// for entries that have never fetched successfully, so the amortized
		// cleanup loop below would never evict it. CompareAndDelete is used to
		// avoid removing a replacement entry written by a concurrent caller.
		p.caches.CompareAndDelete(key, c)
	}

	// Amortized cleanup: evict entries whose exchanged token has expired.
	// This bounds memory growth as inbound OIDC tokens rotate.
	p.caches.Range(func(k, v any) bool {
		if v.(*TokenCache).IsExpired() {
			p.caches.Delete(k)
		}
		return true
	})

	return result, err
}

// KubernetesTokenRequestProvider fetches tokens using the ServiceAccount TokenRequest API.
type KubernetesTokenRequestProvider struct {
	client                  client.Client
	serviceAccountName      string
	serviceAccountNamespace string
	expirationSeconds       *int32
	audiences               []string

	cache *TokenCache
}

// NewKubernetesTokenRequestProvider creates a new KubernetesTokenRequestProvider.
func NewKubernetesTokenRequestProvider(
	cl client.Client,
	saName, saNamespace string,
	expSec *int32,
	auds []string,
) *KubernetesTokenRequestProvider {
	p := &KubernetesTokenRequestProvider{
		client:                  cl,
		serviceAccountName:      saName,
		serviceAccountNamespace: saNamespace,
		expirationSeconds:       expSec,
		audiences:               auds,
	}
	p.cache = NewTokenCache(p.fetchToken)
	return p
}

func (p *KubernetesTokenRequestProvider) fetchToken(ctx context.Context) (string, time.Time, error) {
	var expSec int64 = 300 // default 5 minutes
	if p.expirationSeconds != nil {
		expSec = int64(*p.expirationSeconds)
	}

	tr := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         p.audiences,
			ExpirationSeconds: &expSec,
		},
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.serviceAccountName,
			Namespace: p.serviceAccountNamespace,
		},
	}

	// Create TokenRequest via SubResource
	err := p.client.SubResource("token").Create(ctx, sa, tr)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create TokenRequest for service account %s/%s: %w", p.serviceAccountNamespace, p.serviceAccountName, err)
	}

	if tr.Status.Token == "" {
		return "", time.Time{}, fmt.Errorf("TokenRequest returned empty token for service account %s/%s", p.serviceAccountNamespace, p.serviceAccountName)
	}

	expiresAt := tr.Status.ExpirationTimestamp.Time
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(time.Duration(expSec) * time.Second)
	}

	return tr.Status.Token, expiresAt, nil
}

// Token returns the TokenRequest token (cached).
func (p *KubernetesTokenRequestProvider) Token(ctx context.Context, rawOIDCToken string) (string, error) {
	return p.cache.Get(ctx)
}
