package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua script for atomic quota deduction with idempotency and a durable usage ledger.
// It ensures:
// 1. A request is only billed once (idempotency via usage:req:{request_id})
// 2. Quota deductions are atomic (no race conditions)
// 3. Quotas can go negative (debt system) - blocking happens at pre-check level
// 4. The full usage record is persisted as a ledger hash for audit/settlement/refund
const luaDeductQuota = `
local request_id = ARGV[1]
local org_id = ARGV[2]
local project_id = ARGV[3]
local token_count = tonumber(ARGV[4])
local trace_id = ARGV[5]
local model = ARGV[6]
local prompt_tokens = tonumber(ARGV[7])
local completion_tokens = tonumber(ARGV[8])
local usage_source = ARGV[9]
local request_status = ARGV[10]
local timestamp = ARGV[11]

-- Check if this request was already processed (idempotency)
local idempotency_key = "usage:req:" .. request_id
if redis.call("EXISTS", idempotency_key) == 1 then
	return {0, "already_processed"}
end

-- Deduct tokens from both quotas atomically (allow negative - debt system)
local org_quota_key = "quota:org:" .. org_id
local project_quota_key = "quota:project:" .. project_id

redis.call("DECRBY", org_quota_key, token_count)
redis.call("DECRBY", project_quota_key, token_count)

-- Mark request as processed with 24-hour TTL
redis.call("SET", idempotency_key, "processed", "EX", 86400)

-- Persist the durable usage ledger entry
local ledger_key = "usage:ledger:" .. request_id
redis.call("HSET", ledger_key,
	"request_id", request_id,
	"trace_id", trace_id,
	"model", model,
	"org_id", org_id,
	"project_id", project_id,
	"prompt_tokens", prompt_tokens,
	"completion_tokens", completion_tokens,
	"total_tokens", token_count,
	"usage_source", usage_source,
	"request_status", request_status,
	"state", "billed",
	"timestamp", timestamp
)
-- Ledger retained 90 days for audit/settlement/refund (closeout: 24h -> 90d).
-- The idempotency key (usage:req:*) intentionally stays at 24h.
redis.call("EXPIRE", ledger_key, 7776000)

return {1, "success"}
`

const luaRefundUsage = `
local ledger_key = KEYS[1]
local state = redis.call("HGET", ledger_key, "state")
if not state then
	return {0, "ledger_not_found"}
end
if state == "refunded" then
	return {0, "already_refunded"}
end
if state ~= "billed" then
	return {0, "invalid_state"}
end

local tokens = tonumber(redis.call("HGET", ledger_key, "total_tokens")) or 0
local org_id = redis.call("HGET", ledger_key, "org_id")
local project_id = redis.call("HGET", ledger_key, "project_id")
if not org_id or not project_id then
	return {0, "invalid_ledger"}
end

if tokens > 0 then
	redis.call("INCRBY", "quota:org:" .. org_id, tokens)
	redis.call("INCRBY", "quota:project:" .. project_id, tokens)
end
redis.call("HSET", ledger_key, "state", "refunded")

return {1, "success", tokens}
`

// luaSettleUsage transitions a pending ledger entry to a terminal state
// (Phase 6, T-F3). Only "pending" entries may settle:
//   - "billed": deducts quota exactly once (idempotent via usage:req key) and
//     marks the ledger billed.
//   - "cancelled": marks the ledger cancelled without any deduction.
// The org/project/tokens are read from the ledger itself so callers only need
// the request ID and the desired action.
const luaSettleUsage = `
local ledger_key = KEYS[1]
local action = ARGV[1]
local request_id = ARGV[2]

local state = redis.call("HGET", ledger_key, "state")
if not state then
	return {0, "ledger_not_found"}
end
if state ~= "pending" then
	return {0, "invalid_state"}
end

local org_id = redis.call("HGET", ledger_key, "org_id")
local project_id = redis.call("HGET", ledger_key, "project_id")
local total_tokens = tonumber(redis.call("HGET", ledger_key, "total_tokens")) or 0

if action == "billed" then
	if total_tokens > 0 then
		redis.call("DECRBY", "quota:org:" .. org_id, total_tokens)
		redis.call("DECRBY", "quota:project:" .. project_id, total_tokens)
	end
	redis.call("SET", "usage:req:" .. request_id, "processed", "EX", 86400)
	redis.call("HSET", ledger_key, "state", "billed")
elseif action == "cancelled" then
	redis.call("HSET", ledger_key, "state", "cancelled")
else
	return {0, "invalid_action"}
end
return {1, "success"}
`

