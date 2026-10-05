// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/repo" //nolint:depguard // TODO: refactor to use usecase layer
	"github.com/rtc-agent/server/internal/usecase"
)

// RoleHandler handles role management endpoints.
type RoleHandler struct {
	roleUsecase *usecase.RoleUsecase
}

// NewRoleHandler creates a new RoleHandler.
func NewRoleHandler(roleUsecase *usecase.RoleUsecase) *RoleHandler {
	return &RoleHandler{roleUsecase: roleUsecase}
}

// RegisterRoutes registers role management routes under /api/roles.
func (h *RoleHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/roles", h.List)
	r.GET("/roles/:id", h.Get)
	r.POST("/roles", h.Create)
	r.PUT("/roles/:id", h.Update)
	r.PATCH("/roles/:id", h.Update) // PATCH supports partial update (same handler, pointer fields)
	r.DELETE("/roles/:id", h.Delete)
	r.GET("/roles/:id/policies", h.GetPolicies)
}

// CreateRoleRequest is the request body for POST /api/roles.
type CreateRoleRequest struct {
	Name        string `json:"name" binding:"required,min=2,max=100"`
	DisplayName string `json:"display_name" binding:"required,min=1,max=200"`
	Description string `json:"description" binding:"max=500"`
}

// RoleResponse is the response body for a single role.
type RoleResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	IsSystem    bool   `json:"is_system"`
	IsEnabled   bool   `json:"is_enabled"`
	CreatedAt   string `json:"created_at"`
}

