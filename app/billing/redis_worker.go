package billing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua script for atomic quota deduction with idempotency
// This script ensures that:
// 1. A request is only billed once (idempotency via usage:req:{request_id})
// 2. Quota deductions are atomic (no race conditions)
// 3. Quotas can go negative (debt system) - blocking happens at pre-check level
const luaDeductQuota = `
local request_id = ARGV[1]
local org_id = ARGV[2]
local project_id = ARGV[3]
local token_count = tonumber(ARGV[4])

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
}

// NewRedisBillingService creates a Redis-based billing service
// redisAddr: Redis connection address (e.g., "localhost:6379")
// failOpen: If true, allow traffic when Redis is unavailable (fail-open mode)
func NewRedisBillingService(redisAddr string, failOpen bool) *RedisBillingService {
	ctx, cancel := context.WithCancel(context.Background())

	client := redis.NewClient(&redis.Options{
		Addr:         redisAddr,
		Password:     "", // No password by default
		DB:           0,  // Use default DB
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	return &RedisBillingService{
		client:   client,
		ctx:      ctx,
		cancel:   cancel,
		failOpen: failOpen,
	}
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
}

// Stop gracefully closes the Redis connection
func (s *RedisBillingService) Stop() {
	s.cancel()
	if err := s.client.Close(); err != nil {
		log.Printf("⚠️ [BILLING] Error closing Redis connection: %v", err)
	}
	log.Println("💰 [BILLING] Redis billing service stopped")
}

// AuthenticateAPIKey validates an API key and returns metadata
func (s *RedisBillingService) AuthenticateAPIKey(apiKey string) (*APIKeyInfo, error) {
	key := fmt.Sprintf("apikey:%s", apiKey)

	result, err := s.client.HGetAll(s.ctx, key).Result()
	if err != nil {
		if s.failOpen {
			log.Printf("⚠️ [BILLING] Redis error during auth, fail-open mode: %v", err)
			// In fail-open mode, allow the request with placeholder values
			return &APIKeyInfo{
				APIKey:    apiKey,
				OrgID:     "unknown",
				ProjectID: "unknown",
				Status:    "active",
			}, nil
		}
		return nil, fmt.Errorf("redis error: %w", err)
	}

	if len(result) == 0 {
		return nil, ErrAPIKeyNotFound
	}

	info := &APIKeyInfo{
		APIKey:    apiKey,
		OrgID:     result["org_id"],
		ProjectID: result["project_id"],
		Status:    result["status"],
		Name:      result["name"],
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

// ReportUsage atomically deducts tokens using Lua script with idempotency
func (s *RedisBillingService) ReportUsage(record UsageRecord) error {
	// Skip deduction if token count is 0 or negative
	if record.TotalTokens <= 0 {
		log.Printf("⚠️ [BILLING] Skipping deduction for request %s: token count = %d",
			record.RequestID, record.TotalTokens)
		return nil
	}

	// Execute Lua script atomically
	result, err := s.client.Eval(s.ctx, luaDeductQuota, []string{},
		record.RequestID,
		record.OrgID,
		record.ProjectID,
		record.TotalTokens,
	).Result()

	if err != nil {
		if s.failOpen {
			log.Printf("⚠️ [BILLING] Redis error during deduction, fail-open mode: %v", err)
			return nil
		}
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
		log.Printf("💰 [BILLING] Deducted: Request=%s, Org=%s, Project=%s, Model=%s, Tokens=%d, Cost=$%.6f",
			record.RequestID, record.OrgID, record.ProjectID, record.Model, record.TotalTokens, cost)
		return nil
	case 0:
		// Already processed (idempotency)
		log.Printf("ℹ️ [BILLING] Request %s already processed (idempotent)", record.RequestID)
		return ErrAlreadyProcessed
	default:
		return fmt.Errorf("unknown lua script result: code=%d, message=%s", code, message)
	}
}

// CreateAPIKey creates a new API key in Redis with metadata
func (s *RedisBillingService) CreateAPIKey(apiKey, orgID, projectID, name string) error {
	key := fmt.Sprintf("apikey:%s", apiKey)

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

	log.Printf("✅ [BILLING] Created API key: %s (org=%s, project=%s)", apiKey, orgID, projectID)
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
