package credential

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	token1Val = "token-1"
	token2Val = "token-2"
)

func TestTokenCache_FreshEntry(t *testing.T) {
	cache := NewTokenCache()
	baseTime := time.Now()
	cache.Clock = func() time.Time { return baseTime }

	var fetchCalls int32
	fetch := func(ctx context.Context) (string, time.Time, error) {
		atomic.AddInt32(&fetchCalls, 1)
		return token1Val, baseTime.Add(10 * time.Minute), nil
	}

	// First call should fetch
	tok, err := cache.GetOrFetch(context.Background(), "key1", fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != token1Val {
		t.Errorf("expected token-1, got %q", tok)
	}
	if atomic.LoadInt32(&fetchCalls) != 1 {
		t.Errorf("expected 1 fetch call, got %d", fetchCalls)
	}

	// Second call should return cached token without calling fetch because time is baseTime (expiry is baseTime + 10m, buffer is 5m, so it is fresh)
	tok, err = cache.GetOrFetch(context.Background(), "key1", fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != token1Val {
		t.Errorf("expected token-1, got %q", tok)
	}
	if atomic.LoadInt32(&fetchCalls) != 1 {
		t.Errorf("expected 1 fetch call (cached), got %d", fetchCalls)
	}
}

func TestTokenCache_StaleEntry(t *testing.T) {
	cache := NewTokenCache()
	baseTime := time.Now()
	currentTime := baseTime
	cache.Clock = func() time.Time { return currentTime }

	var fetchCalls int32
	fetch := func(ctx context.Context) (string, time.Time, error) {
		atomic.AddInt32(&fetchCalls, 1)
		// Expiry is 10 minutes in the future relative to the baseTime
		return token1Val, baseTime.Add(10 * time.Minute), nil
	}

	// First call should fetch
	tok, err := cache.GetOrFetch(context.Background(), "key1", fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != token1Val {
		t.Errorf("expected token-1, got %q", tok)
	}

	// Move time forward by 6 minutes (now within 4 minutes of expiry, which is less than 5m refreshBuffer, so stale)
	currentTime = baseTime.Add(6 * time.Minute)

	// Update fetch mock to return a new token
	fetch2 := func(ctx context.Context) (string, time.Time, error) {
		atomic.AddInt32(&fetchCalls, 1)
		return token2Val, currentTime.Add(10 * time.Minute), nil
	}

	tok, err = cache.GetOrFetch(context.Background(), "key1", fetch2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != token2Val {
		t.Errorf("expected %s, got %q", token2Val, tok)
	}
	if atomic.LoadInt32(&fetchCalls) != 2 {
		t.Errorf("expected 2 fetch calls, got %d", fetchCalls)
	}
}

func TestTokenCache_ConcurrentFetchDeduplication(t *testing.T) {
	cache := NewTokenCache()
	baseTime := time.Now()
	cache.Clock = func() time.Time { return baseTime }

	var fetchCalls int32
	var startWg sync.WaitGroup
	startWg.Add(1)

	fetch := func(_ context.Context) (string, time.Time, error) { //nolint:unparam
		atomic.AddInt32(&fetchCalls, 1)
		startWg.Wait() // Block until all goroutines have initiated GetOrFetch
		return "concurrent-token", baseTime.Add(10 * time.Minute), nil
	}

	const concurrentCount = 10
	var wg sync.WaitGroup
	results := make([]string, concurrentCount)
	errs := make([]error, concurrentCount)

	for i := range concurrentCount {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tok, err := cache.GetOrFetch(context.Background(), "key1", fetch)
			results[idx] = tok
			errs[idx] = err
		}(i)
	}

	// Sleep briefly to let all goroutines hit the fetch / singleflight Do
	time.Sleep(50 * time.Millisecond)
	startWg.Done() // release fetch calls

	wg.Wait()

	if atomic.LoadInt32(&fetchCalls) != 1 {
		t.Errorf("expected fetch to be called exactly once, but got %d", fetchCalls)
	}

	for i := range concurrentCount {
		if errs[i] != nil {
			t.Errorf("goroutine %d returned error: %v", i, errs[i])
		}
		if results[i] != "concurrent-token" {
			t.Errorf("goroutine %d returned unexpected token: %s", i, results[i])
		}
	}
}

func TestTokenCache_Delete(t *testing.T) {
	cache := NewTokenCache()
	baseTime := time.Now()
	cache.Clock = func() time.Time { return baseTime }

	var fetchCalls int32
	fetch := func(ctx context.Context) (string, time.Time, error) {
		atomic.AddInt32(&fetchCalls, 1)
		return token1Val, baseTime.Add(10 * time.Minute), nil
	}

	// Fetch once
	_, _ = cache.GetOrFetch(context.Background(), "key1", fetch)
	if atomic.LoadInt32(&fetchCalls) != 1 {
		t.Fatalf("expected 1 fetch call, got %d", fetchCalls)
	}

	// Delete key1
	cache.Delete("key1")

	// Next fetch should call fetch again
	tok, err := cache.GetOrFetch(context.Background(), "key1", fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != token1Val {
		t.Errorf("expected token-1, got %q", tok)
	}
	if atomic.LoadInt32(&fetchCalls) != 2 {
		t.Errorf("expected 2 fetch calls, got %d", fetchCalls)
	}
}

func TestTokenCache_LegacyGet(t *testing.T) {
	cache := NewTokenCache()
	baseTime := time.Now()
	cache.Clock = func() time.Time { return baseTime }

	var fetchCalls int32
	fetch := func(ctx context.Context) (string, time.Time, error) {
		atomic.AddInt32(&fetchCalls, 1)
		return token1Val, baseTime.Add(10 * time.Minute), nil
	}
	cache.fetch = fetch

	// Initial Get should fetch
	tok, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != token1Val {
		t.Errorf("expected token-1, got %q", tok)
	}
	if atomic.LoadInt32(&fetchCalls) != 1 {
		t.Fatalf("expected 1 fetch call, got %d", fetchCalls)
	}

	// Second Get should return cached token without fetching
	tok, err = cache.Get(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != token1Val {
		t.Errorf("expected token-1, got %q", tok)
	}
	if atomic.LoadInt32(&fetchCalls) != 1 {
		t.Errorf("expected 1 fetch call (cached), got %d", fetchCalls)
	}
}

func TestTokenCache_LegacyIsExpired(t *testing.T) {
	cache := NewTokenCache()
	baseTime := time.Now()
	cache.Clock = func() time.Time { return baseTime }

	// No token set: not expired
	if cache.IsExpired() {
		t.Error("expected not expired when token is empty")
	}

	// Set token with future expiry
	cache.token = token1Val
	cache.expiresAt = baseTime.Add(10 * time.Minute)

	// Fresh: not expired
	if cache.IsExpired() {
		t.Error("expected not expired when token is fresh")
	}

	// Advance time past expiry
	cache.Clock = func() time.Time { return baseTime.Add(11 * time.Minute) }
	if !cache.IsExpired() {
		t.Error("expected expired when time is past expiry")
	}
}

func TestTokenCache_LegacyGetExpired(t *testing.T) {
	cache := NewTokenCache()
	baseTime := time.Now()
	currentTime := baseTime
	cache.Clock = func() time.Time { return currentTime }

	var fetchCalls int32
	fetch := func(ctx context.Context) (string, time.Time, error) {
		atomic.AddInt32(&fetchCalls, 1)
		return token1Val, baseTime.Add(10 * time.Minute), nil
	}
	cache.fetch = fetch

	// First Get: fetch
	tok, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != token1Val {
		t.Errorf("expected token-1, got %q", tok)
	}

	// Advance time past expiry
	currentTime = baseTime.Add(11 * time.Minute)

	// Second Get: should fetch again because token is expired
	atomic.StoreInt32(&fetchCalls, 0)
	fetch2 := func(ctx context.Context) (string, time.Time, error) {
		atomic.AddInt32(&fetchCalls, 1)
		return token2Val, currentTime.Add(10 * time.Minute), nil
	}
	cache.fetch = fetch2

	tok, err = cache.Get(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "token-2" {
		t.Errorf("expected %s after expiry, got %q", token2Val, tok)
	}
	if atomic.LoadInt32(&fetchCalls) != 1 {
		t.Errorf("expected 1 fetch call after expiry, got %d", fetchCalls)
	}
}
