package cache

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestLuaQuotaReserve_MultiConcurrentAgg verifies that concurrent reserve calls
// for the same user correctly aggregate pending amounts via the ZSET.
// This tests the fix for issue #2: Lua scripts not aggregating pending.
func TestLuaQuotaReserve_MultiConcurrentAgg(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	ctx := context.Background()
	userID := "user-agg-test"
	quotaKey := OSS3Quota(userID)
	aggKey := OSS3QuotaPendingAgg(userID)
	maxQuota := int64(1000)
	pendingTTL := int64(300)

	scripts := RegisterOSS3Scripts(rdb)
	reserveScript := scripts[OSS3ScriptQuotaReserve]

	// Helper to run reserve for a given requestID.
	reserve := func(reqID string, amount int64) int {
		pendingKey := OSS3QuotaPending(userID, reqID)
		prefix := PrefixOSS3QuotaPending + userID + ":"
		result, err := reserveScript.Run(ctx, rdb,
			[]string{quotaKey, pendingKey, aggKey},
			amount, maxQuota, pendingTTL, reqID, prefix,
		).Int()
		if err != nil {
			t.Fatalf("reserve %s failed: %v", reqID, err)
		}
		return result
	}

	// Reserve 600 for req-A: should succeed (0 + 0 + 600 <= 1000).
	if got := reserve("req-A", 600); got != 1 {
		t.Fatalf("first reserve: expected 1, got %d", got)
	}

	// Reserve 500 for req-B: should FAIL (0 + 600 + 500 = 1100 > 1000).
	// This is the key test: before the fix, req-B would see only its own pending (0)
	// and pass the check (0 + 0 + 500 <= 1000), which was wrong.
	if got := reserve("req-B", 500); got != 0 {
		t.Fatalf("second reserve should fail (quota exceeded): expected 0, got %d", got)
	}

	// Reserve 400 for req-C: should succeed (0 + 600 + 400 = 1000 <= 1000).
	if got := reserve("req-C", 400); got != 1 {
		t.Fatalf("third reserve: expected 1, got %d", got)
	}

	// Verify ZSET has two entries (req-A and req-C).
	zcard, err := rdb.ZCard(ctx, aggKey).Result()
	if err != nil {
		t.Fatalf("ZCard failed: %v", err)
	}
	if zcard != 2 {
		t.Errorf("expected 2 ZSET entries, got %d", zcard)
	}
}

// TestLuaQuotaReserve_Idempotent verifies idempotency: calling reserve again
// with the same requestID and amount returns success without double-counting.
func TestLuaQuotaReserve_Idempotent(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	ctx := context.Background()
	userID := "user-idem-test"
	quotaKey := OSS3Quota(userID)
	reqID := "req-idem"
	pendingKey := OSS3QuotaPending(userID, reqID)
	aggKey := OSS3QuotaPendingAgg(userID)
	prefix := PrefixOSS3QuotaPending + userID + ":"

	scripts := RegisterOSS3Scripts(rdb)
	reserveScript := scripts[OSS3ScriptQuotaReserve]

	// First reserve: 500 out of 1000.
	r1, err := reserveScript.Run(ctx, rdb,
		[]string{quotaKey, pendingKey, aggKey},
		int64(500), int64(1000), int64(300), reqID, prefix,
	).Int()
	if err != nil {
		t.Fatalf("first reserve failed: %v", err)
	}
	if r1 != 1 {
		t.Fatalf("first reserve: expected 1, got %d", r1)
	}

	// Idempotent call: same requestID, same amount → should return 1 without double-counting.
	r2, err := reserveScript.Run(ctx, rdb,
		[]string{quotaKey, pendingKey, aggKey},
		int64(500), int64(1000), int64(300), reqID, prefix,
	).Int()
	if err != nil {
		t.Fatalf("idempotent reserve failed: %v", err)
	}
	if r2 != 1 {
		t.Fatalf("idempotent reserve: expected 1, got %d", r2)
	}

	// ZSET should still have exactly 1 entry (not duplicated).
	zcard, _ := rdb.ZCard(ctx, aggKey).Result()
	if zcard != 1 {
		t.Errorf("expected 1 ZSET entry after idempotent call, got %d", zcard)
	}
}

