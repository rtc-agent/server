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

// AdminUserRoleUsecase handles admin user-role association operations.
type AdminUserRoleUsecase struct {
	adminUserRepo     repo.AdminUserRepo
	adminRoleRepo     repo.AdminRoleRepo
	adminUserRoleRepo repo.AdminUserRoleRepo
	enforcer          *auth.CasbinEnforcer
	auditLogRepo      repo.AuditLogRepo
	policyPublisher   PolicyPublisher // optional multi-instance sync (may be nil)
}

// NewAdminUserRoleUsecase creates a new AdminUserRoleUsecase.
func NewAdminUserRoleUsecase(
	adminUserRepo repo.AdminUserRepo,
	adminRoleRepo repo.AdminRoleRepo,
	adminUserRoleRepo repo.AdminUserRoleRepo,
	enforcer *auth.CasbinEnforcer,
	auditLogRepo repo.AuditLogRepo,
) *AdminUserRoleUsecase {
	return &AdminUserRoleUsecase{
		adminUserRepo:     adminUserRepo,
		adminRoleRepo:     adminRoleRepo,
		adminUserRoleRepo: adminUserRoleRepo,
		enforcer:          enforcer,
		auditLogRepo:      auditLogRepo,
	}
}

// SetPolicyPublisher injects optional policy publisher for multi-instance sync.
func (uc *AdminUserRoleUsecase) SetPolicyPublisher(pp PolicyPublisher) {
	uc.policyPublisher = pp
}

// AssignRolesInput defines the input for batch assigning roles to a user.
type AssignRolesInput struct {
	UserID  uuid.UUID
	RoleIDs []string
}

// validateRolesForAssignment validates that all role IDs exist and are enabled.
// Returns the validated role objects and their UUIDs.
func (uc *AdminUserRoleUsecase) validateRolesForAssignment(ctx context.Context, roleIDStrs []string) ([]*model.AdminRole, []uuid.UUID, error) {
	roles := make([]*model.AdminRole, 0, len(roleIDStrs))
	roleIDs := make([]uuid.UUID, 0, len(roleIDStrs))

	for _, roleIDStr := range roleIDStrs {
		roleID, err := uuid.Parse(roleIDStr)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid role ID format: %s", roleIDStr)
		}

		role, err := uc.adminRoleRepo.GetByID(ctx, roleID)
		if err != nil {
			if repo.IsNotFound(err) {
				return nil, nil, fmt.Errorf("%w: %s", repo.ErrRoleNotFound, roleIDStr)
			}
			return nil, nil, fmt.Errorf("get role: %w", err)
		}
		if !role.IsEnabled {
			return nil, nil, fmt.Errorf("%w: %s", repo.ErrRoleDisabled, role.Name)
		}
		roles = append(roles, role)
		roleIDs = append(roleIDs, roleID)
	}
	return roles, roleIDs, nil
}

// filterNewRoles filters out roles that are already assigned to the admin user.
func (uc *AdminUserRoleUsecase) filterNewRoles(ctx context.Context, userID uuid.UUID, roles []*model.AdminRole, roleIDs []uuid.UUID) ([]*model.AdminRole, []uuid.UUID, error) {
	newRoles := make([]*model.AdminRole, 0, len(roles))
	newRoleIDs := make([]uuid.UUID, 0, len(roleIDs))

	for i, role := range roles {
		exists, err := uc.adminUserRoleRepo.Exists(ctx, userID, role.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("check role assignment: %w", err)
		}
		if !exists {
			newRoles = append(newRoles, role)
			newRoleIDs = append(newRoleIDs, roleIDs[i])
		}
	}
	return newRoles, newRoleIDs, nil
}

