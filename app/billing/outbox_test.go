package billing

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRecord(id string) UsageRecord {
	return UsageRecord{
		RequestID:    id,
		OrgID:        "org-1",
		ProjectID:    "proj-1",
		Model:        "Qwen/Qwen2.5-0.5B-Instruct",
		TotalTokens:  100,
		UsageSource:  "official",
		Timestamp:    time.Now(),
	}
}

// TestOutbox_SettlesBilledRecord enqueues a normal record and settles it via
// the outbox worker path, verifying quota deduction + ledger state.
func TestOutbox_SettlesBilledRecord(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	require.NoError(t, svc.SetOrgQuota("org-1", 1000))
	require.NoError(t, svc.SetProjectQuota("proj-1", 1000))

	record := testRecord("req-outbox-billed")
	require.NoError(t, svc.enqueueOutbox(record))

	// Confirm it is in the outbox list.
	items, err := svc.client.LRange(svc.ctx, outboxKey, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, items, 1)

	svc.settleOutboxItem(items[0])

	// Ledger billed + quota deducted once.
	state, err := svc.client.HGet(svc.ctx, "usage:ledger:req-outbox-billed", "state").Result()
	require.NoError(t, err)
	assert.Equal(t, "billed", state)

	orgQuota, _ := svc.GetOrgQuota("org-1")
	assert.Equal(t, 900, orgQuota, "100 tokens deducted from org quota")

	// Processing list drained.
	processing, err := svc.client.LLen(svc.ctx, outboxProcessing).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), processing)
}

// TestOutbox_SettlesDeferredRecord verifies deferred records settle to a
// pending ledger (no deduction) through the outbox path.
func TestOutbox_SettlesDeferredRecord(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	record := testRecord("req-outbox-deferred")
	record.Deferred = true
	require.NoError(t, svc.enqueueOutbox(record))

	items, err := svc.client.LRange(svc.ctx, outboxKey, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, items, 1)

	svc.settleOutboxItem(items[0])

	state, err := svc.client.HGet(svc.ctx, "usage:ledger:req-outbox-deferred", "state").Result()
	require.NoError(t, err)
	assert.Equal(t, "pending", state)
}

// TestOutbox_CrashRecovery simulates a pod crash: records enqueued by one
// service instance are recovered and settled by a brand-new instance.
func TestOutbox_CrashRecovery(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	// Instance A enqueues then "crashes" (never starts its worker).
	svcA := NewRedisBillingService(mr.Addr(), "", false)
	defer svcA.RedisClient().Close()
	require.NoError(t, svcA.SetOrgQuota("org-1", 1000))
	require.NoError(t, svcA.SetProjectQuota("proj-1", 1000))
	require.NoError(t, svcA.enqueueOutbox(testRecord("req-crash-recovery")))

	// Instance B boots against the same Redis and drains the outbox.
	svcB := NewRedisBillingService(mr.Addr(), "", false)
	svcB.Start()
	defer svcB.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state, _ := svcB.client.HGet(svcB.ctx, "usage:ledger:req-crash-recovery", "state").Result()
		if state == "billed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	state, err := svcB.client.HGet(svcB.ctx, "usage:ledger:req-crash-recovery", "state").Result()
	require.NoError(t, err)
	assert.Equal(t, "billed", state, "new instance must recover and settle the outbox entry")
}

// TestOutbox_AlreadyProcessedRemoved verifies an entry whose idempotency key
// was already set is treated as done (removed) without double deduction.
func TestOutbox_AlreadyProcessedRemoved(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	require.NoError(t, svc.SetOrgQuota("org-1", 1000))
	require.NoError(t, svc.SetProjectQuota("proj-1", 1000))

	// Pre-mark as processed (simulates a prior successful settlement).
	require.NoError(t, svc.client.Set(svc.ctx, "usage:req:req-idem", "processed", 0).Err())

	record := testRecord("req-idem")
	require.NoError(t, svc.enqueueOutbox(record))
	items, _ := svc.client.LRange(svc.ctx, outboxKey, 0, -1).Result()
	require.Len(t, items, 1)

	svc.settleOutboxItem(items[0])

	orgQuota, _ := svc.GetOrgQuota("org-1")
	assert.Equal(t, 1000, orgQuota, "already-processed entry must not deduct again")

	processing, _ := svc.client.LLen(svc.ctx, outboxProcessing).Result()
	assert.Equal(t, int64(0), processing, "already-processed entry must be removed from processing")
}

// TestOutbox_DeadLetterOnUnparseableItem verifies malformed outbox payloads
// are moved to the dead list instead of retried forever.
func TestOutbox_DeadLetterOnUnparseableItem(t *testing.T) {
	svc, mr := setupTestRedis(t)
	defer mr.Close()

	require.NoError(t, svc.client.LPush(svc.ctx, outboxKey, "{not-json").Err())
	items, _ := svc.client.LRange(svc.ctx, outboxKey, 0, -1).Result()
	require.Len(t, items, 1)

	svc.settleOutboxItem(items[0])

	dead, err := svc.client.LLen(svc.ctx, outboxDead).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), dead, "unparseable item must move to dead list")
}

// TestOutbox_RedisDownFallsBackToMemory verifies that when Redis is fully
// unreachable, ReportUsage falls back to the in-memory retry queue (last
// resort), never silently dropping the record.
func TestOutbox_RedisDownFallsBackToMemory(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)

	svc := &RedisBillingService{
		client: redis.NewClient(&redis.Options{
			Addr:         mr.Addr(),
			DialTimeout:  50 * time.Millisecond,
			ReadTimeout:  50 * time.Millisecond,
			WriteTimeout: 50 * time.Millisecond,
			MaxRetries:   0,
		}),
		ctx:      t.Context(),
		failOpen: false,
	}

	mr.Close() // Redis goes down

	err = svc.ReportUsage(testRecord("req-redis-down"))
	require.Error(t, err)

	svc.retryMu.Lock()
	queued := len(svc.retryQueue)
	svc.retryMu.Unlock()
	assert.Equal(t, 1, queued, "record must land in the in-memory retry queue when Redis is down")
}