var (
	ErrAPIKeyNotFound        = errors.New("API key not found")
	ErrAPIKeyInactive        = errors.New("API key is not active")
	ErrInsufficientOrgQuota  = errors.New("organization quota exhausted")
	ErrInsufficientProjQuota = errors.New("project quota exhausted")
	ErrAlreadyProcessed      = errors.New("request already processed")
	ErrRedisUnavailable      = errors.New("redis connection unavailable")
)

const (
	// DebtThreshold is the maximum allowed negative quota (debt limit)
	// If quota falls below this threshold, requests will be blocked
	// Default: -10000 tokens (configurable per deployment)
	DebtThreshold = -10000
)

// RedisBillingService implements BillingService and QuotaService using Redis
type RedisBillingService struct {
	client   *redis.Client
	ctx      context.Context
	cancel   context.CancelFunc
	failOpen bool // If true, allow requests when Redis is down (degradation)

	// Minimum-viable compensation queue: usage records that failed to settle due
	// to transient Redis errors are retried in the background. The usage:req
	// idempotency key makes retries safe (no double deduction).
	retryMu     sync.Mutex
	retryQueue  []*retryEntry
	retryStopCh chan struct{}
	retryWg     sync.WaitGroup

	// Durable outbox (Phase 6, T-F2): a Redis LIST that survives pod restarts.
	// Failed settlements are LPUSHed here and drained by outboxLoop; the
	// in-memory retryQueue above remains the last resort when Redis itself is
	// unreachable.
	outboxStopCh chan struct{}
	outboxWg     sync.WaitGroup
}

// retryEntry is a queued usage record awaiting settlement retry.
type retryEntry struct {
	record   UsageRecord
	attempts int
	nextAt   time.Time
}

const (
	// ledgerTTLSeconds is the retention for the durable usage ledger (90 days).
	ledgerTTLSeconds = 7776000
	// retryInterval / retryMaxAttempts bound the compensation retry window (~1h).
	retryInterval    = 30 * time.Second
	retryMaxAttempts = 120
)

const (
	// Durable outbox keys (Phase 6, T-F2).
	outboxKey        = "usage:outbox"
	outboxProcessing = "usage:outbox:processing"
	outboxDead       = "usage:outbox:dead"

	// outboxPopTimeout bounds a blocking BRPOPLPUSH wait between polls.
	outboxPopTimeout = 5 * time.Second
	// outboxProcessingTTLSeconds bounds how long an item can be stranded in the
	// processing list (e.g. worker crashed mid-settle) before Redis expires it.
	outboxProcessingTTLSeconds = 10 * 60
)

// outboxEntry is the durable outbox payload: the usage record plus its retry
// attempt count (attempts survive pod restarts because the entry is JSON in Redis).
type outboxEntry struct {
	Record   UsageRecord `json:"record"`
	Attempts int         `json:"attempts"`
}

