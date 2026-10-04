// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// AdminTokenSigner is the JWT signing interface for admin tokens.
type AdminTokenSigner interface {
	SignAccessToken(userID uuid.UUID, email, name string) (token string, expiresAt time.Time, err error)
	SignRefreshToken(userID uuid.UUID) (token string, expiresAt time.Time, err error)
	ParseAccessToken(token string) (*auth.AdminClaims, error)
	ParseRefreshToken(token string) (*auth.AdminClaims, error)
	AccessTTL() time.Duration
	RefreshTTL() time.Duration
}

// AdminAuthUsecase handles admin authentication operations.
type AdminAuthUsecase struct {
	userRepo         repo.UserRepo
	refreshTokenRepo repo.RefreshTokenRepo
	signer           AdminTokenSigner
	bcryptCost       int
}

// NewAdminAuthUsecase creates a new AdminAuthUsecase.
func NewAdminAuthUsecase(
	userRepo repo.UserRepo,
	refreshTokenRepo repo.RefreshTokenRepo,
	signer AdminTokenSigner,
) *AdminAuthUsecase {
	return &AdminAuthUsecase{
		userRepo:         userRepo,
		refreshTokenRepo: refreshTokenRepo,
		signer:           signer,
		bcryptCost:       12,
	}
}

// LoginResult holds the result of a successful login.
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	User         *model.User
}

// Login authenticates a user with email and password.
func (uc *AdminAuthUsecase) Login(ctx context.Context, email, password string) (*LoginResult, error) {
	// 1. Find user by email
	user, err := uc.userRepo.GetByEmail(ctx, email)
	if err != nil {
		if repo.IsNotFound(err) {
			// SECURITY: Run bcrypt even when user not found to prevent timing-based
			// email enumeration. An attacker should not be able to distinguish between
			// "user not found" and "wrong password" based on response time.
			// We hash a dummy value to match the cost of a real comparison.
			_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(password))
			logger.Info(ctx, "login failed: user not found", zap.String("email", email))
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("get user by email: %w", err)
	}

	// 2. Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		logger.Info(ctx, "login failed: invalid password", zap.String("email", email))
		return nil, ErrInvalidCredentials
	}

	// 3. Sign access token
	accessToken, _, err := uc.signer.SignAccessToken(user.ID, user.Email, user.Name)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	// 4. Generate and store refresh token
	refreshPlain := generateRefreshTokenPlain()
	refreshHash := hashRefreshToken(refreshPlain)
	refreshExpiresAt := time.Now().Add(uc.signer.RefreshTTL())

	rt := &model.RefreshToken{
		TokenHash: refreshHash,
		UserID:    user.ID,
		ExpiresAt: refreshExpiresAt,
		Revoked:   false,
	}
	if err := uc.refreshTokenRepo.Create(ctx, rt); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}

	logger.Info(ctx, "login succeeded",
		zap.String("user_id", user.ID.String()),
		zap.String("email", email))

	return &LoginResult{
		AccessToken:  accessToken,
		RefreshToken: refreshPlain,
		ExpiresIn:    int64(uc.signer.AccessTTL().Seconds()),
		User:         user,
	}, nil
}

// GetCurrentUser retrieves the current user by ID.
func (uc *AdminAuthUsecase) GetCurrentUser(ctx context.Context, userID uuid.UUID) (*model.User, error) {
	user, err := uc.userRepo.GetByID(ctx, userID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("get user by ID: %w", err)
	}
	return user, nil
}

// RefreshTokenResult holds the result of a token refresh.
type RefreshTokenResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	UserID       uuid.UUID
}

// RefreshToken validates and rotates a refresh token, issuing a new token pair.
func (uc *AdminAuthUsecase) RefreshToken(ctx context.Context, refreshTokenPlain string) (*RefreshTokenResult, error) {
	// 1. Hash the refresh token and look it up
	refreshHash := hashRefreshToken(refreshTokenPlain)
	rt, err := uc.refreshTokenRepo.FindByHash(ctx, refreshHash)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, ErrInvalidRefreshToken
		}
		return nil, fmt.Errorf("find refresh token: %w", err)
	}

	// 2. Check if token is revoked
	if rt.Revoked {
		logger.Warn(ctx, "refresh token reuse detected",
			zap.String("token_hash", refreshHash[:16]+"..."))
		return nil, ErrRefreshTokenRevoked
	}

	// 3. Check if token is expired
	if time.Now().After(rt.ExpiresAt) {
		return nil, ErrRefreshTokenExpired
	}

	// 4. Revoke the old refresh token (rotation)
	if err := uc.refreshTokenRepo.Revoke(ctx, rt.ID); err != nil {
		return nil, fmt.Errorf("revoke old refresh token: %w", err)
	}

	// 5. Get user info
	user, err := uc.userRepo.GetByID(ctx, rt.UserID)
	if err != nil {
		return nil, fmt.Errorf("get user for refresh: %w", err)
	}

	// 6. Sign new access token
	accessToken, _, err := uc.signer.SignAccessToken(user.ID, user.Email, user.Name)
	if err != nil {
		return nil, fmt.Errorf("sign new access token: %w", err)
	}

	// 7. Generate and store new refresh token
	newRefreshPlain := generateRefreshTokenPlain()
	newRefreshHash := hashRefreshToken(newRefreshPlain)
	newRefreshExpiresAt := time.Now().Add(uc.signer.RefreshTTL())

	newRT := &model.RefreshToken{
		TokenHash: newRefreshHash,
		UserID:    user.ID,
		ExpiresAt: newRefreshExpiresAt,
		Revoked:   false,
	}
	if err := uc.refreshTokenRepo.Create(ctx, newRT); err != nil {
		return nil, fmt.Errorf("store new refresh token: %w", err)
	}

	logger.Info(ctx, "token refresh succeeded",
		zap.String("user_id", user.ID.String()))

	return &RefreshTokenResult{
		AccessToken:  accessToken,
		RefreshToken: newRefreshPlain,
		ExpiresIn:    int64(uc.signer.AccessTTL().Seconds()),
		UserID:       user.ID,
	}, nil
}

