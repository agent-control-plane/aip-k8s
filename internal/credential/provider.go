package credential

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Provider defines the interface for retrieving a bearer token for outbound requests.
type Provider interface {
	// Token returns the outbound credential token, potentially using the inbound OIDC raw token.
	Token(ctx context.Context, rawOIDCToken string) (string, error)
}

// TokenCache wraps a fetch function and caches the result until it is close to expiry.
// It uses singleflight to deduplicate concurrent fetch calls.
type TokenCache struct {
	fetch func(ctx context.Context) (string, time.Time, error)

	mu        sync.RWMutex
	token     string
	expiresAt time.Time
	sf        singleflight.Group
}

// NewTokenCache creates a new TokenCache.
func NewTokenCache(fetch func(ctx context.Context) (string, time.Time, error)) *TokenCache {
	return &TokenCache{
		fetch: fetch,
	}
}

// Get retrieves the token, either from the cache if valid or by calling the fetch function.
func (c *TokenCache) Get(ctx context.Context) (string, error) {
	c.mu.RLock()
	// Reuse token if it is valid and has at least 30 seconds left.
	if c.token != "" && time.Now().Add(30*time.Second).Before(c.expiresAt) {
		token := c.token
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	val, err, _ := c.sf.Do("fetch", func() (any, error) {
		c.mu.RLock()
		if c.token != "" && time.Now().Add(30*time.Second).Before(c.expiresAt) {
			token := c.token
			c.mu.RUnlock()
			return token, nil
		}
		c.mu.RUnlock()

		token, expiresAt, err := c.fetch(ctx)
		if err != nil {
			return "", err
		}

		c.mu.Lock()
		c.token = token
		c.expiresAt = expiresAt
		c.mu.Unlock()

		return token, nil
	})

	if err != nil {
		return "", err
	}
	return val.(string), nil
}