// NewRedisBillingService creates a Redis-based billing service
// redisAddr: Redis connection address (e.g., "localhost:6379")
// failOpen: If true, allow traffic when Redis is unavailable (fail-open mode)
func NewRedisBillingService(redisAddr, redisPassword string, failOpen bool) *RedisBillingService {
	ctx, cancel := context.WithCancel(context.Background())

	client := redis.NewClient(&redis.Options{
		Addr:         redisAddr,
		Password:     redisPassword,
		DB:           0, // Use default DB
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	return &RedisBillingService{
		client:      client,
		ctx:         ctx,
		cancel:      cancel,
		failOpen:    failOpen,
		retryStopCh: make(chan struct{}),
	}
}

// RedisClient exposes the shared Redis client so other subsystems (e.g. the
// distributed rate limiter) can reuse the same connection pool without opening
// a second one.
func (s *RedisBillingService) RedisClient() *redis.Client {
	return s.client
}

func apiKeyFingerprint(apiKey string) string {
	digest := sha256.Sum256([]byte(apiKey))
	return fmt.Sprintf("%x", digest[:])
}

func apiKeyRedisKey(apiKey string) string {
	return "apikey:v1:" + apiKeyFingerprint(apiKey)
}

// Start initializes the Redis connection
func (s *RedisBillingService) Start() {
	// Test Redis connection
	if err := s.client.Ping(s.ctx).Err(); err != nil {
		if s.failOpen {
			log.Printf("⚠️ [BILLING] Redis unavailable, running in fail-open mode: %v", err)
		} else {
			log.Fatalf("🔥 [BILLING] Redis connection failed: %v", err)
		}
	} else {
		log.Println("💰 [BILLING] Redis billing service started")
	}

	// Background retry worker (minimum-viable compensation for transient Redis outages).
	if s.retryStopCh == nil {
		s.retryStopCh = make(chan struct{})
	}
	s.retryWg.Add(1)
	go s.retryLoop()

	// Durable outbox worker (Phase 6, T-F2). Requeue items stranded in the
	// processing list by a previously crashed instance, then drain the outbox.
	if s.outboxStopCh == nil {
		s.outboxStopCh = make(chan struct{})
	}
	s.recoverStaleOutboxItems()
	s.outboxWg.Add(1)
	go s.outboxLoop()
}

// Stop gracefully closes the Redis connection
func (s *RedisBillingService) Stop() {
	if s.retryStopCh != nil {
		close(s.retryStopCh)
		s.retryWg.Wait()
	}
	if s.outboxStopCh != nil {
		close(s.outboxStopCh)
		s.outboxWg.Wait()
	}
	s.cancel()
	if err := s.client.Close(); err != nil {
		log.Printf("⚠️ [BILLING] Error closing Redis connection: %v", err)
	}
	log.Println("💰 [BILLING] Redis billing service stopped")
}

// AuthenticateAPIKey validates an API key and returns metadata
func (s *RedisBillingService) AuthenticateAPIKey(apiKey string) (*APIKeyInfo, error) {
	key := apiKeyRedisKey(apiKey)

	result, err := s.client.HGetAll(s.ctx, key).Result()
	if err != nil {
		// Authentication must always fail closed: a missing Redis response cannot prove identity.
		return nil, ErrRedisUnavailable
	}

	if len(result) == 0 {
		return nil, ErrAPIKeyNotFound
	}

	info := &APIKeyInfo{
		Fingerprint: apiKeyFingerprint(apiKey),
		OrgID:       result["org_id"],
		ProjectID:   result["project_id"],
		Status:      result["status"],
		Name:        result["name"],
	}

	// Parse created_at timestamp (stored as Unix timestamp string)
	if createdAtStr, ok := result["created_at"]; ok && createdAtStr != "" {
		if createdAtUnix, err := strconv.ParseInt(createdAtStr, 10, 64); err == nil {
			info.CreatedAt = time.Unix(createdAtUnix, 0)
		}
	}

	if info.Status != "active" {
		return nil, ErrAPIKeyInactive
	}

	return info, nil
}

// CheckQuota verifies if org and project quota are above the debt threshold
// This prevents severely negative accounts from making new requests
// estimatedTokens: rough estimate before processing (typically set to 1 for streaming)
func (s *RedisBillingService) CheckQuota(orgID, projectID string, estimatedTokens int) error {
	// Check organization quota against debt threshold
	orgKey := fmt.Sprintf("quota:org:%s", orgID)
	orgQuota, err := s.client.Get(s.ctx, orgKey).Int()
	if err != nil && err != redis.Nil {
		if s.failOpen {
			log.Printf("⚠️ [BILLING] Redis error checking org quota, fail-open mode: %v", err)
			return nil
		}
		return ErrRedisUnavailable
	}

	// Block if organization is severely in debt
	if orgQuota < DebtThreshold {
		log.Printf("🚫 [BILLING] Org %s blocked: quota=%d (below threshold %d)", orgID, orgQuota, DebtThreshold)
		return ErrInsufficientOrgQuota
	}

	// Check project quota against debt threshold
	projectKey := fmt.Sprintf("quota:project:%s", projectID)
	projectQuota, err := s.client.Get(s.ctx, projectKey).Int()
	if err != nil && err != redis.Nil {
		if s.failOpen {
			log.Printf("⚠️ [BILLING] Redis error checking project quota, fail-open mode: %v", err)
			return nil
		}
		return ErrRedisUnavailable
	}

	// Block if project is severely in debt
	if projectQuota < DebtThreshold {
		log.Printf("🚫 [BILLING] Project %s blocked: quota=%d (below threshold %d)", projectID, projectQuota, DebtThreshold)
		return ErrInsufficientProjQuota
	}

	// Allow request if quotas are above debt threshold
	// Note: Quotas can be negative (debt) but still above the threshold
	// Example: quota = -500 is allowed (above -10000 threshold)
	return nil
}

// ReportUsage atomically deducts tokens using Lua script with idempotency and ledger persistence.
// Deferred records (e.g. upstream_error) are persisted as pending ledger entries without deduction.
func (s *RedisBillingService) ReportUsage(record UsageRecord) error {
	// Skip deduction if token count is 0 or negative
	if record.TotalTokens <= 0 {
		log.Printf("⚠️ [BILLING] Skipping deduction for request %s: token count = %d",
			record.RequestID, record.TotalTokens)
		return nil
	}

	// Deferred settlement: persist a pending ledger entry, no quota deduction.
	if record.Deferred {
		if err := s.recordDeferred(record); err != nil {
			s.enqueueForRetry(record, err)
			if s.failOpen {
				log.Printf("⚠️ [BILLING] Deferred ledger write failed, queued for retry (fail-open): %v", err)
				return nil
			}
			return err
		}
		return nil
	}

	err := s.executeOnce(record)
	if err == nil || errors.Is(err, ErrAlreadyProcessed) {
		return err
	}

	// Transient Redis failure: queue for background retry so usage is not silently
	// lost. The usage:req idempotency key makes retries safe (no double deduction).
	s.enqueueForRetry(record, err)
	if s.failOpen {
		log.Printf("⚠️ [BILLING] Redis error during deduction, queued for retry (fail-open): %v", err)
		return nil
	}
	return err
}

// executeOnce runs the atomic deduction Lua script exactly once and parses its result.
func (s *RedisBillingService) executeOnce(record UsageRecord) error {
	// Execute Lua script atomically
	result, err := s.client.Eval(s.ctx, luaDeductQuota, []string{},
		record.RequestID,
		record.OrgID,
		record.ProjectID,
		record.TotalTokens,
		record.TraceID,
		record.Model,
		record.PromptTokens,
		record.CompletionTokens,
		record.UsageSource,
		record.RequestStatus,
		record.Timestamp.Format(time.RFC3339),
	).Result()

	if err != nil {
		return fmt.Errorf("lua script failed: %w", err)
	}

	// Parse Lua script result
	resultSlice, ok := result.([]interface{})
	if !ok || len(resultSlice) < 2 {
		return fmt.Errorf("unexpected lua script result format: %v", result)
	}

	code := resultSlice[0].(int64)
	message := resultSlice[1].(string)

	switch code {
	case 1:
		// Success - tokens deducted (quota may now be negative, which is allowed)
		cost := float64(record.TotalTokens) * 0.000002 // $0.000002 per token
		log.Printf("💰 [BILLING] Deducted: Request=%s, Org=%s, Project=%s, Model=%s, Tokens=%d, Source=%s, Status=%s, Cost=$%.6f",
			record.RequestID, record.OrgID, record.ProjectID, record.Model, record.TotalTokens, record.UsageSource, record.RequestStatus, cost)
		return nil
	case 0:
		// Already processed (idempotency)
		log.Printf("ℹ️ [BILLING] Request %s already processed (idempotent)", record.RequestID)
		return ErrAlreadyProcessed
	default:
		return fmt.Errorf("unknown lua script result: code=%d, message=%s", code, message)
	}
}

// recordDeferred persists a pending ledger entry for records that must not be
// settled yet (e.g. upstream_error). No quota is deducted and the usage:req
// idempotency key is intentionally NOT set, so a later settlement can still
// deduct through the normal path once the case has been reviewed.
func (s *RedisBillingService) recordDeferred(record UsageRecord) error {
	key := fmt.Sprintf("usage:ledger:%s", record.RequestID)
	fields := map[string]interface{}{
		"request_id":        record.RequestID,
		"trace_id":          record.TraceID,
		"model":             record.Model,
		"org_id":            record.OrgID,
		"project_id":        record.ProjectID,
		"prompt_tokens":     record.PromptTokens,
		"completion_tokens": record.CompletionTokens,
		"total_tokens":      record.TotalTokens,
		"usage_source":      record.UsageSource,
		"request_status":    record.RequestStatus,
		"state":             "pending",
		"timestamp":         record.Timestamp.Format(time.RFC3339),
	}
	if err := s.client.HSet(s.ctx, key, fields).Err(); err != nil {
		return fmt.Errorf("deferred ledger write failed: %w", err)
	}
	_ = s.client.Expire(s.ctx, key, time.Duration(ledgerTTLSeconds)*time.Second).Err() // best effort; 90d
	log.Printf("⏸ [BILLING] Deferred (status=%s): Request=%s, Tokens=%d, Source=%s → ledger state=pending (no quota deduction)",
		record.RequestStatus, record.RequestID, record.TotalTokens, record.UsageSource)
	return nil
}

// enqueueRetry appends a record to the in-memory retry queue.
func (s *RedisBillingService) enqueueRetry(record UsageRecord) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	s.retryQueue = append(s.retryQueue, &retryEntry{record: record, nextAt: time.Now().Add(retryInterval)})
}

// enqueueForRetry persists a failed settlement for background processing
// (Phase 6, T-F2). The durable Redis outbox is preferred because it survives
// pod restarts; the in-memory queue is the last resort when Redis itself is
// unreachable.
func (s *RedisBillingService) enqueueForRetry(record UsageRecord, cause error) {
	if err := s.enqueueOutbox(record); err == nil {
		log.Printf("🔄 [BILLING] Request=%s queued to Redis outbox (outbox-redis): %v", record.RequestID, cause)
		return
	}
	s.enqueueRetry(record)
	log.Printf("🔄 [BILLING] Request=%s queued to in-memory retry (outbox-memory; Redis outbox unreachable): %v", record.RequestID, cause)
}

// enqueueOutbox LPUSHes a JSON-encoded entry onto the durable outbox list.
func (s *RedisBillingService) enqueueOutbox(record UsageRecord) error {
	entry := outboxEntry{Record: record}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return s.client.LPush(s.ctx, outboxKey, data).Err()
}

// recoverStaleOutboxItems moves items stranded in the processing list (left by
// a worker that crashed mid-settle) back to the outbox for re-processing.
func (s *RedisBillingService) recoverStaleOutboxItems() {
	for {
		if _, err := s.client.RPopLPush(s.ctx, outboxProcessing, outboxKey).Result(); err != nil {
			return // redis.Nil (empty) or a real error; either way stop
		}
	}
}

// outboxLoop drains the durable outbox: it atomically moves the next entry to
// the processing list, settles it, then removes it. Failed entries are
// re-queued with an incremented attempt count; exhausted entries go to the
// dead list. (Phase 6, T-F2)
func (s *RedisBillingService) outboxLoop() {
	defer s.outboxWg.Done()
	for {
		select {
		case <-s.outboxStopCh:
			return
		default:
		}

		item, err := s.client.BRPopLPush(s.ctx, outboxKey, outboxProcessing, outboxPopTimeout).Result()
		if err == redis.Nil {
			continue // blocking pop timed out: no items
		}
		if err != nil {
			log.Printf("⚠️ [BILLING] outbox pop failed: %v", err)
			time.Sleep(time.Second)
			continue
		}
		// Bound the processing list lifetime so a crashed worker cannot strand
		// items forever (best-effort; crash recovery also runs on Start).
		_ = s.client.Expire(s.ctx, outboxProcessing, outboxProcessingTTLSeconds*time.Second).Err()
		s.settleOutboxItem(item)
	}
}

// settleOutboxItem settles a single outbox entry and advances it through the
// outbox -> processing -> (outbox|dead) lifecycle.
func (s *RedisBillingService) settleOutboxItem(item string) {
	var entry outboxEntry
	if err := json.Unmarshal([]byte(item), &entry); err != nil {
		log.Printf("🔥 [BILLING] outbox item unparseable, moving to dead list: %v", err)
		s.client.RPush(s.ctx, outboxDead, item)
		s.client.LRem(s.ctx, outboxProcessing, 1, item)
		return
	}

	record := entry.Record
	var err error
	if record.Deferred {
		err = s.recordDeferred(record)
	} else {
		err = s.executeOnce(record)
	}

	switch {
	case err == nil || errors.Is(err, ErrAlreadyProcessed):
		// Settled (or already settled — idempotency key hit): remove from processing.
		s.client.LRem(s.ctx, outboxProcessing, 1, item)
		log.Printf("✅ [BILLING] outbox settled: Request=%s", record.RequestID)
	default:
		entry.Attempts++
		if entry.Attempts >= retryMaxAttempts {
			s.client.LRem(s.ctx, outboxProcessing, 1, item)
			dead, _ := json.Marshal(entry)
			s.client.RPush(s.ctx, outboxDead, dead)
			log.Printf("🔥 [BILLING] outbox exhausted for Request=%s after %d attempts: %v", record.RequestID, entry.Attempts, err)
			return
		}
		// Re-queue at the tail so other entries get a chance, and drop the processing copy.
		data, _ := json.Marshal(entry)
		s.client.RPush(s.ctx, outboxKey, data)
		s.client.LRem(s.ctx, outboxProcessing, 1, item)
		log.Printf("⚠️ [BILLING] outbox retry queued: Request=%s attempt=%d err=%v", record.RequestID, entry.Attempts, err)
	}
}

// retryLoop periodically re-attempts queued settlements.
func (s *RedisBillingService) retryLoop() {
	defer s.retryWg.Done()
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.retryStopCh:
			return
		case now := <-ticker.C:
			s.retryDue(now)
		}
	}
}

