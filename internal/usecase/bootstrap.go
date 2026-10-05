// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
)

// Expected policy counts for default roles (used for bootstrap validation).
const (
	expectedAdminPolicies    = 13 // admin role: role(4) + permission(3) + user_role(3) + audit_log(1) + user(2)
	expectedOperatorPolicies = 5  // operator role: role(1) + user(2) + user_role(2)
	expectedViewerPolicies   = 1  // viewer role: role(1)
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
func BootstrapAdmin(ctx context.Context, db *gorm.DB, roleRepo repo.RoleRepo, enforcer *auth.CasbinEnforcer) error {
	// First, try to find existing roles
	adminRole, adminErr := roleRepo.GetByName(ctx, "admin")
	operatorRole, operatorErr := roleRepo.GetByName(ctx, "operator")
	viewerRole, viewerErr := roleRepo.GetByName(ctx, "viewer")

	allRolesExist := adminErr == nil && operatorErr == nil && viewerErr == nil

	if allRolesExist {
		// Check if all expected policies already exist for the admin role
		// We check content, not just count, to ensure correctness
		existingPolicies := enforcer.GetPoliciesForRole(adminRole.ID.String())
		if hasAllExpectedPolicies(existingPolicies, getExpectedAdminPolicies(adminRole.ID.String())) {
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
			adminRole = &model.Role{
				Name:        "admin",
				DisplayName: "管理员",
				Description: "系统管理员，拥有完整权限",
				IsSystem:    true,
				IsEnabled:   true,
			}
			if err := roleRepo.Create(txCtx, adminRole); err != nil {
				return fmt.Errorf("create admin role: %w", err)
			}
		}
		if operatorErr != nil {
			operatorRole = &model.Role{
				Name:        "operator",
				DisplayName: "运营",
				Description: "运营人员，可管理用户和查看角色",
				IsSystem:    false,
				IsEnabled:   true,
			}
			if err := roleRepo.Create(txCtx, operatorRole); err != nil {
				return fmt.Errorf("create operator role: %w", err)
			}
		}
		if viewerErr != nil {
			viewerRole = &model.Role{
				Name:        "viewer",
				DisplayName: "观察者",
				Description: "只读访问权限",
				IsSystem:    false,
				IsEnabled:   true,
			}
			if err := roleRepo.Create(txCtx, viewerRole); err != nil {
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
	return addAllPolicies(ctx, enforcer, createdAdminID, createdOperatorID, createdViewerID)
}

// addAllPolicies adds all default Casbin policies for the three default roles.
// This is idempotent — Casbin's AddPolicy is a no-op if the policy already exists.
func addAllPolicies(ctx context.Context, enforcer *auth.CasbinEnforcer, adminID, operatorID, viewerID string) error {
	adminPolicies := [][]string{
		{adminID, "user", "read"},
		{adminID, "user", "write"},
		{adminID, "user", "delete"},
		{adminID, "role", "read"},
		{adminID, "role", "write"},
		{adminID, "role", "delete"},
		{adminID, "permission", "read"},
		{adminID, "permission", "write"},
		{adminID, "permission", "delete"},
		{adminID, "user_role", "read"},
		{adminID, "user_role", "write"},
		{adminID, "user_role", "delete"},
		{adminID, "audit_log", "read"},
	}

	operatorPolicies := [][]string{
		{operatorID, "user", "read"},
		{operatorID, "user", "write"},
		{operatorID, "role", "read"},
		{operatorID, "user_role", "read"},
		{operatorID, "user_role", "write"},
	}

	viewerPolicies := [][]string{
		{viewerID, "user", "read"},
	}

	if err := enforcer.AddPolicies(ctx, adminPolicies); err != nil {
		return fmt.Errorf("add admin policies: %w", err)
	}
	if err := enforcer.AddPolicies(ctx, operatorPolicies); err != nil {
		return fmt.Errorf("add operator policies: %w", err)
	}
	if err := enforcer.AddPolicies(ctx, viewerPolicies); err != nil {
		return fmt.Errorf("add viewer policies: %w", err)
	}

	logger.Info(ctx, "bootstrap.policies_added",
		zap.Int("admin_policies", len(adminPolicies)),
		zap.Int("operator_policies", len(operatorPolicies)),
		zap.Int("viewer_policies", len(viewerPolicies)))

	return nil
}

// getExpectedAdminPolicies returns the expected policies for the admin role.
func getExpectedAdminPolicies(adminID string) [][]string {
	return [][]string{
		{adminID, "user", "read"},
		{adminID, "user", "write"},
		{adminID, "user", "delete"},
		{adminID, "role", "read"},
		{adminID, "role", "write"},
		{adminID, "role", "delete"},
		{adminID, "permission", "read"},
		{adminID, "permission", "write"},
		{adminID, "permission", "delete"},
		{adminID, "user_role", "read"},
		{adminID, "user_role", "write"},
		{adminID, "user_role", "delete"},
		{adminID, "audit_log", "read"},
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
