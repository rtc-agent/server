package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"

	"gorm.io/gorm"
)

// AdminRefreshTokenRepo manages admin refresh tokens.
type AdminRefreshTokenRepo interface {
	// Create stores a new admin refresh token record.
	Create(ctx context.Context, rt *model.AdminRefreshToken) error
	// FindByHash looks up an admin refresh token by its hash.
	FindByHash(ctx context.Context, hash string) (*model.AdminRefreshToken, error)
	// Revoke marks an admin refresh token as revoked.
	Revoke(ctx context.Context, id uuid.UUID) error
	// RevokeAllByUserID revokes all admin refresh tokens for a user.
	// This is used when token reuse is detected to invalidate all sessions.
	RevokeAllByUserID(ctx context.Context, userID uuid.UUID) (int64, error)
}

type adminRefreshTokenRepo struct {
	db *gorm.DB
}

// NewAdminRefreshTokenRepo creates a new AdminRefreshTokenRepo.
func NewAdminRefreshTokenRepo(db *gorm.DB) AdminRefreshTokenRepo {
	return &adminRefreshTokenRepo{db: db}
}

func (r *adminRefreshTokenRepo) Create(ctx context.Context, rt *model.AdminRefreshToken) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(rt).Error; err != nil {
		return fmt.Errorf("create admin refresh token: %w", err)
	}
	return nil
}

func (r *adminRefreshTokenRepo) FindByHash(ctx context.Context, hash string) (*model.AdminRefreshToken, error) {
	var rt model.AdminRefreshToken
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("token_hash = ?", hash).First(&rt).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("find admin refresh token: %w", ErrNotFound)
		}
		return nil, fmt.Errorf("find admin refresh token: %w", err)
	}
	return &rt, nil
}

func (r *adminRefreshTokenRepo) Revoke(ctx context.Context, id uuid.UUID) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.AdminRefreshToken{}).
		Where("id = ? AND revoked = false", id).
		Update("revoked", true)
	if result.Error != nil {
		return fmt.Errorf("revoke admin refresh token %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("revoke admin refresh token %s: %w", id, ErrNotFound)
	}
	return nil
}

// RevokeAllByUserID revokes all admin refresh tokens for a user.
// This is used when token reuse is detected to invalidate all sessions.
func (r *adminRefreshTokenRepo) RevokeAllByUserID(ctx context.Context, userID uuid.UUID) (int64, error) {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.AdminRefreshToken{}).
		Where("user_id = ? AND revoked = false", userID).
		Update("revoked", true)
	if result.Error != nil {
		return 0, fmt.Errorf("revoke all admin refresh tokens for user %s: %w", userID, result.Error)
	}
	return result.RowsAffected, nil
}
