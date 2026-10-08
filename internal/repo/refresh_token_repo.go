package repo

import (
	"context"
	"errors"
	"fmt"

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
	// Revoke marks a refresh token as revoked.
	Revoke(ctx context.Context, id uuid.UUID) error
	// RevokeAllByUserID revokes all refresh tokens for a user.
	RevokeAllByUserID(ctx context.Context, userID uuid.UUID) (int64, error)
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

func (r *refreshTokenRepo) Revoke(ctx context.Context, id uuid.UUID) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.RefreshToken{}).
		Where("id = ? AND revoked = false", id).
		Update("revoked", true)
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
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.RefreshToken{}).
		Where("user_id = ? AND revoked = false", userID).
		Update("revoked", true)
	if result.Error != nil {
		return 0, fmt.Errorf("revoke all refresh tokens for user %s: %w", userID, result.Error)
	}
	return result.RowsAffected, nil
}
