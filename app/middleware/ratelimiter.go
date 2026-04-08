package middleware

import (
	"net/http"

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

var limiters = make(map[string]*rate.Limiter)

func getLimiter(userID string) *rate.Limiter {
	// 实际生产中要注意并发安全(sync.Map)和内存清理
	if l, exists := limiters[userID]; exists {
		return l
	}
	// 每秒允许 1000 个请求，桶容量 2000 (针对压测放宽限制)
	l := rate.NewLimiter(1000, 2000)
	limiters[userID] = l
	return l
}
