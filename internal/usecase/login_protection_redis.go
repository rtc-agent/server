// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

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
func (lp *RedisLoginProtection) CheckLoginAllowed(ip, email string) error {
	ctx := context.Background()

	// Check IP lockout
	ipKey := fmt.Sprintf("admin:login:lock:ip:%s", ip)
	if ttl, err := lp.rdb.TTL(ctx, ipKey).Result(); err == nil && ttl > 0 {
		return fmt.Errorf("IP 已被锁定，请在 %d 秒后重试", int(ttl.Seconds()))
	}

	// Check email lockout
	emailKey := fmt.Sprintf("admin:login:lock:email:%s", email)
	if ttl, err := lp.rdb.TTL(ctx, emailKey).Result(); err == nil && ttl > 0 {
		return fmt.Errorf("该账号已被锁定，请在 %d 秒后重试", int(ttl.Seconds()))
	}

	return nil
}

// RecordFailedLogin records a failed login attempt for the given IP and email.
func (lp *RedisLoginProtection) RecordFailedLogin(ip, email string) {
	ctx := context.Background()

	// Record IP failure
	ipAttemptsKey := fmt.Sprintf("admin:login:attempts:ip:%s", ip)
	ipCount, _ := lp.rdb.Incr(ctx, ipAttemptsKey).Result()
	if ipCount == 1 {
		lp.rdb.Expire(ctx, ipAttemptsKey, lp.lockDuration)
	}
	if ipCount >= int64(lp.maxAttempts) {
		ipLockKey := fmt.Sprintf("admin:login:lock:ip:%s", ip)
		lp.rdb.Set(ctx, ipLockKey, "1", lp.lockDuration)
	}

	// Record email failure
	emailAttemptsKey := fmt.Sprintf("admin:login:attempts:email:%s", email)
	emailCount, _ := lp.rdb.Incr(ctx, emailAttemptsKey).Result()
	if emailCount == 1 {
		lp.rdb.Expire(ctx, emailAttemptsKey, lp.lockDuration)
	}
	if emailCount >= int64(lp.maxAttempts) {
		emailLockKey := fmt.Sprintf("admin:login:lock:email:%s", email)
		lp.rdb.Set(ctx, emailLockKey, "1", lp.lockDuration)
	}
}

// ResetLoginAttempts clears the failed login counters for the given IP and email.
func (lp *RedisLoginProtection) ResetLoginAttempts(ip, email string) {
	ctx := context.Background()
	pipe := lp.rdb.Pipeline()
	pipe.Del(ctx, fmt.Sprintf("admin:login:attempts:ip:%s", ip))
	pipe.Del(ctx, fmt.Sprintf("admin:login:attempts:email:%s", email))
	pipe.Del(ctx, fmt.Sprintf("admin:login:lock:ip:%s", ip))
	pipe.Del(ctx, fmt.Sprintf("admin:login:lock:email:%s", email))
	_, _ = pipe.Exec(ctx)
}
