package main

import (
	"context"
	"gate-service/app/billing"
	"gate-service/app/handler"
	"gate-service/app/middleware"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
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

func intFromEnv(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func floatFromEnv(name string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
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

	// Phase 6 (T-F1): distributed rate limiting. When Redis billing is active,
	// a shared fixed-window limiter is used so N replicas enforce the aggregate
	// RPS instead of N× the per-replica limit; on Redis errors it falls back to
	// the process-local token bucket.
	rateLimitRPS := floatFromEnv("RATE_LIMIT_RPS", 1000)
	rateLimitBurst := intFromEnv("RATE_LIMIT_BURST", 2000)
	localLimiter := middleware.NewLocalRateLimiter(rateLimitRPS, rateLimitBurst)
	var rateLimiter middleware.RateLimiter = localLimiter
	if redisBilling, ok := billingSvc.(*billing.RedisBillingService); ok {
		if client := redisBilling.RedisClient(); client != nil {
			redisLimiter := middleware.NewRedisRateLimiter(client, int64(rateLimitRPS), time.Second)
			rateLimiter = middleware.NewFailoverRateLimiter(redisLimiter, localLimiter)
			log.Printf("rate limiter: Redis distributed (limit=%d rps, local fallback enabled)", int64(rateLimitRPS))
		}
	}

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

	// Phase 6 (T-A2): routing strategy switch for the KV-cache routing A/B.
	// ROUTER_STRATEGY=prefix-hash (default) routes by prompt-prefix consistent
	// hash so same-prefix requests reuse one backend's prefix cache;
	// ROUTER_STRATEGY=random spreads uniformly (A/B baseline). Invalid values
	// fall back to the default with a warning.
	routerStrategy, strategyValid := handler.NormalizeStrategy(os.Getenv("ROUTER_STRATEGY"))
	if !strategyValid {
		log.Printf("WARN: invalid ROUTER_STRATEGY=%q, falling back to %q", os.Getenv("ROUTER_STRATEGY"), routerStrategy)
	}
	routerSvc.SetStrategy(routerStrategy)
	log.Printf("gateway routing strategy: %s", routerStrategy)

	// Phase 6 (W7/P1): spill threshold for the prefix-load strategy. Only
	// meaningful when ROUTER_STRATEGY=prefix-load; <=0 disables spilling.
	spillThreshold := int64(intFromEnv("ROUTER_SPILL_THRESHOLD", 64))
	routerSvc.SetInflightThreshold(spillThreshold)
	log.Printf("gateway spill threshold: %d", spillThreshold)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler.StartBackendRefresh(ctx, backendSource, routerSvc, 30*time.Second)

	r := gin.Default() // ships with Logger and Recovery middleware

	r.GET("/health", handler.HealthCheckHandler)

	// Admin route group (Phase 6, T-F3): billing settlement operations.
	// Requires a JWT whose userID is in the ADMIN_USER_IDS allowlist.
	admin := r.Group("/admin")
	admin.Use(middleware.AuthMiddleware(billingSvc))
	admin.Use(middleware.AdminOnlyMiddleware(os.Getenv("ADMIN_USER_IDS")))
	admin.POST("/billing/settle", handler.SettleUsageHandlerFactory(billingSvc))

	// API route group
	api := r.Group("/v1")
	api.Use(middleware.AuthMiddleware(billingSvc))       // mount auth middleware (supports JWT & API Key)
	api.Use(middleware.RateLimitMiddleware(rateLimiter)) // mount rate limiting (distributed + local fallback)
	api.Use(middleware.PrometheusMiddleware())           // mount monitoring

	// --- 3. Expose the /metrics endpoint ---
	// Prometheus scrapes metrics from this endpoint
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Use the factory method to inject the billing service and routing strategy (W13)
	api.POST("/chat/completions", handler.ProxyHandlerFactory(billingSvc, routerSvc))

	// Phase 6 (T-F4): SSE graceful drain. Run the server behind http.Server so
	// SIGTERM/SIGINT trigger Shutdown, which stops accepting new connections and
	// lets in-flight SSE streams finish (bounded by SHUTDOWN_GRACE_PERIOD).
	// Kubernetes must give the pod terminationGracePeriodSeconds >= this grace
	// period so the container is not SIGKILLed mid-drain.
	srv := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		log.Fatalf("gateway server failed: %v", err)
	case sig := <-quit:
		log.Printf("received %s: draining in-flight SSE requests before shutdown", sig)
	}

	gracePeriod := time.Duration(intFromEnv("SHUTDOWN_GRACE_PERIOD", 25)) * time.Second
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), gracePeriod)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// DeadlineExceeded means some streams outlived the grace period; the
		// server force-closed them. Kubernetes will SIGKILL the container shortly
		// after anyway (terminationGracePeriodSeconds).
		log.Printf("graceful shutdown completed with remaining connections closed: %v", err)
	} else {
		log.Printf("graceful shutdown complete: all in-flight requests drained")
	}
}
