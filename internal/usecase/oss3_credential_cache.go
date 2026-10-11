package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// credentialCacheValue is the JSON structure stored in Redis cache.
type credentialCacheValue struct {
	UserID          string    `json:"user_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	SessionToken    string    `json:"session_token"` // encrypted
	ExpiresAt       time.Time `json:"expires_at"`
}

// maxCredentialCacheTTL is the upper bound for cache TTL.
// Even if the credential has longer remaining lifetime, we cap at 5 minutes
// to limit the window of serving stale data after credential revocation.
const maxCredentialCacheTTL = 5 * time.Minute

// LookupCredential retrieves a credential by AccessKeyID.
// Cache-aside pattern with singleflight dedup:
//  1. Check Redis cache (oss3:cred_cache:{accessKeyID})
//  2. On miss: use singleflight to deduplicate concurrent DB queries
//  3. On hit: verify not expired (defensive; Redis TTL should handle this)
//
// Returns nil, nil if credential not found (caller treats as invalid signature).
func (uc *OSS3Usecase) LookupCredential(ctx context.Context, accessKeyID string) (*credentialCacheValue, error) {
	cacheKey := cache.OSS3CredentialCache(accessKeyID)

	// Step 1: Try cache
	if cached, err := uc.redis.Get(ctx, cacheKey).Result(); err == nil {
		if val, decryptErr := uc.getCredentialFromCache(ctx, cacheKey, cached, accessKeyID); decryptErr == nil {
			return val, nil
		}
	}

	// Step 2: Query DB (deduplicated via singleflight to prevent cache stampede).
	cred, err := uc.queryCredentialFromDB(ctx, accessKeyID)
	if err != nil {
		return nil, err
	}
	if cred == nil {
		return nil, nil
	}

	// Step 3: Populate cache
	return uc.populateCredentialCache(ctx, cacheKey, cred, accessKeyID)
}

// getCredentialFromCache attempts to retrieve and decrypt credential from cache.
// Returns the credential value on success, or nil on any failure (expired, decryption error, etc.).
func (uc *OSS3Usecase) getCredentialFromCache(ctx context.Context, cacheKey, cached, accessKeyID string) (*credentialCacheValue, error) {
	var val credentialCacheValue
	if json.Unmarshal([]byte(cached), &val) != nil {
		return nil, fmt.Errorf("unmarshal cache failed")
	}

	// Defensive expiry check
	if !val.ExpiresAt.After(time.Now()) {
		return nil, fmt.Errorf("credential expired")
	}

	// Decrypt SecretAccessKey
	decryptedSecret, err := uc.decryptString(val.SecretAccessKey)
	if err != nil {
		// Decryption failed - invalidate cache and fall through to DB
		logger.Debug(ctx, "credential cache decryption failed, invalidating cache",
			zap.String("access_key_id", accessKeyID),
			zap.Error(err))
		if delErr := uc.redis.Del(ctx, cacheKey).Err(); delErr != nil {
			logger.Warn(ctx, "failed to invalidate corrupted credential cache",
				zap.String("access_key_id", accessKeyID),
				zap.Error(delErr))
		}
		return nil, fmt.Errorf("decrypt cache failed: %w", err)
	}

	val.SecretAccessKey = decryptedSecret
	return &val, nil
}

// queryCredentialFromDB queries credential from database with singleflight deduplication.
// Returns nil, nil if credential not found. Returns nil, error on DB errors.
func (uc *OSS3Usecase) queryCredentialFromDB(ctx context.Context, accessKeyID string) (*model.TemporaryCredential, error) {
	// Use DoChan + background context so the DB query is decoupled from any
	// single request's lifecycle. When the first (leader) request is cancelled,
	// the DB query continues uninterrupted; other waiters still receive the
	// result. Each caller independently decides whether to keep waiting via
	// its own context.
	ch := uc.credFlight.DoChan(accessKeyID, func() (interface{}, error) {
		// Background context: immune to any caller's cancellation.
		return uc.credRepo.GetByAccessKeyID(context.Background(), accessKeyID)
	})

	var result interface{}
	var err error
	select {
	case <-ctx.Done():
		// This caller gave up, but the DB query continues for other waiters.
		return nil, ctx.Err()
	case r := <-ch:
		result, err = r.Val, r.Err
	}

	if err != nil {
		// Distinguish "not found" from actual DB errors.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup credential %q: %w", accessKeyID, err)
	}

	cred, ok := result.(*model.TemporaryCredential)
	if !ok || cred == nil {
		return nil, nil
	}

	return cred, nil
}

// populateCredentialCache validates credential expiry, decrypts secret, and populates cache.
// Returns the credential value on success.
func (uc *OSS3Usecase) populateCredentialCache(
	ctx context.Context,
	cacheKey string,
	cred *model.TemporaryCredential,
	accessKeyID string,
) (*credentialCacheValue, error) {
	// Check if already expired in DB
	remaining := time.Until(cred.ExpiresAt)
	if remaining <= 0 {
		return nil, nil
	}

	// Decrypt SecretAccessKey from DB (stored encrypted).
	decryptedSecret, err := uc.decryptString(cred.SecretAccessKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret key: %w", err)
	}

	// Return value carries the decrypted secret key.
	val := &credentialCacheValue{
		UserID:          cred.UserID,
		SessionToken:    cred.SessionToken,
		ExpiresAt:       cred.ExpiresAt,
		SecretAccessKey: decryptedSecret,
	}

	// Encrypt SecretAccessKey before caching (Redis stores encrypted form).
	encryptedSecret, err := uc.encryptString(decryptedSecret)
	if err != nil {
		return nil, fmt.Errorf("encrypt secret key: %w", err)
	}

	cacheVal := *val // shallow copy common fields
	cacheVal.SecretAccessKey = encryptedSecret

	// Calculate TTL (cap at maxCredentialCacheTTL)
	ttl := remaining
	if ttl > maxCredentialCacheTTL {
		ttl = maxCredentialCacheTTL
	}

	data, err := json.Marshal(cacheVal)
	if err == nil {
		if setErr := uc.redis.Set(ctx, cacheKey, data, ttl).Err(); setErr != nil {
			logger.Warn(ctx, "credential_cache_set_failed",
				zap.String("access_key_id", accessKeyID),
				zap.Duration("ttl", ttl),
				zap.Int("data_size_bytes", len(data)),
				zap.Error(setErr))
		}
		// best-effort; cache miss on next read will repopulate
	}

	return val, nil
}

// InvalidateCredentialCache removes a credential from the cache.
// Called when credential is revoked or deleted.
// Best-effort: logs warning on failure but does not return error to caller.
func (uc *OSS3Usecase) InvalidateCredentialCache(ctx context.Context, accessKeyID string) {
	cacheKey := cache.OSS3CredentialCache(accessKeyID)
	if err := uc.redis.Del(ctx, cacheKey).Err(); err != nil {
		// Log at Warn level so persistent Redis issues are visible in monitoring.
		// The cached credential will eventually expire via TTL, but operators should
		// be aware of ongoing failures that may delay invalidation.
		logger.Warn(ctx, "credential_cache_invalidation_failed",
			zap.String("cache_key", cacheKey),
			zap.String("access_key_id", accessKeyID),
			zap.Error(err))
	}
}

// invalidateCredentialCacheWithReturn is like InvalidateCredentialCache but returns the error.
// P1-4 fix: Used by RevokeCredential to detect cache invalidation failures and switch to
// soft-delete mode. This ensures revoked credentials are rejected even if the cache still
// contains them.
func (uc *OSS3Usecase) invalidateCredentialCacheWithReturn(ctx context.Context, accessKeyID string) error {
	cacheKey := cache.OSS3CredentialCache(accessKeyID)
	if err := uc.redis.Del(ctx, cacheKey).Err(); err != nil {
		return fmt.Errorf("invalidate credential cache for %s: %w", accessKeyID, err)
	}
	return nil
}
