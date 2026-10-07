// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"golang.org/x/crypto/bcrypt"
)

// AdminUserUsecase handles admin user-related operations.
type AdminUserUsecase struct {
	adminUserRepo     repo.AdminUserRepo
	adminUserRoleRepo repo.AdminUserRoleRepo
	roleRepo          repo.AdminRoleRepo
	enforcer          *auth.CasbinEnforcer
}

// NewAdminUserUsecase creates a new AdminUserUsecase.
func NewAdminUserUsecase(
	adminUserRepo repo.AdminUserRepo,
	adminUserRoleRepo repo.AdminUserRoleRepo,
	roleRepo repo.AdminRoleRepo,
	enforcer *auth.CasbinEnforcer,
) *AdminUserUsecase {
	return &AdminUserUsecase{
		adminUserRepo:     adminUserRepo,
		adminUserRoleRepo: adminUserRoleRepo,
		roleRepo:          roleRepo,
		enforcer:          enforcer,
	}
}

// ListUsersPaginated returns a paginated list of admin users.
func (uc *AdminUserUsecase) ListUsersPaginated(ctx context.Context, page, pageSize int) ([]*model.AdminUser, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	users, total, err := uc.adminUserRepo.List(ctx, page, pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("list admin users: %w", err)
	}

	return users, total, nil
}

// GetUserRoles returns the roles assigned to a user.
func (uc *AdminUserUsecase) GetUserRoles(ctx context.Context, userID uuid.UUID) ([]*model.AdminRole, error) {
	userRoles, err := uc.adminUserRoleRepo.ListByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list user roles: %w", err)
	}

	if len(userRoles) == 0 {
		return []*model.AdminRole{}, nil
	}

	// Extract role IDs
	roleIDs := make([]uuid.UUID, 0, len(userRoles))
	for _, ur := range userRoles {
		roleIDs = append(roleIDs, ur.RoleID)
	}

	// Fetch role details
	roles := make([]*model.AdminRole, 0, len(roleIDs))
	for _, roleID := range roleIDs {
		role, err := uc.roleRepo.GetByID(ctx, roleID)
		if err != nil {
			// Skip roles that don't exist
			continue
		}
		roles = append(roles, role)
	}

	return roles, nil
}

// CreateUserInput contains the input for creating a new admin user.
type CreateUserInput struct {
	Email    string
	Password string
	Name     string
	RoleIDs  []uuid.UUID // optional roles to assign
}

// CreateUser creates a new admin user and optionally assigns roles.
func (uc *AdminUserUsecase) CreateUser(ctx context.Context, input CreateUserInput) (*model.AdminUser, error) {
	// Check if email already exists
	existing, err := uc.adminUserRepo.GetByEmail(ctx, input.Email)
	if err != nil && !repo.IsNotFound(err) {
		return nil, fmt.Errorf("check email existence: %w", err)
	}
	if existing != nil {
		return nil, fmt.Errorf("email already exists")
	}

	// Hash password
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	// Create user
	user := &model.AdminUser{
		Email:        input.Email,
		Name:         input.Name,
		PasswordHash: string(passwordHash),
	}
	if err := uc.adminUserRepo.Create(ctx, user); err != nil {
		return nil, fmt.Errorf("create admin user: %w", err)
	}

	// Assign roles if provided
	if len(input.RoleIDs) > 0 {
		if err := uc.adminUserRoleRepo.CreateBatch(ctx, user.ID, input.RoleIDs); err != nil {
			return nil, fmt.Errorf("assign roles: %w", err)
		}

		// Add Casbin grouping policies (user → role)
		for _, roleID := range input.RoleIDs {
			if err := uc.enforcer.AddGroupingPolicy(ctx, user.ID.String(), roleID.String()); err != nil {
				return nil, fmt.Errorf("add casbin grouping policy for user %s role %s: %w", user.ID, roleID, err)
			}
		}
	}

	return user, nil
}

// UpdateUserInput contains the input for updating an admin user.
type UpdateUserInput struct {
	Name     *string
	Password *string
}

// UpdateUser updates an admin user's name and/or password.
func (uc *AdminUserUsecase) UpdateUser(ctx context.Context, userID uuid.UUID, input UpdateUserInput) (*model.AdminUser, error) {
	// Get existing user
	user, err := uc.adminUserRepo.GetByID(ctx, userID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, ErrAdminUserNotFound
		}
		return nil, fmt.Errorf("get admin user: %w", err)
	}

	// Update name if provided
	if input.Name != nil {
		user.Name = *input.Name
	}

	// Update password if provided
	if input.Password != nil {
		passwordHash, err := bcrypt.GenerateFromPassword([]byte(*input.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
		user.PasswordHash = string(passwordHash)
	}

	// Save changes
	if err := uc.adminUserRepo.Update(ctx, user); err != nil {
		return nil, fmt.Errorf("update admin user: %w", err)
	}

	return user, nil
}
