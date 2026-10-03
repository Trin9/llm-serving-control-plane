package middleware

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestLocalRateLimiter_ConcurrentAccess verifies the local limiter is safe for
// concurrent use and returns a limiter-backed decision per key.
func TestLocalRateLimiter_ConcurrentAccess(t *testing.T) {
	limiter := NewLocalRateLimiter(1000, 2000)

	var wg sync.WaitGroup
	results := make([]bool, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ok, err := limiter.Allow(context.Background(), "test-user-concurrent")
			assert.NoError(t, err)
			results[idx] = ok
		}(i)
	}
	wg.Wait()

	// A fresh bucket has full burst capacity, so every concurrent request must
	// be admitted (no rejection until the bucket drains).
	for i, ok := range results {
		if !ok {
			t.Fatalf("request %d unexpectedly rejected on fresh bucket", i)
		}
	}
}

// TestLocalRateLimiter_RejectsAfterBurst verifies the local token bucket still
// enforces the burst ceiling (pre-distributed behavior preserved).
func TestLocalRateLimiter_RejectsAfterBurst(t *testing.T) {
	limiter := NewLocalRateLimiter(1000, 5)

	allowed := 0
	for i := 0; i < 10; i++ {
		ok, err := limiter.Allow(context.Background(), "burst-user")
		assert.NoError(t, err)
		if ok {
			allowed++
		}
	}
	assert.Equal(t, 5, allowed, "burst capacity should cap immediate admission at 5")
}
