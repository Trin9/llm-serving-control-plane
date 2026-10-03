package middleware

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// luaRateLimit is a fixed-window counter executed atomically inside Redis.
// It increments the window counter and, on the first request of a window,
// arms the expiry. The caller compares the returned count against the limit —
// the comparison happens client-side so that the decision policy (and any
// per-identity overrides) stay out of Redis.
const luaRateLimit = `
local current = redis.call('INCR', KEYS[1])
if current == 1 then
	redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return current
`

// RedisRateLimiter is a distributed fixed-window rate limiter backed by a
// shared Redis. Multiple gateway replicas increment the same per-identity
// counter, so the aggregate admission across replicas converges on `limit`
// per `window` instead of `limit * replicas`.
type RedisRateLimiter struct {
	client *redis.Client
	limit  int64
	window time.Duration
	script *redis.Script
}

// NewRedisRateLimiter creates a distributed limiter admitting at most `limit`
// requests per `window` per identity key.
func NewRedisRateLimiter(client *redis.Client, limit int64, window time.Duration) *RedisRateLimiter {
	if window <= 0 {
		window = time.Second
	}
	return &RedisRateLimiter{
		client: client,
		limit:  limit,
		window: window,
		script: redis.NewScript(luaRateLimit),
	}
}

// Allow implements RateLimiter. A Redis error is returned so callers can
// decide to fail open or fall back to a local limiter.
func (r *RedisRateLimiter) Allow(ctx context.Context, key string) (bool, error) {
	if r.client == nil {
		return false, fmt.Errorf("redis rate limiter: nil client")
	}
	redisKey := "ratelimit:" + key
	windowSeconds := int64(r.window / time.Second)
	if windowSeconds < 1 {
		windowSeconds = 1
	}

	current, err := r.script.Run(ctx, r.client, []string{redisKey}, windowSeconds).Int64()
	if err != nil {
		return false, err
	}
	return current <= r.limit, nil
}

// FailoverRateLimiter wraps a primary (distributed) limiter with a local
// fallback. If the primary returns an error (e.g. Redis down), decisions are
// delegated to the fallback so availability is preserved at the cost of
// process-local (potentially per-replica) enforcement.
type FailoverRateLimiter struct {
	primary  RateLimiter
	fallback RateLimiter
}

// NewFailoverRateLimiter builds a limiter that prefers `primary` and falls
// back to `fallback` on primary errors.
func NewFailoverRateLimiter(primary, fallback RateLimiter) *FailoverRateLimiter {
	return &FailoverRateLimiter{primary: primary, fallback: fallback}
}

// Allow implements RateLimiter. It never returns an error: primary errors are
// absorbed and the fallback decision is returned.
func (f *FailoverRateLimiter) Allow(ctx context.Context, key string) (bool, error) {
	if f.primary == nil {
		return f.fallback.Allow(ctx, key)
	}
	allowed, err := f.primary.Allow(ctx, key)
	if err != nil {
		if f.fallback != nil {
			return f.fallback.Allow(ctx, key)
		}
		return true, nil // no fallback configured: fail open
	}
	return allowed, nil
}
