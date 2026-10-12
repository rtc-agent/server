package usecase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// ReconcileResult holds the outcome of a quota reconciliation cycle.
type ReconcileResult struct {
	// UsersChecked is the number of user quota entries compared.
	UsersChecked int
	// UsersAdjusted is the number of users whose Redis counter was corrected.
	UsersAdjusted int
	// TotalDriftBytes is the sum of absolute drift across all users.
	TotalDriftBytes int64
	// Duration is the wall-clock time taken by the reconciliation.
	Duration time.Duration
}

// acquireLock is a generic helper to acquire a distributed lock.
// Returns (holderUUID, true) on success, ("", false) if another node holds it.
func (uc *OSS3Usecase) acquireLock(ctx context.Context, lockName string, ttl time.Duration) (string, bool, error) {
	holderUUID := uuid.New().String()
	lockKey := cache.OSS3Lock(lockName)

	script := uc.scripts[cache.OSS3ScriptLockAcquire]
	result, err := script.Run(ctx, uc.redis,
		[]string{lockKey},
		holderUUID,
		int(ttl.Seconds()),
	).Int()
	if err != nil {
		return "", false, fmt.Errorf("acquire %s lock: %w", lockName, err)
	}
	if result == 0 {
		return "", false, nil // another holder holds the lock
	}
	return holderUUID, true, nil
}

// extendLock is a generic helper to extend the TTL of a distributed lock.
// P3-5 fix: Added retry logic (up to 3 attempts) to handle transient Redis failures.
// This prevents reconciliation from aborting due to temporary network issues.
func (uc *OSS3Usecase) extendLock(ctx context.Context, lockName, holderUUID string, ttl time.Duration) error {
	lockKey := cache.OSS3Lock(lockName)
	script := uc.scripts[cache.OSS3ScriptLockExtend]

	// P3-5 fix: Retry up to 3 times on transient errors
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := script.Run(ctx, uc.redis,
			[]string{lockKey},
			holderUUID,
			int(ttl.Seconds()),
		).Int()
		if err != nil {
			lastErr = fmt.Errorf("extend %s lock: %w", lockName, err)
			// Check if error is transient (connection, timeout)
			// Note: isTransientRedisError is defined in oss3_quota.go
			if isTransientRedisErrorForReconcile(err) && attempt < maxAttempts {
				logger.Debug(ctx, "extend_lock_transient_error_retrying",
					zap.String("lock_name", lockName),
					zap.Int("attempt", attempt),
					zap.Error(err))
				continue
			}
			return lastErr
		}
		if result == 0 {
			return fmt.Errorf("%s lock lost (holder=%s)", lockName, holderUUID)
		}
		// Success
		return nil
	}
	return lastErr
}

// isTransientRedisErrorForReconcile checks if a Redis error is transient and safe to retry.
// This is specific to the reconciler; handler layer has its own isTransientRedisError.
func isTransientRedisErrorForReconcile(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	// Common transient errors: connection reset, timeout, I/O error
	return strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "i/o timeout") ||
		strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "NOSCRIPT")
}

// releaseLock is a generic helper to release a distributed lock.
func (uc *OSS3Usecase) releaseLock(ctx context.Context, lockName, holderUUID string) {
	lockKey := cache.OSS3Lock(lockName)

	script := uc.scripts[cache.OSS3ScriptLockRelease]
	_, err := script.Run(ctx, uc.redis,
		[]string{lockKey},
		holderUUID,
	).Int()
	if err != nil {
		logger.Debug(ctx, "release_lock_failed_best_effort",
			zap.String("lock_name", lockName),
			zap.String("holder", holderUUID),
			zap.Error(err))
	}
}

// acquireReconcileLock tries to acquire the distributed lock for reconciliation.
// Returns (holderUUID, true) on success, ("", false) if another node holds it.
func (uc *OSS3Usecase) acquireReconcileLock(ctx context.Context, ttl time.Duration) (string, bool, error) {
	return uc.acquireLock(ctx, "quota_reconcile", ttl)
}

// extendReconcileLock extends the TTL of the distributed lock.
func (uc *OSS3Usecase) extendReconcileLock(ctx context.Context, holderUUID string, ttl time.Duration) error {
	return uc.extendLock(ctx, "quota_reconcile", holderUUID, ttl)
}

// releaseReconcileLock releases the distributed lock if still held by caller.
func (uc *OSS3Usecase) releaseReconcileLock(ctx context.Context, holderUUID string) {
	uc.releaseLock(ctx, "quota_reconcile", holderUUID)
}

