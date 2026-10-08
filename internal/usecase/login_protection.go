// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// LoginProtectionInterface defines the contract for login brute-force protection.
// Both in-memory and Redis-backed implementations satisfy this interface.
type LoginProtectionInterface interface {
	CheckLoginAllowed(ctx context.Context, ip, email string) error
	RecordFailedLogin(ctx context.Context, ip, email string)
	ResetLoginAttempts(ctx context.Context, ip, email string)
	Stop()
}

// LoginProtectionConfig configures login lockout behavior.
type LoginProtectionConfig struct {
	// MaxAttempts is the maximum number of failed login attempts before lockout.
	MaxAttempts int
	// LockDuration is how long the lockout lasts.
	LockDuration time.Duration
}

// LoginProtection implements IP + email dual lockout for brute-force prevention.
//
// When either the IP address or the email address exceeds the maximum number of
// failed login attempts, further login attempts are rejected until the lockout
// expires. Both dimensions are tracked independently — locking an IP does not
// lock an email, and vice versa.
type LoginProtection struct {
	mu              sync.Mutex
	attempts        map[string]int       // key: "ip:<addr>" or "email:<addr>"
	lastAttemptTime map[string]time.Time // key: same as attempts; tracks last activity
	lockedUntil     map[string]time.Time
	maxAttempts     int
	lockDuration    time.Duration
	stopCh          chan struct{} // signals the cleanup goroutine to stop
}

// NewLoginProtection creates a new LoginProtection instance.
func NewLoginProtection(cfg LoginProtectionConfig) *LoginProtection {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.LockDuration <= 0 {
		cfg.LockDuration = 15 * time.Minute
	}
	lp := &LoginProtection{
		attempts:        make(map[string]int),
		lastAttemptTime: make(map[string]time.Time),
		lockedUntil:     make(map[string]time.Time),
		maxAttempts:     cfg.MaxAttempts,
		lockDuration:    cfg.LockDuration,
		stopCh:          make(chan struct{}),
	}
	go lp.cleanupLoop()
	return lp
}

// CheckLoginAllowed returns nil if login is allowed for the given IP and email,
// or an error describing which dimension is locked out.
func (lp *LoginProtection) CheckLoginAllowed(_ context.Context, ip, email string) error {
	lp.mu.Lock()
	defer lp.mu.Unlock()

	now := time.Now()

	// Check IP lockout
	if lockUntil, ok := lp.lockedUntil["ip:"+ip]; ok && now.Before(lockUntil) {
		retryAfter := lockUntil.Sub(now)
		return fmt.Errorf("IP 已被锁定，请在 %s 后重试", formatDuration(retryAfter))
	}

	// Check email lockout
	if lockUntil, ok := lp.lockedUntil["email:"+email]; ok && now.Before(lockUntil) {
		retryAfter := lockUntil.Sub(now)
		return fmt.Errorf("该账号已被锁定，请在 %s 后重试", formatDuration(retryAfter))
	}

	return nil
}

// RecordFailedLogin records a failed login attempt for the given IP and email.
// If either dimension exceeds the max attempts threshold, it is locked.
func (lp *LoginProtection) RecordFailedLogin(_ context.Context, ip, email string) {
	lp.mu.Lock()
	defer lp.mu.Unlock()

	now := time.Now()

	// Record IP failure
	ipKey := "ip:" + ip
	lp.attempts[ipKey]++
	lp.lastAttemptTime[ipKey] = now
	if lp.attempts[ipKey] >= lp.maxAttempts {
		lp.lockedUntil[ipKey] = now.Add(lp.lockDuration)
	}

	// Record email failure
	emailKey := "email:" + email
	lp.attempts[emailKey]++
	lp.lastAttemptTime[emailKey] = now
	if lp.attempts[emailKey] >= lp.maxAttempts {
		lp.lockedUntil[emailKey] = now.Add(lp.lockDuration)
	}
}

// ResetLoginAttempts clears the failed login counters for the given IP and email.
// Called after a successful login.
func (lp *LoginProtection) ResetLoginAttempts(_ context.Context, ip, email string) {
	lp.mu.Lock()
	defer lp.mu.Unlock()

	delete(lp.attempts, "ip:"+ip)
	delete(lp.attempts, "email:"+email)
	delete(lp.lockedUntil, "ip:"+ip)
	delete(lp.lockedUntil, "email:"+email)
	delete(lp.lastAttemptTime, "ip:"+ip)
	delete(lp.lastAttemptTime, "email:"+email)
}

// Stop stops the cleanup goroutine.
func (lp *LoginProtection) Stop() {
	select {
	case <-lp.stopCh:
		// already stopped
	default:
		close(lp.stopCh)
	}
}

// cleanupLoop periodically removes expired entries from the attempts and lockedUntil maps
// to prevent memory leaks in long-running processes.
func (lp *LoginProtection) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			lp.mu.Lock()
			now := time.Now()
			for key, until := range lp.lockedUntil {
				if now.After(until) {
					delete(lp.lockedUntil, key)
					delete(lp.attempts, key)
					delete(lp.lastAttemptTime, key)
				}
			}
			// Also clean up stale attempt counters that haven't been updated
			// in 30 minutes (prevents unbounded map growth from one-off attempts).
			for key, lastAttempt := range lp.lastAttemptTime {
				if now.Sub(lastAttempt) > 30*time.Minute {
					delete(lp.attempts, key)
					delete(lp.lastAttemptTime, key)
				}
			}
			lp.mu.Unlock()
		case <-lp.stopCh:
			return
		}
	}
}

// formatDuration formats a duration for human-readable display.
func formatDuration(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()+0.5))
	}
	return fmt.Sprintf("%d 秒", int(d.Seconds()+0.5))
}
