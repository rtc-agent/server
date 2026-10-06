// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
)

// Expected policy counts for default roles (used for bootstrap validation).
const (
	expectedAdminPolicies    = 18 // admin role: admin_user(3) + role(3) + permission(3) + admin_user_role(3) + audit_log(1) + rtc_user(2) + server_config(3)
	expectedOperatorPolicies = 9  // operator role: admin_user(2) + role(1) + admin_user_role(2) + rtc_user(2) + server_config(2)
	expectedViewerPolicies   = 3  // viewer role: admin_user(1) + rtc_user(1) + server_config(1)
)

// BootstrapAdmin initializes default roles and permissions if they don't already exist.
//
// This function is idempotent and handles partial failures:
//   - If roles AND policies already exist, it returns immediately.
//   - If roles exist but policies are missing (previous partial failure), it adds the missing policies.
//   - If roles don't exist, it creates them in a transaction, then adds policies.
//
// The Casbin policy creation is intentionally done OUTSIDE the DB transaction because
// the Casbin gorm-adapter manages its own DB session. To handle this safely, we check
// for existing policies before adding them, making the operation idempotent.
func BootstrapAdmin(ctx context.Context, db *gorm.DB, adminRoleRepo repo.AdminRoleRepo, enforcer *auth.CasbinEnforcer) error {
	// First, try to find existing roles
	adminRole, adminErr := adminRoleRepo.GetByName(ctx, "admin")
	operatorRole, operatorErr := adminRoleRepo.GetByName(ctx, "operator")
	viewerRole, viewerErr := adminRoleRepo.GetByName(ctx, "viewer")

	allRolesExist := adminErr == nil && operatorErr == nil && viewerErr == nil

	if allRolesExist {
		// Check if all expected policies already exist for the admin role
		// We check content, not just count, to ensure correctness
		existingPolicies := enforcer.GetPoliciesForRole(adminRole.ID.String())
		if hasAllExpectedPolicies(existingPolicies, getAllExpectedAdminPolicies(adminRole.ID.String())) {
			// Admin has all expected policies — bootstrap already complete
			logger.Info(ctx, "bootstrap.already_complete_skipping",
				zap.Int("admin_policies", len(existingPolicies)),
				zap.Int("expected", expectedAdminPolicies))
			return nil
		}

		// Roles exist but policies are missing/incomplete — add missing policies
		logger.Info(ctx, "bootstrap.roles_exist_policies_missing_adding",
			zap.Int("existing_policies", len(existingPolicies)),
			zap.Int("expected", expectedAdminPolicies))
		return addAllPolicies(ctx, enforcer, adminRole.ID.String(), operatorRole.ID.String(), viewerRole.ID.String())
	}

	// Roles don't all exist — create them in a transaction
	logger.Info(ctx, "bootstrap.creating_default_roles")

	var createdAdminID, createdOperatorID, createdViewerID string

	txErr := db.Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		// Create roles that don't exist yet
		if adminErr != nil {
			adminRole = &model.AdminRole{
				Name:        "admin",
				DisplayName: "管理员",
				Description: "系统管理员，拥有完整权限",
				IsSystem:    true,
				IsEnabled:   true,
			}
			if err := adminRoleRepo.Create(txCtx, adminRole); err != nil {
				return fmt.Errorf("create admin role: %w", err)
			}
		}
		if operatorErr != nil {
			operatorRole = &model.AdminRole{
				Name:        "operator",
				DisplayName: "运营",
				Description: "运营人员，可管理用户和查看角色",
				IsSystem:    false,
				IsEnabled:   true,
			}
			if err := adminRoleRepo.Create(txCtx, operatorRole); err != nil {
				return fmt.Errorf("create operator role: %w", err)
			}
		}
		if viewerErr != nil {
			viewerRole = &model.AdminRole{
				Name:        "viewer",
				DisplayName: "观察者",
				Description: "只读访问权限",
				IsSystem:    false,
				IsEnabled:   true,
			}
			if err := adminRoleRepo.Create(txCtx, viewerRole); err != nil {
				return fmt.Errorf("create viewer role: %w", err)
			}
		}

		createdAdminID = adminRole.ID.String()
		createdOperatorID = operatorRole.ID.String()
		createdViewerID = viewerRole.ID.String()

		logger.Info(ctx, "bootstrap.roles_created_in_transaction",
			zap.String("admin_id", createdAdminID),
			zap.String("operator_id", createdOperatorID),
			zap.String("viewer_id", createdViewerID))

		return nil
		// Transaction commits here.
	})
	if txErr != nil {
		return txErr
	}

	// Add policies AFTER the transaction commits (Casbin adapter uses its own DB session).
	// If this fails, the next restart will detect the missing policies and add them.
	if err := addAllPolicies(ctx, enforcer, createdAdminID, createdOperatorID, createdViewerID); err != nil {
		return err
	}

	// Auto-assign admin role to the first user if no one has admin role yet.
	// This ensures out-of-box experience: the first user created via CLI gets admin access.
	if err := autoAssignAdminRole(ctx, db, enforcer, createdAdminID); err != nil {
		// Log but don't fail — admin can manually assign roles later
		logger.Warn(ctx, "bootstrap.auto_assign_admin_failed", zap.Error(err))
	}

	return nil
}

