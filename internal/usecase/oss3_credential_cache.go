package usecase

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rtc-agent/server/internal/infra/cache"
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
// Cache-aside pattern:
//  1. Check Redis cache (oss3:cred_cache:{accessKeyID})
//  2. On miss: query DB, populate cache with TTL = min(remaining, 5m)
//  3. On hit: verify not expired (defensive; Redis TTL should handle this)
//
// Returns nil, nil if credential not found (caller treats as invalid signature).
func (uc *OSS3Usecase) LookupCredential(ctx context.Context, accessKeyID string) (*credentialCacheValue, error) {
	cacheKey := cache.OSS3CredentialCache(accessKeyID)

	// Step 1: Try cache
	cached, err := uc.redis.Get(ctx, cacheKey).Result()
	if err == nil {
		var val credentialCacheValue
		if json.Unmarshal([]byte(cached), &val) == nil {
			// Defensive expiry check
			if val.ExpiresAt.After(time.Now()) {
				return &val, nil
			}
			// Cached but expired — fall through to DB
		}
	}
	// Redis error or cache miss — fall through to DB

	// Step 2: Query DB
	cred, err := uc.credRepo.GetByAccessKeyID(ctx, accessKeyID)
	if err != nil {
		// Credential not found — return nil, nil (caller treats as invalid)
		return nil, nil
	}

	// Step 3: Populate cache
	remaining := time.Until(cred.ExpiresAt)
	if remaining <= 0 {
		// Already expired in DB
		return nil, nil
	}

	ttl := remaining
	if ttl > maxCredentialCacheTTL {
		ttl = maxCredentialCacheTTL
	}

	val := &credentialCacheValue{
		UserID:          cred.UserID,
		SecretAccessKey: cred.SecretAccessKey,
		SessionToken:    cred.SessionToken,
		ExpiresAt:       cred.ExpiresAt,
	}

	data, err := json.Marshal(val)
	if err == nil {
		_ = uc.redis.Set(ctx, cacheKey, data, ttl).Err()
		// best-effort; cache miss on next read will repopulate
	}

	return val, nil
}

// InvalidateCredentialCache removes a credential from the cache.
// Called when credential is revoked or deleted.
func (uc *OSS3Usecase) InvalidateCredentialCache(ctx context.Context, accessKeyID string) {
	cacheKey := cache.OSS3CredentialCache(accessKeyID)
	_ = uc.redis.Del(ctx, cacheKey).Err()
	// best-effort
}
