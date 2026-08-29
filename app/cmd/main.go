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
	failOpen := boolFromEnv("BILLING_FAIL_OPEN", false)

	if redisAddr != "" {
		// Use Redis-based billing for multi-tenant quota enforcement
		billingSvc = billing.NewRedisBillingService(redisAddr, failOpen)
	} else {
		// Fallback to memory-based billing
		billingSvc = billing.NewMemoryBillingService(1000)
	}

	billingSvc.Start()
	defer billingSvc.Stop() // 确保程序退出时优雅关闭

	// 1. 初始化语义路由 (W13)
	// 生产环境下优先使用 Kubernetes Endpoints 自动发现后端；本地开发仍支持 VLLM_URLS 静态配置。
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

	r := gin.Default() // 自带 Logger 和 Recovery 中间件

	r.GET("/health", handler.HealthCheckHandler)

	// API 路由组
	api := r.Group("/v1")
	api.Use(middleware.AuthMiddleware(billingSvc)) // 挂载鉴权 (支持 JWT & API Key)
	api.Use(middleware.RateLimitMiddleware())      // 挂载限流
	api.Use(middleware.PrometheusMiddleware())     // 挂载监控

	// --- 3. 暴露 /metrics 接口 ---
	// Prometheus 会访问这个接口来“刮取”数据
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// 使用工厂方法注入计费服务和路由策略 (W13)
	api.POST("/chat/completions", handler.ProxyHandlerFactory(billingSvc, routerSvc))

	r.Run(":8080")
}