// retryDue attempts to settle every queued record whose backoff has elapsed.
func (s *RedisBillingService) retryDue(now time.Time) {
	s.retryMu.Lock()
	pending := s.retryQueue
	s.retryQueue = nil
	s.retryMu.Unlock()

	var keep []*retryEntry
	for _, item := range pending {
		if item.nextAt.After(now) {
			keep = append(keep, item)
			continue
		}
		var err error
		if item.record.Deferred {
			err = s.recordDeferred(item.record)
		} else {
			err = s.executeOnce(item.record)
		}
		switch {
		case err == nil || errors.Is(err, ErrAlreadyProcessed):
			log.Printf("✅ [BILLING] Retry settled: Request=%s (attempt %d)", item.record.RequestID, item.attempts+1)
		default:
			item.attempts++
			if item.attempts >= retryMaxAttempts {
				log.Printf("🔥 [BILLING] Retry exhausted for Request=%s after %d attempts: %v", item.record.RequestID, item.attempts, err)
				continue
			}
			item.nextAt = now.Add(retryInterval)
			keep = append(keep, item)
		}
	}
	if len(keep) > 0 {
		s.retryMu.Lock()
		s.retryQueue = append(s.retryQueue, keep...)
		s.retryMu.Unlock()
	}
}

// GetUsageLedger retrieves the persisted ledger entry for a request, if present.
func (s *RedisBillingService) GetUsageLedger(requestID string) (map[string]string, error) {
	key := fmt.Sprintf("usage:ledger:%s", requestID)
	return s.client.HGetAll(s.ctx, key).Result()
}