// TestLuaQuotaCommit_ReturnValues verifies that commit returns proper values
// for success (1), pending-expired (0), and amount-overflow (-1).
// This tests the fix for issue #3: CommitQuota discarding return values.
func TestLuaQuotaCommit_ReturnValues(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	ctx := context.Background()
	scripts := RegisterOSS3Scripts(rdb)
	reserveScript := scripts[OSS3ScriptQuotaReserve]
	commitScript := scripts[OSS3ScriptQuotaCommit]

	// --- Case 1: Successful commit (returns 1) ---
	t.Run("success", func(t *testing.T) {
		userID := "user-commit-ok"
		quotaKey := OSS3Quota(userID)
		reqID := "req-ok"
		pendingKey := OSS3QuotaPending(userID, reqID)
		aggKey := OSS3QuotaPendingAgg(userID)
		prefix := PrefixOSS3QuotaPending + userID + ":"

		// Reserve first.
		_, err := reserveScript.Run(ctx, rdb,
			[]string{quotaKey, pendingKey, aggKey},
			int64(100), int64(1000), int64(300), reqID, prefix,
		).Int()
		if err != nil {
			t.Fatalf("reserve failed: %v", err)
		}

		// Commit with correct amount.
		result, err := commitScript.Run(ctx, rdb,
			[]string{quotaKey, pendingKey, aggKey},
			int64(100), reqID,
		).Int()
		if err != nil {
			t.Fatalf("commit failed: %v", err)
		}
		if result != 1 {
			t.Errorf("expected 1 (success), got %d", result)
		}

		// Quota counter should be incremented.
		val, _ := rdb.Get(ctx, quotaKey).Int64()
		if val != 100 {
			t.Errorf("quota counter: expected 100, got %d", val)
		}

		// Pending key should be deleted.
		if mr.Exists(pendingKey) {
			t.Error("pending key should be deleted after commit")
		}

		// ZSET entry should be removed.
		zcard, _ := rdb.ZCard(ctx, aggKey).Result()
		if zcard != 0 {
			t.Errorf("ZSET should be empty after commit, got %d entries", zcard)
		}
	})

	// --- Case 2: Pending expired (returns 0) ---
	t.Run("pending_expired", func(t *testing.T) {
		userID := "user-commit-expired"
		quotaKey := OSS3Quota(userID)
		reqID := "req-expired"
		pendingKey := OSS3QuotaPending(userID, reqID)
		aggKey := OSS3QuotaPendingAgg(userID)
		prefix := PrefixOSS3QuotaPending + userID + ":"

		// Reserve first.
		_, err := reserveScript.Run(ctx, rdb,
			[]string{quotaKey, pendingKey, aggKey},
			int64(100), int64(1000), int64(300), reqID, prefix,
		).Int()
		if err != nil {
			t.Fatalf("reserve failed: %v", err)
		}

		// Simulate TTL expiry by deleting the pending key (miniredis FastForward
		// may not clean up keys set by Lua scripts reliably).
		rdb.Del(ctx, pendingKey)

		// Commit should return 0 (pending expired).
		result, err := commitScript.Run(ctx, rdb,
			[]string{quotaKey, pendingKey, aggKey},
			int64(100), reqID,
		).Int()
		if err != nil {
			t.Fatalf("commit returned error: %v", err)
		}
		if result != 0 {
			t.Errorf("expected 0 (pending expired), got %d", result)
		}

		// ZSET entry should also be cleaned up.
		zcard, _ := rdb.ZCard(ctx, aggKey).Result()
		if zcard != 0 {
			t.Errorf("ZSET should be empty after expired commit cleanup, got %d", zcard)
		}
	})

	// --- Case 3: Amount exceeds pending (returns -1) ---
	t.Run("amount_overflow", func(t *testing.T) {
		userID := "user-commit-overflow"
		quotaKey := OSS3Quota(userID)
		reqID := "req-overflow"
		pendingKey := OSS3QuotaPending(userID, reqID)
		aggKey := OSS3QuotaPendingAgg(userID)
		prefix := PrefixOSS3QuotaPending + userID + ":"

		// Reserve 100.
		_, err := reserveScript.Run(ctx, rdb,
			[]string{quotaKey, pendingKey, aggKey},
			int64(100), int64(1000), int64(300), reqID, prefix,
		).Int()
		if err != nil {
			t.Fatalf("reserve failed: %v", err)
		}

		// Commit with amount > reserved (caller bug).
		result, err := commitScript.Run(ctx, rdb,
			[]string{quotaKey, pendingKey, aggKey},
			int64(200), reqID,
		).Int()
		if err != nil {
			t.Fatalf("commit returned error: %v", err)
		}
		if result != -1 {
			t.Errorf("expected -1 (amount overflow), got %d", result)
		}

		// Pending key should still exist (not consumed).
		if !mr.Exists(pendingKey) {
			t.Error("pending key should still exist after overflow")
		}
	})
}

