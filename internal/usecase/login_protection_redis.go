// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// loginAttemptScript atomically increments an attempt counter and refreshes its TTL.
// TTL is refreshed on every increment (not just the first) to implement a sliding window:
// an attacker cannot evade lockout by spacing attempts beyond the initial TTL window.
var loginAttemptScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], ARGV[1])
return count
`)

// RedisLoginProtection implements IP + email dual lockout using Redis.
//
// Uses Redis INCR + EXPIRE for atomic counting with automatic TTL.
// This ensures consistent lockout across multiple server instances.
type RedisLoginProtection struct {
	rdb          *redis.Client
	maxAttempts  int
	lockDuration time.Duration
}

// NewRedisLoginProtection creates a Redis-backed login protection.
func NewRedisLoginProtection(rdb *redis.Client, cfg LoginProtectionConfig) *RedisLoginProtection {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.LockDuration <= 0 {
		cfg.LockDuration = 15 * time.Minute
	}
	return &RedisLoginProtection{
		rdb:          rdb,
		maxAttempts:  cfg.MaxAttempts,
		lockDuration: cfg.LockDuration,
	}
}

// CheckLoginAllowed returns nil if login is allowed for the given IP and email.
func (lp *RedisLoginProtection) CheckLoginAllowed(ctx context.Context, ip, email string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Check IP lockout
	ipKey := fmt.Sprintf("admin:login:lock:ip:%s", ip)
	ipLocked, ipTTLErr := lp.isKeyLocked(ctx, ipKey)
	if ipTTLErr != nil {
		// Fail-open: allow login if Redis is unavailable (with warning)
		logger.Warn(ctx, "redis_login_protection_check_ip_error",
			zap.String("ip", ip),
			zap.Error(ipTTLErr))
	}
	if ipLocked {
		ttl, _ := lp.rdb.TTL(ctx, ipKey).Result()
		return fmt.Errorf("IP locked, please retry in %d seconds", int(ttl.Seconds()))
	}

	// Check email lockout
	emailKey := fmt.Sprintf("admin:login:lock:email:%s", email)
	emailLocked, emailTTLErr := lp.isKeyLocked(ctx, emailKey)
	if emailTTLErr != nil {
		logger.Warn(ctx, "redis_login_protection_check_email_error",
			zap.String("email", email),
			zap.Error(emailTTLErr))
	}
	if emailLocked {
		ttl, _ := lp.rdb.TTL(ctx, emailKey).Result()
		return fmt.Errorf("account locked, please retry in %d seconds", int(ttl.Seconds()))
	}

	return nil
}

// isKeyLocked checks if a Redis key exists and has a positive TTL (i.e., is actively locked).
func (lp *RedisLoginProtection) isKeyLocked(ctx context.Context, key string) (bool, error) {
	ttl, err := lp.rdb.TTL(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return ttl > 0, nil
}

// RecordFailedLogin records a failed login attempt for the given IP and email.
func (lp *RedisLoginProtection) RecordFailedLogin(ctx context.Context, ip, email string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	lockDurationSec := int(lp.lockDuration.Seconds())

	// Record IP failure using atomic Lua script
	ipAttemptsKey := fmt.Sprintf("admin:login:attempts:ip:%s", ip)
	ipCount, err := loginAttemptScript.Run(ctx, lp.rdb, []string{ipAttemptsKey}, lockDurationSec).Int()
	if err != nil {
		logger.Error(ctx, "redis_login_protection_record_ip_failed",
			zap.String("ip", ip),
			zap.Error(err))
		return
	}
	if ipCount >= lp.maxAttempts {
		ipLockKey := fmt.Sprintf("admin:login:lock:ip:%s", ip)
		if err := lp.rdb.Set(ctx, ipLockKey, "1", lp.lockDuration).Err(); err != nil {
			logger.Error(ctx, "redis_login_protection_lock_ip_failed",
				zap.String("ip", ip),
				zap.Int("attempts", ipCount),
				zap.Error(err))
		}
	}

	// Record email failure using atomic Lua script
	emailAttemptsKey := fmt.Sprintf("admin:login:attempts:email:%s", email)
	emailCount, err := loginAttemptScript.Run(ctx, lp.rdb, []string{emailAttemptsKey}, lockDurationSec).Int()
	if err != nil {
		logger.Error(ctx, "redis_login_protection_record_email_failed",
			zap.String("email", email),
			zap.Error(err))
		return
	}
	if emailCount >= lp.maxAttempts {
		emailLockKey := fmt.Sprintf("admin:login:lock:email:%s", email)
		if err := lp.rdb.Set(ctx, emailLockKey, "1", lp.lockDuration).Err(); err != nil {
			logger.Error(ctx, "redis_login_protection_lock_email_failed",
				zap.String("email", email),
				zap.Int("attempts", emailCount),
				zap.Error(err))
		}
	}
}

// ResetLoginAttempts clears the failed login counters for the given IP and email.
func (lp *RedisLoginProtection) ResetLoginAttempts(ctx context.Context, ip, email string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	pipe := lp.rdb.Pipeline()
	pipe.Del(ctx, fmt.Sprintf("admin:login:attempts:ip:%s", ip))
	pipe.Del(ctx, fmt.Sprintf("admin:login:attempts:email:%s", email))
	pipe.Del(ctx, fmt.Sprintf("admin:login:lock:ip:%s", ip))
	pipe.Del(ctx, fmt.Sprintf("admin:login:lock:email:%s", email))
	if _, err := pipe.Exec(ctx); err != nil {
		logger.Debug(ctx, "redis_login_protection_reset_failed", zap.Error(err))
	}
}

// Stop is a no-op for Redis login protection (no background goroutines).
func (lp *RedisLoginProtection) Stop() {}
