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

// UserRepo provides user persistence operations.
type UserRepo interface {
	// Create stores a new user record.
	Create(ctx context.Context, user *model.User) error
	// GetByEmail looks up a user by email address.
	GetByEmail(ctx context.Context, email string) (*model.User, error)
	// GetByID looks up a user by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*model.User, error)
	// GetByProviderAndSubject looks up a user by OAuth2 provider and subject ID.
	GetByProviderAndSubject(ctx context.Context, provider, subject string) (*model.User, error)
	// Update persists changes to a user record.
	Update(ctx context.Context, user *model.User) error
	// GetByIDs returns users matching the given IDs. Missing IDs are silently omitted.
	GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*model.User, error)
	// List returns a paginated list of users with total count.
	List(ctx context.Context, page, pageSize int) ([]*model.User, int64, error)
}

type userRepo struct {
	db *gorm.DB
}

// NewUserRepo creates a new UserRepo.
func NewUserRepo(db *gorm.DB) UserRepo {
	return &userRepo{db: db}
}

func (r *userRepo) Create(ctx context.Context, user *model.User) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(user).Error; err != nil {
		// Check for unique constraint violation (duplicate email)
		if IsDuplicateKeyError(err) {
			return fmt.Errorf("create user: %w", ErrDuplicateEmail)
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (r *userRepo) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	var user model.User
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("email = ?", email).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get user by email %s: %w", email, ErrNotFound)
		}
		return nil, fmt.Errorf("get user by email %s: %w", email, err)
	}
	return &user, nil
}

func (r *userRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.User, error) {
	var user model.User
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&user, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get user %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get user %s: %w", id, err)
	}
	return &user, nil
}

// GetByProviderAndSubject looks up a user by OAuth2 provider and subject ID.
func (r *userRepo) GetByProviderAndSubject(ctx context.Context, provider, subject string) (*model.User, error) {
	var user model.User
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("provider = ? AND provider_subject = ?", provider, subject).
		First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get user by provider %s subject %s: %w", provider, subject, ErrNotFound)
		}
		return nil, fmt.Errorf("get user by provider %s subject %s: %w", provider, subject, err)
	}
	return &user, nil
}

func (r *userRepo) Update(ctx context.Context, user *model.User) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Save(user).Error; err != nil {
		return fmt.Errorf("update user %s: %w", user.ID, err)
	}
	return nil
}

func (r *userRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*model.User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var users []*model.User
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, fmt.Errorf("get users by ids: %w", err)
	}
	return users, nil
}

func (r *userRepo) List(ctx context.Context, page, pageSize int) ([]*model.User, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	var total int64
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.User{}).Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}

	var users []*model.User
	offset := (page - 1) * pageSize
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Order("created_at DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&users).Error; err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}

	return users, total, nil
}

// ErrDuplicateEmail is returned when attempting to create a user with an email that already exists.
var ErrDuplicateEmail = errors.New("email already exists")
