// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"golang.org/x/crypto/bcrypt"
)

// AdminUserUsecase handles admin user-related operations.
type AdminUserUsecase struct {
	db                *gorm.DB
	adminUserRepo     repo.AdminUserRepo
	adminUserRoleRepo repo.AdminUserRoleRepo
	roleRepo          repo.AdminRoleRepo
	enforcer          *auth.CasbinEnforcer
}

// ErrEmailAlreadyExists is returned when attempting to create a user with an email that already exists.
var ErrEmailAlreadyExists = errors.New("email already exists")

// NewAdminUserUsecase creates a new AdminUserUsecase.
func NewAdminUserUsecase(
	db *gorm.DB,
	adminUserRepo repo.AdminUserRepo,
	adminUserRoleRepo repo.AdminUserRoleRepo,
	roleRepo repo.AdminRoleRepo,
	enforcer *auth.CasbinEnforcer,
) *AdminUserUsecase {
	return &AdminUserUsecase{
		db:                db,
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

	// Batch-fetch all roles in a single query to avoid N+1.
	roles, err := uc.roleRepo.GetByIDs(ctx, roleIDs)
	if err != nil {
		return nil, fmt.Errorf("batch get roles: %w", err)
	}

	// Build a map for O(1) lookup, preserving order and skipping missing roles.
	roleMap := make(map[uuid.UUID]*model.AdminRole, len(roles))
	for _, role := range roles {
		roleMap[role.ID] = role
	}

	result := make([]*model.AdminRole, 0, len(roleIDs))
	for _, roleID := range roleIDs {
		if role, ok := roleMap[roleID]; ok {
			result = append(result, role)
		}
	}

	return result, nil
}

// GetUsersRolesBatch returns a map of userID -> roles for multiple users in a single batch operation.
// This avoids N+1 query problems when listing users with their roles.
func (uc *AdminUserUsecase) GetUsersRolesBatch(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID][]*model.AdminRole, error) {
	if len(userIDs) == 0 {
		return make(map[uuid.UUID][]*model.AdminRole), nil
	}

	// Fetch all user-role assignments in one query
	userRoles, err := uc.adminUserRoleRepo.ListByUserIDs(ctx, userIDs)
	if err != nil {
		return nil, fmt.Errorf("batch list user roles: %w", err)
	}

	if len(userRoles) == 0 {
		// No roles for any user, return empty map
		result := make(map[uuid.UUID][]*model.AdminRole, len(userIDs))
		for _, userID := range userIDs {
			result[userID] = []*model.AdminRole{}
		}
		return result, nil
	}

	// Collect unique role IDs
	roleIDSet := make(map[uuid.UUID]struct{})
	for _, ur := range userRoles {
		roleIDSet[ur.RoleID] = struct{}{}
	}
	roleIDs := make([]uuid.UUID, 0, len(roleIDSet))
	for roleID := range roleIDSet {
		roleIDs = append(roleIDs, roleID)
	}

	// Fetch all roles in one query
	roles, err := uc.roleRepo.GetByIDs(ctx, roleIDs)
	if err != nil {
		return nil, fmt.Errorf("batch get roles: %w", err)
	}

	// Build role map
	roleMap := make(map[uuid.UUID]*model.AdminRole, len(roles))
	for _, role := range roles {
		roleMap[role.ID] = role
	}

	// Build user -> roles map
	userRolesMap := make(map[uuid.UUID][]*model.AdminRole)
	for _, ur := range userRoles {
		if role, ok := roleMap[ur.RoleID]; ok {
			userRolesMap[ur.UserID] = append(userRolesMap[ur.UserID], role)
		}
	}

	// Ensure all userIDs have an entry (even if empty)
	for _, userID := range userIDs {
		if _, ok := userRolesMap[userID]; !ok {
			userRolesMap[userID] = []*model.AdminRole{}
		}
	}

	return userRolesMap, nil
}

// CreateUserInput contains the input for creating a new admin user.
type CreateUserInput struct {
	Email    string
	Password string
	Name     string
	RoleIDs  []uuid.UUID // optional roles to assign
}

// CreateUser creates a new admin user and optionally assigns roles.
// Wrapped in a transaction to prevent partial failures (user created but role assignment fails).
// The method relies on the database unique constraint on email to reject
// duplicates, avoiding a TOCTOU race between GetByEmail and Create.
func (uc *AdminUserUsecase) CreateUser(ctx context.Context, input CreateUserInput) (*model.AdminUser, error) {
	// Hash password
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	var user *model.AdminUser

	err = uc.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		// Create user — DB unique constraint rejects duplicate emails atomically.
		user = &model.AdminUser{
			Email:        input.Email,
			Name:         input.Name,
			PasswordHash: string(passwordHash),
		}
		if err := uc.adminUserRepo.Create(txCtx, user); err != nil {
			if errors.Is(err, repo.ErrDuplicateEmail) {
				return ErrEmailAlreadyExists
			}
			return fmt.Errorf("create admin user: %w", err)
		}

		// Assign roles if provided (within same transaction)
		if len(input.RoleIDs) > 0 {
			if err := uc.adminUserRoleRepo.CreateBatch(txCtx, user.ID, input.RoleIDs); err != nil {
				return fmt.Errorf("assign roles: %w", err)
			}

			// Add Casbin grouping policies (user → role)
			for _, roleID := range input.RoleIDs {
				if err := uc.enforcer.AddGroupingPolicy(ctx, user.ID.String(), roleID.String()); err != nil {
					return fmt.Errorf("add casbin grouping policy for user %s role %s: %w", user.ID, roleID, err)
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
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
		passwordHash, err := bcrypt.GenerateFromPassword([]byte(*input.Password), bcryptCost)
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
