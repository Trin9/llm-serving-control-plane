package billing

import (
	"time"
)

// UsageRecord 定义了单次请求的计费数据
type UsageRecord struct {
	RequestID   string    `json:"request_id"`
	Model       string    `json:"model"`
	User        string    `json:"user"` // 可扩展为 UserID 或 API Key
	TotalTokens int       `json:"total_tokens"`
	Timestamp   time.Time `json:"timestamp"`
}

// BillingService 定义了计费服务的接口
// 未来如果切换到 Redis，只需要实现这个接口即可
type BillingService interface {
	// ReportUsage 上报一条计费记录（异步或同步取决于实现）
	ReportUsage(record UsageRecord) error
	
	// Start 启动后台处理任务（如 Worker）
	Start()
	
	// Stop 停止服务
	Stop()
}
