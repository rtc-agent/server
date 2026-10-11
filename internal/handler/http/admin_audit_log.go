// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/usecase"
)

// AuditLogHandler handles audit log query endpoints.
type AuditLogHandler struct {
	auditLogUC *usecase.AuditLogUsecase
}

// NewAuditLogHandler creates a new AuditLogHandler.
func NewAuditLogHandler(auditLogUC *usecase.AuditLogUsecase) *AuditLogHandler {
	return &AuditLogHandler{auditLogUC: auditLogUC}
}

// RegisterRoutes registers audit log routes.
func (h *AuditLogHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/audit-logs", h.List)
	r.GET("/audit-logs/:id", h.Get)
}

// AuditLogResponse is the response body for a single audit log entry.
type AuditLogResponse struct {
	ID           string          `json:"id"`
	OperatorID   string          `json:"operator_id"`
	OperatorIP   string          `json:"operator_ip,omitempty"`
	EventType    string          `json:"event_type"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id,omitempty"`
	Details      json.RawMessage `json:"details,omitempty"`
	CreatedAt    string          `json:"created_at"`
}

// List queries audit logs with filters and pagination.
func (h *AuditLogHandler) List(c *gin.Context) {
	filter := usecase.AuditLogFilter{}

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
	// Parse and validate time range filters
	var startTime, endTime *time.Time
	if startTimeStr := c.Query("start_time"); startTimeStr != "" {
		if t, err := time.Parse(time.RFC3339, startTimeStr); err == nil {
			startTime = &t
			filter.StartTime = startTime
		} else {
			Error(c, "validation_error", "Invalid start_time format, use RFC3339")
			return
		}
	}
	if endTimeStr := c.Query("end_time"); endTimeStr != "" {
		if t, err := time.Parse(time.RFC3339, endTimeStr); err == nil {
			endTime = &t
			filter.EndTime = endTime
		} else {
			Error(c, "validation_error", "Invalid end_time format, use RFC3339")
			return
		}
	}
	// Validate time range: start must be before end
	if startTime != nil && endTime != nil && startTime.After(*endTime) {
		Error(c, "validation_error", "start_time must be before end_time")
		return
	}

	// Parse pagination
	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)
	if pageSize > 100 {
		pageSize = 100
	}

	ctx := c.Request.Context()
	result, err := h.auditLogUC.List(ctx, filter, page, pageSize)
	if err != nil {
		Error(c, "server_error", "Failed to query audit logs")
		return
	}

	respItems := make([]AuditLogResponse, 0, len(result.Items))
	for _, l := range result.Items {
		respItems = append(respItems, AuditLogResponse{
			ID:           l.ID.String(),
			OperatorID:   l.OperatorID.String(),
			OperatorIP:   l.OperatorIP,
			EventType:    l.EventType,
			ResourceType: l.ResourceType,
			ResourceID:   l.ResourceID.String(),
			Details:      json.RawMessage(l.Details),
			CreatedAt:    l.CreatedAt.Format(TimeFormatRFC3339),
		})
	}

	Success(c, gin.H{"items": respItems, "total": result.Total, "page": page, "page_size": pageSize})
}

// Get retrieves a single audit log by ID.
func (h *AuditLogHandler) Get(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		Error(c, "validation_error", "Invalid audit log ID")
		return
	}

	ctx := c.Request.Context()
	log, err := h.auditLogUC.Get(ctx, id)
	if err != nil {
		if errors.Is(err, usecase.ErrNotFound) {
			Error(c, "audit_log_not_found", "Audit log not found")
			return
		}
		Error(c, "server_error", "Failed to query audit logs")
		return
	}

	result := AuditLogResponse{
		ID:           log.ID.String(),
		OperatorID:   log.OperatorID.String(),
		OperatorIP:   log.OperatorIP,
		EventType:    log.EventType,
		ResourceType: log.ResourceType,
		ResourceID:   log.ResourceID.String(),
		Details:      json.RawMessage(log.Details),
		CreatedAt:    log.CreatedAt.Format(TimeFormatRFC3339),
	}

	Success(c, result)
}

// parseIntDefault parses an int from a string, returning the default if parsing fails.
func parseIntDefault(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	if n < 1 {
		return defaultVal
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