// addAllPolicies adds all default Casbin policies for the three default roles.
//
// It filters out policies that already exist before adding, so each role only gets
// the policies it's missing. This avoids relying on Casbin's idempotency check and
// provides clear logging about which policies are actually new.
func addAllPolicies(ctx context.Context, enforcer *auth.CasbinEnforcer, adminID, operatorID, viewerID string) error {
	allRolePolicies := []struct {
		roleName string
		roleID   string
		policies [][]string
	}{
		{"admin", adminID, getAllExpectedAdminPolicies(adminID)},
		{"operator", operatorID, getAllExpectedOperatorPolicies(operatorID)},
		{"viewer", viewerID, getAllExpectedViewerPolicies(viewerID)},
	}

	for _, rp := range allRolePolicies {
		existing := enforcer.GetPoliciesForRole(rp.roleID)
		missing := filterMissingPolicies(existing, rp.policies)

		if len(missing) == 0 {
			logger.Info(ctx, "bootstrap.role_policies_complete",
				zap.String("role", rp.roleName),
				zap.Int("existing", len(existing)))
			continue
		}

		if err := enforcer.AddPolicies(ctx, missing); err != nil {
			return fmt.Errorf("add %s policies: %w", rp.roleName, err)
		}

		logger.Info(ctx, "bootstrap.role_policies_added",
			zap.String("role", rp.roleName),
			zap.Int("added", len(missing)),
			zap.Int("total", len(existing)+len(missing)))
	}

	return nil
}

// filterMissingPolicies returns the subset of expected policies that don't exist yet.
// existing contains [resource, action] pairs; expected contains [roleID, resource, action] triples.
func filterMissingPolicies(existing [][]string, expected [][]string) [][]string {
	existingSet := make(map[string]struct{}, len(existing))
	for _, p := range existing {
		if len(p) >= 2 {
			existingSet[p[0]+":"+p[1]] = struct{}{}
		}
	}

	var missing [][]string
	for _, p := range expected {
		if len(p) >= 3 {
			key := p[1] + ":" + p[2]
			if _, ok := existingSet[key]; !ok {
				missing = append(missing, p)
			}
		}
	}
	return missing
}

// getAllExpectedAdminPolicies returns the complete expected policy set for the admin role.
func getAllExpectedAdminPolicies(adminID string) [][]string {
	return [][]string{
		{adminID, "admin_user", "read"},
		{adminID, "admin_user", "write"},
		{adminID, "admin_user", "delete"},
		{adminID, "role", "read"},
		{adminID, "role", "write"},
		{adminID, "role", "delete"},
		{adminID, "permission", "read"},
		{adminID, "permission", "write"},
		{adminID, "permission", "delete"},
		{adminID, "admin_user_role", "read"},
		{adminID, "admin_user_role", "write"},
		{adminID, "admin_user_role", "delete"},
		{adminID, "audit_log", "read"},
		{adminID, "rtc_user", "read"},
		{adminID, "rtc_user", "ban"},
		{adminID, "server_config", "read"},
		{adminID, "server_config", "write"},
		{adminID, "server_config", "delete"},
	}
}