// AssignRoles assigns roles to an admin user in a transaction (P0 #3 fix).
// If any role assignment fails, the entire batch is rolled back.
// Casbin policies are updated synchronously - if Casbin sync fails, the entire operation fails.
func (uc *AdminUserRoleUsecase) AssignRoles(ctx context.Context, input AssignRolesInput, operatorID uuid.UUID, operatorIP string) error {
	// Verify admin user exists
	if _, err := uc.adminUserRepo.GetByID(ctx, input.UserID); err != nil {
		if repo.IsNotFound(err) {
			return ErrAdminUserNotFound
		}
		return fmt.Errorf("get admin user: %w", err)
	}

	// Validate all roles exist and are enabled
	rolesToAssign, roleIDsToAssign, err := uc.validateRolesForAssignment(ctx, input.RoleIDs)
	if err != nil {
		return err
	}

	// Filter out already-assigned roles to avoid duplicate key errors
	newRoles, newRoleIDs, err := uc.filterNewRoles(ctx, input.UserID, rolesToAssign, roleIDsToAssign)
	if err != nil {
		return err
	}

	// Create all assignments atomically in a transaction
	if len(newRoleIDs) > 0 {
		if err := uc.adminUserRoleRepo.CreateBatch(ctx, input.UserID, newRoleIDs); err != nil {
			return fmt.Errorf("batch create role assignments: %w", err)
		}
	}

	// Add Casbin grouping policies synchronously
	for _, role := range newRoles {
		if err := uc.enforcer.AddGroupingPolicy(ctx, input.UserID.String(), role.ID.String()); err != nil {
			return fmt.Errorf("add casbin grouping policy for user %s role %s: %w",
				input.UserID.String(), role.ID.String(), err)
		}
	}

	// Publish policy change for multi-instance sync
	if uc.policyPublisher != nil && len(newRoleIDs) > 0 {
		rules := make([][]string, 0, len(newRoleIDs))
		for _, roleID := range newRoleIDs {
			rules = append(rules, []string{input.UserID.String(), roleID.String()})
		}
		if err := uc.policyPublisher.PublishChange(ctx, "g", "add_grouping_policies", rules); err != nil {
			logger.Warn(ctx, "admin_user_role.publish_policy_change_failed", zap.Error(err))
		}
	}

	// Audit log
	if uc.auditLogRepo != nil {
		roleNames := make([]string, 0, len(newRoles))
		for _, role := range newRoles {
			roleNames = append(roleNames, role.Name)
		}
		if err := uc.auditLogRepo.Create(ctx, repo.NewAuditLog(
			operatorID, operatorIP, "assign_roles", "admin_user", input.UserID,
			map[string]any{"role_ids": newRoleIDs, "role_names": roleNames},
		)); err != nil {
			logger.Error(ctx, "admin_user_role.assign_audit_log_failed", zap.Error(err))
		}
	}

	return nil
}

// RemoveRoleInput defines the input for removing a role from a user.
type RemoveRoleInput struct {
	UserID uuid.UUID
	RoleID uuid.UUID
}

// RemoveRole removes a role from an admin user with safety checks (P0 #5 fix).
// Uses atomic DeleteWithAdminCheck to prevent race conditions.
func (uc *AdminUserRoleUsecase) RemoveRole(ctx context.Context, input RemoveRoleInput, operatorID uuid.UUID, operatorIP string) error {
	// Get role info
	role, err := uc.adminRoleRepo.GetByID(ctx, input.RoleID)
	if err != nil {
		if repo.IsNotFound(err) {
			return repo.ErrRoleNotFound
		}
		return fmt.Errorf("get role: %w", err)
	}

	// Safety checks for admin role removal
	if role.Name == SystemRoleAdmin {
		// Check 1: prevent user from removing their own admin role (ALWAYS checked)
		if operatorID == input.UserID {
			return repo.ErrCannotRemoveSelfAdmin
		}

		// Check 2: atomic check and delete to prevent race condition (P0 #5 fix)
		if err := uc.adminUserRoleRepo.DeleteWithAdminCheck(ctx, input.UserID, input.RoleID); err != nil {
			if errors.Is(err, repo.ErrCannotRemoveLastAdmin) {
				return err
			}
			return fmt.Errorf("delete admin role with check: %w", err)
		}
	} else {
		// Non-admin role: simple delete
		if err := uc.adminUserRoleRepo.Delete(ctx, input.UserID, input.RoleID); err != nil {
			return fmt.Errorf("remove role assignment: %w", err)
		}
	}

	// Remove Casbin grouping policy
	// If Casbin operation fails, the entire operation fails to ensure consistency
	// between DB and Casbin in-memory state
	if err := uc.enforcer.RemoveGroupingPolicy(ctx, input.UserID.String(), input.RoleID.String()); err != nil {
		return fmt.Errorf("remove casbin grouping policy for user %s role %s: %w",
			input.UserID.String(), input.RoleID.String(), err)
	}

	// Publish policy change for multi-instance sync (P0 #1 fix)
	if uc.policyPublisher != nil {
		if err := uc.policyPublisher.PublishChange(ctx, "g", "remove_grouping_policy", [][]string{{input.UserID.String(), input.RoleID.String()}}); err != nil {
			logger.Warn(ctx, "admin_user_role.publish_policy_change_failed", zap.Error(err))
		}
	}

	// Audit log
	if uc.auditLogRepo != nil {
		if err := uc.auditLogRepo.Create(ctx, repo.NewAuditLog(
			operatorID, operatorIP, "revoke_role", "admin_user", input.UserID,
			map[string]any{"role_id": input.RoleID.String(), "role_name": role.Name},
		)); err != nil {
			logger.Error(ctx, "admin_user_role.revoke_audit_log_failed", zap.Error(err))
		}
	}

	return nil
}