// TestLuaQuotaRollback_CleansUpZSET verifies that rollback removes both the
// pending key and the ZSET entry.
func TestLuaQuotaRollback_CleansUpZSET(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	ctx := context.Background()
	scripts := RegisterOSS3Scripts(rdb)
	reserveScript := scripts[OSS3ScriptQuotaReserve]
	rollbackScript := scripts[OSS3ScriptQuotaRollback]

	userID := "user-rollback-test"
	quotaKey := OSS3Quota(userID)
	reqID := "req-rollback"
	pendingKey := OSS3QuotaPending(userID, reqID)
	aggKey := OSS3QuotaPendingAgg(userID)
	prefix := PrefixOSS3QuotaPending + userID + ":"

	// Reserve first.
	_, err := reserveScript.Run(ctx, rdb,
		[]string{quotaKey, pendingKey, aggKey},
		int64(100), int64(1000), int64(300), reqID, prefix,
	).Int()
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}

	// Verify ZSET has 1 entry before rollback.
	zcard, _ := rdb.ZCard(ctx, aggKey).Result()
	if zcard != 1 {
		t.Fatalf("expected 1 ZSET entry before rollback, got %d", zcard)
	}

	// Rollback.
	result, err := rollbackScript.Run(ctx, rdb,
		[]string{pendingKey, aggKey}, reqID,
	).Int()
	if err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	if result != 1 {
		t.Errorf("expected 1 (deleted), got %d", result)
	}

	// Pending key should be deleted.
	if mr.Exists(pendingKey) {
		t.Error("pending key should be deleted after rollback")
	}

	// ZSET should be empty.
	zcard, _ = rdb.ZCard(ctx, aggKey).Result()
	if zcard != 0 {
		t.Errorf("ZSET should be empty after rollback, got %d entries", zcard)
	}

	// Rollback again: should return 0 (already gone).
	result, err = rollbackScript.Run(ctx, rdb,
		[]string{pendingKey, aggKey}, reqID,
	).Int()
	if err != nil {
		t.Fatalf("second rollback failed: %v", err)
	}
	if result != 0 {
		t.Errorf("expected 0 (already gone), got %d", result)
	}
}

// TestLuaQuotaReserve_StaleZSETCleanup verifies that stale ZSET entries (whose
// pending keys expired via TTL) are cleaned up during reserve.
func TestLuaQuotaReserve_StaleZSETCleanup(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	ctx := context.Background()
	scripts := RegisterOSS3Scripts(rdb)
	reserveScript := scripts[OSS3ScriptQuotaReserve]

	userID := "user-stale-test"
	quotaKey := OSS3Quota(userID)
	aggKey := OSS3QuotaPendingAgg(userID)
	prefix := PrefixOSS3QuotaPending + userID + ":"

	// Manually create a stale ZSET entry (pending key doesn't exist).
	rdb.ZAdd(ctx, aggKey, redis.Z{Score: 500, Member: "stale-req"})

	// Now reserve 600 bytes. If the stale entry is not cleaned up,
	// totalPending would be 500 and (0 + 500 + 600 = 1100 > 1000) would fail.
	// With cleanup, the stale entry is removed and totalPending = 0,
	// so (0 + 0 + 600 = 600 <= 1000) succeeds.
	reqID := "req-fresh"
	pendingKey := OSS3QuotaPending(userID, reqID)
	result, err := reserveScript.Run(ctx, rdb,
		[]string{quotaKey, pendingKey, aggKey},
		int64(600), int64(1000), int64(300), reqID, prefix,
	).Int()
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if result != 1 {
		t.Fatalf("expected 1 (success after stale cleanup), got %d", result)
	}

	// ZSET should have 1 entry (the fresh one; stale was cleaned up).
	zcard, _ := rdb.ZCard(ctx, aggKey).Result()
	if zcard != 1 {
		t.Errorf("expected 1 ZSET entry after stale cleanup, got %d", zcard)
	}

	members, _ := rdb.ZRange(ctx, aggKey, 0, -1).Result()
	if len(members) != 1 || members[0] != reqID {
		t.Errorf("expected only %q in ZSET, got %v", reqID, members)
	}
}