// getRedisQuota reads the current Redis quota counter for a user.
func (uc *OSS3Usecase) getRedisQuota(ctx context.Context, userID string) (int64, error) {
	key := cache.OSS3Quota(userID)
	val, err := uc.redis.Get(ctx, key).Int64()
	if err != nil {
		// redis.Nil means the key does not exist (counter is 0).
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("get redis quota for user %s: %w", userID, err)
	}
	return val, nil
}

// setRedisQuota sets the Redis quota counter for a user.
func (uc *OSS3Usecase) setRedisQuota(ctx context.Context, userID string, value int64) error {
	key := cache.OSS3Quota(userID)
	if err := uc.redis.Set(ctx, key, value, 0).Err(); err != nil {
		return fmt.Errorf("set redis quota for user %s: %w", userID, err)
	}
	return nil
}

// getPendingQuotaTotal calculates the total amount of pending quota reservations for a user.
// Uses the pending aggregation ZSET (OSS3QuotaPendingAgg) which stores all pending reservations
// with their amounts as scores. Returns the sum of all pending amounts.
//
// This is used during quota reconciliation to account for in-flight uploads.
// The correct Redis quota value should be: dbSum + pendingTotal.
//
// P1-3 fix: Only counts pending entries whose backing pending key still exists.
// This prevents stale ZSET entries (whose pending keys expired but weren't cleaned)
// from inflating the pending total and causing over-correction.
//
// P2-4 fix: Added COUNT parameter to ZRANGEBYSCORE to prevent unbounded memory usage.
// The limit of 1000 entries per query balances between:
//   - Completeness: Most users won't have more than 1000 concurrent pending uploads
//   - Memory safety: Prevents OOM if a user has an unusually large number of pending entries
//   - Performance: Avoids loading excessive data in a single Redis call
//
// If a user exceeds this limit, reconciliation will undercount their pending total,
// but this is acceptable as it's a rare edge case and the next reconciliation cycle
// will correct any drift.
func (uc *OSS3Usecase) getPendingQuotaTotal(ctx context.Context, userID string) (int64, error) {
	aggKey := cache.OSS3QuotaPendingAgg(userID)
	pendingKeyPrefix := cache.PrefixOSS3QuotaPending + userID + ":"

	// Use ZRANGEBYSCORE with COUNT limit to get members with their scores.
	// The ZSET stores requestID as member and amount as score.
	// We sum scores only for entries whose backing pending key still exists.
	//
	// COUNT 1000 prevents unbounded memory usage when a user has many pending entries.
	// This is a pragmatic limit: most users won't exceed 1000 concurrent pending uploads,
	// and even if they do, the next reconciliation cycle will correct any drift.
	const maxPendingEntries = 1000
	members, err := uc.redis.ZRangeWithScores(ctx, aggKey, 0, int64(maxPendingEntries-1)).Result()
	if err != nil {
		return 0, fmt.Errorf("get pending quota total for user %s: %w", userID, err)
	}

	var total int64
	for _, m := range members {
		requestID := m.Member.(string)
		// Verify the backing pending key exists.
		// If the key is gone (expired), this ZSET entry is stale and should be skipped.
		pendingKey := pendingKeyPrefix + requestID
		if exists, getErr := uc.redis.Exists(ctx, pendingKey).Result(); getErr != nil {
			// Redis error — skip this entry but log warning
			logger.Warn(ctx, "reconcile: failed to check pending key existence, skipping entry",
				zap.String("user_id", userID),
				zap.String("request_id", requestID),
				zap.Error(getErr))
			continue
		} else if exists == 0 {
			// Pending key gone — stale ZSET entry, skip it
			continue
		}
		total += int64(m.Score)
	}
	return total, nil
}

