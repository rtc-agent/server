// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/rtc-agent/server/internal/usecase"
)

// UserConfigHandler handles user-level config override endpoints.
type UserConfigHandler struct {
	uc *usecase.ServerConfigUsecase
}

// NewUserConfigHandler creates a new UserConfigHandler.
func NewUserConfigHandler(uc *usecase.ServerConfigUsecase) *UserConfigHandler {
	return &UserConfigHandler{uc: uc}
}

// RegisterRoutes registers user config routes.
func (h *UserConfigHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/rtc-users/:userId/configs", h.List)
	r.GET("/rtc-users/:userId/configs/:key", h.Get)
	r.PUT("/rtc-users/:userId/configs/:key", h.Update)
	r.DELETE("/rtc-users/:userId/configs/:key", h.Delete)
	r.GET("/rtc-users/:userId/configs/:key/history", h.GetHistory)
	r.POST("/rtc-users/:userId/configs/:key/rollback", h.Rollback)
}

// List returns the complete config view for a user (yaml + system + user merged).
func (h *UserConfigHandler) List(c *gin.Context) {
	userID, ok := parseUserIDParam(c)
	if !ok {
		return
	}
	category := c.Query("category")
	items, err := h.uc.GetUserConfigsView(c.Request.Context(), userID, category)
	if err != nil {
		Error(c, "server_error", "查询用户配置失败")
		return
	}
	Success(c, gin.H{"user_id": userID.String(), "items": items})
}

// Get returns a single user config view item.
func (h *UserConfigHandler) Get(c *gin.Context) {
	userID, ok := parseUserIDParam(c)
	if !ok {
		return
	}
	key := c.Param("key")
	item, err := h.uc.GetUserConfig(c.Request.Context(), userID, key)
	if err != nil {
		if errors.Is(err, usecase.ErrConfigKeyNotFound) {
			Error(c, "CONFIG_KEY_NOT_FOUND", "配置项不存在")
			return
		}
		Error(c, "server_error", "查询用户配置失败")
		return
	}
	Success(c, item)
}

// Update sets a user-level config override.
func (h *UserConfigHandler) Update(c *gin.Context) {
	userID, ok := parseUserIDParam(c)
	if !ok {
		return
	}
	key := c.Param("key")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req updateConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "VALIDATION_ERROR", "请求参数错误: "+err.Error())
		return
	}

	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()

	item, err := h.uc.SetUserConfigOverride(c.Request.Context(), userID, key, usecase.UpdateConfigInput{
		Value:      req.Value,
		Version:    req.Version,
		ChangeNote: req.ChangeNote,
	}, operatorID, operatorIP)
	if err != nil {
		h.handleUserUpdateError(c, err)
		return
	}
	Success(c, item)
}

// Delete deletes a user-level config override.
func (h *UserConfigHandler) Delete(c *gin.Context) {
	userID, ok := parseUserIDParam(c)
	if !ok {
		return
	}
	key := c.Param("key")
	versionStr := c.Query("version")
	if versionStr == "" {
		Error(c, "VALIDATION_ERROR", "version 参数必填")
		return
	}
	version := parseIntDefault(versionStr, -1)
	if version < 0 {
		Error(c, "VALIDATION_ERROR", "version 参数无效")
		return
	}

	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()

	deletedValue, deletedVersion, err := h.uc.DeleteUserConfigOverride(c.Request.Context(), userID, key, version, operatorID, operatorIP)
	if err != nil {
		h.handleUserDeleteError(c, err)
		return
	}
	Success(c, gin.H{
		"key":             key,
		"user_id":         userID.String(),
		"deleted_value":   deletedValue,
		"deleted_version": deletedVersion,
	})
}

