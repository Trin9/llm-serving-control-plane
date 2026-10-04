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

	// Router load-aware metrics (Phase 6, W7 prefix-load). Low-cardinality only:
	// no prompt/token/API-key content ever becomes a label.
	RouterActiveRequests = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gateway_router_active_requests",
			Help: "In-flight requests currently routed to each backend, by model.",
		},
		[]string{"model", "backend"},
	)

	RouterSpillTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_router_spill_total",
			Help: "Number of routing decisions that spilled off the affinity owner due to load.",
		},
		[]string{"model"},
	)

	RouterSelectionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_router_selection_total",
			Help: "Number of backend selections by model and strategy.",
		},
		[]string{"model", "strategy"},
	)
)

func init() {
	// Register metrics
	prometheus.MustRegister(RequestCount)
	prometheus.MustRegister(RequestDuration)
	prometheus.MustRegister(AITimeToFirstToken)
	prometheus.MustRegister(AITimePerOutputToken)
	prometheus.MustRegister(RouterActiveRequests)
	prometheus.MustRegister(RouterSpillTotal)
	prometheus.MustRegister(RouterSelectionTotal)
}