// CountEnabledAdmins counts the number of admin users who have the admin role assigned.
// This is used to prevent removing the last admin.
// Optimized: uses a single COUNT query via CountByRoleID instead of N+1 admin user lookups.
// Note: This may slightly over-count if soft-deleted admin users have stale admin_user_roles records,
// which is the safer failure mode (prevents removal when it would be safe, not vice versa).
func (uc *AdminUserRoleUsecase) CountEnabledAdmins(ctx context.Context) (int64, error) {
	// Find the admin role
	adminRole, err := uc.adminRoleRepo.GetByName(ctx, SystemRoleAdmin)
	if err != nil {
		if errors.Is(err, repo.ErrRoleNotFound) {
			return 0, nil // No admin role exists, so no admins
		}
		return 0, fmt.Errorf("get admin role: %w", err)
	}

	// Count admin users with admin role — single query, no N+1
	return uc.adminUserRoleRepo.CountByRoleID(ctx, adminRole.ID)
}

// UserRoleResponse represents an admin user-role assignment.
type UserRoleResponse struct {
	UserID     uuid.UUID
	RoleID     uuid.UUID
	RoleName   string
	AssignedAt string
}

// ListUserRoles lists all roles assigned to an admin user.
func (uc *AdminUserRoleUsecase) ListUserRoles(ctx context.Context, userID uuid.UUID) ([]UserRoleResponse, error) {
	// Verify admin user exists
	if _, err := uc.adminUserRepo.GetByID(ctx, userID); err != nil {
		if repo.IsNotFound(err) {
			return nil, ErrAdminUserNotFound
		}
		return nil, fmt.Errorf("get admin user: %w", err)
	}

	urs, err := uc.adminUserRoleRepo.ListByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list user roles: %w", err)
	}

	// Batch-fetch all roles in a single query to avoid N+1
	roleIDs := make([]uuid.UUID, 0, len(urs))
	for _, ur := range urs {
		roleIDs = append(roleIDs, ur.RoleID)
	}
	roles, err := uc.adminRoleRepo.GetByIDs(ctx, roleIDs)
	if err != nil {
		return nil, fmt.Errorf("batch get roles: %w", err)
	}
	roleMap := make(map[uuid.UUID]*model.AdminRole, len(roles))
	for _, role := range roles {
		roleMap[role.ID] = role
	}

	result := make([]UserRoleResponse, 0, len(urs))
	for _, ur := range urs {
		role, ok := roleMap[ur.RoleID]
		if !ok {
			continue // Skip if role not found (may have been deleted)
		}
		result = append(result, UserRoleResponse{
			UserID:     ur.UserID,
			RoleID:     ur.RoleID,
			RoleName:   role.Name,
			AssignedAt: ur.AssignedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	return result, nil
}

// RoleUserResponse represents an admin user in a role.
type RoleUserResponse struct {
	UserID     uuid.UUID
	UserEmail  string
	UserName   string
	AssignedAt string
}

// ListRoleUsers lists all admin users assigned to a role.
func (uc *AdminUserRoleUsecase) ListRoleUsers(ctx context.Context, roleID uuid.UUID) ([]RoleUserResponse, error) {
	// Verify role exists
	if _, err := uc.adminRoleRepo.GetByID(ctx, roleID); err != nil {
		if repo.IsNotFound(err) {
			return nil, repo.ErrRoleNotFound
		}
		return nil, fmt.Errorf("get role: %w", err)
	}

	urs, err := uc.adminUserRoleRepo.ListByRoleID(ctx, roleID)
	if err != nil {
		return nil, fmt.Errorf("list role admin users: %w", err)
	}

	// Batch-fetch all admin users in a single query to avoid N+1
	userIDs := make([]uuid.UUID, 0, len(urs))
	for _, ur := range urs {
		userIDs = append(userIDs, ur.UserID)
	}
	users, err := uc.adminUserRepo.GetByIDs(ctx, userIDs)
	if err != nil {
		return nil, fmt.Errorf("batch get admin users: %w", err)
	}
	userMap := make(map[uuid.UUID]*model.AdminUser, len(users))
	for _, user := range users {
		userMap[user.ID] = user
	}

	result := make([]RoleUserResponse, 0, len(urs))
	for _, ur := range urs {
		user, ok := userMap[ur.UserID]
		if !ok {
			continue // Skip if admin user not found (may have been deleted)
		}
		result = append(result, RoleUserResponse{
			UserID:     ur.UserID,
			UserEmail:  user.Email,
			UserName:   user.Name,
			AssignedAt: ur.AssignedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	return result, nil
}
