// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// Handler-local interfaces for data access (depguard: handlers cannot import repo).
// The actual implementations from repo package satisfy these via Go's implicit interfaces.
type (
	adminUserRoleLister interface {
		ListByUserID(ctx context.Context, userID uuid.UUID) ([]model.AdminUserRole, error)
	}
	roleLookup interface {
		GetByID(ctx context.Context, id uuid.UUID) (*model.AdminRole, error)
	}
	adminUserLookup interface {
		GetByID(ctx context.Context, id uuid.UUID) (*model.AdminUser, error)
	}
)

// AdminUserRoleHandler handles admin user-role association endpoints.
type AdminUserRoleHandler struct {
	adminUserRoleUsecase *usecase.AdminUserRoleUsecase
	// Retained for GetCurrentUserWithRoles (called from AdminAuthHandler)
	adminUserRepo     adminUserLookup
	roleRepo          roleLookup
	adminUserRoleRepo adminUserRoleLister
	enforcer          *auth.CasbinEnforcer
}

// NewAdminUserRoleHandler creates a new AdminUserRoleHandler.
func NewAdminUserRoleHandler(
	adminUserRoleUsecase *usecase.AdminUserRoleUsecase,
	adminUserRepo adminUserLookup,
	roleRepo roleLookup,
	adminUserRoleRepo adminUserRoleLister,
	enforcer *auth.CasbinEnforcer,
) *AdminUserRoleHandler {
	return &AdminUserRoleHandler{
		adminUserRoleUsecase: adminUserRoleUsecase,
		adminUserRepo:        adminUserRepo,
		roleRepo:             roleRepo,
		adminUserRoleRepo:    adminUserRoleRepo,
		enforcer:             enforcer,
	}
}

// RegisterRoutes registers admin user-role routes.
func (h *AdminUserRoleHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/admin-users/:id/roles", h.ListUserRoles)
	r.POST("/admin-users/:id/roles", h.AssignRoles)
	r.DELETE("/admin-users/:id/roles/:roleId", h.RemoveRole)
	r.GET("/roles/:id/admin-users", h.ListRoleUsers)
}

// AssignRolesRequest is the request body for POST /api/admin-users/:id/roles.
type AssignRolesRequest struct {
	RoleIDs []string `json:"role_ids" binding:"required,min=1,dive,uuid"`
}

// UserRoleResponse is the response for an admin user-role assignment.
type UserRoleResponse struct {
	UserID     string `json:"user_id"`
	RoleID     string `json:"role_id"`
	RoleName   string `json:"role_name"`
	AssignedAt string `json:"assigned_at"`
}

// ListUserRoles lists all roles assigned to an admin user.
func (h *AdminUserRoleHandler) ListUserRoles(c *gin.Context) {
	userIDStr := c.Param("id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		Error(c, "validation_error", "无效的管理员 ID")
		return
	}

	ctx := c.Request.Context()
	urs, err := h.adminUserRoleUsecase.ListUserRoles(ctx, userID)
	if err != nil {
		Error(c, "server_error", "查询管理员角色失败")
		return
	}

	result := make([]UserRoleResponse, 0, len(urs))
	for _, ur := range urs {
		result = append(result, UserRoleResponse{
			UserID:     ur.UserID.String(),
			RoleID:     ur.RoleID.String(),
			RoleName:   ur.RoleName,
			AssignedAt: ur.AssignedAt,
		})
	}

	Success(c, gin.H{"items": result, "total": len(result)})
}

// AssignRoles assigns roles to an admin user (batch).
func (h *AdminUserRoleHandler) AssignRoles(c *gin.Context) {
	userIDStr := c.Param("id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		Error(c, "validation_error", "无效的管理员 ID")
		return
	}

	var req AssignRolesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
		return
	}

	ctx := c.Request.Context()
	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()

	if err := h.adminUserRoleUsecase.AssignRoles(ctx, usecase.AssignRolesInput{
		UserID:  userID,
		RoleIDs: req.RoleIDs,
	}, operatorID, operatorIP); err != nil {
		switch {
		case errors.Is(err, usecase.ErrAdminUserNotFound):
			Error(c, "admin_user_not_found", "管理员不存在")
		case errors.Is(err, usecase.ErrRoleNotFound):
			Error(c, "role_not_found", "角色不存在")
		case errors.Is(err, usecase.ErrRoleDisabled):
			Error(c, "role_disabled", "角色已禁用")
		default:
			logger.Error(ctx, "admin_user_role.assign_failed", zap.Error(err))
			Error(c, "server_error", "分配角色失败")
		}
		return
	}

	Success(c, gin.H{"status": "ok"})
}

