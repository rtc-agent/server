// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/repo" //nolint:depguard // TODO: refactor to use usecase layer
)

// AuditLogHandler handles audit log query endpoints.
type AuditLogHandler struct {
	auditLogRepo repo.AuditLogRepo
}

// NewAuditLogHandler creates a new AuditLogHandler.
func NewAuditLogHandler(auditLogRepo repo.AuditLogRepo) *AuditLogHandler {
	return &AuditLogHandler{auditLogRepo: auditLogRepo}
}

// RegisterRoutes registers audit log routes.
func (h *AuditLogHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/audit-logs", h.List)
}

// AuditLogResponse is the response body for a single audit log entry.
type AuditLogResponse struct {
	ID           string      `json:"id"`
	OperatorID   string      `json:"operator_id"`
	OperatorIP   string      `json:"operator_ip,omitempty"`
	EventType    string      `json:"event_type"`
	ResourceType string      `json:"resource_type"`
	ResourceID   string      `json:"resource_id,omitempty"`
	Details      interface{} `json:"details,omitempty"`
	CreatedAt    string      `json:"created_at"`
}

// List queries audit logs with filters and pagination.
func (h *AuditLogHandler) List(c *gin.Context) {
	filter := repo.AuditLogFilter{}

	// Parse filters (with aliases for common variations)
	// operator_id / actor_id
	if operatorIDStr := firstNonEmpty(c.Query("operator_id"), c.Query("actor_id")); operatorIDStr != "" {
		if id, err := uuid.Parse(operatorIDStr); err == nil {
			filter.OperatorID = &id
		}
	}
	// resource_type / target_type
	if resourceType := firstNonEmpty(c.Query("resource_type"), c.Query("target_type")); resourceType != "" {
		filter.ResourceType = resourceType
	}
	// event_type / action
	if eventType := firstNonEmpty(c.Query("event_type"), c.Query("action")); eventType != "" {
		filter.EventType = eventType
	}
	// resource_id / target_id
	if resourceIDStr := firstNonEmpty(c.Query("resource_id"), c.Query("target_id")); resourceIDStr != "" {
		if id, err := uuid.Parse(resourceIDStr); err == nil {
			filter.ResourceID = &id
		}
	}
	if startTimeStr := c.Query("start_time"); startTimeStr != "" {
		if t, err := time.Parse(time.RFC3339, startTimeStr); err == nil {
			filter.StartTime = &t
		}
	}
	if endTimeStr := c.Query("end_time"); endTimeStr != "" {
		if t, err := time.Parse(time.RFC3339, endTimeStr); err == nil {
			filter.EndTime = &t
		}
	}

	// Parse pagination
	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)

	ctx := c.Request.Context()
	logs, total, err := h.auditLogRepo.List(ctx, filter, page, pageSize)
	if err != nil {
		Error(c, "server_error", "查询审计日志失败")
		return
	}

	result := make([]AuditLogResponse, 0, len(logs))
	for _, l := range logs {
		result = append(result, AuditLogResponse{
			ID:           l.ID.String(),
			OperatorID:   l.OperatorID.String(),
			OperatorIP:   l.OperatorIP,
			EventType:    l.EventType,
			ResourceType: l.ResourceType,
			ResourceID:   l.ResourceID.String(),
			Details:      l.Details,
			CreatedAt:    l.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	Success(c, gin.H{"items": result, "total": total})
}

// parseIntDefault parses an int from a string, returning the default if parsing fails.
func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n < 1 {
		return def
	}
	return n
}

// firstNonEmpty returns the first non-empty string from the given arguments.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
