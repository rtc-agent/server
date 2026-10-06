package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"

	"gorm.io/gorm"
)

// OAuth2UserRepo provides OAuth2 user persistence operations.
type OAuth2UserRepo interface {
	// Create stores a new OAuth2 user record.
	Create(ctx context.Context, user *model.OAuth2User) error
	// FindByID looks up an OAuth2 user by ID.
	FindByID(ctx context.Context, id uuid.UUID) (*model.OAuth2User, error)
	// FindByProvider looks up an OAuth2 user by provider name and subject.
	FindByProvider(ctx context.Context, provider, sub string) (*model.OAuth2User, error)
	// FindOrCreate looks up an OAuth2 user by provider+sub, creating if not found.
	// Returns the user and a boolean indicating whether it was newly created.
	// Concurrency safe: uses "find + unique constraint fallback" pattern.
	FindOrCreate(ctx context.Context, user *model.OAuth2User) (*model.OAuth2User, bool, error)
	// Update persists changes to an OAuth2 user record.
	Update(ctx context.Context, user *model.OAuth2User) error
	// ListWithFilters returns a paginated list of users with optional filters.
	ListWithFilters(ctx context.Context, filter OAuth2UserFilter) ([]*model.OAuth2User, int64, error)
	// IsUserBanned checks if a user account is banned.
	IsUserBanned(ctx context.Context, userID uuid.UUID) (bool, error)
}

// OAuth2UserFilter holds filter criteria for listing users.
type OAuth2UserFilter struct {
	Status   string // "active" or "banned", empty means all
	Search   string // search in email and name
	Page     int
	PageSize int
}

type oauth2UserRepo struct {
	db *gorm.DB
}

// NewOAuth2UserRepo creates a new OAuth2UserRepo.
func NewOAuth2UserRepo(db *gorm.DB) OAuth2UserRepo {
	return &oauth2UserRepo{db: db}
}

func (r *oauth2UserRepo) Create(ctx context.Context, user *model.OAuth2User) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(user).Error; err != nil {
		return fmt.Errorf("create oauth2 user: %w", err)
	}
	return nil
}

func (r *oauth2UserRepo) FindByID(ctx context.Context, id uuid.UUID) (*model.OAuth2User, error) {
	var user model.OAuth2User
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&user, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("find oauth2 user %s: %w", id, ErrOAuth2UserNotFound)
		}
		return nil, fmt.Errorf("find oauth2 user %s: %w", id, err)
	}
	return &user, nil
}

func (r *oauth2UserRepo) FindByProvider(ctx context.Context, provider, sub string) (*model.OAuth2User, error) {
	var user model.OAuth2User
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("provider = ? AND sub = ?", provider, sub).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("find oauth2 user by provider %s/%s: %w", provider, sub, ErrOAuth2UserNotFound)
		}
		return nil, fmt.Errorf("find oauth2 user by provider %s/%s: %w", provider, sub, err)
	}
	return &user, nil
}

func (r *oauth2UserRepo) Update(ctx context.Context, user *model.OAuth2User) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Save(user).Error; err != nil {
		return fmt.Errorf("update oauth2 user %s: %w", user.ID, err)
	}
	return nil
}

// FindOrCreate looks up an OAuth2 user by provider+sub, creating if not found.
// Concurrency safe: uses "find + unique constraint fallback" pattern (23505).
func (r *oauth2UserRepo) FindOrCreate(ctx context.Context, user *model.OAuth2User) (*model.OAuth2User, bool, error) {
	// Fast path: look up existing record.
	existing, err := r.FindByProvider(ctx, user.Provider, user.Sub)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrOAuth2UserNotFound) {
		return nil, false, fmt.Errorf("findOrCreate oauth2 user: %w", err)
	}

	// Not found: attempt to create.
	if err := r.Create(ctx, user); err != nil {
		// Check for unique constraint violation (PostgreSQL 23505).
		if IsDuplicateKeyError(err) {
			// Concurrent creation: re-lookup.
			existing, err = r.FindByProvider(ctx, user.Provider, user.Sub)
			if err != nil {
				return nil, false, fmt.Errorf("findOrCreate re-find after conflict: %w", err)
			}
			return existing, false, nil
		}
		return nil, false, fmt.Errorf("findOrCreate create oauth2 user: %w", err)
	}
	return user, true, nil
}

// ListWithFilters returns a paginated list of users with optional filters.
func (r *oauth2UserRepo) ListWithFilters(ctx context.Context, filter OAuth2UserFilter) ([]*model.OAuth2User, int64, error) {
	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.PageSize < 1 || filter.PageSize > 100 {
		filter.PageSize = 20
	}

	query := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.OAuth2User{})

	// Apply status filter
	switch filter.Status {
	case "banned":
		query = query.Where("banned_at IS NOT NULL")
	case "active":
		query = query.Where("banned_at IS NULL")
	}

	// Apply search filter
	if filter.Search != "" {
		searchPattern := "%" + filter.Search + "%"
		query = query.Where("email ILIKE ? OR name ILIKE ?", searchPattern, searchPattern)
	}

	// Count total
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count oauth2 users: %w", err)
	}

	// Fetch paginated results
	var users []*model.OAuth2User
	offset := (filter.Page - 1) * filter.PageSize
	if err := query.Order("created_at DESC").Offset(offset).Limit(filter.PageSize).Find(&users).Error; err != nil {
		return nil, 0, fmt.Errorf("list oauth2 users: %w", err)
	}

	return users, total, nil
}

// IsUserBanned checks if a user account is banned.
func (r *oauth2UserRepo) IsUserBanned(ctx context.Context, userID uuid.UUID) (bool, error) {
	var user model.OAuth2User
	err := DBFromContext(ctx, r.db).WithContext(ctx).Select("banned_at").First(&user, "id = ?", userID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil // User not found, not banned
		}
		return false, fmt.Errorf("check user ban status %s: %w", userID, err)
	}
	return user.BannedAt != nil, nil
}
