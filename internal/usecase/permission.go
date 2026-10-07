// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
)

// PermissionUsecase handles permission policy management.
type PermissionUsecase struct {
	adminRoleRepo   repo.AdminRoleRepo
	enforcer        *auth.CasbinEnforcer
	auditLogRepo    repo.AuditLogRepo
	policyPublisher PolicyPublisher // optional multi-instance sync (may be nil)
}

// NewPermissionUsecase creates a new PermissionUsecase.
func NewPermissionUsecase(
	adminRoleRepo repo.AdminRoleRepo,
	enforcer *auth.CasbinEnforcer,
	auditLogRepo repo.AuditLogRepo,
) *PermissionUsecase {
	return &PermissionUsecase{
		adminRoleRepo: adminRoleRepo,
		enforcer:      enforcer,
		auditLogRepo:  auditLogRepo,
	}
}

// SetPolicyPublisher injects optional policy publisher for multi-instance sync.
func (uc *PermissionUsecase) SetPolicyPublisher(pp PolicyPublisher) {
	uc.policyPublisher = pp
}

// CreatePermissionInput defines the input for creating a permission policy.
type CreatePermissionInput struct {
	RoleID   string // Role UUID as string
	Resource string
	Action   string
}

// CreatePermission adds a permission policy to a role.
// Returns ErrPermissionExists if the (role, resource, action) combination already exists.
func (uc *PermissionUsecase) CreatePermission(ctx context.Context, input CreatePermissionInput, operatorID uuid.UUID, operatorIP string) error {
	// Validate role exists
	roleID, err := uuid.Parse(input.RoleID)
	if err != nil {
		return fmt.Errorf("invalid role ID: %w", err)
	}
	role, err := uc.adminRoleRepo.GetByID(ctx, roleID)
	if err != nil {
		return err
	}

	// Add policy to Casbin — returns false if the policy already exists
	added, err := uc.enforcer.AddPolicy(ctx, input.RoleID, input.Resource, input.Action)
	if err != nil {
		return fmt.Errorf("add permission policy: %w", err)
	}
	if !added {
		return repo.ErrPermissionExists
	}

	// Publish policy change for multi-instance sync (P0 #1 fix)
	if uc.policyPublisher != nil {
		rules := [][]string{{input.RoleID, input.Resource, input.Action}}
		if err := uc.policyPublisher.PublishChange(ctx, "p", "add_policy", rules); err != nil {
			logger.Warn(ctx, "permission.publish_policy_change_failed", zap.Error(err))
		}
	}

	// Audit log
	if uc.auditLogRepo != nil {
		if err := uc.auditLogRepo.Create(ctx, repo.NewAuditLog(
			operatorID, operatorIP, "create_permission", "permission", roleID,
			map[string]any{"role_name": role.Name, "resource": input.Resource, "action": input.Action},
		)); err != nil {
			logger.Error(ctx, "permission.create_audit_log_failed", zap.Error(err))
		}
	}

	return nil
}

// DeletePermissionInput defines the input for deleting a permission policy.
type DeletePermissionInput struct {
	RoleID   string
	Resource string
	Action   string
}

// DeletePermission removes a permission policy from a role.
func (uc *PermissionUsecase) DeletePermission(ctx context.Context, input DeletePermissionInput, operatorID uuid.UUID, operatorIP string) error {
	roleID, err := uuid.Parse(input.RoleID)
	if err != nil {
		return fmt.Errorf("invalid role ID: %w", err)
	}

	// Get role info for audit before deletion
	var roleName string
	if role, err := uc.adminRoleRepo.GetByID(ctx, roleID); err == nil {
		roleName = role.Name
	}

	if err := uc.enforcer.RemovePolicy(ctx, input.RoleID, input.Resource, input.Action); err != nil {
		return fmt.Errorf("remove permission policy: %w", err)
	}

	// Publish policy change for multi-instance sync (P0 #1 fix)
	if uc.policyPublisher != nil {
		rules := [][]string{{input.RoleID, input.Resource, input.Action}}
		if err := uc.policyPublisher.PublishChange(ctx, "p", "remove_policy", rules); err != nil {
			logger.Warn(ctx, "permission.publish_policy_change_failed", zap.Error(err))
		}
	}

	// Audit log
	if uc.auditLogRepo != nil {
		if err := uc.auditLogRepo.Create(ctx, repo.NewAuditLog(
			operatorID, operatorIP, "delete_permission", "permission", roleID,
			map[string]any{"role_name": roleName, "resource": input.Resource, "action": input.Action},
		)); err != nil {
			logger.Error(ctx, "permission.delete_audit_log_failed", zap.Error(err))
		}
	}

	return nil
}

// CheckPermission checks if a user has permission to perform an action on a resource.
func (uc *PermissionUsecase) CheckPermission(ctx context.Context, userID uuid.UUID, resource, action string) (bool, error) {
	return uc.enforcer.Enforce(ctx, userID.String(), resource, action)
}

// ListPermissionsFilter defines optional filters for listing permission policies.
type ListPermissionsFilter struct {
	RoleID   string
	Resource string
}

// ListPermissions returns permission policies, optionally filtered by role_id or resource.
func (uc *PermissionUsecase) ListPermissions(_ context.Context, filter ListPermissionsFilter) ([][]string, error) {
	enforcer := uc.enforcer.Enforcer()
	policies, err := enforcer.GetPolicy()
	if err != nil {
		return nil, fmt.Errorf("get policies: %w", err)
	}

	// Apply filters
	if filter.RoleID == "" && filter.Resource == "" {
		return policies, nil
	}

	filtered := make([][]string, 0, len(policies))
	for _, p := range policies {
		if len(p) < 3 {
			continue
		}
		if filter.RoleID != "" && p[0] != filter.RoleID {
			continue
		}
		if filter.Resource != "" && p[1] != filter.Resource {
			continue
		}
		filtered = append(filtered, p)
	}
	return filtered, nil
}
