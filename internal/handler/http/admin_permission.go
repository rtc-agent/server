// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"errors"
	"net/http"

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
	r.DELETE("/permissions/:role_id/:resource/:action", h.Delete)
	r.POST("/permissions/check", h.Check)
}

// CreatePermissionRequest is the request body for POST /api/permissions.
type CreatePermissionRequest struct {
	RoleID   string `json:"role_id" binding:"required"`
	Resource string `json:"resource" binding:"required,min=1,max=100"`
	Action   string `json:"action" binding:"required,min=1,max=100"`
}

// DeletePermissionRequest is no longer used — DELETE now uses path parameters.
// Deprecated: kept for backwards compatibility reference only.
// Use path parameters: DELETE /permissions/:role_id/:resource/:action

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

	// Parse pagination parameters (with caps to prevent excessive memory use)
	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)
	if pageSize > 200 {
		pageSize = 200
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if page < 1 {
		page = 1
	}

	policies, err := h.permissionUsecase.ListPermissionsPaginated(ctx, filter, page, pageSize)
	if err != nil {
		Error(c, "server_error", "Failed to query permissions")
		return
	}

	// Build response items
	items := make([]PolicyResponse, 0, len(policies.Items))
	for _, p := range policies.Items {
		if len(p) >= 3 {
			items = append(items, PolicyResponse{
				RoleID:   p[0],
				Resource: p[1],
				Action:   p[2],
			})
		}
	}

	Success(c, gin.H{"items": items, "total": policies.Total, "page": page, "page_size": pageSize})
}

// Create adds a new permission policy.
func (h *PermissionHandler) Create(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req CreatePermissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
		return
	}

	// Validate UUID format
	if _, err := uuid.Parse(req.RoleID); err != nil {
		Error(c, "validation_error", "Invalid role ID format")
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
			Error(c, "role_not_found", "Role not found")
			return
		}
		if errors.Is(err, usecase.ErrPermissionExists) {
			Error(c, "permission_exists", "Permission policy already exists")
			return
		}
		Error(c, "server_error", "Failed to create permission policy")
		return
	}

	Success(c, gin.H{"status": "ok"})
}

// Delete removes a permission policy.
// Uses path parameters instead of request body (P2 fix: DELETE with body).
func (h *PermissionHandler) Delete(c *gin.Context) {
	roleID := c.Param("role_id")
	resource := c.Param("resource")
	action := c.Param("action")

	// Validate role_id is a valid UUID
	if _, err := uuid.Parse(roleID); err != nil {
		Error(c, "validation_error", "Invalid role ID format")
		return
	}

	// Validate resource and action are not empty
	if resource == "" || action == "" {
		Error(c, "validation_error", "Resource and action are required")
		return
	}

	ctx := c.Request.Context()
	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()
	if err := h.permissionUsecase.DeletePermission(ctx, usecase.DeletePermissionInput{
		RoleID:   roleID,
		Resource: resource,
		Action:   action,
	}, operatorID, operatorIP); err != nil {
		Error(c, "server_error", "Failed to delete permission policy")
		return
	}

	Success(c, gin.H{"status": "ok"})
}

// Check checks whether a user has a specific permission.
func (h *PermissionHandler) Check(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req CheckPermissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
		return
	}

	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		Error(c, "validation_error", "Invalid user ID format")
		return
	}

	ctx := c.Request.Context()
	allowed, err := h.permissionUsecase.CheckPermission(ctx, userID, req.Resource, req.Action)
	if err != nil {
		Error(c, "server_error", "Permission check failed")
		return
	}

	Success(c, gin.H{"allowed": allowed})
}
