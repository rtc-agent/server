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
	oauth2UserRepo   repo.OAuth2UserRepo
	deviceRepo       repo.DeviceRepo
	refreshTokenRepo repo.RefreshTokenRepo
	signer           TokenSigner
	refreshTokenTTL  time.Duration
}

// NewAuthUsecase creates a new AuthUsecase.
func NewAuthUsecase(
	oauth2UserRepo repo.OAuth2UserRepo,
	deviceRepo repo.DeviceRepo,
	refreshTokenRepo repo.RefreshTokenRepo,
	signer TokenSigner,
	refreshTokenTTL time.Duration,
) *AuthUsecase {
	return &AuthUsecase{
		oauth2UserRepo:   oauth2UserRepo,
		deviceRepo:       deviceRepo,
		refreshTokenRepo: refreshTokenRepo,
		signer:           signer,
		refreshTokenTTL:  refreshTokenTTL,
	}
}

// FindOrCreateUserInfo holds the result of FindOrCreateUser.
type FindOrCreateUserInfo struct {
	UserID uuid.UUID
	IsNew  bool
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
				return &FindOrCreateUserInfo{UserID: user.ID, IsNew: false}, nil
			}
			return nil, fmt.Errorf("create user: %w", err)
		}
		return &FindOrCreateUserInfo{UserID: user.ID, IsNew: true}, nil
	}
	// Already exists, update user info.
	user.Name = userInfo.Username
	user.Email = userInfo.Email
	user.AvatarURL = userInfo.AvatarURL
	if err := uc.oauth2UserRepo.Update(ctx, user); err != nil {
		logger.Warn(ctx, "failed to update user info", zap.Error(err))
	}
	return &FindOrCreateUserInfo{UserID: user.ID, IsNew: false}, nil
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
	device := &model.Device{
		UserID:       input.UserID,
		DeviceID:     input.DeviceID,
		Name:         input.Name,
		UserAgent:    input.UserAgent,
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
	accessToken, expiresAt, err := uc.signer.SignAccessToken(userID, deviceID)
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
		ExpiresAt: expiresAt.Add(uc.refreshTokenTTL),
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

// RefreshTokenInfo holds the result of ValidateRefreshToken.
type RefreshTokenInfo struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	DeviceID string
}

// ValidateRefreshToken looks up a refresh token by its plain value, validates
// it (not revoked, not expired), and revokes it (token rotation).
// Returns the token info on success.
func (uc *AuthUsecase) ValidateRefreshToken(ctx context.Context, plainToken string) (*RefreshTokenInfo, error) {
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
		return nil, ErrRefreshTokenRevoked
	}
	if time.Now().After(rt.ExpiresAt) {
		return nil, ErrRefreshTokenExpired
	}

	// Revoke the old refresh token (token rotation).
	if err := uc.refreshTokenRepo.Revoke(ctx, rt.ID); err != nil {
		return nil, fmt.Errorf("revoke old refresh token: %w", err)
	}

	return &RefreshTokenInfo{
		ID:       rt.ID,
		UserID:   rt.UserID,
		DeviceID: rt.DeviceID,
	}, nil
}

// Sentinel errors for refresh token validation.
var (
	ErrInvalidRefreshToken = errors.New("refresh_token is invalid")
	ErrRefreshTokenRevoked = errors.New("refresh_token has been revoked")
	ErrRefreshTokenExpired = errors.New("refresh_token has expired")
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
