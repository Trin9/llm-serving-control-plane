package monitor

import "github.com/prometheus/client_golang/prometheus"

// --- 1. 定义指标 ---
var (
	// 请求总数计数器 (Counter)
	RequestCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "status"}, // 标签：方法、状态码
	)

	// 请求耗时直方图 (Histogram)
	RequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Duration of HTTP requests in seconds",
			Buckets: prometheus.DefBuckets, // 默认的分桶: .005, .01, .25, .5, 1, 2.5, 5, 10
		},
		[]string{"method"},
	)

	// AI 指标 - 首字延迟 (Time To First Token)
	AITimeToFirstToken = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ai_ttft_seconds",
			Help:    "Time to first token in seconds",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10}, // 适配 AI 场景的分桶
		},
		[]string{"model", "route"}, // 标签：模型名、路由
	)

	// AI 指标 - 单token生成速度 (Time Per Output Token)
	AITimePerOutputToken = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ai_tpot_seconds",
			Help:    "Time per output token in seconds",
			Buckets: []float64{0.01, 0.05, 0.1, 0.2, 0.5}, // 更细粒度的分桶
		},
		[]string{"model", "route"}, // 标签：模型名、路由
	)
)

func init() {
	// 注册指标
	prometheus.MustRegister(RequestCount)
	prometheus.MustRegister(RequestDuration)
	prometheus.MustRegister(AITimeToFirstToken)
	prometheus.MustRegister(AITimePerOutputToken)
}
