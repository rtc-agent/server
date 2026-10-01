package usecase

import (
	"context"
	"errors"
	"fmt"
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

// acquireReconcileLock tries to acquire the distributed lock for reconciliation.
// Returns (holderUUID, true) on success, ("", false) if another node holds it.
func (uc *OSS3Usecase) acquireReconcileLock(ctx context.Context, ttl time.Duration) (string, bool, error) {
	holderUUID := uuid.New().String()
	lockKey := cache.OSS3Lock("quota_reconcile")

	script := uc.scripts[cache.OSS3ScriptLockAcquire]
	result, err := script.Run(ctx, uc.redis,
		[]string{lockKey},
		holderUUID,
		int(ttl.Seconds()),
	).Int()
	if err != nil {
		return "", false, fmt.Errorf("acquire reconcile lock: %w", err)
	}
	if result == 0 {
		return "", false, nil // another holder holds the lock
	}
	return holderUUID, true, nil
}

// extendReconcileLock extends the TTL of the distributed lock.
func (uc *OSS3Usecase) extendReconcileLock(ctx context.Context, holderUUID string, ttl time.Duration) error {
	lockKey := cache.OSS3Lock("quota_reconcile")

	script := uc.scripts[cache.OSS3ScriptLockExtend]
	result, err := script.Run(ctx, uc.redis,
		[]string{lockKey},
		holderUUID,
		int(ttl.Seconds()),
	).Int()
	if err != nil {
		return fmt.Errorf("extend reconcile lock: %w", err)
	}
	if result == 0 {
		return fmt.Errorf("reconcile lock lost (holder=%s)", holderUUID)
	}
	return nil
}

// releaseReconcileLock releases the distributed lock if still held by caller.
func (uc *OSS3Usecase) releaseReconcileLock(ctx context.Context, holderUUID string) {
	lockKey := cache.OSS3Lock("quota_reconcile")

	script := uc.scripts[cache.OSS3ScriptLockRelease]
	_, err := script.Run(ctx, uc.redis,
		[]string{lockKey},
		holderUUID,
	).Int()
	if err != nil {
		logger.Debug(ctx, "release reconcile lock failed (best-effort)",
			zap.String("holder", holderUUID),
			zap.Error(err))
	}
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

// hasPendingQuotaReservations checks if a user has any pending quota reservations.
// Pending reservations are stored with keys matching pattern "oss3:quota:pending:{userID}:*".
// Returns true if any pending reservations exist, false otherwise.
func (uc *OSS3Usecase) hasPendingQuotaReservations(ctx context.Context, userID string) (bool, error) {
	pattern := cache.PrefixOSS3QuotaPending + userID + ":*"
	iter := uc.redis.Scan(ctx, 0, pattern, 100).Iterator()
	if iter.Err() != nil {
		return false, fmt.Errorf("scan pending reservations for user %s: %w", userID, iter.Err())
	}
	return iter.Next(ctx), nil
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

		for userID, dbSum := range page {
			result.UsersChecked++

			// Check for pending reservations before adjusting quota.
			// Pending reservations indicate in-flight uploads; adjusting now could
			// cause double-counting or lost reservations.
			hasPending, err := uc.hasPendingQuotaReservations(ctx, userID)
			if err != nil {
				logger.Warn(ctx, "reconcile: failed to check pending reservations, skipping user",
					zap.String("user_id", userID),
					zap.Error(err))
				continue
			}
			if hasPending {
				logger.Debug(ctx, "reconcile: user has pending reservations, skipping adjustment",
					zap.String("user_id", userID),
					zap.Int64("db_sum", dbSum))
				continue
			}

			redisVal, err := uc.getRedisQuota(ctx, userID)
			if err != nil {
				logger.Warn(ctx, "reconcile: failed to read redis quota, skipping user",
					zap.String("user_id", userID),
					zap.Error(err))
				continue
			}

			if redisVal != dbSum {
				drift := dbSum - redisVal
				absDrift := drift
				if absDrift < 0 {
					absDrift = -absDrift
				}
				result.TotalDriftBytes += absDrift
				result.UsersAdjusted++

				if setErr := uc.setRedisQuota(ctx, userID, dbSum); setErr != nil {
					logger.Error(ctx, "reconcile: failed to adjust redis quota",
						zap.String("user_id", userID),
						zap.Int64("db_sum", dbSum),
						zap.Int64("redis_val", redisVal),
						zap.Int64("drift", drift),
						zap.Error(setErr))
					continue
				}

				logger.Info(ctx, "reconcile: quota adjusted",
					zap.String("user_id", userID),
					zap.Int64("db_sum", dbSum),
					zap.Int64("redis_val", redisVal),
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
