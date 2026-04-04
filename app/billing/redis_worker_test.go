package billing

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

func setupTestRedis(t *testing.T) (*RedisBillingService, *miniredis.Miniredis) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}

	service := &RedisBillingService{
		client: redis.NewClient(&redis.Options{
			Addr: mr.Addr(),
		}),
		ctx:      context.Background(),
		failOpen: false,
	}

	return service, mr
}

func TestRedisBillingService_AuthenticateAPIKey(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	// 1. Test success
	apiKey := "sk-test-123"
	err := svc.CreateAPIKey(apiKey, "org-1", "proj-1", "Test Key")
	assert.NoError(t, err)

	info, err := svc.AuthenticateAPIKey(apiKey)
	assert.NoError(t, err)
	assert.Equal(t, "org-1", info.OrgID)
	assert.Equal(t, "proj-1", info.ProjectID)
	assert.Equal(t, "active", info.Status)

	// 2. Test not found
	_, err = svc.AuthenticateAPIKey("invalid-key")
	assert.Equal(t, ErrAPIKeyNotFound, err)

	// 3. Test inactive
	mr.HSet("apikey:"+apiKey, "status", "suspended")
	_, err = svc.AuthenticateAPIKey(apiKey)
	assert.Equal(t, ErrAPIKeyInactive, err)
}

func TestRedisBillingService_CheckQuota(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	orgID := "org-1"
	projID := "proj-1"

	// 1. Test success (no quota set defaults to 0, which is > DebtThreshold)
	err := svc.CheckQuota(orgID, projID, 0)
	assert.NoError(t, err)

	// 2. Test org quota exhausted (below debt threshold)
	err = svc.SetOrgQuota(orgID, DebtThreshold-100)
	assert.NoError(t, err)
	err = svc.CheckQuota(orgID, projID, 0)
	assert.Equal(t, ErrInsufficientOrgQuota, err)

	// Reset org quota
	err = svc.SetOrgQuota(orgID, 1000)
	assert.NoError(t, err)

	// 3. Test project quota exhausted
	err = svc.SetProjectQuota(projID, DebtThreshold-50)
	assert.NoError(t, err)
	err = svc.CheckQuota(orgID, projID, 0)
	assert.Equal(t, ErrInsufficientProjQuota, err)
}

func TestRedisBillingService_ReportUsage(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	orgID := "org-1"
	projID := "proj-1"
	reqID := "req-123"
	svc.SetOrgQuota(orgID, 1000)
	svc.SetProjectQuota(projID, 500)

	record := UsageRecord{
		RequestID:   reqID,
		OrgID:       orgID,
		ProjectID:   projID,
		TotalTokens: 100,
		Model:       "gpt-3.5-turbo",
	}

	// 1. Successful deduction
	err := svc.ReportUsage(record)
	assert.NoError(t, err)

	orgQuota, _ := svc.GetOrgQuota(orgID)
	projQuota, _ := svc.GetProjectQuota(projID)
	assert.Equal(t, 900, orgQuota)
	assert.Equal(t, 400, projQuota)

	// 2. Idempotency (same request ID should not deduct again)
	err = svc.ReportUsage(record)
	assert.Equal(t, ErrAlreadyProcessed, err)

	// Verify quotas haven't changed
	orgQuota, _ = svc.GetOrgQuota(orgID)
	assert.Equal(t, 900, orgQuota)

	// 3. Debt allowed (quota can go negative)
	record.RequestID = "req-456"
	record.TotalTokens = 1000
	err = svc.ReportUsage(record)
	assert.NoError(t, err)

	orgQuota, _ = svc.GetOrgQuota(orgID)
	assert.Equal(t, -100, orgQuota) // 900 - 1000 = -100
}