// ReconcileQuota compares DB file sums against Redis quota counters and adjusts
// any drift. This is the Layer 2 defense against quota counter drift caused by
// commit failures that survive Layer 1 retries.
//
// It acquires a distributed lock (so only one node runs reconciliation at a time),
// paginates through all users in the DB, compares each user's DB sum with their
// Redis counter, and overwrites the Redis counter if they differ.
//
// Should be called periodically (e.g., from RunCleanup).
func (uc *OSS3Usecase) ReconcileQuota(ctx context.Context) (*ReconcileResult, error) {
	start := time.Now()

	// Acquire distributed lock (TTL = 5 minutes, covers full pagination).
	const lockTTL = 5 * time.Minute
	holderUUID, acquired, err := uc.acquireReconcileLock(ctx, lockTTL)
	if err != nil {
		return nil, fmt.Errorf("acquire reconcile lock: %w", err)
	}
	if !acquired {
		// Another node is running reconciliation — skip this cycle.
		logger.Debug(ctx, "reconcile: lock held by another node, skipping")
		return &ReconcileResult{Duration: time.Since(start)}, nil
	}
	defer uc.releaseReconcileLock(ctx, holderUUID)

	result := &ReconcileResult{}

	// Paginate through users in batches to avoid unbounded memory.
	const pageSize = 500
	cursor := ""
	for {
		// Extend lock before each page to prevent expiry during long runs.
		if extendErr := uc.extendReconcileLock(ctx, holderUUID, lockTTL); extendErr != nil {
			return nil, fmt.Errorf("extend reconcile lock: %w", extendErr)
		}

		page, nextCursor, err := uc.fileRepo.SumSizeByUserGroupedPaginated(ctx, cursor, pageSize)
		if err != nil {
			return nil, fmt.Errorf("reconcile: page query: %w", err)
		}

		// Sort user IDs for deterministic processing order.
		// Map iteration order is non-deterministic in Go; sorting ensures
		// consistent log ordering and predictable behavior on partial failures.
		userIDs := make([]string, 0, len(page))
		for uid := range page {
			userIDs = append(userIDs, uid)
		}
		sort.Strings(userIDs)

		for _, userID := range userIDs {
			dbSum := page[userID]
			result.UsersChecked++

			// Get pending reservations total for this user.
			// Pending reservations represent in-flight uploads that have reserved quota
			// but not yet committed. The correct Redis quota value should be:
			//   redisVal == dbSum + pendingTotal
			// This allows reconciliation to proceed even when uploads are active,
			// while still detecting drift caused by commit failures.
			pendingTotal, err := uc.getPendingQuotaTotal(ctx, userID)
			if err != nil {
				logger.Warn(ctx, "reconcile: failed to get pending quota total, skipping user",
					zap.String("user_id", userID),
					zap.Error(err))
				continue
			}

			redisVal, err := uc.getRedisQuota(ctx, userID)
			if err != nil {
				logger.Warn(ctx, "reconcile: failed to read redis quota, skipping user",
					zap.String("user_id", userID),
					zap.Error(err))
				continue
			}

			// Expected Redis value: DB sum + pending reservations.
			// Drift is the difference between actual Redis value and expected value.
			expectedRedisVal := dbSum + pendingTotal
			if redisVal != expectedRedisVal {
				drift := expectedRedisVal - redisVal
				absDrift := drift
				if absDrift < 0 {
					absDrift = -absDrift
				}
				result.TotalDriftBytes += absDrift
				result.UsersAdjusted++

				if setErr := uc.setRedisQuota(ctx, userID, expectedRedisVal); setErr != nil {
					logger.Error(ctx, "reconcile: failed to adjust redis quota",
						zap.String("user_id", userID),
						zap.Int64("db_sum", dbSum),
						zap.Int64("pending_total", pendingTotal),
						zap.Int64("expected_redis_val", expectedRedisVal),
						zap.Int64("actual_redis_val", redisVal),
						zap.Int64("drift", drift),
						zap.Error(setErr))
					continue
				}

				logger.Info(ctx, "reconcile: quota adjusted",
					zap.String("user_id", userID),
					zap.Int64("db_sum", dbSum),
					zap.Int64("pending_total", pendingTotal),
					zap.Int64("expected_redis_val", expectedRedisVal),
					zap.Int64("actual_redis_val", redisVal),
					zap.Int64("drift", drift))
			}
		}

		if nextCursor == "" {
			break // last page
		}
		cursor = nextCursor

		// Check context cancellation between pages.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("reconcile: context cancelled: %w", ctx.Err())
		}
	}

	result.Duration = time.Since(start)

	if result.UsersAdjusted > 0 {
		logger.Info(ctx, "reconcile: cycle complete",
			zap.Int("users_checked", result.UsersChecked),
			zap.Int("users_adjusted", result.UsersAdjusted),
			zap.Int64("total_drift_bytes", result.TotalDriftBytes),
			zap.Duration("duration", result.Duration))
	} else {
		logger.Debug(ctx, "reconcile: cycle complete, no drift",
			zap.Int("users_checked", result.UsersChecked),
			zap.Duration("duration", result.Duration))
	}

	return result, nil
}
