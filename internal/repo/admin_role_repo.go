// Package repo provides data access layer (Repository) implementations.
package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
)

// AdminRoleRepo provides admin role persistence operations.
type AdminRoleRepo interface {
	// Create stores a new admin role.
	Create(ctx context.Context, role *model.AdminRole) error
	// GetByID looks up an admin role by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*model.AdminRole, error)
	// GetByName looks up an admin role by name.
	GetByName(ctx context.Context, name string) (*model.AdminRole, error)
	// List returns all admin roles.
	List(ctx context.Context) ([]*model.AdminRole, error)
	// ListPaginated returns admin roles with pagination support.
	ListPaginated(ctx context.Context, page, pageSize int) ([]*model.AdminRole, int64, error)
	// Update persists changes to an admin role.
	Update(ctx context.Context, role *model.AdminRole) error
	// SoftDelete disables an admin role by setting is_enabled=false.
	// This follows the spec: admin roles are never hard-deleted to preserve audit trail
	// and foreign key references in admin_user_roles / admin_casbin_rule.
	SoftDelete(ctx context.Context, id uuid.UUID) error
	// Count returns total number of admin roles.
	Count(ctx context.Context) (int64, error)
	// GetByIDs returns admin roles matching the given IDs. Missing IDs are silently omitted.
	GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*model.AdminRole, error)
}

type adminRoleRepo struct {
	db *gorm.DB
}

// NewAdminRoleRepo creates a new AdminRoleRepo.
func NewAdminRoleRepo(db *gorm.DB) AdminRoleRepo {
	return &adminRoleRepo{db: db}
}

func (r *adminRoleRepo) Create(ctx context.Context, role *model.AdminRole) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(role).Error; err != nil {
		if IsDuplicateKeyError(err) {
			return fmt.Errorf("create role: %w", ErrRoleNameExists)
		}
		return fmt.Errorf("create role: %w", err)
	}
	return nil
}

func (r *adminRoleRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.AdminRole, error) {
	var role model.AdminRole
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&role, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get role %s: %w", id, ErrRoleNotFound)
		}
		return nil, fmt.Errorf("get role %s: %w", id, err)
	}
	return &role, nil
}

func (r *adminRoleRepo) GetByName(ctx context.Context, name string) (*model.AdminRole, error) {
	var role model.AdminRole
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("name = ?", name).First(&role).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get role by name %s: %w", name, ErrRoleNotFound)
		}
		return nil, fmt.Errorf("get role by name %s: %w", name, err)
	}
	return &role, nil
}

func (r *adminRoleRepo) List(ctx context.Context) ([]*model.AdminRole, error) {
	var roles []*model.AdminRole
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Order("created_at ASC").Find(&roles).Error; err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	return roles, nil
}

func (r *adminRoleRepo) ListPaginated(ctx context.Context, page, pageSize int) ([]*model.AdminRole, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	var total int64
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.AdminRole{}).Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count roles: %w", err)
	}

	var roles []*model.AdminRole
	offset := (page - 1) * pageSize
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Order("created_at ASC").
		Offset(offset).
		Limit(pageSize).
		Find(&roles).Error; err != nil {
		return nil, 0, fmt.Errorf("list roles paginated: %w", err)
	}

	return roles, total, nil
}

func (r *adminRoleRepo) Update(ctx context.Context, role *model.AdminRole) error {
	// Optimistic locking: only update if version matches current DB version.
	// This follows the same pattern as GORM's optimisticlock plugin but uses int64
	// instead of sql.NullInt64 for simpler API. The WHERE clause ensures we only
	// update if no other writer has modified the row since we read it.
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.AdminRole{}).
		Where("id = ? AND version = ?", role.ID, role.Version).
		Updates(map[string]any{
			"name":         role.Name,
			"display_name": role.DisplayName,
			"description":  role.Description,
			"is_enabled":   role.IsEnabled,
			"version":      role.Version + 1,
			"updated_at":   role.UpdatedAt,
		})
	if result.Error != nil {
		if IsDuplicateKeyError(result.Error) {
			return fmt.Errorf("update role: %w", ErrRoleNameExists)
		}
		return fmt.Errorf("update role %s: %w", role.ID, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update role %s: %w", role.ID, ErrConflict)
	}
	role.Version++
	return nil
}

// SoftDelete performs a soft delete by setting is_enabled=false.
// This follows the spec: admin roles are never hard-deleted to preserve audit trail
// and foreign key references in admin_user_roles / admin_casbin_rule.
func (r *adminRoleRepo) SoftDelete(ctx context.Context, id uuid.UUID) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.AdminRole{}).
		Where("id = ?", id).
		Update("is_enabled", false)
	if result.Error != nil {
		return fmt.Errorf("soft delete role %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("soft delete role %s: %w", id, ErrRoleNotFound)
	}
	return nil
}

func (r *adminRoleRepo) Count(ctx context.Context) (int64, error) {
	var count int64
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.AdminRole{}).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count roles: %w", err)
	}
	return count, nil
}

func (r *adminRoleRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*model.AdminRole, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var roles []*model.AdminRole
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Where("id IN ?", ids).Find(&roles).Error; err != nil {
		return nil, fmt.Errorf("get roles by ids: %w", err)
	}
	return roles, nil
}
