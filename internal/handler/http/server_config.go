// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/usecase"
)

// ServerConfigHandler handles system-level dynamic configuration endpoints.
type ServerConfigHandler struct {
	uc *usecase.ServerConfigUsecase
}

// NewServerConfigHandler creates a new ServerConfigHandler.
func NewServerConfigHandler(uc *usecase.ServerConfigUsecase) *ServerConfigHandler {
	return &ServerConfigHandler{uc: uc}
}

// RegisterRoutes registers system config routes.
func (h *ServerConfigHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/configs", h.List)
	r.GET("/configs/:key", h.Get)
	r.PUT("/configs/:key", h.Update)
	r.DELETE("/configs/:key", h.Delete)
	r.GET("/configs/:key/history", h.GetHistory)
	r.POST("/configs/:key/rollback", h.Rollback)
}

// List returns registered config keys merged with DB values, with pagination.
func (h *ServerConfigHandler) List(c *gin.Context) {
	category := c.Query("category")
	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)

	items, total, err := h.uc.ListSystemConfigs(c.Request.Context(), category, page, pageSize)
	if err != nil {
		Error(c, "server_error", "Failed to query configs")
		return
	}
	Success(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// Get returns a single system config by key.
func (h *ServerConfigHandler) Get(c *gin.Context) {
	key := c.Param("key")
	item, err := h.uc.GetSystemConfig(c.Request.Context(), key)
	if err != nil {
		if errors.Is(err, usecase.ErrConfigKeyNotFound) {
			Error(c, "CONFIG_KEY_NOT_FOUND", "Config key not found")
			return
		}
		Error(c, "server_error", "Failed to query config")
		return
	}
	Success(c, item)
}

// updateConfigRequest is the request body for updating a config.
// Version is optional: omitted → force overwrite (skip optimistic lock check).
type updateConfigRequest struct {
	Value      any    `json:"value" binding:"required"`
	Version    *int   `json:"version" binding:"omitempty,gte=0"`
	ChangeNote string `json:"change_note"`
}

// Update updates a system config value.
func (h *ServerConfigHandler) Update(c *gin.Context) {
	key := c.Param("key")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req updateConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
		return
	}

	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()

	// version omitted → force overwrite (skip optimistic lock)
	version := -1
	if req.Version != nil {
		version = *req.Version
	}

	item, err := h.uc.UpdateSystemConfig(c.Request.Context(), key, usecase.UpdateConfigInput{
		Value:      req.Value,
		Version:    version,
		ChangeNote: req.ChangeNote,
	}, operatorID, operatorIP)
	if err != nil {
		h.handleUpdateError(c, err)
		return
	}
	Success(c, item)
}

// Delete deletes a system config (reverts to yaml default).
func (h *ServerConfigHandler) Delete(c *gin.Context) {
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

	deletedValue, deletedVersion, err := h.uc.DeleteSystemConfig(c.Request.Context(), key, version, operatorID, operatorIP)
	if err != nil {
		h.handleDeleteError(c, err)
		return
	}
	Success(c, gin.H{
		"key":             key,
		"deleted_value":   deletedValue,
		"deleted_version": deletedVersion,
	})
}

// GetHistory returns config change history.
func (h *ServerConfigHandler) GetHistory(c *gin.Context) {
	key := c.Param("key")
	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)

	items, total, err := h.uc.GetHistory(c.Request.Context(), key, nil, page, pageSize)
	if err != nil {
		if errors.Is(err, usecase.ErrConfigKeyNotFound) {
			Error(c, "CONFIG_KEY_NOT_FOUND", "Config key not found")
			return
		}
		Error(c, "server_error", "Failed to query change history")
		return
	}
	Success(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// rollbackRequest is the request body for rolling back a config.
type rollbackRequest struct {
	TargetVersion int    `json:"target_version" binding:"required,gte=1"`
	Version       int    `json:"version" binding:"required,gte=1"`
	ChangeNote    string `json:"change_note"`
}

// Rollback rolls back a config to a historical version.
func (h *ServerConfigHandler) Rollback(c *gin.Context) {
	key := c.Param("key")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var req rollbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, "validation_error", sanitizeBindingError(err))
		return
	}

	operatorID := getOperatorID(c)
	operatorIP := c.ClientIP()

	value, newVersion, err := h.uc.RollbackConfig(c.Request.Context(), key, nil, usecase.RollbackConfigInput{
		TargetVersion: req.TargetVersion,
		Version:       req.Version,
		ChangeNote:    req.ChangeNote,
	}, operatorID, operatorIP)
	if err != nil {
		h.handleRollbackError(c, err)
		return
	}
	Success(c, gin.H{
		"key":                      key,
		"value":                    value,
		"version":                  newVersion,
		"rolled_back_from_version": req.Version,
		"change_note":              req.ChangeNote,
	})
}

// handleUpdateError maps usecase errors to HTTP responses for update operations.
func (h *ServerConfigHandler) handleUpdateError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, usecase.ErrConfigKeyNotFound):
		Error(c, "CONFIG_KEY_NOT_FOUND", "Config key not found")
	case errors.Is(err, usecase.ErrOptimisticLock):
		h.respondOptimisticLock(c, "配置已被其他管理员修改，请重新加载后重试")
	case errors.Is(err, usecase.ErrValidation):
		Error(c, "VALIDATION_ERROR", err.Error())
	default:
		Error(c, "server_error", "Failed to update config")
	}
}

// handleDeleteError maps usecase errors to HTTP responses for delete operations.
func (h *ServerConfigHandler) handleDeleteError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, usecase.ErrConfigKeyNotFound):
		Error(c, "CONFIG_KEY_NOT_FOUND", "Config key not found")
	case errors.Is(err, usecase.ErrConfigNotFound):
		Error(c, "CONFIG_NOT_FOUND", "No database record for this config, cannot delete")
	case errors.Is(err, usecase.ErrOptimisticLock):
		h.respondOptimisticLock(c, "配置已被其他管理员修改，请重新加载后重试")
	default:
		Error(c, "server_error", "Failed to delete config")
	}
}

// handleRollbackError maps usecase errors to HTTP responses for rollback operations.
func (h *ServerConfigHandler) handleRollbackError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, usecase.ErrConfigKeyNotFound):
		Error(c, "CONFIG_KEY_NOT_FOUND", "Config key not found")
	case errors.Is(err, usecase.ErrConfigNotFound):
		Error(c, "CONFIG_NOT_FOUND", "No database record for this config, cannot rollback")
	case errors.Is(err, usecase.ErrVersionNotFound):
		Error(c, "VERSION_NOT_FOUND", "Target version not found, may have exceeded retention period")
	case errors.Is(err, usecase.ErrNoOp):
		Error(c, "NO_OP", "Target version is same as current, no rollback needed")
	case errors.Is(err, usecase.ErrOptimisticLock):
		h.respondOptimisticLock(c, "配置已被其他管理员修改，请重新加载后重试")
	default:
		Error(c, "server_error", "Failed to rollback config")
	}
}

// respondOptimisticLock returns the conflict response with current config details.
func (h *ServerConfigHandler) respondOptimisticLock(c *gin.Context, message string) {
	Error(c, "OPTIMISTIC_LOCK_CONFLICT", message)
}

// ── User config helper used by UserConfigHandler ─────────────────────

// parseUserIDParam parses :id path parameter.
func parseUserIDParam(c *gin.Context) (uuid.UUID, bool) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		Error(c, "VALIDATION_ERROR", "Invalid user ID")
		return uuid.Nil, false
	}
	return id, true
}