// GetHistory returns config change history for a user key.
func (h *UserConfigHandler) GetHistory(c *gin.Context) {
	userID, ok := parseUserIDParam(c)
	if !ok {
		return
	}
	key := c.Param("key")
	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)

	items, total, err := h.uc.GetHistory(c.Request.Context(), key, &userID, page, pageSize)
	if err != nil {
		if errors.Is(err, usecase.ErrConfigKeyNotFound) {
			Error(c, "CONFIG_KEY_NOT_FOUND", "配置项不存在")
			return
		}
		Error(c, "server_error", "查询变更历史失败")
		return
	}
	Success(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// Rollback rolls back a user config to a historical version.
func (h *UserConfigHandler) Rollback(c *gin.Context) {
	userID, ok := parseUserIDParam(c)
	if !ok {
		return
	}
	key := c.Param("key")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req rollbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "VALIDATION_ERROR", "请求参数错误: "+err.Error())
		return
	}

	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()

	value, newVersion, err := h.uc.RollbackConfig(c.Request.Context(), key, &userID, usecase.RollbackConfigInput{
		TargetVersion: req.TargetVersion,
		Version:       req.Version,
		ChangeNote:    req.ChangeNote,
	}, operatorID, operatorIP)
	if err != nil {
		h.handleUserRollbackError(c, err)
		return
	}
	Success(c, gin.H{
		"key":                      key,
		"user_id":                  userID.String(),
		"value":                    value,
		"version":                  newVersion,
		"rolled_back_from_version": req.Version,
		"change_note":              req.ChangeNote,
	})
}

// handleUserUpdateError maps usecase errors to HTTP responses for user update operations.
func (h *UserConfigHandler) handleUserUpdateError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, usecase.ErrConfigKeyNotFound):
		Error(c, "CONFIG_KEY_NOT_FOUND", "配置项不存在")
	case errors.Is(err, usecase.ErrUserNotFound):
		Error(c, "USER_NOT_FOUND", "用户不存在")
	case errors.Is(err, usecase.ErrOptimisticLock):
		Error(c, "OPTIMISTIC_LOCK_CONFLICT", "配置已被其他管理员修改，请重新加载后重试")
	case errors.Is(err, usecase.ErrValidation):
		Error(c, "VALIDATION_ERROR", err.Error())
	default:
		Error(c, "server_error", "更新用户配置失败")
	}
}

// handleUserDeleteError maps usecase errors to HTTP responses for user delete operations.
func (h *UserConfigHandler) handleUserDeleteError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, usecase.ErrConfigKeyNotFound):
		Error(c, "CONFIG_KEY_NOT_FOUND", "配置项不存在")
	case errors.Is(err, usecase.ErrConfigNotFound):
		Error(c, "CONFIG_NOT_FOUND", "该用户无此配置覆盖记录")
	case errors.Is(err, usecase.ErrOptimisticLock):
		Error(c, "OPTIMISTIC_LOCK_CONFLICT", "配置已被其他管理员修改，请重新加载后重试")
	default:
		Error(c, "server_error", "删除用户配置失败")
	}
}

// handleUserRollbackError maps usecase errors to HTTP responses for user rollback operations.
func (h *UserConfigHandler) handleUserRollbackError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, usecase.ErrConfigKeyNotFound):
		Error(c, "CONFIG_KEY_NOT_FOUND", "配置项不存在")
	case errors.Is(err, usecase.ErrConfigNotFound):
		Error(c, "CONFIG_NOT_FOUND", "该配置项无数据库记录，无法回滚")
	case errors.Is(err, usecase.ErrVersionNotFound):
		Error(c, "VERSION_NOT_FOUND", "目标版本不存在，可能已超出保留期限")
	case errors.Is(err, usecase.ErrNoOp):
		Error(c, "NO_OP", "目标版本与当前版本相同，无需回滚")
	case errors.Is(err, usecase.ErrOptimisticLock):
		Error(c, "OPTIMISTIC_LOCK_CONFLICT", "配置已被其他管理员修改，请重新加载后重试")
	default:
		Error(c, "server_error", "回滚用户配置失败")
	}
}
