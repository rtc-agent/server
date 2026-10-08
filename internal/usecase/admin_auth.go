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
	"gorm.io/gorm"

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

// bcryptCost is the computational cost factor for password hashing.
// Higher values increase security but also increase CPU time for hashing.
const bcryptCost = 12

// AdminAuthUsecase handles admin authentication operations.
type AdminAuthUsecase struct {
	db               *gorm.DB
	adminUserRepo    repo.AdminUserRepo
	refreshTokenRepo repo.AdminRefreshTokenRepo
	signer           AdminTokenSigner
	loginProtection  LoginProtectionInterface // optional brute-force protection (may be nil)
}

// NewAdminAuthUsecase creates a new AdminAuthUsecase.
func NewAdminAuthUsecase(
	db *gorm.DB,
	adminUserRepo repo.AdminUserRepo,
	refreshTokenRepo repo.AdminRefreshTokenRepo,
	signer AdminTokenSigner,
) *AdminAuthUsecase {
	return &AdminAuthUsecase{
		db:               db,
		adminUserRepo:    adminUserRepo,
		refreshTokenRepo: refreshTokenRepo,
		signer:           signer,
	}
}

// SetLoginProtection injects optional login brute-force protection.
func (uc *AdminAuthUsecase) SetLoginProtection(lp LoginProtectionInterface) {
	uc.loginProtection = lp
}

// LoginResult holds the result of a successful login.
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	User         *model.AdminUser
}

// Login authenticates a user with email and password.
// clientIP is used for brute-force login protection tracking.
func (uc *AdminAuthUsecase) Login(ctx context.Context, email, password, clientIP string) (*LoginResult, error) {
	// 0. Check login protection (IP + email dual lockout)
	if uc.loginProtection != nil {
		if err := uc.loginProtection.CheckLoginAllowed(ctx, clientIP, email); err != nil {
			logger.Warn(ctx, "admin_auth.login_blocked_by_protection",
				zap.String("email", email),
				zap.String("ip", clientIP),
				zap.Error(err))
			return nil, fmt.Errorf("%w: %v", ErrLoginLocked, err)
		}
	}

	// 1. Find admin user by email
	user, err := uc.adminUserRepo.GetByEmail(ctx, email)
	if err != nil {
		if repo.IsNotFound(err) {
			// SECURITY: Run bcrypt even when user not found to prevent timing-based
			// email enumeration. An attacker should not be able to distinguish between
			// "user not found" and "wrong password" based on response time.
			// We hash a dummy value to match the cost of a real comparison.
			_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(password))
			// Record failed attempt even for unknown user (email enumeration countermeasure)
			if uc.loginProtection != nil {
				uc.loginProtection.RecordFailedLogin(ctx, clientIP, email)
			}
			logger.Info(ctx, "admin_auth.login_user_not_found", zap.String("email", email))
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("admin auth get user by email: %w", err)
	}

	// 2. Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		if uc.loginProtection != nil {
			uc.loginProtection.RecordFailedLogin(ctx, clientIP, email)
		}
		logger.Info(ctx, "admin_auth.login_invalid_password", zap.String("email", email))
		return nil, ErrInvalidCredentials
	}

	// 3. Sign access token
	accessToken, _, err := uc.signer.SignAccessToken(user.ID, user.Email, user.Name)
	if err != nil {
		return nil, fmt.Errorf("admin auth sign access token: %w", err)
	}

	// 4. Generate and store refresh token
	refreshPlain := generateRefreshTokenPlain()
	refreshHash := hashRefreshToken(refreshPlain)
	refreshExpiresAt := time.Now().Add(uc.signer.RefreshTTL())

	rt := &model.AdminRefreshToken{
		TokenHash: refreshHash,
		UserID:    user.ID,
		ExpiresAt: refreshExpiresAt,
		Revoked:   false,
	}
	if err := uc.refreshTokenRepo.Create(ctx, rt); err != nil {
		return nil, fmt.Errorf("admin auth store refresh token: %w", err)
	}

	// Reset login protection counters on success
	if uc.loginProtection != nil {
		uc.loginProtection.ResetLoginAttempts(ctx, clientIP, email)
	}

	logger.Info(ctx, "admin_auth.login_succeeded",
		zap.String("user_id", user.ID.String()),
		zap.String("email", email))

	return &LoginResult{
		AccessToken:  accessToken,
		RefreshToken: refreshPlain,
		ExpiresIn:    int64(uc.signer.AccessTTL().Seconds()),
		User:         user,
	}, nil
}

// LoginWithOTP authenticates a user after OTP verification.
// This is called after the OTP has been verified by EmailOTPUsecase.
// Unlike password login, this does not need login protection (OTP has its own).
func (uc *AdminAuthUsecase) LoginWithOTP(ctx context.Context, email string) (*LoginResult, error) {
	// 1. Find admin user by email
	user, err := uc.adminUserRepo.GetByEmail(ctx, email)
	if err != nil {
		if repo.IsNotFound(err) {
			// This should not happen if OTP verification was successful,
			// but handle it defensively
			logger.Warn(ctx, "admin_auth.otp_login_user_not_found", zap.String("email", email))
			return nil, ErrAdminUserNotFound
		}
		return nil, fmt.Errorf("admin auth get user by email: %w", err)
	}

	// 2. Sign access token
	accessToken, _, err := uc.signer.SignAccessToken(user.ID, user.Email, user.Name)
	if err != nil {
		return nil, fmt.Errorf("admin auth sign access token: %w", err)
	}

	// 3. Generate and store refresh token
	refreshPlain := generateRefreshTokenPlain()
	refreshHash := hashRefreshToken(refreshPlain)
	refreshExpiresAt := time.Now().Add(uc.signer.RefreshTTL())

	rt := &model.AdminRefreshToken{
		TokenHash: refreshHash,
		UserID:    user.ID,
		ExpiresAt: refreshExpiresAt,
		Revoked:   false,
	}
	if err := uc.refreshTokenRepo.Create(ctx, rt); err != nil {
		return nil, fmt.Errorf("admin auth store refresh token: %w", err)
	}

	logger.Info(ctx, "admin_auth.otp_login_succeeded",
		zap.String("user_id", user.ID.String()),
		zap.String("email", email))

	return &LoginResult{
		AccessToken:  accessToken,
		RefreshToken: refreshPlain,
		ExpiresIn:    int64(uc.signer.AccessTTL().Seconds()),
		User:         user,
	}, nil
}