// Logout revokes a refresh token.
func (uc *AdminAuthUsecase) Logout(ctx context.Context, refreshTokenPlain string) error {
	refreshHash := hashRefreshToken(refreshTokenPlain)
	rt, err := uc.refreshTokenRepo.FindByHash(ctx, refreshHash)
	if err != nil {
		if repo.IsNotFound(err) {
			// Token not found, but we don't expose this to the client
			logger.Info(ctx, "logout: refresh token not found",
				zap.String("token_hash", refreshHash[:16]+"..."))
			return nil
		}
		return fmt.Errorf("find refresh token: %w", err)
	}

	if rt.Revoked {
		// Already revoked, no action needed
		return nil
	}

	if err := uc.refreshTokenRepo.Revoke(ctx, rt.ID); err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}

	logger.Info(ctx, "logout succeeded",
		zap.String("user_id", rt.UserID.String()))

	return nil
}

// OAuthUserInfo holds OAuth user information for FindOrCreateUser.
type OAuthUserInfo struct {
	Provider  string
	Sub       string
	Email     string
	Name      string
	AvatarURL string
}

// FindOrCreateUser finds or creates a user from OAuth provider information.
// This is used for OAuth-based admin authentication.
func (uc *AdminAuthUsecase) FindOrCreateUser(ctx context.Context, provider, sub, email, name, avatarURL string) (*model.User, error) {
	// For admin-server, we use email as the unique identifier
	// Try to find existing user by email
	user, err := uc.userRepo.GetByEmail(ctx, email)
	if err != nil {
		if !repo.IsNotFound(err) {
			return nil, fmt.Errorf("get user by email: %w", err)
		}

		// User not found, create new user
		// Generate a random password hash (OAuth users don't use password login)
		randomPassword := generateRandomPassword()
		passwordHash, err := bcrypt.GenerateFromPassword([]byte(randomPassword), uc.bcryptCost)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}

		user = &model.User{
			Email:        email,
			Name:         name,
			AvatarURL:    avatarURL,
			PasswordHash: string(passwordHash),
		}

		if err := uc.userRepo.Create(ctx, user); err != nil {
			if errors.Is(err, repo.ErrDuplicateEmail) {
				// Concurrent creation, try to find again
				user, err = uc.userRepo.GetByEmail(ctx, email)
				if err != nil {
					return nil, fmt.Errorf("re-find user after concurrent create: %w", err)
				}
				return user, nil
			}
			return nil, fmt.Errorf("create user: %w", err)
		}

		logger.Info(ctx, "created new admin user from OAuth",
			zap.String("user_id", user.ID.String()),
			zap.String("provider", provider),
			zap.String("email", email))

		return user, nil
	}

	// User exists, update OAuth info if needed
	updated := false
	if name != "" && user.Name != name {
		user.Name = name
		updated = true
	}
	if avatarURL != "" && user.AvatarURL != avatarURL {
		user.AvatarURL = avatarURL
		updated = true
	}

	if updated {
		if err := uc.userRepo.Update(ctx, user); err != nil {
			logger.Warn(ctx, "failed to update user OAuth info",
				zap.Error(err),
				zap.String("user_id", user.ID.String()))
		}
	}

	return user, nil
}

// HashPassword hashes a password using bcrypt.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// Sentinel errors for admin authentication.
var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrUserNotFound       = errors.New("user not found")
)

// dummyPasswordHash is a pre-computed bcrypt hash used during login when the user
// is not found. This ensures constant-time authentication responses regardless of
// whether the email exists in the database, preventing timing-based email enumeration.
// Generated with bcrypt cost 12 (matching the production cost factor).
var dummyPasswordHash = func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("dummy"), 12)
	return h
}()

// generateRefreshTokenPlain generates an opaque refresh token.
func generateRefreshTokenPlain() string {
	b := make([]byte, 32)
	if _, err := randRead(b); err != nil {
		// This should never happen in practice
		panic(fmt.Sprintf("failed to generate random bytes: %v", err))
	}
	return "rt_" + hex.EncodeToString(b)
}

// generateRandomPassword generates a random password for OAuth users.
func generateRandomPassword() string {
	b := make([]byte, 32)
	if _, err := randRead(b); err != nil {
		panic(fmt.Sprintf("failed to generate random password: %v", err))
	}
	return hex.EncodeToString(b)
}

// randRead is a variable for testing purposes.
var randRead = randReadImpl

func randReadImpl(b []byte) (int, error) {
	return rand.Read(b)
}
