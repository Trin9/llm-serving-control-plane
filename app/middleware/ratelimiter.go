package middleware

import (
	"net/http"
	"sync"

	"gate-service/app/monitor"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

func RateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Use userID (JWT) or projectID (API Key) as the limiter key
		authID := c.GetString("userID")
		if authID == "" {
			authID = c.GetString("projectID")
		}

		if authID == "" {
			authID = "anonymous"
		}

		limiter := getLimiter(authID)
		if !limiter.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "Too many requests"})
			return
		}

		// Simulate vLLM requests waiting/in-flight metric for KEDA scaling
		monitor.VllmRequestsWaiting.Inc()
		defer monitor.VllmRequestsWaiting.Dec()

		c.Next()
	}
}

var limiters sync.Map

func getLimiter(userID string) *rate.Limiter {
	if l, ok := limiters.Load(userID); ok {
		return l.(*rate.Limiter)
	}

	// 每秒允许 1000 个请求，桶容量 2000 (针对压测放宽限制)
	newLimiter := rate.NewLimiter(rate.Limit(1000), 2000)
	actual, _ := limiters.LoadOrStore(userID, newLimiter)
	return actual.(*rate.Limiter)
}
