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

// RtcUserResponse is the structured response for RTC user data.
type RtcUserResponse struct {
	ID           string  `json:"id"`
	Provider     string  `json:"provider"`
	Sub          string  `json:"sub"`
	Email        string  `json:"email"`
	Name         string  `json:"name"`
	AvatarURL    string  `json:"avatar_url"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	BannedAt     *string `json:"banned_at,omitempty"`
	BannedReason *string `json:"banned_reason,omitempty"`
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

	// Build response with structured type
	items := make([]RtcUserResponse, 0, len(result.Users))
	for _, user := range result.Users {
		item := RtcUserResponse{
			ID:        user.ID.String(),
			Provider:  user.Provider,
			Sub:       user.Sub,
			Email:     user.Email,
			Name:      user.Name,
			AvatarURL: user.AvatarURL,
			Status:    "active",
			CreatedAt: user.CreatedAt.Format(TimeFormatRFC3339),
			UpdatedAt: user.UpdatedAt.Format(TimeFormatRFC3339),
		}

		if user.BannedAt != nil {
			item.Status = "banned"
			bannedAt := user.BannedAt.Format(TimeFormatRFC3339)
			item.BannedAt = &bannedAt
			item.BannedReason = &user.BannedReason
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

	result := RtcUserResponse{
		ID:        user.ID.String(),
		Provider:  user.Provider,
		Sub:       user.Sub,
		Email:     user.Email,
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
		Status:    "active",
		CreatedAt: user.CreatedAt.Format(TimeFormatRFC3339),
		UpdatedAt: user.UpdatedAt.Format(TimeFormatRFC3339),
	}

	if user.BannedAt != nil {
		result.Status = "banned"
		bannedAt := user.BannedAt.Format(TimeFormatRFC3339)
		result.BannedAt = &bannedAt
		result.BannedReason = &user.BannedReason
	}

	Success(c, result)
}

// BanUserInput represents the input for banning a user.
type BanUserInput struct {
	Reason string `json:"reason" binding:"required,max=500"`
}

// BanUserResponse is the response for ban/unban operations.
type BanUserResponse struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	BannedAt     *string `json:"banned_at,omitempty"`
	BannedReason *string `json:"banned_reason,omitempty"`
}

// BanUser bans a user.
func (h *RtcUserHandler) BanUser(c *gin.Context) {
	ctx := c.Request.Context()

	userID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		Error(c, "invalid_param", "invalid user ID")
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodySize)
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

	bannedAt := user.BannedAt.Format(TimeFormatRFC3339)
	Success(c, BanUserResponse{
		ID:           user.ID.String(),
		Status:       "banned",
		BannedAt:     &bannedAt,
		BannedReason: &user.BannedReason,
	})
}

// UnbanUserResponse is the response for unban operation.
type UnbanUserResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
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

	Success(c, UnbanUserResponse{
		ID:     user.ID.String(),
		Status: "active",
	})
}

// DeviceResponse is the structured response for device data.
type DeviceResponse struct {
	ID           string `json:"id"`
	UserID       string `json:"user_id"`
	DeviceID     string `json:"device_id"`
	Name         string `json:"name"`
	UserAgent    string `json:"user_agent"`
	LastActiveAt string `json:"last_active_at"`
	CreatedAt    string `json:"created_at"`
	IsOnline     bool   `json:"is_online"`
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

	items := make([]DeviceResponse, 0, len(devices))
	for _, d := range devices {
		items = append(items, DeviceResponse{
			ID:           d.Device.ID.String(),
			UserID:       d.Device.UserID.String(),
			DeviceID:     d.Device.DeviceID,
			Name:         d.Device.Name,
			UserAgent:    d.Device.UserAgent,
			LastActiveAt: d.Device.LastActiveAt.Format(TimeFormatRFC3339),
			CreatedAt:    d.Device.CreatedAt.Format(TimeFormatRFC3339),
			IsOnline:     d.IsOnline,
		})
	}

	Success(c, gin.H{
		"items": items,
	})
}