// RefundUsage reverses a billed request: it credits back the token cost and marks the
// ledger entry as refunded. This is the settlement/compensation path for partial output,
// upstream failure, or duplicate billing.
func (s *RedisBillingService) RefundUsage(requestID string) error {
	key := fmt.Sprintf("usage:ledger:%s", requestID)
	result, err := s.client.Eval(s.ctx, luaRefundUsage, []string{key}).Result()
	if err != nil {
		return fmt.Errorf("refund lua script failed: %w", err)
	}

	resultSlice, ok := result.([]interface{})
	if !ok || len(resultSlice) < 2 {
		return fmt.Errorf("unexpected refund lua script result format: %v", result)
	}
	code, ok := resultSlice[0].(int64)
	if !ok {
		return fmt.Errorf("unexpected refund lua script result code: %v", resultSlice[0])
	}
	message, ok := resultSlice[1].(string)
	if !ok {
		return fmt.Errorf("unexpected refund lua script result message: %v", resultSlice[1])
	}
	if code == 0 {
		switch message {
		case "already_refunded":
			return ErrAlreadyProcessed
		case "ledger_not_found":
			return fmt.Errorf("ledger entry not found for request %s", requestID)
		default:
			return fmt.Errorf("refund rejected: %s", message)
		}
	}
	if code != 1 || len(resultSlice) < 3 {
		return fmt.Errorf("unexpected refund lua script result: %v", result)
	}
	tokens, ok := resultSlice[2].(int64)
	if !ok {
		return fmt.Errorf("unexpected refund token count: %v", resultSlice[2])
	}
	log.Printf("🔁 [BILLING] Refunded request=%s tokens=%d", requestID, tokens)
	return nil
}

