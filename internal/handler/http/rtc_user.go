// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"net/http"
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
	r.GET("/rtc-users/:id/devices", h.ListUserDevices)
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

	// Validate and parse status filter
	status := c.Query("status")
	if status != "" && status != "active" && status != "banned" {
		Error(c, "validation_error", "Invalid status value, must be 'active' or 'banned'")
		return
	}

	// Validate search length to prevent excessive queries
	search := c.Query("search")
	if len(search) > 200 {
		Error(c, "validation_error", "Search query too long, maximum 200 characters")
		return
	}

	filter := usecase.ListUsersFilter{
		Status:   status,
		Search:   search,
		Page:     page,
		PageSize: pageSize,
	}

	result, err := h.rtcUserUsecase.ListUsers(ctx, filter)
	if err != nil {
		Error(c, "server_error", "failed to list users")
		return
	}

	// Build response
	items := make([]gin.H, 0, len(result.Users))
	for _, user := range result.Users {
		status := "active"
		if user.BannedAt != nil {
			status = "banned"
		}

		item := map[string]any{
			"id":         user.ID,
			"provider":   user.Provider,
			"sub":        user.Sub,
			"email":      user.Email,
			"name":       user.Name,
			"avatar_url": user.AvatarURL,
			"status":     status,
			"created_at": user.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			"updated_at": user.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}

		if user.BannedAt != nil {
			item["banned_at"] = user.BannedAt.Format("2006-01-02T15:04:05Z07:00")
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
		Error(c, "invalid_param", "invalid user ID")
		return
	}

	user, err := h.rtcUserUsecase.GetUser(ctx, userID)
	if err != nil {
		Error(c, "not_found", "user not found")
		return
	}

	status := "active"
	if user.BannedAt != nil {
		status = "banned"
	}

	result := map[string]any{
		"id":         user.ID,
		"provider":   user.Provider,
		"sub":        user.Sub,
		"email":      user.Email,
		"name":       user.Name,
		"avatar_url": user.AvatarURL,
		"status":     status,
		"created_at": user.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		"updated_at": user.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}

	if user.BannedAt != nil {
		result["banned_at"] = user.BannedAt.Format("2006-01-02T15:04:05Z07:00")
		result["banned_reason"] = user.BannedReason
	}

	Success(c, result)
}

// BanUserInput represents the input for banning a user.
type BanUserInput struct {
	Reason string `json:"reason" binding:"required,max=500"`
}

// BanUser bans a user.
func (h *RtcUserHandler) BanUser(c *gin.Context) {
	ctx := c.Request.Context()

	userID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		Error(c, "invalid_param", "invalid user ID")
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminRequestBodySize)
	var input BanUserInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Error(c, "invalid_param", "reason is required")
		return
	}

	adminUserID := getOperatorID(c)
	if adminUserID == uuid.Nil {
		Error(c, "unauthorized", "admin user ID not found in context")
		return
	}

	// Get admin IP
	adminIP := c.ClientIP()

	user, err := h.rtcUserUsecase.BanUser(ctx, usecase.BanUserInput{
		UserID: userID,
		Reason: input.Reason,
	}, adminUserID, adminIP)
	if err != nil {
		Error(c, "server_error", "failed to ban user")
		return
	}

	Success(c, gin.H{
		"id":            user.ID,
		"status":        "banned",
		"banned_at":     user.BannedAt.Format("2006-01-02T15:04:05Z07:00"),
		"banned_reason": user.BannedReason,
	})
}

// UnbanUser unbans a user.
func (h *RtcUserHandler) UnbanUser(c *gin.Context) {
	ctx := c.Request.Context()

	userID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		Error(c, "invalid_param", "invalid user ID")
		return
	}

	adminUserID := getOperatorID(c)
	if adminUserID == uuid.Nil {
		Error(c, "unauthorized", "admin user ID not found in context")
		return
	}

	// Get admin IP
	adminIP := c.ClientIP()

	user, err := h.rtcUserUsecase.UnbanUser(ctx, userID, adminUserID, adminIP)
	if err != nil {
		Error(c, "server_error", "failed to unban user")
		return
	}

	Success(c, gin.H{
		"id":     user.ID,
		"status": "active",
	})
}

// ListUserDevices returns the list of devices for a user.
func (h *RtcUserHandler) ListUserDevices(c *gin.Context) {
	ctx := c.Request.Context()

	userID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		Error(c, "invalid_param", "invalid user ID")
		return
	}

	devices, err := h.rtcUserUsecase.ListUserDevices(ctx, userID)
	if err != nil {
		Error(c, "server_error", "failed to list devices")
		return
	}

	items := make([]gin.H, 0, len(devices))
	for _, d := range devices {
		items = append(items, gin.H{
			"id":             d.Device.ID,
			"user_id":        d.Device.UserID,
			"device_id":      d.Device.DeviceID,
			"name":           d.Device.Name,
			"user_agent":     d.Device.UserAgent,
			"last_active_at": d.Device.LastActiveAt.Format("2006-01-02T15:04:05Z07:00"),
			"created_at":     d.Device.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			"is_online":      d.IsOnline,
		})
	}

	Success(c, gin.H{
		"items": items,
	})
}
