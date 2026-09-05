package billing

import (
	"fmt"
	"log"
	"sync"
)

// MemoryBillingService is a simple implementation backed by an in-memory channel.
// It is intended for development/testing or single-node deployments; data is lost on restart.
type MemoryBillingService struct {
	queue  chan UsageRecord
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewMemoryBillingService creates an in-memory billing service.
// bufferSize: queue buffer size
func NewMemoryBillingService(bufferSize int) *MemoryBillingService {
	return &MemoryBillingService{
		queue:  make(chan UsageRecord, bufferSize),
		stopCh: make(chan struct{}),
	}
}

// ReportUsage implements the interface by delivering records to the channel.
// If the queue is full, records are dropped or an error is logged so the main flow is not blocked.
func (s *MemoryBillingService) ReportUsage(record UsageRecord) error {
	select {
	case s.queue <- record:
		return nil
	default:
		// Queue is full: log an error but do not block
		log.Printf("⚠️ [BILLING] Queue full, dropping record for request %s", record.RequestID)
		return fmt.Errorf("billing queue full")
	}
}

// Start launches the background worker.
func (s *MemoryBillingService) Start() {
	s.wg.Add(1)
	go s.processQueue()
	log.Println("💰 [BILLING] Memory billing worker started")
}

// Stop gracefully shuts the worker down.
func (s *MemoryBillingService) Stop() {
	close(s.stopCh)
	s.wg.Wait()
	close(s.queue)
	log.Println("💰 [BILLING] Memory billing worker stopped")
}

// processQueue is the internal processing loop.
func (s *MemoryBillingService) processQueue() {
	defer s.wg.Done()

	for {
		select {
		case record := <-s.queue:
			s.handleRecord(record)
		case <-s.stopCh:
			// Drain the remaining queue before exiting
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

// handleRecord performs the actual business logic (simulated billing).
func (s *MemoryBillingService) handleRecord(record UsageRecord) {
	// Simulate cost calculation: assume $0.000002 / token
	cost := float64(record.TotalTokens) * 0.000002

	// In production this would write to a database or call a payment service
	log.Printf("💰 [BILLING] Processed: Request=%s, Model=%s, Tokens=%d, Cost=$%.6f",
		record.RequestID, record.Model, record.TotalTokens, cost)
}
