// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/usecase"
)

// PermissionHandler handles permission management endpoints.
type PermissionHandler struct {
	permissionUsecase *usecase.PermissionUsecase
}

// NewPermissionHandler creates a new PermissionHandler.
func NewPermissionHandler(permissionUsecase *usecase.PermissionUsecase) *PermissionHandler {
	return &PermissionHandler{permissionUsecase: permissionUsecase}
}

// RegisterRoutes registers permission management routes under /api/permissions.
func (h *PermissionHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/permissions", h.List)
	r.POST("/permissions", h.Create)
	r.DELETE("/permissions", h.Delete)
	r.POST("/permissions/check", h.Check)
}

// CreatePermissionRequest is the request body for POST /api/permissions.
type CreatePermissionRequest struct {
	RoleID   string `json:"role_id" binding:"required"`
	Resource string `json:"resource" binding:"required,min=1,max=100"`
	Action   string `json:"action" binding:"required,min=1,max=100"`
}

// DeletePermissionRequest is the request body for DELETE /api/permissions.
type DeletePermissionRequest struct {
	RoleID   string `json:"role_id" binding:"required"`
	Resource string `json:"resource" binding:"required"`
	Action   string `json:"action" binding:"required"`
}

// CheckPermissionRequest is the request body for POST /api/permissions/check.
type CheckPermissionRequest struct {
	UserID   string `json:"user_id" binding:"required"`
	Resource string `json:"resource" binding:"required"`
	Action   string `json:"action" binding:"required"`
}

// PolicyResponse is the response body for a single policy.
type PolicyResponse struct {
	RoleID   string `json:"role_id"`
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

// List lists permission policies with optional filtering and pagination.
func (h *PermissionHandler) List(c *gin.Context) {
	ctx := c.Request.Context()

	// Parse query parameters
	filter := usecase.ListPermissionsFilter{
		RoleID:   c.Query("role_id"),
		Resource: c.Query("resource"),
	}

	policies, err := h.permissionUsecase.ListPermissions(ctx, filter)
	if err != nil {
		Error(c, "server_error", "查询权限列表失败")
		return
	}

	// Build response items
	items := make([]PolicyResponse, 0, len(policies))
	for _, p := range policies {
		if len(p) >= 3 {
			items = append(items, PolicyResponse{
				RoleID:   p[0],
				Resource: p[1],
				Action:   p[2],
			})
		}
	}

	total := len(items)

	// Apply pagination
	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	items = items[start:end]

	Success(c, gin.H{"items": items, "total": total})
}

// Create adds a new permission policy.
func (h *PermissionHandler) Create(c *gin.Context) {
	var req CreatePermissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
		return
	}

	// Validate UUID format
	if _, err := uuid.Parse(req.RoleID); err != nil {
		Error(c, "validation_error", "无效的角色 ID 格式")
		return
	}

	ctx := c.Request.Context()
	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()
	if err := h.permissionUsecase.CreatePermission(ctx, usecase.CreatePermissionInput{
		RoleID:   req.RoleID,
		Resource: req.Resource,
		Action:   req.Action,
	}, operatorID, operatorIP); err != nil {
		if errors.Is(err, usecase.ErrRoleNotFound) {
			Error(c, "role_not_found", "角色不存在")
			return
		}
		if errors.Is(err, usecase.ErrPermissionExists) {
			Error(c, "permission_exists", "权限策略已存在")
			return
		}
		Error(c, "server_error", "创建权限策略失败")
		return
	}

	Success(c, gin.H{"status": "ok"})
}

// Delete removes a permission policy.
func (h *PermissionHandler) Delete(c *gin.Context) {
	var req DeletePermissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
		return
	}

	ctx := c.Request.Context()
	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()
	if err := h.permissionUsecase.DeletePermission(ctx, usecase.DeletePermissionInput{
		RoleID:   req.RoleID,
		Resource: req.Resource,
		Action:   req.Action,
	}, operatorID, operatorIP); err != nil {
		Error(c, "server_error", "删除权限策略失败")
		return
	}

	Success(c, gin.H{"status": "ok"})
}

// Check checks whether a user has a specific permission.
func (h *PermissionHandler) Check(c *gin.Context) {
	var req CheckPermissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
		return
	}

	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		Error(c, "validation_error", "无效的用户 ID 格式")
		return
	}

	ctx := c.Request.Context()
	allowed, err := h.permissionUsecase.CheckPermission(ctx, userID, req.Resource, req.Action)
	if err != nil {
		Error(c, "server_error", "权限检查失败")
		return
	}

	Success(c, gin.H{"allowed": allowed})
}
