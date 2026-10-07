// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// loginAttemptScript atomically increments an attempt counter and sets its TTL on first increment.
// This prevents the race condition where INCR succeeds but EXPIRE is lost.
var loginAttemptScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
    redis.call('EXPIRE', KEYS[1], ARGV[1])
end
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
func (lp *RedisLoginProtection) RecordFailedLogin(ctx context.Context, ip, email string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	lockDurationSec := int(lp.lockDuration.Seconds())

	// Record IP failure using atomic Lua script
	ipAttemptsKey := fmt.Sprintf("admin:login:attempts:ip:%s", ip)
	ipCount, _ := loginAttemptScript.Run(ctx, lp.rdb, []string{ipAttemptsKey}, lockDurationSec).Int()
	if ipCount >= lp.maxAttempts {
		ipLockKey := fmt.Sprintf("admin:login:lock:ip:%s", ip)
		lp.rdb.Set(ctx, ipLockKey, "1", lp.lockDuration)
	}

	// Record email failure using atomic Lua script
	emailAttemptsKey := fmt.Sprintf("admin:login:attempts:email:%s", email)
	emailCount, _ := loginAttemptScript.Run(ctx, lp.rdb, []string{emailAttemptsKey}, lockDurationSec).Int()
	if emailCount >= lp.maxAttempts {
		emailLockKey := fmt.Sprintf("admin:login:lock:email:%s", email)
		lp.rdb.Set(ctx, emailLockKey, "1", lp.lockDuration)
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
	_, _ = pipe.Exec(ctx)
}

// Stop is a no-op for Redis login protection (no background goroutines).
func (lp *RedisLoginProtection) Stop() {}
