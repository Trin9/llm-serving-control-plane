package main

import (
	"context"
	"gate-service/app/billing"
	"gate-service/app/handler"
	"gate-service/app/middleware"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func boolFromEnv(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func main() {
	// 0. Initialize Billing Service (W12)
	// Supports Redis for production and memory for local development
	var billingSvc billing.BillingService
	redisAddr := os.Getenv("REDIS_ADDR")
	redisPassword := os.Getenv("REDIS_PASSWORD")
	failOpen := boolFromEnv("BILLING_FAIL_OPEN", false)

	if redisAddr != "" {
		// Use Redis-based billing for multi-tenant quota enforcement
		billingSvc = billing.NewRedisBillingService(redisAddr, redisPassword, failOpen)
	} else {
		// Fallback to memory-based billing
		billingSvc = billing.NewMemoryBillingService(1000)
	}

	billingSvc.Start()
	defer billingSvc.Stop() // ensure graceful shutdown on exit

	// 1. Initialize semantic routing (W13)
	// In production, prefer Kubernetes Endpoints for automatic backend discovery;
	// local development still supports the static VLLM_URLS configuration.
	staticSource := handler.NewStaticBackendSourceFromEnv("VLLM_URLS", []string{"http://localhost:8000/v1/chat/completions"})
	var backendSource handler.BackendSource = staticSource
	usingKubernetesDiscovery := false
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" || os.Getenv("USE_KUBERNETES_BACKEND_DISCOVERY") == "true" {
		if k8sSource, err := handler.NewKubernetesBackendSourceFromEnv(); err == nil {
			backendSource = k8sSource
			usingKubernetesDiscovery = true
		}
	}
	backendList := []string{}
	if !usingKubernetesDiscovery {
		var err error
		backendList, err = staticSource.Discover()
		if err != nil || len(backendList) == 0 {
			backendList = []string{"http://localhost:8000/v1/chat/completions"}
		}
	}
	routerSvc := handler.NewModelConsistentHashRouter(backendList)
	if modelSource, ok := backendSource.(handler.ModelBackendSource); ok {
		if modelBackends, err := modelSource.DiscoverByModel(); err == nil {
			routerSvc.UpdateModelBackends(modelBackends)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler.StartBackendRefresh(ctx, backendSource, routerSvc, 30*time.Second)

	r := gin.Default() // ships with Logger and Recovery middleware

	r.GET("/health", handler.HealthCheckHandler)

	// API route group
	api := r.Group("/v1")
	api.Use(middleware.AuthMiddleware(billingSvc)) // mount auth middleware (supports JWT & API Key)
	api.Use(middleware.RateLimitMiddleware())      // mount rate limiting
	api.Use(middleware.PrometheusMiddleware())     // mount monitoring

	// --- 3. Expose the /metrics endpoint ---
	// Prometheus scrapes metrics from this endpoint
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Use the factory method to inject the billing service and routing strategy (W13)
	api.POST("/chat/completions", handler.ProxyHandlerFactory(billingSvc, routerSvc))

	r.Run(":8080")
}