// SettleUsage transitions a pending ledger entry to a terminal state
// (Phase 6, T-F3). Only "pending" entries may settle: "billed" deducts quota
// exactly once (idempotent via the usage:req key); "cancelled" marks the entry
// cancelled without deduction. Billed/refunded/cancelled entries are rejected.
func (s *RedisBillingService) SettleUsage(requestID, action string) error {
	if action != "billed" && action != "cancelled" {
		return fmt.Errorf("invalid settle action %q (want billed|cancelled)", action)
	}

	key := fmt.Sprintf("usage:ledger:%s", requestID)
	result, err := s.client.Eval(s.ctx, luaSettleUsage, []string{key}, action, requestID).Result()
	if err != nil {
		return fmt.Errorf("settle lua script failed: %w", err)
	}

	resultSlice, ok := result.([]interface{})
	if !ok || len(resultSlice) < 2 {
		return fmt.Errorf("unexpected settle lua script result: %v", result)
	}
	code, _ := resultSlice[0].(int64)
	message, _ := resultSlice[1].(string)
	if code != 1 {
		switch message {
		case "ledger_not_found":
			return fmt.Errorf("ledger entry not found for request %s", requestID)
		case "invalid_state":
			return fmt.Errorf("request %s is not in pending state", requestID)
		case "invalid_action":
			return fmt.Errorf("invalid settle action %q", action)
		default:
			return fmt.Errorf("settle rejected: %s", message)
		}
	}

	log.Printf("📝 [BILLING] Settled request=%s action=%s", requestID, action)
	return nil
}

