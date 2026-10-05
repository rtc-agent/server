// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
)

// System role names (used for special handling in business logic).
const (
	SystemRoleAdmin = "admin" // Admin role name - cannot be deleted
)

// RoleUsecase handles role management operations.
type RoleUsecase struct {
	roleRepo        repo.RoleRepo
	userRoleRepo    repo.UserRoleRepo
	auditLogRepo    repo.AuditLogRepo
	enforcer        *auth.CasbinEnforcer
	policyPublisher PolicyPublisher // optional multi-instance sync (may be nil)
}

// NewRoleUsecase creates a new RoleUsecase.
func NewRoleUsecase(
	roleRepo repo.RoleRepo,
	userRoleRepo repo.UserRoleRepo,
	auditLogRepo repo.AuditLogRepo,
	enforcer *auth.CasbinEnforcer,
) *RoleUsecase {
	return &RoleUsecase{
		roleRepo:     roleRepo,
		userRoleRepo: userRoleRepo,
		auditLogRepo: auditLogRepo,
		enforcer:     enforcer,
	}
}

// SetPolicyPublisher injects optional policy publisher for multi-instance sync.
func (uc *RoleUsecase) SetPolicyPublisher(pp PolicyPublisher) {
	uc.policyPublisher = pp
}

// CreateRoleInput defines the input for creating a role.
type CreateRoleInput struct {
	Name        string
	DisplayName string
	Description string
}

// CreateRole creates a new role.
func (uc *RoleUsecase) CreateRole(ctx context.Context, input CreateRoleInput, operatorID uuid.UUID, operatorIP string) (*model.Role, error) {
	role := &model.Role{
		Name:        input.Name,
		DisplayName: input.DisplayName,
		Description: input.Description,
		IsSystem:    false,
		IsEnabled:   true,
	}

	if err := uc.roleRepo.Create(ctx, role); err != nil {
		if errors.Is(err, repo.ErrRoleNameExists) {
			return nil, err
		}
		return nil, fmt.Errorf("create role: %w", err)
	}

	// Audit log
	if err := uc.auditLogRepo.Create(ctx, repo.NewAuditLog(
		operatorID, operatorIP, "create_role", "role", role.ID,
		map[string]any{"name": role.Name, "display_name": role.DisplayName},
	)); err != nil {
		logger.Error(ctx, "role.create_audit_log_failed", zap.Error(err))
	}

	return role, nil
}

// UpdateRoleInput defines the input for updating a role.
type UpdateRoleInput struct {
	DisplayName *string
	Description *string
	IsEnabled   *bool
}

// UpdateRole updates an existing role.
func (uc *RoleUsecase) UpdateRole(ctx context.Context, roleID uuid.UUID, input UpdateRoleInput, operatorID uuid.UUID, operatorIP string) (*model.Role, error) {
	role, err := uc.roleRepo.GetByID(ctx, roleID)
	if err != nil {
		return nil, err
	}

	if input.DisplayName != nil {
		role.DisplayName = *input.DisplayName
	}
	if input.Description != nil {
		role.Description = *input.Description
	}
	if input.IsEnabled != nil {
		role.IsEnabled = *input.IsEnabled
	}

	if err := uc.roleRepo.Update(ctx, role); err != nil {
		if errors.Is(err, repo.ErrRoleNameExists) {
			return nil, err
		}
		return nil, fmt.Errorf("update role: %w", err)
	}

	// Audit log
	if err := uc.auditLogRepo.Create(ctx, repo.NewAuditLog(
		operatorID, operatorIP, "update_role", "role", role.ID,
		map[string]any{"display_name": role.DisplayName, "is_enabled": role.IsEnabled},
	)); err != nil {
		logger.Error(ctx, "role.update_audit_log_failed", zap.Error(err))
	}

	return role, nil
}

// DeleteRole disables a role, cleaning up associated policies and assignments.
//
// Per spec, this performs:
//  1. System role protection (is_system=true cannot be deleted)
//  2. Soft delete: set is_enabled=false
//  3. Cascade delete: remove all user_roles assignments for this role
//  4. Remove Casbin p policies (role permissions) and g policies (user-role groupings)
func (uc *RoleUsecase) DeleteRole(ctx context.Context, roleID uuid.UUID, operatorID uuid.UUID, operatorIP string) error {
	role, err := uc.roleRepo.GetByID(ctx, roleID)
	if err != nil {
		return err
	}

	// System roles cannot be deleted (includes admin role which is marked as IsSystem=true in bootstrap)
	if role.IsSystem {
		return repo.ErrCannotDeleteSystemRole
	}

	// Step 1: Remove Casbin p policies (role permissions like "roleID resource action")
	if err := uc.enforcer.RemoveFilteredPolicy(ctx, 0, role.ID.String()); err != nil {
		return fmt.Errorf("remove casbin p policies for role: %w", err)
	}

	// Step 2: Remove Casbin g policies (user-role groupings like "userID roleID")
	if err := uc.enforcer.RemoveFilteredGroupingPolicy(ctx, 1, role.ID.String()); err != nil {
		return fmt.Errorf("remove casbin g policies for role: %w", err)
	}

	// Step 3: Cascade delete user_roles assignments for this role
	if err := uc.userRoleRepo.DeleteByRoleID(ctx, roleID); err != nil {
		return fmt.Errorf("cascade delete user_roles for role: %w", err)
	}

	// Step 4: Soft delete the role (set is_enabled=false)
	if err := uc.roleRepo.Delete(ctx, roleID); err != nil {
		return fmt.Errorf("soft delete role: %w", err)
	}

	// Publish policy change for multi-instance sync
	if uc.policyPublisher != nil {
		if err := uc.policyPublisher.PublishChange(ctx, "p", "remove_role_policies", [][]string{{role.ID.String()}}); err != nil {
			logger.Warn(ctx, "role.publish_p_policy_change_failed", zap.Error(err))
		}
		if err := uc.policyPublisher.PublishChange(ctx, "g", "remove_role_grouping_policies", [][]string{{role.ID.String()}}); err != nil {
			logger.Warn(ctx, "role.publish_g_policy_change_failed", zap.Error(err))
		}
	}

	// Audit log
	if err := uc.auditLogRepo.Create(ctx, repo.NewAuditLog(
		operatorID, operatorIP, "delete_role", "role", roleID,
		map[string]any{"name": role.Name},
	)); err != nil {
		logger.Error(ctx, "role.delete_audit_log_failed", zap.Error(err))
	}

	return nil
}

// GetRole returns a role by ID.
func (uc *RoleUsecase) GetRole(ctx context.Context, roleID uuid.UUID) (*model.Role, error) {
	return uc.roleRepo.GetByID(ctx, roleID)
}

// ListRoles returns all roles.
func (uc *RoleUsecase) ListRoles(ctx context.Context) ([]*model.Role, error) {
	return uc.roleRepo.List(ctx)
}

// ListRolesPaginated returns roles with pagination support.
// Returns (roles, total, error).
func (uc *RoleUsecase) ListRolesPaginated(ctx context.Context, page, pageSize int) ([]*model.Role, int64, error) {
	return uc.roleRepo.ListPaginated(ctx, page, pageSize)
}

// GetRolePolicies returns all permission policies for a role.
func (uc *RoleUsecase) GetRolePolicies(ctx context.Context, roleID uuid.UUID) ([][]string, error) {
	_, err := uc.roleRepo.GetByID(ctx, roleID)
	if err != nil {
		return nil, err
	}
	return uc.enforcer.GetPoliciesForRole(roleID.String()), nil
}
