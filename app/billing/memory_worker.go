package billing

import (
	"fmt"
	"log"
	"sync"
)

// MemoryBillingService 是基于内存 Channel 的简单实现
// 适用于开发测试或单机部署，重启后数据丢失
type MemoryBillingService struct {
	queue  chan UsageRecord
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewMemoryBillingService 创建一个内存版计费服务
// bufferSize: 队列缓冲大小
func NewMemoryBillingService(bufferSize int) *MemoryBillingService {
	return &MemoryBillingService{
		queue:  make(chan UsageRecord, bufferSize),
		stopCh: make(chan struct{}),
	}
}

// ReportUsage 实现接口：将记录投递到 Channel
// 如果队列已满，为了不阻塞主流程，可以选择丢弃或记录错误
func (s *MemoryBillingService) ReportUsage(record UsageRecord) error {
	select {
	case s.queue <- record:
		return nil
	default:
		// 队列满，记录错误但不阻塞
		log.Printf("⚠️ [BILLING] Queue full, dropping record for request %s", record.RequestID)
		return fmt.Errorf("billing queue full")
	}
}

// Start 启动后台 Worker
func (s *MemoryBillingService) Start() {
	s.wg.Add(1)
	go s.processQueue()
	log.Println("💰 [BILLING] Memory billing worker started")
}

// Stop 优雅停止
func (s *MemoryBillingService) Stop() {
	close(s.stopCh)
	s.wg.Wait()
	close(s.queue)
	log.Println("💰 [BILLING] Memory billing worker stopped")
}

// processQueue 内部处理循环
func (s *MemoryBillingService) processQueue() {
	defer s.wg.Done()

	for {
		select {
		case record := <-s.queue:
			s.handleRecord(record)
		case <-s.stopCh:
			// 处理完剩余队列再退出
			for {
				select {
				case record := <-s.queue:
					s.handleRecord(record)
				default:
					return
				}
			}
		}
	}
}

// handleRecord 实际的业务处理逻辑（模拟扣费）
func (s *MemoryBillingService) handleRecord(record UsageRecord) {
	// 模拟计算费用：假设 $0.000002 / Token
	cost := float64(record.TotalTokens) * 0.000002

	// 在生产环境中，这里会写入数据库或调用支付服务
	log.Printf("💰 [BILLING] Processed: Request=%s, Model=%s, Tokens=%d, Cost=$%.6f",
		record.RequestID, record.Model, record.TotalTokens, cost)
}
