// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisOTPStore implements OTPStore using Redis.
// Suitable for distributed multi-instance deployments.
//
// Redis key design:
//   - admin:otp:code:{email}            - OTP value, TTL = configured TTL
//   - admin:otp:cooldown:{email}        - Send cooldown marker, TTL = cooldown duration
//   - admin:otp:send:ip:{ip}            - IP send count, TTL = 1 minute
//   - admin:otp:verify:{email}          - Verify failure count, TTL = lock duration
//   - admin:otp:lock:{email}            - Verify lockout marker, TTL = lock duration
type RedisOTPStore struct {
	rdb               *redis.Client
	maxSendPerIP      int
	maxVerifyAttempts int
	lockDuration      time.Duration
	sendCooldown      time.Duration
}

// NewRedisOTPStore creates a new RedisOTPStore.
func NewRedisOTPStore(rdb *redis.Client, maxSendPerIP, maxVerifyAttempts int, lockDuration, sendCooldown time.Duration) *RedisOTPStore {
	return &RedisOTPStore{
		rdb:               rdb,
		maxSendPerIP:      maxSendPerIP,
		maxVerifyAttempts: maxVerifyAttempts,
		lockDuration:      lockDuration,
		sendCooldown:      sendCooldown,
	}
}

// Store stores an OTP for the given email with TTL.
func (s *RedisOTPStore) Store(ctx context.Context, emailAddr, otp string, ttl time.Duration) error {
	key := fmt.Sprintf("admin:otp:code:%s", emailAddr)
	return s.rdb.Set(ctx, key, otp, ttl).Err()
}

// Get retrieves the stored OTP for the given email.
// Returns ErrOTPNotFound if not found or expired.
func (s *RedisOTPStore) Get(ctx context.Context, emailAddr string) (string, error) {
	key := fmt.Sprintf("admin:otp:code:%s", emailAddr)
	otp, err := s.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", ErrOTPNotFound
	}
	if err != nil {
		return "", fmt.Errorf("redis get otp: %w", err)
	}
	return otp, nil
}

// Delete removes the stored OTP for the given email.
func (s *RedisOTPStore) Delete(ctx context.Context, emailAddr string) {
	key := fmt.Sprintf("admin:otp:code:%s", emailAddr)
	_ = s.rdb.Del(ctx, key).Err()
}

// CheckSendAllowed returns nil if sending OTP is allowed for the email and IP.
func (s *RedisOTPStore) CheckSendAllowed(ctx context.Context, emailAddr, clientIP string) error {
	// Check email cooldown
	cooldownKey := fmt.Sprintf("admin:otp:cooldown:%s", emailAddr)
	if ttl, err := s.rdb.TTL(ctx, cooldownKey).Result(); err == nil && ttl > 0 {
		return fmt.Errorf("please wait %d seconds before requesting another code", int(ttl.Seconds()))
	}

	// Check IP rate limit
	ipKey := fmt.Sprintf("admin:otp:send:ip:%s", clientIP)
	count, err := s.rdb.Get(ctx, ipKey).Int()
	if err == nil && count >= s.maxSendPerIP {
		return fmt.Errorf("too many requests from this IP, please try again later")
	}

	return nil
}

// RecordSend records an OTP send event for rate limiting.
func (s *RedisOTPStore) RecordSend(ctx context.Context, emailAddr, clientIP string) {
	pipe := s.rdb.Pipeline()

	// Set email cooldown
	cooldownKey := fmt.Sprintf("admin:otp:cooldown:%s", emailAddr)
	pipe.Set(ctx, cooldownKey, "1", s.sendCooldown)

	// Increment IP rate limit
	ipKey := fmt.Sprintf("admin:otp:send:ip:%s", clientIP)
	pipe.Incr(ctx, ipKey)
	pipe.Expire(ctx, ipKey, time.Minute)

	_, _ = pipe.Exec(ctx)
}

// CheckVerifyAllowed returns nil if verification is allowed for the email.
func (s *RedisOTPStore) CheckVerifyAllowed(ctx context.Context, emailAddr string) error {
	lockKey := fmt.Sprintf("admin:otp:lock:%s", emailAddr)
	if ttl, err := s.rdb.TTL(ctx, lockKey).Result(); err == nil && ttl > 0 {
		return fmt.Errorf("verification locked, please try again in %d seconds", int(ttl.Seconds()))
	}
	return nil
}

// RecordVerifyFailure records a failed verification attempt.
func (s *RedisOTPStore) RecordVerifyFailure(ctx context.Context, emailAddr string) {
	failKey := fmt.Sprintf("admin:otp:verify:%s", emailAddr)
	count, _ := s.rdb.Incr(ctx, failKey).Result()

	if count == 1 {
		s.rdb.Expire(ctx, failKey, s.lockDuration)
	}

	if count >= int64(s.maxVerifyAttempts) {
		lockKey := fmt.Sprintf("admin:otp:lock:%s", emailAddr)
		s.rdb.Set(ctx, lockKey, "1", s.lockDuration)
		// Reset failure counter after lockout
		s.rdb.Del(ctx, failKey)
	}
}

// ResetVerifyFailures resets the failure counter for the email.
func (s *RedisOTPStore) ResetVerifyFailures(ctx context.Context, emailAddr string) {
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, fmt.Sprintf("admin:otp:verify:%s", emailAddr))
	pipe.Del(ctx, fmt.Sprintf("admin:otp:lock:%s", emailAddr))
	_, _ = pipe.Exec(ctx)
}
