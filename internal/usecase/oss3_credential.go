package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
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
	accessKeyID := "AKIA" + generateRandomString(16) // 20 chars total
	secretKey := generateRandomString(40)            // 40 chars

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

	cred := &model.TemporaryCredential{
		ID:              uuid.New(),
		UserID:          userID,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretKey,
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

	if cred.IsExpired() {
		return nil, rtcoss3.ErrExpiredToken
	}

	if cred.IsRevoked() {
		return nil, rtcoss3.ErrAccessDenied
	}

	// Decrypt and verify session token
	payload, err := uc.decryptSessionToken(cred.SessionToken)
	if err != nil {
		return nil, fmt.Errorf("decrypt session token: %w", err)
	}

	return payload, nil
}

// RevokeCredential revokes a credential by AccessKeyID.
func (uc *OSS3Usecase) RevokeCredential(ctx context.Context, accessKeyID string) error {
	cred, err := uc.credRepo.GetByAccessKeyID(ctx, accessKeyID)
	if err != nil {
		return fmt.Errorf("get credential: %w", err)
	}

	now := time.Now()
	cred.RevokedAt = &now
	if err := uc.credRepo.Delete(ctx, cred.ID); err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}

	// Invalidate cache so revoked credential is immediately rejected
	uc.InvalidateCredentialCache(ctx, accessKeyID)
	return nil
}