// CreateAPIKey creates a new API key in Redis with metadata
func (s *RedisBillingService) CreateAPIKey(apiKey, orgID, projectID, name string) error {
	key := apiKeyRedisKey(apiKey)

	err := s.client.HSet(s.ctx, key, map[string]interface{}{
		"org_id":     orgID,
		"project_id": projectID,
		"status":     "active",
		"created_at": fmt.Sprintf("%d", time.Now().Unix()),
		"name":       name,
	}).Err()

	if err != nil {
		return fmt.Errorf("failed to create API key: %w", err)
	}

	log.Printf("[BILLING] Created API key fingerprint=%s (org=%s, project=%s)", apiKeyFingerprint(apiKey)[:12], orgID, projectID)
	return nil
}

// SetOrgQuota sets the quota for an organization
func (s *RedisBillingService) SetOrgQuota(orgID string, tokens int) error {
	key := fmt.Sprintf("quota:org:%s", orgID)

	err := s.client.Set(s.ctx, key, tokens, 0).Err()
	if err != nil {
		return fmt.Errorf("failed to set org quota: %w", err)
	}

	log.Printf("💳 [BILLING] Set org quota: org=%s, tokens=%d", orgID, tokens)
	return nil
}

// SetProjectQuota sets the quota for a project
func (s *RedisBillingService) SetProjectQuota(projectID string, tokens int) error {
	key := fmt.Sprintf("quota:project:%s", projectID)

	err := s.client.Set(s.ctx, key, tokens, 0).Err()
	if err != nil {
		return fmt.Errorf("failed to set project quota: %w", err)
	}

	log.Printf("💳 [BILLING] Set project quota: project=%s, tokens=%d", projectID, tokens)
	return nil
}

// GetOrgQuota retrieves current org quota balance
func (s *RedisBillingService) GetOrgQuota(orgID string) (int, error) {
	key := fmt.Sprintf("quota:org:%s", orgID)

	quota, err := s.client.Get(s.ctx, key).Int()
	if err == redis.Nil {
		return 0, nil // No quota set means 0
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get org quota: %w", err)
	}

	return quota, nil
}

// GetProjectQuota retrieves current project quota balance
func (s *RedisBillingService) GetProjectQuota(projectID string) (int, error) {
	key := fmt.Sprintf("quota:project:%s", projectID)

	quota, err := s.client.Get(s.ctx, key).Int()
	if err == redis.Nil {
		return 0, nil // No quota set means 0
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get project quota: %w", err)
	}

	return quota, nil
}