// getAllExpectedOperatorPolicies returns the complete expected policy set for the operator role.
func getAllExpectedOperatorPolicies(operatorID string) [][]string {
	return [][]string{
		{operatorID, "admin_user", "read"},
		{operatorID, "admin_user", "write"},
		{operatorID, "role", "read"},
		{operatorID, "admin_user_role", "read"},
		{operatorID, "admin_user_role", "write"},
		{operatorID, "rtc_user", "read"},
		{operatorID, "rtc_user", "ban"},
		{operatorID, "server_config", "read"},
		{operatorID, "server_config", "write"},
	}
}

// getAllExpectedViewerPolicies returns the complete expected policy set for the viewer role.
func getAllExpectedViewerPolicies(viewerID string) [][]string {
	return [][]string{
		{viewerID, "admin_user", "read"},
		{viewerID, "rtc_user", "read"},
		{viewerID, "server_config", "read"},
	}
}

// hasAllExpectedPolicies checks if all expected policies exist in the current policies.
// Both existing and expected are [resource, action] pairs (role ID stripped).
func hasAllExpectedPolicies(existing [][]string, expected [][]string) bool {
	if len(existing) < len(expected) {
		return false
	}

	// Build a set of existing policies for O(1) lookup
	existingSet := make(map[string]struct{}, len(existing))
	for _, p := range existing {
		if len(p) >= 2 {
			key := p[0] + ":" + p[1] // resource:action
			existingSet[key] = struct{}{}
		}
	}

	// Check all expected policies exist
	for _, p := range expected {
		if len(p) >= 2 {
			key := p[1] + ":" + p[2] // resource:action (skip role ID at index 0)
			if _, exists := existingSet[key]; !exists {
				return false
			}
		}
	}

	return true
}

// autoAssignAdminRole automatically assigns the admin role to the first admin user if no one has it.
// This ensures out-of-box experience: the first admin user created via CLI gets admin access immediately.
//
// Logic:
//  1. Check if any admin user already has the admin role (via Casbin grouping policies)
//  2. If yes, do nothing
//  3. If no, find the first admin user in the database
//  4. Assign admin role to that admin user (both in admin_user_roles table and Casbin)
func autoAssignAdminRole(
	ctx context.Context,
	db *gorm.DB,
	enforcer *auth.CasbinEnforcer,
	adminRoleID string,
) error {
	// Check if any admin user already has admin role
	// GetGroupingPolicy returns ([][]string, error)
	allGroupingPolicies, err := enforcer.Enforcer().GetGroupingPolicy()
	if err != nil {
		return fmt.Errorf("get grouping policies: %w", err)
	}
	for _, policy := range allGroupingPolicies {
		if len(policy) >= 2 && policy[1] == adminRoleID {
			// At least one admin user has admin role — nothing to do
			logger.Info(ctx, "bootstrap.admin_role_already_assigned",
				zap.String("admin_user_id", policy[0]))
			return nil
		}
	}

	// No admin user has admin role — find the first admin user
	var firstUser model.AdminUser
	if err := db.WithContext(ctx).First(&firstUser).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			// No admin users exist yet — nothing to do
			logger.Info(ctx, "bootstrap.no_admin_users_exist_skipping_admin_assignment")
			return nil
		}
		return fmt.Errorf("find first admin user: %w", err)
	}

	// Assign admin role to the first admin user
	userID := firstUser.ID.String()
	logger.Info(ctx, "bootstrap.auto_assigning_admin_role",
		zap.String("user_id", userID),
		zap.String("user_email", firstUser.Email),
		zap.String("admin_role_id", adminRoleID))

	// 1. Create admin_user_roles record
	userRole := &model.AdminUserRole{
		UserID:     firstUser.ID,
		RoleID:     uuid.MustParse(adminRoleID),
		AssignedAt: time.Now(),
	}
	if err := db.WithContext(ctx).Create(userRole).Error; err != nil {
		return fmt.Errorf("create admin_user_role record: %w", err)
	}

	// 2. Add Casbin grouping policy
	if err := enforcer.AddGroupingPolicy(ctx, userID, adminRoleID); err != nil {
		return fmt.Errorf("add casbin grouping policy: %w", err)
	}

	logger.Info(ctx, "bootstrap.admin_role_auto_assigned",
		zap.String("user_id", userID),
		zap.String("user_email", firstUser.Email))

	return nil
}
