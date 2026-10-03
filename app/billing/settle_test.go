package billing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func settleTestRecord() UsageRecord {
	return UsageRecord{
		RequestID:        "req-settle",
		OrgID:            "org-1",
		ProjectID:        "proj-1",
		Model:            "Qwen/Qwen2.5-0.5B-Instruct",
		PromptTokens:     40,
		CompletionTokens: 60,
		TotalTokens:      100,
		UsageSource:      "estimated",
		RequestStatus:    "upstream_error",
		Deferred:         true,
	}
}

// TestSettleUsage_BilledDeductsOnce verifies pending -> billed deducts quota
// exactly once (second settle is rejected), and sets the idempotency key.
func TestSettleUsage_BilledDeductsOnce(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	require.NoError(t, svc.SetOrgQuota("org-1", 1000))
	require.NoError(t, svc.SetProjectQuota("proj-1", 1000))
	require.NoError(t, svc.recordDeferred(settleTestRecord()))

	require.NoError(t, svc.SettleUsage("req-settle", "billed"))

	state, err := svc.client.HGet(svc.ctx, "usage:ledger:req-settle", "state").Result()
	require.NoError(t, err)
	assert.Equal(t, "billed", state)

	orgQuota, _ := svc.GetOrgQuota("org-1")
	projQuota, _ := svc.GetProjectQuota("proj-1")
	assert.Equal(t, 900, orgQuota)
	assert.Equal(t, 900, projQuota)

	// Idempotency key set so the request can never be billed again.
	exists, err := svc.client.Exists(svc.ctx, "usage:req:req-settle").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists)

	// Second settle must be rejected (not pending) and must not double-deduct.
	err = svc.SettleUsage("req-settle", "billed")
	require.Error(t, err)
	orgQuota, _ = svc.GetOrgQuota("org-1")
	assert.Equal(t, 900, orgQuota, "second settle must not deduct again")
}

// TestSettleUsage_CancelledNoDeduction verifies pending -> cancelled marks the
// entry cancelled without touching quota or setting the idempotency key.
func TestSettleUsage_CancelledNoDeduction(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	require.NoError(t, svc.SetOrgQuota("org-1", 1000))
	require.NoError(t, svc.SetProjectQuota("proj-1", 1000))
	require.NoError(t, svc.recordDeferred(settleTestRecord()))

	require.NoError(t, svc.SettleUsage("req-settle", "cancelled"))

	state, err := svc.client.HGet(svc.ctx, "usage:ledger:req-settle", "state").Result()
	require.NoError(t, err)
	assert.Equal(t, "cancelled", state)

	orgQuota, _ := svc.GetOrgQuota("org-1")
	assert.Equal(t, 1000, orgQuota, "cancelled settle must not deduct quota")

	exists, err := svc.client.Exists(svc.ctx, "usage:req:req-settle").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "cancelled settle must not set the idempotency key")
}

// TestSettleUsage_RejectsNonPending verifies billed and refunded entries cannot
// transition.
func TestSettleUsage_RejectsNonPending(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	// A normally-billed record (state=billed).
	record := settleTestRecord()
	record.Deferred = false
	record.RequestStatus = "completed"
	require.NoError(t, svc.ReportUsage(record))
	err := svc.SettleUsage("req-settle", "cancelled")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in pending state")

	// A refunded record (state=refunded).
	require.NoError(t, svc.recordDeferred(UsageRecord{RequestID: "req-refund", OrgID: "org-1", ProjectID: "proj-1", TotalTokens: 50}))
	require.NoError(t, svc.SettleUsage("req-refund", "billed"))
	require.NoError(t, svc.RefundUsage("req-refund"))
	err = svc.SettleUsage("req-refund", "billed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in pending state")
}

// TestSettleUsage_LedgerNotFound verifies settling a missing request errors.
func TestSettleUsage_LedgerNotFound(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	err := svc.SettleUsage("req-nonexistent", "billed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ledger entry not found")
}

// TestSettleUsage_InvalidAction verifies an unknown action is rejected up front.
func TestSettleUsage_InvalidAction(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	err := svc.SettleUsage("req-any", "foo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid settle action")
}
