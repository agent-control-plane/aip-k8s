package credential

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AWSWebIdentityProvider exchanges the agent's OIDC token for AWS STS temporary credentials.
// It caches the exchanged credential by the raw OIDC token's SHA-256 digest to prevent
// cross-identity contamination under concurrent load.
type AWSWebIdentityProvider struct {
	roleARN         string
	roleSessionName string
	region          string
	durationSeconds *int32
	stsEndpoint     string
	client          *http.Client

	// caches is keyed by sha256(rawOIDCToken) hex; each entry is a *TokenCache.
	caches sync.Map
}

// NewAWSWebIdentityProvider creates a new AWSWebIdentityProvider.
func NewAWSWebIdentityProvider(roleARN, roleSessionName, region string, durationSeconds *int32, stsEndpoint string) *AWSWebIdentityProvider {
	return &AWSWebIdentityProvider{
		roleARN:         roleARN,
		roleSessionName: roleSessionName,
		region:          region,
		durationSeconds: durationSeconds,
		stsEndpoint:     stsEndpoint,
		client:          &http.Client{Timeout: 15 * time.Second},
	}
}

// WithClient overrides the HTTP client (used for testing).
func (p *AWSWebIdentityProvider) WithClient(client *http.Client) *AWSWebIdentityProvider {
	p.client = client
	return p
}

// WithSTSEndpoint overrides the STS endpoint URL (used for testing).
func (p *AWSWebIdentityProvider) WithSTSEndpoint(endpoint string) *AWSWebIdentityProvider {
	p.stsEndpoint = endpoint
	return p
}

// awsSTSResponse matches the XML returned by STS AssumeRoleWithWebIdentity
type awsSTSResponse struct {
	XMLName xml.Name `xml:"AssumeRoleWithWebIdentityResponse"`
	Result  struct {
		Credentials struct {
			AccessKeyId     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"Credentials"`
	} `xml:"AssumeRoleWithWebIdentityResult"`
}

func (p *AWSWebIdentityProvider) doExchange(ctx context.Context, assertion string) (string, time.Time, error) {
	endpoint := p.stsEndpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://sts.%s.amazonaws.com/", p.region)
	}

	data := url.Values{}
	data.Set("Action", "AssumeRoleWithWebIdentity")
	data.Set("Version", "2011-06-15")
	data.Set("RoleArn", p.roleARN)
	data.Set("RoleSessionName", p.roleSessionName)
	data.Set("WebIdentityToken", assertion)
	if p.durationSeconds != nil {
		data.Set("DurationSeconds", fmt.Sprintf("%d", *p.durationSeconds))
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create STS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("STS request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", time.Time{}, fmt.Errorf("STS request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var stsResp awsSTSResponse
	if err := xml.NewDecoder(resp.Body).Decode(&stsResp); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to decode STS XML response: %w", err)
	}

	creds := stsResp.Result.Credentials
	if creds.SessionToken == "" {
		return "", time.Time{}, fmt.Errorf("STS response did not contain SessionToken")
	}

	var expiresAt time.Time
	if creds.Expiration != "" {
		if t, err := time.Parse(time.RFC3339, creds.Expiration); err == nil {
			expiresAt = t
		}
	}
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(3600 * time.Second)
	}

	bundle := struct {
		AccessKeyID     string `json:"accessKeyID"`
		SecretAccessKey string `json:"secretAccessKey"`
		SessionToken    string `json:"sessionToken"`
		Expiration      string `json:"expiration,omitempty"`
	}{
		AccessKeyID:     creds.AccessKeyId,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
		Expiration:      creds.Expiration,
	}

	b, err := json.Marshal(bundle)
	if err != nil {
		return "", time.Time{}, err
	}

	return string(b), expiresAt, nil
}

// Token returns the cached or freshly exchanged AWS credentials.
// Each distinct rawOIDCToken gets its own TokenCache keyed by sha256(token).
func (p *AWSWebIdentityProvider) Token(ctx context.Context, rawOIDCToken string) (string, error) {
	if rawOIDCToken == "" {
		return "", fmt.Errorf("raw OIDC token is required for AWS exchange")
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
		// First fetch for this new entry failed. Evict it.
		p.caches.CompareAndDelete(key, c)
	}

	// Amortized cleanup: evict entries whose exchanged token has expired.
	p.caches.Range(func(k, v any) bool {
		if v.(*TokenCache).IsExpired() {
			p.caches.Delete(k)
		}
		return true
	})

	return result, err
}
