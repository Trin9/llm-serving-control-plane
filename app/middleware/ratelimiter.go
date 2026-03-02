package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

func RateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("userID") // 从 JWT 中获取
		limiter := getLimiter(userID)
		if !limiter.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "Too many requests"})
			return
		}
		c.Next()
	}
}

var limiters = make(map[string]*rate.Limiter)

func getLimiter(userID string) *rate.Limiter {
	// 实际生产中要注意并发安全(sync.Map)和内存清理
	if l, exists := limiters[userID]; exists {
		return l
	}
	// 每秒允许 1 个请求，桶容量 5
	l := rate.NewLimiter(50, 100)
	limiters[userID] = l
	return l
}
