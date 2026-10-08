package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/infra/cache"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// CheckRateLimit atomically checks and records a request in the sliding window.
// Returns the current request count in the window.
// Caller should reject with ErrSlowDown if count > maxRequests.
//
// requestID must be unique per request (e.g., snowflake ID or UUID).
// The Lua script OSS3RateLimitCheck:
//  1. Removes entries older than windowStart (ZREMRANGEBYSCORE)
//  2. Counts remaining entries (ZCARD)
//  3. If count >= max, rejects without adding
//  4. Otherwise, adds the new request (ZADD) and sets key expiry
//
// All steps are atomic — no race between check and record.
func (uc *OSS3Usecase) CheckRateLimit(ctx context.Context, userID string, requestID string) (count int64, err error) {
	rateKey := cache.OSS3RateLimit(userID)
	now := float64(time.Now().UnixMilli()) / 1000.0
	windowStart := now - 60.0 // 60-second sliding window

	script := uc.scripts[cache.OSS3ScriptRateLimit]
	result, err := script.Run(ctx, uc.redis,
		[]string{rateKey},
		now,
		windowStart,
		uc.cfg.RateLimit.RequestsPerMinute,
		requestID,
	).Int()
	if err != nil {
		return 0, fmt.Errorf("rate limit check: %w", err)
	}
	return int64(result), nil
}

// CheckRateLimitOrReject is a convenience wrapper that returns ErrSlowDown if rate exceeded.
func (uc *OSS3Usecase) CheckRateLimitOrReject(ctx context.Context, userID string, requestID string) error {
	count, err := uc.CheckRateLimit(ctx, userID, requestID)
	if err != nil {
		return fmt.Errorf("check rate limit: %w", err)
	}
	if count > int64(uc.cfg.RateLimit.RequestsPerMinute) {
		return rtcoss3.ErrSlowDown
	}
	return nil
}
