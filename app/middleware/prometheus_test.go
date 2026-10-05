package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gate-service/app/monitor"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// TestPrometheusMiddleware_InFlightGauge verifies the T-F4 drain observability
// gauge: it must be 1 while a request is in flight and return to 0 after the
// handler exits (the defer must fire on every exit path).
func TestPrometheusMiddleware_InFlightGauge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	entered := make(chan struct{})
	release := make(chan struct{})

	router := gin.New()
	router.Use(PrometheusMiddleware())
	router.POST("/slow", func(c *gin.Context) {
		close(entered)
		<-release
		c.Status(http.StatusNoContent)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/slow", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
	}()

	// Wait until the handler is entered so the request is definitely in flight.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not entered within timeout")
	}

	gauge := monitor.GatewayInFlightRequests.WithLabelValues(http.MethodPost)
	assert.Equal(t, float64(1), testutil.ToFloat64(gauge), "in-flight gauge should be 1 while the request is active")

	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not complete within timeout")
	}

	assert.Equal(t, float64(0), testutil.ToFloat64(gauge), "in-flight gauge should return to 0 after the handler exits")
}
