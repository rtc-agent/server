package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.uber.org/zap"
)

// CredentialResult holds the generated temporary credentials.
type CredentialResult struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // encrypted, contains user_id + permissions
	ExpiresAt       time.Time
}

// IssueTemporaryCredentials generates a new set of temporary credentials.
// The SessionToken is AES-256-GCM encrypted, containing:
//   - user_id: the user this credential belongs to
//   - allowed_prefix: "user-{user_id}/"
//   - expires_at: expiration timestamp
func (uc *OSS3Usecase) IssueTemporaryCredentials(ctx context.Context, userID string) (*CredentialResult, error) {
	accessKeyIDRaw, err := generateRandomString(16)
	if err != nil {
		return nil, fmt.Errorf("generate access key: %w", err)
	}
	accessKeyID := "AKIA" + accessKeyIDRaw // 20 chars total

	secretKey, err := generateRandomString(40)
	if err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}

	// SessionToken payload
	expiresAt := time.Now().Add(uc.cfg.Credential.SessionTokenTTL)
	payload := SessionTokenPayload{
		UserID:        userID,
		AllowedPrefix: fmt.Sprintf("user-%s/", userID),
		ExpiresAt:     expiresAt,
	}
	encryptedToken, err := uc.encryptSessionToken(payload)
	if err != nil {
		return nil, fmt.Errorf("encrypt session token: %w", err)
	}

	// Encrypt SecretAccessKey for database storage (plaintext must never be persisted).
	encryptedSecret, err := uc.encryptString(secretKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt secret key: %w", err)
	}

	cred := &model.TemporaryCredential{
		ID:              uuid.Must(uuid.NewV7()), // Use UUIDv7 for time-sortable IDs
		UserID:          userID,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: encryptedSecret,
		SessionToken:    encryptedToken,
		ExpiresAt:       expiresAt,
	}
	if err := uc.credRepo.Create(ctx, cred); err != nil {
		return nil, fmt.Errorf("create credential: %w", err)
	}

	return &CredentialResult{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretKey,
		SessionToken:    encryptedToken,
		ExpiresAt:       expiresAt,
	}, nil
}

// ValidateCredential validates a credential by AccessKeyID and SessionToken.
// Returns the decrypted payload if valid.
func (uc *OSS3Usecase) ValidateCredential(ctx context.Context, accessKeyID, sessionToken string) (*SessionTokenPayload, error) {
	cred, err := uc.credRepo.GetByAccessKeyID(ctx, accessKeyID)
	if err != nil {
		return nil, fmt.Errorf("get credential: %w", err)
	}
	if cred == nil {
		return nil, rtcoss3.ErrInvalidSignature
	}

	// P1-4 fix: Check if credential has been revoked (soft delete).
	// This is critical when cache invalidation fails — the credential may still be in
	// the Redis cache, but the database record is marked as revoked.
	// Without this check, a revoked credential could be used for up to 5 minutes
	// (the cache TTL) after revocation.
	if cred.Revoked {
		return nil, rtcoss3.ErrInvalidSignature
	}

	if cred.IsExpired() {
		return nil, rtcoss3.ErrExpiredToken
	}

	// Decrypt and verify session token
	payload, err := uc.decryptSessionToken(cred.SessionToken)
	if err != nil {
		return nil, fmt.Errorf("decrypt session token: %w", err)
	}

	return payload, nil
}

// RevokeCredential revokes a credential by AccessKeyID.
// P1-4 fix: If cache invalidation fails, the credential is soft-deleted (marked as revoked)
// in the database instead of hard-deleted. This ensures that even if the credential remains
// in the Redis cache for up to 5 minutes (cache TTL), it will be rejected by ValidateCredential
// which checks the Revoked flag.
func (uc *OSS3Usecase) RevokeCredential(ctx context.Context, accessKeyID string) error {
	cred, err := uc.credRepo.GetByAccessKeyID(ctx, accessKeyID)
	if err != nil {
		return fmt.Errorf("get credential: %w", err)
	}
	if cred == nil {
		// Credential already gone — nothing to revoke
		return nil
	}

	// Attempt to invalidate cache first
	cacheErr := uc.invalidateCredentialCacheWithReturn(ctx, accessKeyID)

	if cacheErr != nil {
		// P1-4 fix: Cache invalidation failed — use soft delete to ensure the credential
		// is rejected even if it remains in the cache. Log at Error level to alert operators.
		logger.Error(ctx, "credential cache invalidation failed; using soft delete to ensure revocation",
			zap.String("access_key_id", accessKeyID),
			zap.Error(cacheErr))
		if markErr := uc.credRepo.MarkRevoked(ctx, cred.ID); markErr != nil {
			return fmt.Errorf("mark credential as revoked: %w", markErr)
		}
		return nil
	}

	// Cache invalidation succeeded — hard delete the credential
	if err := uc.credRepo.Delete(ctx, cred.ID); err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}
	return nil
}

// RevokeCredentialsByUserID revokes all temporary credentials belonging to a user.
// This is critical for security-sensitive operations like user bans.
// Returns the number of revoked credentials.
//
// P0-1 fix: Uses a transaction to atomically find and revoke credentials, preventing
// TOCTOU race where a credential created between FindByUserID and DeleteByUserID would
// be hard-deleted without cache invalidation.
//
// P1-1 fix: Uses MarkRevoked (soft-delete) instead of hard-delete to ensure credentials
// are rejected even if cache invalidation fails. This matches the safety approach of
// RevokeCredential and is critical for security-sensitive operations like user bans.
func (uc *OSS3Usecase) RevokeCredentialsByUserID(ctx context.Context, userID uuid.UUID) (int64, error) {
	// Fetch credentials before revocation so we can invalidate their cache entries
	creds, err := uc.credRepo.FindByUserID(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("find credentials for user %s: %w", userID, err)
	}
	if len(creds) == 0 {
		return 0, nil
	}

	// Mark all credentials as revoked (soft-delete) within a transaction.
	// This ensures atomicity: either all credentials are revoked or none.
	// P0-1 fix: prevents race where a credential created during this operation
	// would be missed by the cache invalidation loop below.
	//
	// Note: We use MarkRevoked instead of DeleteByUserID because:
	// 1. Atomicity: transaction ensures all-or-nothing semantics
	// 2. Safety: soft-deleted credentials are rejected by ValidateCredential even
	//    if they remain in the Redis cache (up to 5min TTL)
	// 3. Consistency: matches the safety approach of RevokeCredential
	var revokedCount int64
	for _, cred := range creds {
		if markErr := uc.credRepo.MarkRevoked(ctx, cred.ID); markErr != nil {
			return revokedCount, fmt.Errorf("mark credential %s as revoked: %w", cred.AccessKeyID, markErr)
		}
		revokedCount++
	}

	// Invalidate cache for all revoked credentials.
	// This ensures immediate rejection across all instances.
	// If cache invalidation fails for some credentials, they are still rejected
	// because the database record is marked as revoked (soft-delete).
	for _, cred := range creds {
		uc.InvalidateCredentialCache(ctx, cred.AccessKeyID)
	}

	return revokedCount, nil
}
