package monitor

import "github.com/prometheus/client_golang/prometheus"

// --- 1. Define metrics ---
var (
	// Total request counter
	RequestCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "status"}, // labels: method, status code
	)

	// Request duration histogram
	RequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Duration of HTTP requests in seconds",
			Buckets: prometheus.DefBuckets, // default buckets
		},
		[]string{"method"},
	)

	// AI metric - time to first token (TTFT)
	AITimeToFirstToken = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ai_ttft_seconds",
			Help:    "Time to first token in seconds",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10}, // buckets tuned for AI workloads
		},
		[]string{"model", "route"}, // labels: model name, route
	)

	// AI metric - time per output token (TPOT)
	AITimePerOutputToken = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ai_tpot_seconds",
			Help:    "Time per output token in seconds",
			Buckets: []float64{0.01, 0.05, 0.1, 0.2, 0.5}, // finer-grained buckets
		},
		[]string{"model", "route"}, // labels: model name, route
	)
)

func init() {
	// Register metrics
	prometheus.MustRegister(RequestCount)
	prometheus.MustRegister(RequestDuration)
	prometheus.MustRegister(AITimeToFirstToken)
	prometheus.MustRegister(AITimePerOutputToken)
}
