// internal/usecase/auth.go
package usecase

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/agent/stringutil"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/oauth"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// TokenSigner is the JWT signing interface.
type TokenSigner interface {
	SignAccessToken(userID uuid.UUID, deviceID string) (token string, expiresAt time.Time, err error)
	AccessTTL() time.Duration
}

// AuthUsecase handles authentication and authorization operations.
type AuthUsecase struct {
	db               *gorm.DB
	oauth2UserRepo   repo.OAuth2UserRepo
	deviceRepo       repo.DeviceRepo
	refreshTokenRepo repo.RefreshTokenRepo
	signer           TokenSigner
	refreshTokenTTL  time.Duration
}

// NewAuthUsecase creates a new AuthUsecase.
func NewAuthUsecase(
	db *gorm.DB,
	oauth2UserRepo repo.OAuth2UserRepo,
	deviceRepo repo.DeviceRepo,
	refreshTokenRepo repo.RefreshTokenRepo,
	signer TokenSigner,
	refreshTokenTTL time.Duration,
) *AuthUsecase {
	return &AuthUsecase{
		db:               db,
		oauth2UserRepo:   oauth2UserRepo,
		deviceRepo:       deviceRepo,
		refreshTokenRepo: refreshTokenRepo,
		signer:           signer,
		refreshTokenTTL:  refreshTokenTTL,
	}
}

// FindOrCreateUserInfo holds the result of FindOrCreateUser.
type FindOrCreateUserInfo struct {
	UserID   uuid.UUID
	IsNew    bool
	BannedAt *time.Time // Non-nil if the user account is banned
}

// FindOrCreateUser finds or creates an OAuth2 user using a "lookup + unique
// constraint fallback" pattern.
//
// Concurrency safe: when two requests with the same provider+sub arrive
// simultaneously, one Create succeeds and the other triggers a unique
// constraint violation (23505), falling back to re-lookup.
func (uc *AuthUsecase) FindOrCreateUser(ctx context.Context, provider string, userInfo *oauth.ProviderUserInfo) (*FindOrCreateUserInfo, error) {
	user, err := uc.oauth2UserRepo.FindByProvider(ctx, provider, userInfo.ProviderUserID)
	if err != nil {
		if !repo.IsNotFound(err) {
			return nil, fmt.Errorf("find user by provider: %w", err)
		}
		// Record does not exist, create it.
		user = &model.OAuth2User{
			Provider:  provider,
			Sub:       userInfo.ProviderUserID,
			Name:      userInfo.Username,
			Email:     userInfo.Email,
			AvatarURL: userInfo.AvatarURL,
		}
		if err := uc.oauth2UserRepo.Create(ctx, user); err != nil {
			// Unique constraint violation → concurrent creation → re-lookup.
			if repo.IsDuplicateKeyError(err) {
				logger.Info(ctx, "findOrCreateUser: concurrent create detected, re-fetching",
					zap.String("provider", provider))
				user, err = uc.oauth2UserRepo.FindByProvider(ctx, provider, userInfo.ProviderUserID)
				if err != nil {
					return nil, fmt.Errorf("re-find user after concurrent create: %w", err)
				}
				return &FindOrCreateUserInfo{UserID: user.ID, IsNew: false, BannedAt: user.BannedAt}, nil
			}
			return nil, fmt.Errorf("create user: %w", err)
		}
		return &FindOrCreateUserInfo{UserID: user.ID, IsNew: true, BannedAt: user.BannedAt}, nil
	}
	// Already exists, update user info.
	user.Name = userInfo.Username
	user.Email = userInfo.Email
	user.AvatarURL = userInfo.AvatarURL
	if err := uc.oauth2UserRepo.Update(ctx, user); err != nil {
		// Log the error but don't fail the operation - user profile sync is best-effort
		// The user can still authenticate even if profile update fails
		logger.Warn(ctx, "failed to update user info (best-effort sync)",
			zap.String("provider", provider),
			zap.String("user_id", user.ID.String()),
			zap.Error(err))
	}
	return &FindOrCreateUserInfo{UserID: user.ID, IsNew: false, BannedAt: user.BannedAt}, nil
}

// UpsertDeviceInput holds the input for UpsertDevice.
type UpsertDeviceInput struct {
	UserID    uuid.UUID
	DeviceID  string
	Name      string
	UserAgent string
}

// UpsertDevice finds or updates device information.
func (uc *AuthUsecase) UpsertDevice(ctx context.Context, input *UpsertDeviceInput) error {
	// DeviceID is critical for device identification - reject if too long.
	if len([]rune(input.DeviceID)) > 100 {
		return fmt.Errorf("device_id exceeds maximum length of 100 characters")
	}

	device := &model.Device{
		UserID:       input.UserID,
		DeviceID:     input.DeviceID,
		Name:         stringutil.TruncateByRune(input.Name, 100),
		UserAgent:    stringutil.TruncateByRune(input.UserAgent, 500),
		LastActiveAt: time.Now(),
	}
	return uc.deviceRepo.Upsert(ctx, device)
}

