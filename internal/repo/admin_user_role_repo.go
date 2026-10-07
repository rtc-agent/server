// Package repo provides data access layer (Repository) implementations.
package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/rtc-agent/server/internal/model"
)

// AdminUserRoleRepo provides admin user-role association persistence operations.
type AdminUserRoleRepo interface {
	// Create assigns a role to a user.
	Create(ctx context.Context, userID, roleID uuid.UUID) error
	// CreateBatch assigns multiple roles to a user atomically in a transaction.
	// Returns error if any assignment fails, rolling back all changes.
	CreateBatch(ctx context.Context, userID uuid.UUID, roleIDs []uuid.UUID) error
	// Delete removes a role assignment.
	Delete(ctx context.Context, userID, roleID uuid.UUID) error
	// DeleteWithAdminCheck atomically checks if removing an admin role is safe.
	// Uses SELECT FOR UPDATE to prevent race conditions.
	// Returns ErrCannotRemoveLastAdmin if this would remove the last admin.
	DeleteWithAdminCheck(ctx context.Context, userID, adminRoleID uuid.UUID) error
	// ListByUserID returns all role IDs assigned to a user.
	ListByUserID(ctx context.Context, userID uuid.UUID) ([]model.AdminUserRole, error)
	// ListByRoleID returns all user IDs assigned to a role.
	ListByRoleID(ctx context.Context, roleID uuid.UUID) ([]model.AdminUserRole, error)
	// CountByRoleID counts how many users are assigned a given role.
	CountByRoleID(ctx context.Context, roleID uuid.UUID) (int64, error)
	// Exists checks whether a specific user-role assignment exists.
	Exists(ctx context.Context, userID, roleID uuid.UUID) (bool, error)
	// DeleteByRoleID removes all user assignments for a role.
	DeleteByRoleID(ctx context.Context, roleID uuid.UUID) error
}

type adminUserRoleRepo struct {
	db *gorm.DB
}

// NewAdminUserRoleRepo creates a new AdminUserRoleRepo.
func NewAdminUserRoleRepo(db *gorm.DB) AdminUserRoleRepo {
	return &adminUserRoleRepo{db: db}
}

func (r *adminUserRoleRepo) Create(ctx context.Context, userID, roleID uuid.UUID) error {
	ur := &model.AdminUserRole{UserID: userID, RoleID: roleID}
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(ur).Error; err != nil {
		return fmt.Errorf("create user role (user=%s, role=%s): %w", userID, roleID, err)
	}
	return nil
}

// CreateBatch assigns multiple roles to a user atomically in a transaction.
// If any assignment fails, all changes are rolled back.
func (r *adminUserRoleRepo) CreateBatch(ctx context.Context, userID uuid.UUID, roleIDs []uuid.UUID) error {
	if len(roleIDs) == 0 {
		return nil
	}

	return DBFromContext(ctx, r.db).Transaction(func(tx *gorm.DB) error {
		for _, roleID := range roleIDs {
			ur := &model.AdminUserRole{UserID: userID, RoleID: roleID}
			if err := tx.WithContext(ctx).Create(ur).Error; err != nil {
				return fmt.Errorf("create user role (user=%s, role=%s): %w", userID, roleID, err)
			}
		}
		return nil
	})
}

// DeleteWithAdminCheck atomically checks if removing an admin role is safe and deletes if so.
// Uses a transaction with row-level locking to prevent race conditions.
// Returns ErrCannotRemoveLastAdmin if this would remove the last admin role assignment.
func (r *adminUserRoleRepo) DeleteWithAdminCheck(ctx context.Context, userID, adminRoleID uuid.UUID) error {
	return DBFromContext(ctx, r.db).Transaction(func(tx *gorm.DB) error {
		// First, lock all admin role assignments with SELECT FOR UPDATE
		// Note: PostgreSQL's FOR UPDATE locks actual rows, not aggregate results
		var adminRoles []model.AdminUserRole
		if err := tx.WithContext(ctx).
			Where("role_id = ?", adminRoleID).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Find(&adminRoles).Error; err != nil {
			return fmt.Errorf("lock admin roles: %w", err)
		}

		count := int64(len(adminRoles))

		// Check if this is the last admin
		if count <= 1 {
			// Verify the target user actually has this role
			var exists int64
			if err := tx.WithContext(ctx).
				Model(&model.AdminUserRole{}).
				Where("user_id = ? AND role_id = ?", userID, adminRoleID).
				Count(&exists).Error; err != nil {
				return fmt.Errorf("check user admin role: %w", err)
			}
			if exists > 0 {
				return ErrCannotRemoveLastAdmin
			}
		}

		// Safe to delete
		result := tx.WithContext(ctx).
			Where("user_id = ? AND role_id = ?", userID, adminRoleID).
			Delete(&model.AdminUserRole{})
		if result.Error != nil {
			return fmt.Errorf("delete user role: %w", result.Error)
		}
		return nil
	})
}

func (r *adminUserRoleRepo) Delete(ctx context.Context, userID, roleID uuid.UUID) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ? AND role_id = ?", userID, roleID).
		Delete(&model.AdminUserRole{})
	if result.Error != nil {
		return fmt.Errorf("delete user role (user=%s, role=%s): %w", userID, roleID, result.Error)
	}
	return nil
}

func (r *adminUserRoleRepo) ListByUserID(ctx context.Context, userID uuid.UUID) ([]model.AdminUserRole, error) {
	var urs []model.AdminUserRole
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ?", userID).
		Find(&urs).Error; err != nil {
		return nil, fmt.Errorf("list user roles for user %s: %w", userID, err)
	}
	return urs, nil
}

func (r *adminUserRoleRepo) ListByRoleID(ctx context.Context, roleID uuid.UUID) ([]model.AdminUserRole, error) {
	var urs []model.AdminUserRole
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("role_id = ?", roleID).
		Find(&urs).Error; err != nil {
		return nil, fmt.Errorf("list user roles for role %s: %w", roleID, err)
	}
	return urs, nil
}

func (r *adminUserRoleRepo) CountByRoleID(ctx context.Context, roleID uuid.UUID) (int64, error) {
	var count int64
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.AdminUserRole{}).
		Where("role_id = ?", roleID).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count users for role %s: %w", roleID, err)
	}
	return count, nil
}

func (r *adminUserRoleRepo) Exists(ctx context.Context, userID, roleID uuid.UUID) (bool, error) {
	var count int64
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.AdminUserRole{}).
		Where("user_id = ? AND role_id = ?", userID, roleID).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("check user role exists: %w", err)
	}
	return count > 0, nil
}

func (r *adminUserRoleRepo) DeleteByRoleID(ctx context.Context, roleID uuid.UUID) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("role_id = ?", roleID).
		Delete(&model.AdminUserRole{}).Error; err != nil {
		return fmt.Errorf("delete user roles for role %s: %w", roleID, err)
	}
	return nil
}

// IsUserRoleNotFound checks whether the error represents a missing user-role record.
// Currently used when deleting: gorm doesn't return ErrRecordNotFound for deletes,
// so callers typically check RowsAffected instead. Reserved for future use.
func IsUserRoleNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
