package middleware

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestRedisClient builds a client tuned for tests: no retries and short
// timeouts so a closed server fails fast instead of stalling the suite.
func newTestRedisClient(t *testing.T, mr *miniredis.Miniredis) *redis.Client {
	t.Helper()
	return redis.NewClient(&redis.Options{
		Addr:         mr.Addr(),
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
		MaxRetries:   0,
	})
}

// errLimiter is a stub primary that always errors, used to exercise the
// failover path deterministically (no network latency in the test).
type errLimiter struct{}

func (errLimiter) Allow(context.Context, string) (bool, error) { return false, errors.New("primary down") }

// TestRedisRateLimiter_EnforcesFixedWindow fires 2×limit requests through one
// limiter and asserts exactly `limit` are admitted within the window.
func TestRedisRateLimiter_EnforcesFixedWindow(t *testing.T) {
	const limit = 50
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	limiter := NewRedisRateLimiter(newTestRedisClient(t, mr), limit, time.Second)

	allowed := 0
	for i := 0; i < 2*limit; i++ {
		ok, err := limiter.Allow(context.Background(), "org-1")
		require.NoError(t, err)
		if ok {
			allowed++
		}
	}
	assert.Equal(t, int(limit), allowed, "exactly limit requests should be admitted in one window")
}

// TestRedisRateLimiter_ThreeReplicasShareQuota simulates 3 gateway replicas
// (3 limiter instances, 3 clients, one shared Redis) each firing 1/3 of a 2×
// overload. The aggregate admission must stay within limit+10% — proving the
// distributed counter converges instead of tripling the quota (L7).
func TestRedisRateLimiter_ThreeReplicasShareQuota(t *testing.T) {
	const limit = 60
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	replicas := 3
	limiters := make([]*RedisRateLimiter, replicas)
	for i := 0; i < replicas; i++ {
		limiters[i] = NewRedisRateLimiter(newTestRedisClient(t, mr), limit, time.Second)
	}

	var mu sync.Mutex
	admitted := 0
	var wg sync.WaitGroup
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(lim *RedisRateLimiter) {
			defer wg.Done()
			local := 0
			for j := 0; j < (2*limit)/replicas; j++ {
				ok, err := lim.Allow(context.Background(), "shared-user")
				if err == nil && ok {
					local++
				}
			}
			mu.Lock()
			admitted += local
			mu.Unlock()
		}(limiters[i])
	}
	wg.Wait()

	upper := int64(float64(limit) * 1.1)
	assert.LessOrEqual(t, int64(admitted), upper,
		"aggregate admission %d across %d replicas exceeds shared limit +10%% (%d)", admitted, replicas, upper)
	assert.GreaterOrEqual(t, int64(admitted), int64(limit)-3,
		"aggregate admission %d unexpectedly far below limit %d", admitted, limit)
}

// TestRedisRateLimiter_ErrorPropagatesWhenRedisDown verifies a Redis outage
// surfaces as an error (so the failover limiter can fall back) rather than a
// silent fail-open or fail-closed.
func TestRedisRateLimiter_ErrorPropagatesWhenRedisDown(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	client := newTestRedisClient(t, mr)
	limiter := NewRedisRateLimiter(client, 10, time.Second)

	mr.Close() // simulate Redis outage

	_, err = limiter.Allow(context.Background(), "org-1")
	assert.Error(t, err, "redis outage must propagate an error")
}

// TestFailoverRateLimiter_FallsBackToLocal verifies that when the primary
// limiter errors, the failover limiter delegates to the local token bucket
// (preserving the pre-distributed behavior) and never returns an error.
func TestFailoverRateLimiter_FallsBackToLocal(t *testing.T) {
	localLimiter := NewLocalRateLimiter(1, 5) // 1 rps, burst 5
	failover := NewFailoverRateLimiter(errLimiter{}, localLimiter)

	allowed := 0
	for i := 0; i < 10; i++ {
		ok, err := failover.Allow(context.Background(), "fallback-user")
		require.NoError(t, err)
		if ok {
			allowed++
		}
	}
	assert.Equal(t, 5, allowed, "fallback local limiter burst=5 should cap admission at 5")
}

// TestFailoverRateLimiter_PrefersPrimaryWhenHealthy verifies the primary
// decision is honored and the fallback is not consulted while primary is up.
func TestFailoverRateLimiter_PrefersPrimaryWhenHealthy(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	redisLimiter := NewRedisRateLimiter(newTestRedisClient(t, mr), 2, time.Second)
	failover := NewFailoverRateLimiter(redisLimiter, NewLocalRateLimiter(1000, 1000))

	allowed := 0
	for i := 0; i < 5; i++ {
		ok, err := failover.Allow(context.Background(), "prefer-primary")
		require.NoError(t, err)
		if ok {
			allowed++
		}
	}
	// Primary (Redis, limit=2) governs: only 2 of 5 admitted, not the fallback's 1000.
	assert.Equal(t, 2, allowed, "primary Redis limit=2 should govern while healthy")
}
