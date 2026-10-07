// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"errors"
	"net/http"
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

// AdminUserResponse is the response body for an admin user.
type AdminUserResponse struct {
	ID        string           `json:"id"`
	Email     string           `json:"email"`
	Name      string           `json:"name"`
	AvatarURL string           `json:"avatar_url,omitempty"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
	Roles     []AdminRoleBrief `json:"roles,omitempty"`
}

// AdminRoleBrief is a brief representation of a role embedded in user responses.
type AdminRoleBrief struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// ListUsersResponse is the response body for ListUsers.
type ListUsersResponse struct {
	Items    []AdminUserResponse `json:"items"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
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
		Error(c, "server_error", "Failed to get admin users")
		return
	}

	// Collect user IDs for batch role fetching
	userIDs := make([]uuid.UUID, 0, len(users))
	for _, user := range users {
		userIDs = append(userIDs, user.ID)
	}

	// Batch-fetch roles for all users to avoid N+1 queries
	rolesMap, err := h.adminUserUsecase.GetUsersRolesBatch(ctx, userIDs)
	if err != nil {
		Error(c, "server_error", "Failed to get user roles")
		return
	}

	// Build response with user details and their roles
	result := make([]AdminUserResponse, 0, len(users))
	for _, user := range users {
		roles := rolesMap[user.ID]
		roleBriefs := make([]AdminRoleBrief, 0, len(roles))
		for _, role := range roles {
			roleBriefs = append(roleBriefs, AdminRoleBrief{
				ID:          role.ID.String(),
				Name:        role.Name,
				DisplayName: role.DisplayName,
			})
		}

		result = append(result, AdminUserResponse{
			ID:        user.ID.String(),
			Email:     user.Email,
			Name:      user.Name,
			AvatarURL: user.AvatarURL,
			CreatedAt: user.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			UpdatedAt: user.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
			Roles:     roleBriefs,
		})
	}

	Success(c, ListUsersResponse{
		Items:    result,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// CreateUser creates a new admin user.
func (h *AdminUserHandler) CreateUser(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req CreateAdminUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
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
			Error(c, "admin_user_not_found", "Admin user not found")
			return
		}
		if errors.Is(err, usecase.ErrEmailAlreadyExists) {
			Error(c, "email_exists", "Email already exists")
			return
		}
		Error(c, "server_error", "Failed to create admin user")
		return
	}

	Success(c, AdminUserResponse{
		ID:        user.ID.String(),
		Email:     user.Email,
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
		CreatedAt: user.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: user.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
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

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req UpdateAdminUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
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
			Error(c, "admin_user_not_found", "Admin user not found")
			return
		}
		Error(c, "server_error", "Failed to update admin user")
		return
	}

	Success(c, AdminUserResponse{
		ID:        user.ID.String(),
		Email:     user.Email,
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
		CreatedAt: user.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: user.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}
