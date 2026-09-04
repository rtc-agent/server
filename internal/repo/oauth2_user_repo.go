package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/dbmodel"

	"gorm.io/gorm"
)

// OAuth2UserRepo OAuth2 用户仓储接口
type OAuth2UserRepo interface {
	Create(ctx context.Context, user *dbmodel.OAuth2User) error
	FindByID(ctx context.Context, id uuid.UUID) (*dbmodel.OAuth2User, error)
	FindByProvider(ctx context.Context, provider, sub string) (*dbmodel.OAuth2User, error)
	Update(ctx context.Context, user *dbmodel.OAuth2User) error
}

type oauth2UserRepo struct {
	db *gorm.DB
}

// NewOAuth2UserRepo 创建 OAuth2UserRepo
func NewOAuth2UserRepo(db *gorm.DB) OAuth2UserRepo {
	return &oauth2UserRepo{db: db}
}

func (r *oauth2UserRepo) Create(ctx context.Context, user *dbmodel.OAuth2User) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(user).Error; err != nil {
		return fmt.Errorf("create oauth2 user: %w", err)
	}
	return nil
}

func (r *oauth2UserRepo) FindByID(ctx context.Context, id uuid.UUID) (*dbmodel.OAuth2User, error) {
	var user dbmodel.OAuth2User
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&user, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("find oauth2 user %s: %w", id, ErrOAuth2UserNotFound)
		}
		return nil, fmt.Errorf("find oauth2 user %s: %w", id, err)
	}
	return &user, nil
}

func (r *oauth2UserRepo) FindByProvider(ctx context.Context, provider, sub string) (*dbmodel.OAuth2User, error) {
	var user dbmodel.OAuth2User
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("provider = ? AND sub = ?", provider, sub).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("find oauth2 user by provider %s/%s: %w", provider, sub, ErrOAuth2UserNotFound)
		}
		return nil, fmt.Errorf("find oauth2 user by provider %s/%s: %w", provider, sub, err)
	}
	return &user, nil
}

func (r *oauth2UserRepo) Update(ctx context.Context, user *dbmodel.OAuth2User) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Save(user).Error; err != nil {
		return fmt.Errorf("update oauth2 user %s: %w", user.ID, err)
	}
	return nil
}