// List lists roles with pagination support.
// Query parameters: page (default 1), page_size (default 20, max 100)
func (h *RoleHandler) List(c *gin.Context) {
	ctx := c.Request.Context()

	// Parse pagination parameters
	page := 1
	pageSize := 20
	if p := c.Query("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v >= 1 {
			page = v
		}
	}
	if ps := c.Query("page_size"); ps != "" {
		if v, err := strconv.Atoi(ps); err == nil && v >= 1 && v <= 100 {
			pageSize = v
		}
	}

	roles, total, err := h.roleUsecase.ListRolesPaginated(ctx, page, pageSize)
	if err != nil {
		Error(c, "server_error", "查询角色列表失败")
		return
	}

	result := make([]*RoleResponse, 0, len(roles))
	for _, r := range roles {
		result = append(result, &RoleResponse{
			ID:          r.ID.String(),
			Name:        r.Name,
			DisplayName: r.DisplayName,
			Description: r.Description,
			IsSystem:    r.IsSystem,
			IsEnabled:   r.IsEnabled,
			CreatedAt:   r.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	Success(c, gin.H{
		"items":     result,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// Get returns a single role by ID.
func (h *RoleHandler) Get(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		Error(c, "validation_error", "无效的角色 ID")
		return
	}

	ctx := c.Request.Context()
	role, err := h.roleUsecase.GetRole(ctx, id)
	if err != nil {
		if repo.IsNotFound(err) {
			Error(c, "role_not_found", "角色不存在")
			return
		}
		Error(c, "server_error", "查询角色失败")
		return
	}

	Success(c, &RoleResponse{
		ID:          role.ID.String(),
		Name:        role.Name,
		DisplayName: role.DisplayName,
		Description: role.Description,
		IsSystem:    role.IsSystem,
		IsEnabled:   role.IsEnabled,
		CreatedAt:   role.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// Create creates a new role.
func (h *RoleHandler) Create(c *gin.Context) {
	var req CreateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
		return
	}

	// Sanitize name: lowercase, trim space
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))

	// Validate name contains only ASCII characters (role names are system identifiers)
	for i, r := range req.Name {
		if r > 127 {
			Error(c, "validation_error", "角色名称只能包含 ASCII 字符")
			return
		}
		if i == 0 && (r < 'a' || r > 'z') {
			Error(c, "validation_error", "角色名称必须以小写字母开头")
			return
		}
	}

	ctx := c.Request.Context()
	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()

	role, err := h.roleUsecase.CreateRole(ctx, usecase.CreateRoleInput{
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Description: req.Description,
	}, operatorID, operatorIP)
	if err != nil {
		if errors.Is(err, repo.ErrRoleNameExists) {
			Error(c, "role_name_exists", "角色名称已存在")
			return
		}
		Error(c, "server_error", "创建角色失败")
		return
	}

	Success(c, &RoleResponse{
		ID:          role.ID.String(),
		Name:        role.Name,
		DisplayName: role.DisplayName,
		Description: role.Description,
		IsSystem:    role.IsSystem,
		IsEnabled:   role.IsEnabled,
		CreatedAt:   role.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// UpdateRoleRequest is the request body for PUT /api/roles/:id.
type UpdateRoleRequest struct {
	DisplayName *string `json:"display_name" binding:"min=1,max=200"`
	Description *string `json:"description" binding:"max=500"`
	IsEnabled   *bool   `json:"is_enabled"`
}

// Update updates an existing role.
func (h *RoleHandler) Update(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		Error(c, "validation_error", "无效的角色 ID")
		return
	}

	var req UpdateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
		return
	}

	ctx := c.Request.Context()
	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()

	role, err := h.roleUsecase.UpdateRole(ctx, id, usecase.UpdateRoleInput{
		DisplayName: req.DisplayName,
		Description: req.Description,
		IsEnabled:   req.IsEnabled,
	}, operatorID, operatorIP)
	if err != nil {
		if repo.IsNotFound(err) {
			Error(c, "role_not_found", "角色不存在")
			return
		}
		if errors.Is(err, repo.ErrConflict) {
			Error(c, "conflict", "角色已被其他用户修改，请刷新后重试")
			return
		}
		Error(c, "server_error", "更新角色失败")
		return
	}

	Success(c, &RoleResponse{
		ID:          role.ID.String(),
		Name:        role.Name,
		DisplayName: role.DisplayName,
		Description: role.Description,
		IsSystem:    role.IsSystem,
		IsEnabled:   role.IsEnabled,
		CreatedAt:   role.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// Delete deletes a role.
func (h *RoleHandler) Delete(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		Error(c, "validation_error", "无效的角色 ID")
		return
	}

	ctx := c.Request.Context()
	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()

	if err := h.roleUsecase.DeleteRole(ctx, id, operatorID, operatorIP); err != nil {
		switch {
		case errors.Is(err, repo.ErrCannotDeleteSystemRole):
			Error(c, "cannot_delete_system_role", "系统内置角色不可删除")
		case errors.Is(err, repo.ErrCannotRemoveLastAdmin):
			Error(c, "cannot_remove_last_admin", "不能删除最后一个管理员角色")
		case repo.IsNotFound(err):
			Error(c, "role_not_found", "角色不存在")
		default:
			Error(c, "server_error", "删除角色失败")
		}
		return
	}

	Success(c, gin.H{"status": "ok"})
}

// GetPolicies returns all permission policies for a role.
func (h *RoleHandler) GetPolicies(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		Error(c, "validation_error", "无效的角色 ID")
		return
	}

	ctx := c.Request.Context()
	policies, err := h.roleUsecase.GetRolePolicies(ctx, id)
	if err != nil {
		if repo.IsNotFound(err) {
			Error(c, "role_not_found", "角色不存在")
			return
		}
		Error(c, "server_error", "查询角色权限失败")
		return
	}

	// Convert to structured format
	type PolicyItem struct {
		Resource string `json:"resource"`
		Action   string `json:"action"`
	}
	items := make([]PolicyItem, 0, len(policies))
	for _, p := range policies {
		if len(p) >= 2 {
			items = append(items, PolicyItem{Resource: p[0], Action: p[1]})
		}
	}

	Success(c, gin.H{"items": items, "total": len(items)})
}

// getOperatorID extracts the operator's user ID from the gin context.
func getOperatorID(c *gin.Context) uuid.UUID {
	userIDStr, exists := c.Get("user_id")
	if !exists {
		return uuid.Nil
	}
	userID, ok := userIDStr.(string)
	if !ok {
		return uuid.Nil
	}
	id, err := uuid.Parse(userID)
	if err != nil {
		return uuid.Nil
	}
	return id
}
