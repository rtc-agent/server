// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/usecase"
)

// validSessionStatuses defines the allowed session status filter values.
var validSessionStatuses = map[string]bool{
	"active": true,
	"idle":   true,
	"closed": true,
}

// validMessageRoles defines the allowed message role filter values.
var validMessageRoles = map[string]bool{
	"user":      true,
	"assistant": true,
	"system":    true,
}

// RtcSessionHandler handles RTC session and message management HTTP requests.
type RtcSessionHandler struct {
	uc *usecase.RtcSessionUsecase
}

// NewRtcSessionHandler creates a new RtcSessionHandler.
func NewRtcSessionHandler(uc *usecase.RtcSessionUsecase) *RtcSessionHandler {
	return &RtcSessionHandler{uc: uc}
}

// RegisterRoutes registers RTC session routes under /api/rtc-users.
func (h *RtcSessionHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/rtc-users/sessions", h.ListSessions)
	r.GET("/rtc-users/sessions/stats", h.GetSessionStats)
	r.GET("/rtc-users/sessions/:id/messages", h.ListMessages)
}

// SessionResponse is the structured response for session data.
type SessionResponse struct {
	ID                    string `json:"id"`
	ClientID              string `json:"client_id"`
	Title                 string `json:"title"`
	Status                string `json:"status"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
	TotalInputTokens      int64  `json:"total_input_tokens"`
	TotalOutputTokens     int64  `json:"total_output_tokens"`
	TotalTokens           int64  `json:"total_tokens"`
	TotalCachedReadTokens int64  `json:"total_cached_read_tokens"`
	TotalCostMicros       int64  `json:"total_cost_micros"`
}

// ListSessionsResponse is the paginated response for session list.
type ListSessionsResponse struct {
	Items    []SessionResponse `json:"items"`
	Total    int64             `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
}

// ListSessions returns a paginated list of sessions for a user.
func (h *RtcSessionHandler) ListSessions(c *gin.Context) {
	ctx := c.Request.Context()

	userID := c.Query("user_id")
	if userID == "" {
		Error(c, "invalid_param", "user_id is required")
		return
	}

	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)
	if pageSize > 100 {
		pageSize = 100
	}

	filter := usecase.ListSessionsFilter{
		UserID:   userID,
		Status:   c.Query("status"),
		Search:   c.Query("search"),
		Page:     page,
		PageSize: pageSize,
	}

	// Validate optional status filter.
	if filter.Status != "" && !validSessionStatuses[filter.Status] {
		Error(c, "invalid_param", "status must be one of: active, idle, closed")
		return
	}

	startTime, err := parseOptionalTime(c.Query("start_time"))
	if err != nil {
		Error(c, "invalid_param", "start_time must be in RFC3339 format")
		return
	}
	filter.StartTime = startTime

	endTime, err := parseOptionalTime(c.Query("end_time"))
	if err != nil {
		Error(c, "invalid_param", "end_time must be in RFC3339 format")
		return
	}
	filter.EndTime = endTime

	result, err := h.uc.ListSessions(ctx, filter)
	if err != nil {
		Error(c, "server_error", "failed to list sessions")
		return
	}

	items := make([]SessionResponse, 0, len(result.Sessions))
	for _, s := range result.Sessions {
		items = append(items, SessionResponse{
			ID:                    s.ID.String(),
			ClientID:              s.ClientID,
			Title:                 s.Title,
			Status:                s.Status,
			CreatedAt:             s.CreatedAt.Format(TimeFormatRFC3339),
			UpdatedAt:             s.UpdatedAt.Format(TimeFormatRFC3339),
			TotalInputTokens:      s.TotalInputTokens,
			TotalOutputTokens:     s.TotalOutputTokens,
			TotalTokens:           s.TotalTokens,
			TotalCachedReadTokens: s.TotalCachedReadTokens,
			TotalCostMicros:       s.TotalCostMicros,
		})
	}

	Success(c, ListSessionsResponse{
		Items:    items,
		Total:    result.Total,
		Page:     result.Page,
		PageSize: result.PageSize,
	})
}

// GetSessionStats returns token usage statistics for a user.
func (h *RtcSessionHandler) GetSessionStats(c *gin.Context) {
	ctx := c.Request.Context()

	userID := c.Query("user_id")
	if userID == "" {
		Error(c, "invalid_param", "user_id is required")
		return
	}

	days := parseIntDefault(c.Query("days"), 30)
	if days > 365 {
		days = 365
	}

	result, err := h.uc.GetTokenStats(ctx, userID, days)
	if err != nil {
		Error(c, "server_error", "failed to get session stats")
		return
	}

	Success(c, gin.H{
		"daily_stats":  result.DailyStats,
		"summary":      result.Summary,
		"top_sessions": result.TopSessions,
	})
}

// MessageResponse is the structured response for message data.
type MessageResponse struct {
	ID           string `json:"id"`
	SessionID    string `json:"session_id"`
	Role         string `json:"role"`
	Content      string `json:"content"`
	CreatedAt    string `json:"created_at"`
	GlobalOffset uint32 `json:"global_offset"`
	InputTokens  *int   `json:"input_tokens"`
	OutputTokens *int   `json:"output_tokens"`
	TotalTokens  *int   `json:"total_tokens"`
}

// ListMessagesResponse is the paginated response for message list.
type ListMessagesResponse struct {
	Items    []MessageResponse `json:"items"`
	Total    int64             `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
}

// ListMessages returns a paginated list of messages for a session.
func (h *RtcSessionHandler) ListMessages(c *gin.Context) {
	ctx := c.Request.Context()

	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		Error(c, "invalid_param", "invalid session ID")
		return
	}

	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)
	if pageSize > 100 {
		pageSize = 100
	}

	filter := usecase.ListMessagesFilter{
		Role:     c.Query("role"),
		Page:     page,
		PageSize: pageSize,
	}

	// Validate optional role filter.
	if filter.Role != "" && !validMessageRoles[filter.Role] {
		Error(c, "invalid_param", "role must be one of: user, assistant, system")
		return
	}

	createdAfter, err := parseOptionalTime(c.Query("created_after"))
	if err != nil {
		Error(c, "invalid_param", "created_after must be in RFC3339 format")
		return
	}
	filter.CreatedAfter = createdAfter

	createdBefore, err := parseOptionalTime(c.Query("created_before"))
	if err != nil {
		Error(c, "invalid_param", "created_before must be in RFC3339 format")
		return
	}
	filter.CreatedBefore = createdBefore

	result, err := h.uc.ListMessages(ctx, sessionID, filter)
	if err != nil {
		Error(c, "server_error", "failed to list messages")
		return
	}

	items := make([]MessageResponse, 0, len(result.Messages))
	for _, m := range result.Messages {
		items = append(items, MessageResponse{
			ID:           m.ID.String(),
			SessionID:    m.SessionID.String(),
			Role:         m.Role,
			Content:      m.Content,
			CreatedAt:    m.CreatedAt.Format(TimeFormatRFC3339),
			GlobalOffset: m.GlobalOffset,
			InputTokens:  m.InputTokens,
			OutputTokens: m.OutputTokens,
			TotalTokens:  m.TotalTokens,
		})
	}

	Success(c, ListMessagesResponse{
		Items:    items,
		Total:    result.Total,
		Page:     result.Page,
		PageSize: result.PageSize,
	})
}

// parseOptionalTime parses an RFC3339 time string. Returns nil for empty input,
// or an error if the format is invalid.
func parseOptionalTime(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
