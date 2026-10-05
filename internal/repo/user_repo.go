// Package repo provides data access layer (Repository) implementations.
package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"

	"gorm.io/gorm"
)

// AdminUserRepo provides admin user persistence operations.
type AdminUserRepo interface {
	// Create stores a new admin user record.
	Create(ctx context.Context, user *model.AdminUser) error
	// GetByEmail looks up an admin user by email address.
	GetByEmail(ctx context.Context, email string) (*model.AdminUser, error)
	// GetByID looks up an admin user by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*model.AdminUser, error)
	// GetByProviderAndSubject looks up an admin user by OAuth2 provider and subject ID.
	GetByProviderAndSubject(ctx context.Context, provider, subject string) (*model.AdminUser, error)
	// Update persists changes to an admin user record.
	Update(ctx context.Context, user *model.AdminUser) error
	// GetByIDs returns admin users matching the given IDs. Missing IDs are silently omitted.
	GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*model.AdminUser, error)
	// List returns a paginated list of admin users with total count.
	List(ctx context.Context, page, pageSize int) ([]*model.AdminUser, int64, error)
}

type adminUserRepo struct {
	db *gorm.DB
}

// NewAdminUserRepo creates a new AdminUserRepo.
func NewAdminUserRepo(db *gorm.DB) AdminUserRepo {
	return &adminUserRepo{db: db}
}

func (r *adminUserRepo) Create(ctx context.Context, user *model.AdminUser) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(user).Error; err != nil {
		// Check for unique constraint violation (duplicate email)
		if IsDuplicateKeyError(err) {
			return fmt.Errorf("create admin user: %w", ErrDuplicateEmail)
		}
		return fmt.Errorf("create admin user: %w", err)
	}
	return nil
}

func (r *adminUserRepo) GetByEmail(ctx context.Context, email string) (*model.AdminUser, error) {
	var user model.AdminUser
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("email = ?", email).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get admin user by email %s: %w", email, ErrNotFound)
		}
		return nil, fmt.Errorf("get admin user by email %s: %w", email, err)
	}
	return &user, nil
}

func (r *adminUserRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.AdminUser, error) {
	var user model.AdminUser
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&user, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get admin user %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get admin user %s: %w", id, err)
	}
	return &user, nil
}

// GetByProviderAndSubject looks up an admin user by OAuth2 provider and subject ID.
func (r *adminUserRepo) GetByProviderAndSubject(ctx context.Context, provider, subject string) (*model.AdminUser, error) {
	var user model.AdminUser
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("provider = ? AND provider_subject = ?", provider, subject).
		First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get admin user by provider %s subject %s: %w", provider, subject, ErrNotFound)
		}
		return nil, fmt.Errorf("get admin user by provider %s subject %s: %w", provider, subject, err)
	}
	return &user, nil
}

func (r *adminUserRepo) Update(ctx context.Context, user *model.AdminUser) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Save(user).Error; err != nil {
		return fmt.Errorf("update admin user %s: %w", user.ID, err)
	}
	return nil
}

func (r *adminUserRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*model.AdminUser, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var users []*model.AdminUser
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, fmt.Errorf("get admin users by ids: %w", err)
	}
	return users, nil
}

func (r *adminUserRepo) List(ctx context.Context, page, pageSize int) ([]*model.AdminUser, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	var total int64
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.AdminUser{}).Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count admin users: %w", err)
	}

	var users []*model.AdminUser
	offset := (page - 1) * pageSize
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Order("created_at DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&users).Error; err != nil {
		return nil, 0, fmt.Errorf("list admin users: %w", err)
	}

	return users, total, nil
}

// ErrDuplicateEmail is returned when attempting to create a user with an email that already exists.
var ErrDuplicateEmail = errors.New("email already exists")
