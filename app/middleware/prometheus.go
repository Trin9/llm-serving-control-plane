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

		// Phase 6 (T-F4): track in-flight requests so the SSE graceful drain can
		// be observed during shutdown (gauge must always return to zero when the
		// handler exits, including panic/abort paths).
		monitor.GatewayInFlightRequests.WithLabelValues(c.Request.Method).Inc()
		defer monitor.GatewayInFlightRequests.WithLabelValues(c.Request.Method).Dec()

		c.Next() // execute downstream handlers

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(c.Writer.Status())

		// Record metrics
		monitor.RequestCount.WithLabelValues(c.Request.Method, status).Inc()
		monitor.RequestDuration.WithLabelValues(c.Request.Method).Observe(duration)
	}
}
