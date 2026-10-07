// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/usecase"
)

// AdminUserHandler handles admin user-related HTTP requests.
type AdminUserHandler struct {
	adminUserUsecase *usecase.AdminUserUsecase
}

// NewAdminUserHandler creates a new AdminUserHandler.
func NewAdminUserHandler(adminUserUsecase *usecase.AdminUserUsecase) *AdminUserHandler {
	return &AdminUserHandler{
		adminUserUsecase: adminUserUsecase,
	}
}

// RegisterRoutes registers admin user routes under /api/admin-users.
func (h *AdminUserHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/admin-users", h.ListUsers)
	r.POST("/admin-users", h.CreateUser)
	r.PUT("/admin-users/:id", h.UpdateUser)
}

// ListUsers returns a paginated list of admin users.
// Query parameters: page (default 1), page_size (default 20, max 100)
func (h *AdminUserHandler) ListUsers(c *gin.Context) {
	ctx := c.Request.Context()

	page := 1
	if p := c.Query("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}

	pageSize := 20
	if ps := c.Query("page_size"); ps != "" {
		if v, err := strconv.Atoi(ps); err == nil && v > 0 && v <= 100 {
			pageSize = v
		}
	}

	users, total, err := h.adminUserUsecase.ListUsersPaginated(ctx, page, pageSize)
	if err != nil {
		Error(c, "server_error", "获取管理员列表失败")
		return
	}

	// Build response with user details and their roles
	result := make([]gin.H, 0, len(users))
	for _, user := range users {
		// Fetch roles for this user
		roles, err := h.adminUserUsecase.GetUserRoles(ctx, user.ID)
		if err != nil {
			// Log error but continue, roles will be empty
			roles = nil
		}

		result = append(result, gin.H{
			"id":         user.ID,
			"email":      user.Email,
			"name":       user.Name,
			"avatar_url": user.AvatarURL,
			"created_at": user.CreatedAt,
			"updated_at": user.UpdatedAt,
			"roles":      roles,
		})
	}

	Success(c, gin.H{
		"items":     result,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// CreateUser creates a new admin user.
func (h *AdminUserHandler) CreateUser(c *gin.Context) {
	var req CreateAdminUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", err.Error())
		return
	}

	ctx := c.Request.Context()

	// Parse role IDs if provided
	roleIDs := make([]uuid.UUID, 0, len(req.RoleIDs))
	for _, roleIDStr := range req.RoleIDs {
		roleID, err := uuid.Parse(roleIDStr)
		if err != nil {
			Error(c, "validation_error", "invalid role_id format")
			return
		}
		roleIDs = append(roleIDs, roleID)
	}

	// Create user
	user, err := h.adminUserUsecase.CreateUser(ctx, usecase.CreateUserInput{
		Email:    req.Email,
		Password: req.Password,
		Name:     req.Name,
		RoleIDs:  roleIDs,
	})
	if err != nil {
		if errors.Is(err, usecase.ErrAdminUserNotFound) {
			Error(c, "admin_user_not_found", "管理员不存在")
			return
		}
		// Check if email already exists
		if err.Error() == "email already exists" {
			Error(c, "email_exists", "邮箱已存在")
			return
		}
		Error(c, "server_error", "创建管理员失败")
		return
	}

	Success(c, gin.H{
		"id":         user.ID,
		"email":      user.Email,
		"name":       user.Name,
		"avatar_url": user.AvatarURL,
		"created_at": user.CreatedAt,
		"updated_at": user.UpdatedAt,
	})
}

// UpdateUser updates an admin user's name and/or password.
func (h *AdminUserHandler) UpdateUser(c *gin.Context) {
	userIDStr := c.Param("id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		Error(c, "validation_error", "invalid user ID")
		return
	}

	var req UpdateAdminUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", err.Error())
		return
	}

	ctx := c.Request.Context()

	// Update user
	user, err := h.adminUserUsecase.UpdateUser(ctx, userID, usecase.UpdateUserInput{
		Name:     req.Name,
		Password: req.Password,
	})
	if err != nil {
		if errors.Is(err, usecase.ErrAdminUserNotFound) {
			Error(c, "admin_user_not_found", "管理员不存在")
			return
		}
		Error(c, "server_error", "更新管理员失败")
		return
	}

	Success(c, gin.H{
		"id":         user.ID,
		"email":      user.Email,
		"name":       user.Name,
		"avatar_url": user.AvatarURL,
		"created_at": user.CreatedAt,
		"updated_at": user.UpdatedAt,
	})
}
