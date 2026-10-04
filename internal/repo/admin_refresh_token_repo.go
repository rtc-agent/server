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
	result := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.AdminRefreshToken{}).Where("id = ?", id).Update("revoked", true)
	if result.Error != nil {
		return fmt.Errorf("revoke admin refresh token %s: %w", id, result.Error)
	}
	return nil
}
