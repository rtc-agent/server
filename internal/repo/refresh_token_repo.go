package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"

	"gorm.io/gorm"
)

// RefreshTokenRepo provides refresh token persistence operations.
type RefreshTokenRepo interface {
	// Create stores a new refresh token record.
	Create(ctx context.Context, rt *model.RefreshToken) error
	// FindByHash looks up a refresh token by its hash.
	FindByHash(ctx context.Context, hash string) (*model.RefreshToken, error)
	// FindByHashForUpdate looks up a refresh token by its hash with a row lock (SELECT ... FOR UPDATE).
	// This prevents concurrent modifications and is critical for detecting token reuse attacks.
	FindByHashForUpdate(ctx context.Context, hash string) (*model.RefreshToken, error)
	// Revoke marks a refresh token as revoked.
	Revoke(ctx context.Context, id uuid.UUID) error
	// RevokeAllByUserID revokes all refresh tokens for a user.
	RevokeAllByUserID(ctx context.Context, userID uuid.UUID) (int64, error)
	// RevokeAllByUserAndDevice revokes all refresh tokens for a user on a specific device.
	// This is used when token reuse is detected to invalidate all sessions for that device.
	RevokeAllByUserAndDevice(ctx context.Context, userID uuid.UUID, deviceID string) (int64, error)
	// DeleteExpired removes refresh tokens that expired before the given cutoff time.
	// Returns the number of deleted rows.
	DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error)
}

type refreshTokenRepo struct {
	db *gorm.DB
}

// NewRefreshTokenRepo creates a new RefreshTokenRepo.
func NewRefreshTokenRepo(db *gorm.DB) RefreshTokenRepo {
	return &refreshTokenRepo{db: db}
}

func (r *refreshTokenRepo) Create(ctx context.Context, rt *model.RefreshToken) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(rt).Error; err != nil {
		return fmt.Errorf("create refresh token: %w", err)
	}
	return nil
}

func (r *refreshTokenRepo) FindByHash(ctx context.Context, hash string) (*model.RefreshToken, error) {
	var rt model.RefreshToken
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("token_hash = ?", hash).First(&rt).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("find refresh token: %w", ErrRefreshTokenNotFound)
		}
		return nil, fmt.Errorf("find refresh token: %w", err)
	}
	return &rt, nil
}

// FindByHashForUpdate looks up a refresh token by its hash with a row lock (SELECT ... FOR UPDATE).
// This prevents concurrent modifications and is critical for detecting token reuse attacks.
func (r *refreshTokenRepo) FindByHashForUpdate(ctx context.Context, hash string) (*model.RefreshToken, error) {
	var rt model.RefreshToken
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Set("gorm:query_option", "FOR UPDATE").
		Where("token_hash = ?", hash).
		First(&rt).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("find refresh token for update: %w", ErrRefreshTokenNotFound)
		}
		return nil, fmt.Errorf("find refresh token for update: %w", err)
	}
	return &rt, nil
}

func (r *refreshTokenRepo) Revoke(ctx context.Context, id uuid.UUID) error {
	now := time.Now()
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.RefreshToken{}).
		Where("id = ? AND revoked = false", id).
		Updates(map[string]interface{}{
			"revoked":    true,
			"revoked_at": now,
		})
	if result.Error != nil {
		return fmt.Errorf("revoke refresh token %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("revoke refresh token %s: %w", id, ErrRefreshTokenNotFound)
	}
	return nil
}

// RevokeAllByUserID revokes all refresh tokens for a user.
func (r *refreshTokenRepo) RevokeAllByUserID(ctx context.Context, userID uuid.UUID) (int64, error) {
	now := time.Now()
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.RefreshToken{}).
		Where("user_id = ? AND revoked = false", userID).
		Updates(map[string]interface{}{
			"revoked":    true,
			"revoked_at": now,
		})
	if result.Error != nil {
		return 0, fmt.Errorf("revoke all refresh tokens for user %s: %w", userID, result.Error)
	}
	return result.RowsAffected, nil
}

// RevokeAllByUserAndDevice revokes all refresh tokens for a user on a specific device.
// This is used when token reuse is detected to invalidate all sessions for that device.
func (r *refreshTokenRepo) RevokeAllByUserAndDevice(ctx context.Context, userID uuid.UUID, deviceID string) (int64, error) {
	now := time.Now()
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.RefreshToken{}).
		Where("user_id = ? AND device_id = ? AND revoked = false", userID, deviceID).
		Updates(map[string]interface{}{
			"revoked":    true,
			"revoked_at": now,
		})
	if result.Error != nil {
		return 0, fmt.Errorf("revoke all refresh tokens for user %s device %s: %w", userID, deviceID, result.Error)
	}
	return result.RowsAffected, nil
}

// DeleteExpired removes refresh tokens that expired before the given cutoff time.
func (r *refreshTokenRepo) DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error) {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("expires_at < ?", cutoff).
		Delete(&model.RefreshToken{})
	if result.Error != nil {
		return 0, fmt.Errorf("delete expired refresh tokens: %w", result.Error)
	}
	return result.RowsAffected, nil
}
