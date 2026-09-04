package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/dbmodel"

	"gorm.io/gorm"
)

// RefreshTokenRepo 刷新令牌仓储接口
type RefreshTokenRepo interface {
	Create(ctx context.Context, rt *dbmodel.RefreshToken) error
	FindByHash(ctx context.Context, hash string) (*dbmodel.RefreshToken, error)
	Revoke(ctx context.Context, id uuid.UUID) error
}

type refreshTokenRepo struct {
	db *gorm.DB
}

// NewRefreshTokenRepo 创建 RefreshTokenRepo
func NewRefreshTokenRepo(db *gorm.DB) RefreshTokenRepo {
	return &refreshTokenRepo{db: db}
}

func (r *refreshTokenRepo) Create(ctx context.Context, rt *dbmodel.RefreshToken) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(rt).Error; err != nil {
		return fmt.Errorf("create refresh token: %w", err)
	}
	return nil
}

func (r *refreshTokenRepo) FindByHash(ctx context.Context, hash string) (*dbmodel.RefreshToken, error) {
	var rt dbmodel.RefreshToken
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
	result := DBFromContext(ctx, r.db).WithContext(ctx).Model(&dbmodel.RefreshToken{}).Where("id = ?", id).Update("revoked", true)
	if result.Error != nil {
		return fmt.Errorf("revoke refresh token %s: %w", id, result.Error)
	}
	return nil
}
