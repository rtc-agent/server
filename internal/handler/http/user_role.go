// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo" //nolint:depguard // TODO: refactor to use usecase layer
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/usecase"
)

// UserRoleHandler handles user-role association endpoints.
type UserRoleHandler struct {
	userRoleUsecase *usecase.UserRoleUsecase
	// Retained for GetCurrentUserWithRoles (called from AdminAuthHandler)
	userRepo     repo.UserRepo
	roleRepo     repo.RoleRepo
	userRoleRepo repo.UserRoleRepo
	enforcer     *auth.CasbinEnforcer
}

// NewUserRoleHandler creates a new UserRoleHandler.
func NewUserRoleHandler(
	userRoleUsecase *usecase.UserRoleUsecase,
	userRepo repo.UserRepo,
	roleRepo repo.RoleRepo,
	userRoleRepo repo.UserRoleRepo,
	enforcer *auth.CasbinEnforcer,
) *UserRoleHandler {
	return &UserRoleHandler{
		userRoleUsecase: userRoleUsecase,
		userRepo:        userRepo,
		roleRepo:        roleRepo,
		userRoleRepo:    userRoleRepo,
		enforcer:        enforcer,
	}
}

// RegisterRoutes registers user-role routes.
func (h *UserRoleHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/users/:id/roles", h.ListUserRoles)
	r.POST("/users/:id/roles", h.AssignRoles)
	r.DELETE("/users/:id/roles/:roleId", h.RemoveRole)
	r.GET("/roles/:id/users", h.ListRoleUsers)
}

// AssignRolesRequest is the request body for POST /api/users/:id/roles.
type AssignRolesRequest struct {
	RoleIDs []string `json:"role_ids" binding:"required,min=1,dive,uuid"`
}

// UserRoleResponse is the response for a user-role assignment.
type UserRoleResponse struct {
	UserID     string `json:"user_id"`
	RoleID     string `json:"role_id"`
	RoleName   string `json:"role_name"`
	AssignedAt string `json:"assigned_at"`
}

// ListUserRoles lists all roles assigned to a user.
func (h *UserRoleHandler) ListUserRoles(c *gin.Context) {
	userIDStr := c.Param("id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		Error(c, "validation_error", "无效的用户 ID")
		return
	}

	ctx := c.Request.Context()
	urs, err := h.userRoleUsecase.ListUserRoles(ctx, userID)
	if err != nil {
		Error(c, "server_error", "查询用户角色失败")
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

// AssignRoles assigns roles to a user (batch).
func (h *UserRoleHandler) AssignRoles(c *gin.Context) {
	userIDStr := c.Param("id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		Error(c, "validation_error", "无效的用户 ID")
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

	if err := h.userRoleUsecase.AssignRoles(ctx, usecase.AssignRolesInput{
		UserID:  userID,
		RoleIDs: req.RoleIDs,
	}, operatorID, operatorIP); err != nil {
		switch {
		case errors.Is(err, usecase.ErrUserNotFound):
			Error(c, "user_not_found", "用户不存在")
		case errors.Is(err, repo.ErrRoleNotFound):
			Error(c, "role_not_found", "角色不存在")
		case errors.Is(err, repo.ErrRoleDisabled):
			Error(c, "role_disabled", "角色已禁用")
		default:
			logger.Error(ctx, "user_role.assign_failed", zap.Error(err))
			Error(c, "server_error", "分配角色失败")
		}
		return
	}

	Success(c, gin.H{"status": "ok"})
}

// RemoveRole removes a role from a user.
func (h *UserRoleHandler) RemoveRole(c *gin.Context) {
	userIDStr := c.Param("id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		Error(c, "validation_error", "无效的用户 ID")
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

	if err := h.userRoleUsecase.RemoveRole(ctx, usecase.RemoveRoleInput{
		UserID: userID,
		RoleID: roleID,
	}, operatorID, operatorIP); err != nil {
		switch {
		case errors.Is(err, repo.ErrRoleNotFound):
			Error(c, "role_not_found", "角色不存在")
		case errors.Is(err, repo.ErrCannotRemoveLastAdmin):
			Error(c, "cannot_remove_last_admin", "不能移除最后一个管理员角色")
		case errors.Is(err, repo.ErrCannotRemoveSelfAdmin):
			Error(c, "cannot_remove_self_admin", "不能移除自己的管理员角色")
		default:
			logger.Error(ctx, "user_role.remove_failed", zap.Error(err))
			Error(c, "server_error", "移除角色失败")
		}
		return
	}

	Success(c, gin.H{"status": "ok"})
}

// RoleUserResponse is the response for a user in a role.
type RoleUserResponse struct {
	UserID     string `json:"user_id"`
	UserEmail  string `json:"user_email"`
	UserName   string `json:"user_name"`
	AssignedAt string `json:"assigned_at"`
}

// ListRoleUsers lists all users assigned to a role.
func (h *UserRoleHandler) ListRoleUsers(c *gin.Context) {
	roleIDStr := c.Param("id")
	roleID, err := uuid.Parse(roleIDStr)
	if err != nil {
		Error(c, "validation_error", "无效的角色 ID")
		return
	}

	ctx := c.Request.Context()
	users, err := h.userRoleUsecase.ListRoleUsers(ctx, roleID)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrRoleNotFound):
			Error(c, "role_not_found", "角色不存在")
		default:
			Error(c, "server_error", "查询角色用户失败")
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
	user *model.User,
	userRoleRepo repo.UserRoleRepo,
	roleRepo repo.RoleRepo,
	enforcer *auth.CasbinEnforcer,
) (*UserWithRolesResponse, error) {
	resp := &UserWithRolesResponse{
		ID:        user.ID.String(),
		Email:     user.Email,
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
	}

	// Get user roles
	urs, err := userRoleRepo.ListByUserID(ctx, user.ID)
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
			if errors.Is(err, repo.ErrRoleNotFound) {
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
