package middleware

import (
	"context"
	"log"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// RateLimiter is the per-request admission interface shared by the local
// token-bucket limiter and the Redis distributed limiter.
type RateLimiter interface {
	// Allow reports whether a single request keyed by `key` may proceed.
	// A non-nil error means the limiter could not make a decision (the caller
	// should apply its own degradation policy, e.g. fail-open or fallback).
	Allow(ctx context.Context, key string) (bool, error)
}

// LocalRateLimiter is a process-local token bucket limiter (golang.org/x/time/rate).
// It is the pre-distributed behavior and remains the fallback when Redis is
// unavailable. Each auth identity gets its own bucket.
type LocalRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	limit   rate.Limit
	burst   int
}

// NewLocalRateLimiter creates a process-local limiter admitting `rps` sustained
// requests/second with a burst capacity of `burst`.
func NewLocalRateLimiter(rps float64, burst int) *LocalRateLimiter {
	return &LocalRateLimiter{
		buckets: make(map[string]*rate.Limiter),
		limit:   rate.Limit(rps),
		burst:   burst,
	}
}

// Allow implements RateLimiter.
func (l *LocalRateLimiter) Allow(_ context.Context, key string) (bool, error) {
	return l.bucket(key).Allow(), nil
}

func (l *LocalRateLimiter) bucket(key string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		return b
	}
	b := rate.NewLimiter(l.limit, l.burst)
	l.buckets[key] = b
	return b
}

// RateLimitMiddleware enforces a per-identity rate limit using the provided
// limiter. The identity is userID (JWT) or projectID (API Key), falling back
// to "anonymous" when neither is present.
func RateLimitMiddleware(limiter RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		authID := c.GetString("userID")
		if authID == "" {
			authID = c.GetString("projectID")
		}
		if authID == "" {
			authID = "anonymous"
		}

		allowed, err := limiter.Allow(c.Request.Context(), authID)
		if err != nil {
			// Degradation policy: if the limiter cannot make a decision, fail
			// open to preserve availability (auth/quota still gate upstream).
			log.Printf("WARN [RATELIMIT] limiter unavailable for key=%q: %v; allowing", authID, err)
			c.Next()
			return
		}
		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "Too many requests"})
			return
		}

		c.Next()
	}
}

