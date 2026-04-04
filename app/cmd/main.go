package main

import (
	"gate-service/app/billing"
	"gate-service/app/handler"
	"gate-service/app/middleware"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"strings"
)

func main() {
	// 0. Initialize Billing Service (W12)
	// Supports Redis for production and memory for local development
	var billingSvc billing.BillingService
	redisAddr := os.Getenv("REDIS_ADDR")

	if redisAddr != "" {
		// Use Redis-based billing for multi-tenant quota enforcement
		billingSvc = billing.NewRedisBillingService(redisAddr, true) // failOpen=true
	} else {
		// Fallback to memory-based billing
		billingSvc = billing.NewMemoryBillingService(1000)
	}

	billingSvc.Start()
	defer billingSvc.Stop() // 确保程序退出时优雅关闭

	// 1. 初始化语义路由 (W13)
	// 支持从环境变量读取多个后端，用逗号分隔，例如: http://pod1:8000,http://pod2:8000
	vllmURLs := os.Getenv("VLLM_URLS")
	if vllmURLs == "" {
		// 默认回退到单个后端
		vllmURLs = "http://localhost:8000/v1/chat/completions"
	}
	backendList := strings.Split(vllmURLs, ",")
	routerSvc := handler.NewConsistentHashRouter(backendList)

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