// TokenPairResult holds the issued token pair.
type TokenPairResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	UserID       uuid.UUID
}

// IssueTokenPair issues an access_token + refresh_token pair.
func (uc *AuthUsecase) IssueTokenPair(ctx context.Context, userID uuid.UUID, deviceID string) (*TokenPairResult, error) {
	return uc.issueTokenPairInTx(ctx, userID, deviceID)
}

// RefreshTokenInfo holds the result of ValidateRefreshToken.
type RefreshTokenInfo struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	DeviceID string
}

// ValidateRefreshToken looks up a refresh token by its plain value, validates
// it (not revoked, not expired), and revokes it (token rotation).
// Returns the token info on success.
//
// Note: This operation is not wrapped in a transaction because the underlying
// repo layer uses atomic UPDATE with WHERE clause to prevent double-revocation.
// The Revoke operation uses optimistic locking via the revoked flag.
// For atomic refresh operations, use RefreshTokenPair instead.
func (uc *AuthUsecase) ValidateRefreshToken(ctx context.Context, plainToken string) (*RefreshTokenInfo, error) {
	return uc.validateRefreshTokenInTx(ctx, plainToken)
}

// RefreshTokenPair validates a refresh token and issues a new token pair atomically.
// This operation is wrapped in a transaction to prevent concurrent refresh race conditions.
func (uc *AuthUsecase) RefreshTokenPair(ctx context.Context, plainToken string) (*TokenPairResult, error) {
	var result *TokenPairResult

	err := uc.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		// 1. Validate and revoke the old refresh token.
		rtInfo, err := uc.validateRefreshTokenInTx(txCtx, plainToken)
		if err != nil {
			return err
		}

		// 2. Issue new token pair within the same transaction.
		result, err = uc.issueTokenPairInTx(txCtx, rtInfo.UserID, rtInfo.DeviceID)
		if err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return result, nil
}

// validateRefreshTokenInTx is the transactional version of ValidateRefreshToken.
func (uc *AuthUsecase) validateRefreshTokenInTx(ctx context.Context, plainToken string) (*RefreshTokenInfo, error) {
	rtHash := hashRefreshToken(plainToken)
	rt, err := uc.refreshTokenRepo.FindByHash(ctx, rtHash)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, ErrInvalidRefreshToken
		}
		return nil, fmt.Errorf("find refresh token: %w", err)
	}

	if rt.Revoked {
		// Potential token reuse detection: if a revoked token is used, it may indicate
		// that the token was stolen.
		logger.Warn(ctx, "auth.refresh_token_reuse_detected",
			zap.String("token_hash_prefix", rtHash[:16]))
		return nil, ErrRefreshTokenRevoked
	}
	if time.Now().After(rt.ExpiresAt) {
		return nil, ErrRefreshTokenExpired
	}

	// Check if user is banned
	user, err := uc.oauth2UserRepo.FindByID(ctx, rt.UserID)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}
	if user.BannedAt != nil {
		return nil, ErrAccountBanned
	}

	// Revoke the old refresh token (token rotation).
	// The repo layer should use atomic UPDATE to prevent race conditions.
	if err := uc.refreshTokenRepo.Revoke(ctx, rt.ID); err != nil {
		return nil, fmt.Errorf("revoke old refresh token: %w", err)
	}

	return &RefreshTokenInfo{
		ID:       rt.ID,
		UserID:   rt.UserID,
		DeviceID: rt.DeviceID,
	}, nil
}

// issueTokenPairInTx is the transactional version of IssueTokenPair.
func (uc *AuthUsecase) issueTokenPairInTx(ctx context.Context, userID uuid.UUID, deviceID string) (*TokenPairResult, error) {
	accessToken, _, err := uc.signer.SignAccessToken(userID, deviceID)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	rtPlain, err := generateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}
	rtHash := hashRefreshToken(rtPlain)

	rt := &model.RefreshToken{
		TokenHash: rtHash,
		UserID:    userID,
		DeviceID:  deviceID,
		ExpiresAt: time.Now().Add(uc.refreshTokenTTL),
		Revoked:   false,
	}
	if err := uc.refreshTokenRepo.Create(ctx, rt); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}

	return &TokenPairResult{
		AccessToken:  accessToken,
		RefreshToken: rtPlain,
		ExpiresIn:    int64(uc.signer.AccessTTL().Seconds()),
		UserID:       userID,
	}, nil
}

// Sentinel errors for refresh token validation.
var (
	ErrInvalidRefreshToken = errors.New("refresh_token is invalid")
	ErrRefreshTokenRevoked = errors.New("refresh_token has been revoked")
	ErrRefreshTokenExpired = errors.New("refresh_token has expired")
	ErrAccountBanned       = errors.New("account has been banned")
)

// generateRefreshToken generates an opaque refresh_token.
func generateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "rt_" + hex.EncodeToString(b), nil
}

// hashRefreshToken computes the SHA-256 hash of a refresh_token.
func hashRefreshToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
