// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/usecase"
)

// RtcUserHandler handles RTC user-related HTTP requests.
type RtcUserHandler struct {
	rtcUserUsecase *usecase.RtcUserUsecase
}

// NewRtcUserHandler creates a new RtcUserHandler.
func NewRtcUserHandler(rtcUserUsecase *usecase.RtcUserUsecase) *RtcUserHandler {
	return &RtcUserHandler{
		rtcUserUsecase: rtcUserUsecase,
	}
}

// RegisterRoutes registers RTC user routes under /api/rtc-users.
func (h *RtcUserHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/rtc-users", h.ListUsers)
	r.GET("/rtc-users/:id", h.GetUser)
	r.POST("/rtc-users/:id/ban", h.BanUser)
	r.POST("/rtc-users/:id/unban", h.UnbanUser)
}

// ListUsers returns a paginated list of RTC users.
// Query parameters: status (active/banned), search, page, page_size
func (h *RtcUserHandler) ListUsers(c *gin.Context) {
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

	filter := usecase.ListUsersFilter{
		Status:   c.Query("status"),
		Search:   c.Query("search"),
		Page:     page,
		PageSize: pageSize,
	}

	result, err := h.rtcUserUsecase.ListUsers(ctx, filter)
	if err != nil {
		Error(c, "server_error", "获取用户列表失败")
		return
	}

	// Build response
	items := make([]gin.H, 0, len(result.Users))
	for _, user := range result.Users {
		status := "active"
		if user.BannedAt != nil {
			status = "banned"
		}

		item := gin.H{
			"id":         user.ID,
			"provider":   user.Provider,
			"email":      user.Email,
			"name":       user.Name,
			"avatar_url": user.AvatarURL,
			"status":     status,
			"created_at": user.CreatedAt,
			"updated_at": user.UpdatedAt,
		}

		if user.BannedAt != nil {
			item["banned_at"] = user.BannedAt
			item["banned_reason"] = user.BannedReason
		}

		items = append(items, item)
	}

	Success(c, gin.H{
		"items":     items,
		"total":     result.Total,
		"page":      result.Page,
		"page_size": result.PageSize,
	})
}

// GetUser returns a user by ID.
func (h *RtcUserHandler) GetUser(c *gin.Context) {
	ctx := c.Request.Context()

	userID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		Error(c, "invalid_param", "无效的用户 ID")
		return
	}

	user, err := h.rtcUserUsecase.GetUser(ctx, userID)
	if err != nil {
		Error(c, "not_found", "用户不存在")
		return
	}

	status := "active"
	if user.BannedAt != nil {
		status = "banned"
	}

	result := gin.H{
		"id":         user.ID,
		"provider":   user.Provider,
		"sub":        user.Sub,
		"email":      user.Email,
		"name":       user.Name,
		"avatar_url": user.AvatarURL,
		"status":     status,
		"created_at": user.CreatedAt,
		"updated_at": user.UpdatedAt,
	}

	if user.BannedAt != nil {
		result["banned_at"] = user.BannedAt
		result["banned_reason"] = user.BannedReason
	}

	Success(c, result)
}

// BanUserInput represents the input for banning a user.
type BanUserInput struct {
	Reason string `json:"reason" binding:"required"`
}

// BanUser bans a user.
func (h *RtcUserHandler) BanUser(c *gin.Context) {
	ctx := c.Request.Context()

	userID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		Error(c, "invalid_param", "无效的用户 ID")
		return
	}

	var input BanUserInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Error(c, "invalid_param", "请提供封禁原因")
		return
	}

	user, err := h.rtcUserUsecase.BanUser(ctx, usecase.BanUserInput{
		UserID: userID,
		Reason: input.Reason,
	})
	if err != nil {
		Error(c, "server_error", "封禁用户失败")
		return
	}

	Success(c, gin.H{
		"id":            user.ID,
		"status":        "banned",
		"banned_at":     user.BannedAt,
		"banned_reason": user.BannedReason,
	})
}

// UnbanUser unbans a user.
func (h *RtcUserHandler) UnbanUser(c *gin.Context) {
	ctx := c.Request.Context()

	userID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		Error(c, "invalid_param", "无效的用户 ID")
		return
	}

	user, err := h.rtcUserUsecase.UnbanUser(ctx, userID)
	if err != nil {
		Error(c, "server_error", "解封用户失败")
		return
	}

	Success(c, gin.H{
		"id":     user.ID,
		"status": "active",
	})
}
