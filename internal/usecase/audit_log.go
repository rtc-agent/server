// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
)

// AuditLogFilter defines optional filters for listing audit logs.
// Defined at the usecase layer so handlers don't need to import model directly.
type AuditLogFilter struct {
	OperatorID   *uuid.UUID
	ResourceType string
	EventType    string
	ResourceID   *uuid.UUID
	StartTime    *time.Time
	EndTime      *time.Time
}

// AuditLogEntry represents a single audit log record returned to the handler.
type AuditLogEntry struct {
	ID           uuid.UUID
	OperatorID   uuid.UUID
	OperatorIP   string
	EventType    string
	ResourceType string
	ResourceID   uuid.UUID
	Details      []byte
	CreatedAt    time.Time
}

// AuditLogListResult holds the paginated audit log query result.
type AuditLogListResult struct {
	Items []*AuditLogEntry
	Total int64
}

// AuditLogUsecase provides audit log query operations.
type AuditLogUsecase struct {
	repo repo.AuditLogRepo
}

// NewAuditLogUsecase creates a new AuditLogUsecase.
func NewAuditLogUsecase(repo repo.AuditLogRepo) *AuditLogUsecase {
	return &AuditLogUsecase{repo: repo}
}

// List returns audit logs matching the filter with pagination.
func (uc *AuditLogUsecase) List(ctx context.Context, filter AuditLogFilter, page, pageSize int) (*AuditLogListResult, error) {
	// Validate pagination parameters
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		return nil, fmt.Errorf("pageSize %d exceeds maximum 100", pageSize)
	}

	// Validate time range filter
	if filter.StartTime != nil && filter.EndTime != nil {
		if filter.StartTime.After(*filter.EndTime) {
			return nil, fmt.Errorf("startTime must be before or equal to endTime")
		}
	}

	repoFilter := model.AuditLogFilter{
		OperatorID:   filter.OperatorID,
		ResourceType: filter.ResourceType,
		EventType:    filter.EventType,
		ResourceID:   filter.ResourceID,
		StartTime:    filter.StartTime,
		EndTime:      filter.EndTime,
	}

	logs, total, err := uc.repo.List(ctx, repoFilter, page, pageSize)
	if err != nil {
		return nil, err
	}

	items := make([]*AuditLogEntry, len(logs))
	for i, l := range logs {
		items[i] = &AuditLogEntry{
			ID:           l.ID,
			OperatorID:   l.OperatorID,
			OperatorIP:   l.OperatorIP,
			EventType:    l.EventType,
			ResourceType: l.ResourceType,
			ResourceID:   l.ResourceID,
			Details:      l.Details,
			CreatedAt:    l.CreatedAt,
		}
	}

	return &AuditLogListResult{Items: items, Total: total}, nil
}

// Get returns a single audit log by ID.
func (uc *AuditLogUsecase) Get(ctx context.Context, id uuid.UUID) (*AuditLogEntry, error) {
	log, err := uc.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	return &AuditLogEntry{
		ID:           log.ID,
		OperatorID:   log.OperatorID,
		OperatorIP:   log.OperatorIP,
		EventType:    log.EventType,
		ResourceType: log.ResourceType,
		ResourceID:   log.ResourceID,
		Details:      log.Details,
		CreatedAt:    log.CreatedAt,
	}, nil
}
