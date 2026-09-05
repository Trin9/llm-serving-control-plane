package middleware

import (
	"gate-service/app/monitor"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// --- 2. Monitoring middleware ---
func PrometheusMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		c.Next() // execute downstream handlers

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(c.Writer.Status())

		// Record metrics
		monitor.RequestCount.WithLabelValues(c.Request.Method, status).Inc()
		monitor.RequestDuration.WithLabelValues(c.Request.Method).Observe(duration)
	}
}