// RemoveRole removes a role from an admin user.
func (h *AdminUserRoleHandler) RemoveRole(c *gin.Context) {
	userIDStr := c.Param("id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		Error(c, "validation_error", "无效的管理员 ID")
		return
	}

	roleIDStr := c.Param("roleId")
	roleID, err := uuid.Parse(roleIDStr)
	if err != nil {
		Error(c, "validation_error", "无效的角色 ID")
		return
	}

	ctx := c.Request.Context()
	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()

	if err := h.adminUserRoleUsecase.RemoveRole(ctx, usecase.RemoveRoleInput{
		UserID: userID,
		RoleID: roleID,
	}, operatorID, operatorIP); err != nil {
		switch {
		case errors.Is(err, usecase.ErrRoleNotFound):
			Error(c, "role_not_found", "角色不存在")
		case errors.Is(err, usecase.ErrCannotRemoveLastAdmin):
			Error(c, "cannot_remove_last_admin", "不能移除最后一个管理员角色")
		case errors.Is(err, usecase.ErrCannotRemoveSelfAdmin):
			Error(c, "cannot_remove_self_admin", "不能移除自己的管理员角色")
		default:
			logger.Error(ctx, "admin_user_role.remove_failed", zap.Error(err))
			Error(c, "server_error", "移除角色失败")
		}
		return
	}

	Success(c, gin.H{"status": "ok"})
}

// RoleUserResponse is the response for an admin user in a role.
type RoleUserResponse struct {
	UserID     string `json:"user_id"`
	UserEmail  string `json:"user_email"`
	UserName   string `json:"user_name"`
	AssignedAt string `json:"assigned_at"`
}

// ListRoleUsers lists all admin users assigned to a role.
func (h *AdminUserRoleHandler) ListRoleUsers(c *gin.Context) {
	roleIDStr := c.Param("id")
	roleID, err := uuid.Parse(roleIDStr)
	if err != nil {
		Error(c, "validation_error", "无效的角色 ID")
		return
	}

	ctx := c.Request.Context()
	users, err := h.adminUserRoleUsecase.ListRoleUsers(ctx, roleID)
	if err != nil {
		switch {
		case errors.Is(err, usecase.ErrRoleNotFound):
			Error(c, "role_not_found", "角色不存在")
		default:
			Error(c, "server_error", "查询角色管理员失败")
		}
		return
	}

	result := make([]RoleUserResponse, 0, len(users))
	for _, u := range users {
		result = append(result, RoleUserResponse{
			UserID:     u.UserID.String(),
			UserEmail:  u.UserEmail,
			UserName:   u.UserName,
			AssignedAt: u.AssignedAt,
		})
	}

	Success(c, gin.H{"items": result, "total": len(result)})
}

// GetCurrentUserWithRoles is used by the /api/auth/me endpoint to include roles and permissions.
// This function is called from AdminAuthHandler.GetCurrentUser.
func GetCurrentUserWithRoles(
	ctx context.Context,
	user *model.AdminUser,
	adminUserRoleRepo adminUserRoleLister,
	roleRepo roleLookup,
	enforcer *auth.CasbinEnforcer,
) (*UserWithRolesResponse, error) {
	resp := &UserWithRolesResponse{
		ID:        user.ID.String(),
		Email:     user.Email,
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
	}

	// Get admin user roles
	urs, err := adminUserRoleRepo.ListByUserID(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	type RoleBrief struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}

	roles := make([]RoleBrief, 0, len(urs))
	for _, ur := range urs {
		role, err := roleRepo.GetByID(ctx, ur.RoleID)
		if err != nil {
			if errors.Is(err, usecase.ErrRoleNotFound) {
				continue
			}
			return nil, err
		}
		if !role.IsEnabled {
			continue // Skip disabled roles
		}
		roles = append(roles, RoleBrief{
			ID:          role.ID.String(),
			Name:        role.Name,
			DisplayName: role.DisplayName,
		})
	}
	resp.Roles = roles

	// Get user permissions (aggregated from all roles)
	policies := enforcer.GetPermissionsForUser(user.ID.String())
	type PermBrief struct {
		Resource string `json:"resource"`
		Action   string `json:"action"`
	}
	perms := make([]PermBrief, 0, len(policies))
	for _, p := range policies {
		if len(p) >= 2 {
			perms = append(perms, PermBrief{Resource: p[0], Action: p[1]})
		}
	}
	resp.Permissions = perms

	return resp, nil
}

// UserWithRolesResponse extends UserResponse with roles and permissions.
type UserWithRolesResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Name        string `json:"name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	Roles       any    `json:"roles,omitempty"`
	Permissions any    `json:"permissions,omitempty"`
}
