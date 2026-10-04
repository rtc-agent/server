// Package repo provides data access layer (Repository) implementations.
package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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
		if isDuplicateKeyError(err) {
			return fmt.Errorf("create user: %w", ErrDuplicateEmail)
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// isDuplicateKeyError checks whether the error is a PostgreSQL unique
// constraint violation (code 23505).
func isDuplicateKeyError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
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

// ErrDuplicateEmail is returned when attempting to create a user with an email that already exists.
var ErrDuplicateEmail = errors.New("email already exists")