// GetCurrentUser retrieves the current admin user by ID.
func (uc *AdminAuthUsecase) GetCurrentUser(ctx context.Context, userID uuid.UUID) (*model.AdminUser, error) {
	user, err := uc.adminUserRepo.GetByID(ctx, userID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, ErrAdminUserNotFound
		}
		return nil, fmt.Errorf("admin auth get current admin user: %w", err)
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
// Wrapped in a transaction to prevent concurrent refresh token use from creating
// multiple valid token pairs (P0 race condition fix).
func (uc *AdminAuthUsecase) RefreshToken(ctx context.Context, refreshTokenPlain string) (*RefreshTokenResult, error) {
	var result *RefreshTokenResult

	err := uc.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		// 1. Hash the refresh token and look it up (within transaction)
		refreshHash := hashRefreshToken(refreshTokenPlain)
		rt, err := uc.refreshTokenRepo.FindByHash(txCtx, refreshHash)
		if err != nil {
			if repo.IsNotFound(err) {
				return ErrInvalidRefreshToken
			}
			return fmt.Errorf("admin auth find refresh token: %w", err)
		}

		// 2. Check if token is revoked
		if rt.Revoked {
			logger.Warn(txCtx, "admin_auth.refresh_token_reuse_detected",
				zap.String("token_hash_prefix", refreshHash[:16]))
			return ErrRefreshTokenRevoked
		}

		// 3. Check if token is expired
		if time.Now().After(rt.ExpiresAt) {
			return ErrRefreshTokenExpired
		}

		// 4. Revoke the old refresh token (rotation)
		if err := uc.refreshTokenRepo.Revoke(txCtx, rt.ID); err != nil {
			return fmt.Errorf("admin auth revoke old refresh token: %w", err)
		}

		// 5. Get admin user info
		user, err := uc.adminUserRepo.GetByID(txCtx, rt.UserID)
		if err != nil {
			return fmt.Errorf("admin auth get admin user for refresh: %w", err)
		}

		// 6. Sign new access token
		accessToken, _, err := uc.signer.SignAccessToken(user.ID, user.Email, user.Name)
		if err != nil {
			return fmt.Errorf("admin auth sign new access token: %w", err)
		}

		// 7. Generate and store new refresh token
		newRefreshPlain := generateRefreshTokenPlain()
		newRefreshHash := hashRefreshToken(newRefreshPlain)
		newRefreshExpiresAt := time.Now().Add(uc.signer.RefreshTTL())

		newRT := &model.AdminRefreshToken{
			TokenHash: newRefreshHash,
			UserID:    user.ID,
			ExpiresAt: newRefreshExpiresAt,
			Revoked:   false,
		}
		if err := uc.refreshTokenRepo.Create(txCtx, newRT); err != nil {
			return fmt.Errorf("admin auth store new refresh token: %w", err)
		}

		result = &RefreshTokenResult{
			AccessToken:  accessToken,
			RefreshToken: newRefreshPlain,
			ExpiresIn:    int64(uc.signer.AccessTTL().Seconds()),
			UserID:       user.ID,
		}
		return nil
	})
	if err != nil {
		// Return sentinel errors as-is; wrap unexpected errors
		if errors.Is(err, ErrInvalidRefreshToken) ||
			errors.Is(err, ErrRefreshTokenRevoked) ||
			errors.Is(err, ErrRefreshTokenExpired) {
			return nil, err
		}
		return nil, err
	}

	logger.Info(ctx, "admin_auth.token_refresh_succeeded",
		zap.String("user_id", result.UserID.String()))

	return result, nil
}

// Logout revokes a refresh token.
func (uc *AdminAuthUsecase) Logout(ctx context.Context, refreshTokenPlain string) error {
	refreshHash := hashRefreshToken(refreshTokenPlain)
	rt, err := uc.refreshTokenRepo.FindByHash(ctx, refreshHash)
	if err != nil {
		if repo.IsNotFound(err) {
			// Token not found, but we don't expose this to the client
			logger.Info(ctx, "admin_auth.logout_token_not_found",
				zap.String("token_hash_prefix", refreshHash[:16]))
			return nil
		}
		return fmt.Errorf("admin auth find refresh token: %w", err)
	}

	if rt.Revoked {
		// Already revoked, no action needed
		return nil
	}

	if err := uc.refreshTokenRepo.Revoke(ctx, rt.ID); err != nil {
		return fmt.Errorf("admin auth revoke refresh token: %w", err)
	}

	logger.Info(ctx, "admin_auth.logout_succeeded",
		zap.String("user_id", rt.UserID.String()))

	return nil
}

// HashPassword hashes a password using bcrypt.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// Sentinel errors for admin authentication.
var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrAdminUserNotFound  = errors.New("admin user not found")
	ErrLoginLocked        = errors.New("login temporarily locked due to too many failed attempts")
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

// randRead is a variable for testing purposes.
var randRead = randReadImpl

func randReadImpl(b []byte) (int, error) {
	return rand.Read(b)
}
