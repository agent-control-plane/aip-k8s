package credential

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const refreshBuffer = 5 * time.Minute

type TokenCache struct {
	// Legacy single-value cache fields (for backward compatibility)
	fetch     func(ctx context.Context) (string, time.Time, error)
	token     string
	expiresAt time.Time
	sf        singleflight.Group

	// New multi-value cache fields (Step 3)
	mu    sync.RWMutex
	store map[string]*cachedEntry
	group singleflight.Group
	Clock func() time.Time
}

type cachedEntry struct {
	token  string
	expiry time.Time
}

// NewTokenCache creates a new TokenCache.
// It accepts an optional fetch function for backward compatibility with the legacy TokenCache.
func NewTokenCache(fetch ...func(ctx context.Context) (string, time.Time, error)) *TokenCache {
	c := &TokenCache{
		store: make(map[string]*cachedEntry),
	}
	if len(fetch) > 0 {
		c.fetch = fetch[0]
	}
	return c
}

func (c *TokenCache) now() time.Time {
	if c.Clock != nil {
		return c.Clock()
	}
	return time.Now()
}

// GetOrFetch returns the cached token if fresh (time.Until(expiry) >= refreshBuffer).
// If stale or missing, calls fetch exactly once per key even under concurrent callers —
// singleflight deduplicates the in-flight request.
func (c *TokenCache) GetOrFetch(
	ctx context.Context,
	key string,
	fetch func(context.Context) (token string, expiry time.Time, err error),
) (string, error) {
	c.mu.RLock()
	entry, exists := c.store[key]
	if exists && entry.expiry.Sub(c.now()) >= refreshBuffer {
		token := entry.token
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	// Use singleflight to deduplicate concurrent fetch calls.
	val, err, _ := c.group.Do(key, func() (any, error) {
		// Double check cache inside singleflight block in case another concurrent fetch finished
		c.mu.RLock()
		entry, exists := c.store[key]
		if exists && entry.expiry.Sub(c.now()) >= refreshBuffer {
			token := entry.token
			c.mu.RUnlock()
			return token, nil
		}
		c.mu.RUnlock()

		token, expiry, err := fetch(ctx)
		if err != nil {
			return "", err
		}

		c.mu.Lock()
		c.store[key] = &cachedEntry{
			token:  token,
			expiry: expiry,
		}
		c.mu.Unlock()

		return token, nil
	})

	if err != nil {
		return "", err
	}
	return val.(string), nil
}

// Delete removes the cached entry for key. Called by Invalidate() on providers
// when their AgentRegistration changes.
func (c *TokenCache) Delete(key string) {
	c.mu.Lock()
	delete(c.store, key)
	c.mu.Unlock()
}

// IsExpired reports whether the legacy cached token has passed its expiry time.
func (c *TokenCache) IsExpired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token != "" && c.now().After(c.expiresAt)
}

// Get retrieves the legacy token, either from the cache if valid or by calling the fetch function.
func (c *TokenCache) Get(ctx context.Context) (string, error) {
	c.mu.RLock()
	// Reuse token if it is valid and has at least 30 seconds left.
	if c.token != "" && c.now().Add(30*time.Second).Before(c.expiresAt) {
		token := c.token
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	val, err, _ := c.sf.Do("fetch", func() (any, error) {
		c.mu.RLock()
		if c.token != "" && c.now().Add(30*time.Second).Before(c.expiresAt) {
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
