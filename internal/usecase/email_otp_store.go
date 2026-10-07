// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"errors"
	"sync"
	"time"
)

// OTPStore defines the interface for OTP storage and rate limiting.
// Both in-memory and Redis-backed implementations satisfy this interface.
type OTPStore interface {
	// Store stores an OTP for the given email with TTL.
	Store(ctx context.Context, emailAddr, otp string, ttl time.Duration) error
	// Get retrieves the stored OTP for the given email.
	// Returns ErrOTPNotFound if not found or expired.
	Get(ctx context.Context, emailAddr string) (string, error)
	// Delete removes the stored OTP for the given email.
	Delete(ctx context.Context, emailAddr string)
	// CheckSendAllowed returns nil if sending OTP is allowed for the email and IP.
	CheckSendAllowed(ctx context.Context, emailAddr, clientIP string) error
	// RecordSend records an OTP send event for rate limiting.
	RecordSend(ctx context.Context, emailAddr, clientIP string)
	// CheckVerifyAllowed returns nil if verification is allowed for the email.
	CheckVerifyAllowed(ctx context.Context, emailAddr string) error
	// RecordVerifyFailure records a failed verification attempt.
	RecordVerifyFailure(ctx context.Context, emailAddr string)
	// ResetVerifyFailures resets the failure counter for the email.
	ResetVerifyFailures(ctx context.Context, emailAddr string)
}

// MemoryOTPStore implements OTPStore using in-memory storage.
// Suitable for single-instance deployments.
type MemoryOTPStore struct {
	mu sync.Mutex
	// OTP storage: email -> {code, expiresAt}
	otps map[string]otpEntry
	// Send cooldown: email -> lastSendTime
	sendCooldown map[string]time.Time
	// Send rate limit: IP -> count, resetTime
	sendRateIP map[string]rateEntry
	// Verify failures: email -> count
	verifyFailures map[string]int
	// Verify lockout: email -> lockedUntil
	verifyLockout map[string]time.Time
	// Config
	maxSendPerIP      int
	maxVerifyAttempts int
	lockDuration      time.Duration
	sendCooldownDur   time.Duration
}

type otpEntry struct {
	code      string
	expiresAt time.Time
}

type rateEntry struct {
	count     int
	resetTime time.Time
}

// NewMemoryOTPStore creates a new MemoryOTPStore.
func NewMemoryOTPStore(maxSendPerIP, maxVerifyAttempts int, lockDuration, sendCooldown time.Duration) *MemoryOTPStore {
	return &MemoryOTPStore{
		otps:              make(map[string]otpEntry),
		sendCooldown:      make(map[string]time.Time),
		sendRateIP:        make(map[string]rateEntry),
		verifyFailures:    make(map[string]int),
		verifyLockout:     make(map[string]time.Time),
		maxSendPerIP:      maxSendPerIP,
		maxVerifyAttempts: maxVerifyAttempts,
		lockDuration:      lockDuration,
		sendCooldownDur:   sendCooldown,
	}
}

// Store stores an OTP for the given email with TTL.
func (s *MemoryOTPStore) Store(ctx context.Context, emailAddr, otp string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.otps[emailAddr] = otpEntry{
		code:      otp,
		expiresAt: time.Now().Add(ttl),
	}
	return nil
}

// Get retrieves the stored OTP for the given email.
// Returns ErrOTPNotFound if not found or expired.
func (s *MemoryOTPStore) Get(ctx context.Context, emailAddr string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.otps[emailAddr]
	if !ok || time.Now().After(entry.expiresAt) {
		return "", ErrOTPNotFound
	}
	return entry.code, nil
}

// Delete removes the stored OTP for the given email.
func (s *MemoryOTPStore) Delete(ctx context.Context, emailAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.otps, emailAddr)
}

// CheckSendAllowed returns nil if sending OTP is allowed for the email and IP.
func (s *MemoryOTPStore) CheckSendAllowed(ctx context.Context, emailAddr, clientIP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	// Check email cooldown
	if lastSend, ok := s.sendCooldown[emailAddr]; ok && now.Before(lastSend.Add(s.sendCooldownDur)) {
		return errors.New("please wait before requesting another code")
	}

	// Check IP rate limit
	if entry, ok := s.sendRateIP[clientIP]; ok {
		if now.Before(entry.resetTime) {
			if entry.count >= s.maxSendPerIP {
				return errors.New("too many requests from this IP")
			}
		}
	}

	return nil
}

// RecordSend records an OTP send event for rate limiting.
func (s *MemoryOTPStore) RecordSend(ctx context.Context, emailAddr, clientIP string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	// Record email cooldown
	s.sendCooldown[emailAddr] = now

	// Record IP rate limit
	entry, ok := s.sendRateIP[clientIP]
	if !ok || now.After(entry.resetTime) {
		s.sendRateIP[clientIP] = rateEntry{count: 1, resetTime: now.Add(time.Minute)}
	} else {
		entry.count++
		s.sendRateIP[clientIP] = entry
	}
}

// CheckVerifyAllowed returns nil if verification is allowed for the email.
func (s *MemoryOTPStore) CheckVerifyAllowed(ctx context.Context, emailAddr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lockedUntil, ok := s.verifyLockout[emailAddr]; ok && time.Now().Before(lockedUntil) {
		return errors.New("verification temporarily locked")
	}
	return nil
}

// RecordVerifyFailure records a failed verification attempt.
func (s *MemoryOTPStore) RecordVerifyFailure(ctx context.Context, emailAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verifyFailures[emailAddr]++
	if s.verifyFailures[emailAddr] >= s.maxVerifyAttempts {
		s.verifyLockout[emailAddr] = time.Now().Add(s.lockDuration)
		s.verifyFailures[emailAddr] = 0
	}
}

// ResetVerifyFailures resets the failure counter for the email.
func (s *MemoryOTPStore) ResetVerifyFailures(ctx context.Context, emailAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.verifyFailures, emailAddr)
	delete(s.verifyLockout, emailAddr)
}
