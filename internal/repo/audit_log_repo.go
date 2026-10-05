// Package repo provides data access layer (Repository) implementations.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
)

// AuditLogFilter is an alias for model.AuditLogFilter for backward compatibility.
// New code should use model.AuditLogFilter directly.
type AuditLogFilter = model.AuditLogFilter

// AuditLogRepo provides audit log persistence operations.
type AuditLogRepo interface {
	// Create stores a new audit log entry.
	Create(ctx context.Context, log *model.AuditLog) error
	// Get returns a single audit log by ID.
	Get(ctx context.Context, id uuid.UUID) (*model.AuditLog, error)
	// List returns audit logs matching the filter, ordered by created_at DESC.
	List(ctx context.Context, filter AuditLogFilter, page, pageSize int) ([]*model.AuditLog, int64, error)
}

type auditLogRepo struct {
	db *gorm.DB
}

// NewAuditLogRepo creates a new AuditLogRepo.
func NewAuditLogRepo(db *gorm.DB) AuditLogRepo {
	return &auditLogRepo{db: db}
}

func (r *auditLogRepo) Create(ctx context.Context, log *model.AuditLog) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(log).Error; err != nil {
		return fmt.Errorf("create audit log: %w", err)
	}
	return nil
}

func (r *auditLogRepo) Get(ctx context.Context, id uuid.UUID) (*model.AuditLog, error) {
	var log model.AuditLog
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&log, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get audit log %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get audit log %s: %w", id, err)
	}
	return &log, nil
}

func (r *auditLogRepo) List(ctx context.Context, filter AuditLogFilter, page, pageSize int) ([]*model.AuditLog, int64, error) {
	db := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.AuditLog{})

	if filter.OperatorID != nil {
		db = db.Where("operator_id = ?", *filter.OperatorID)
	}
	if filter.ResourceType != "" {
		db = db.Where("resource_type = ?", filter.ResourceType)
	}
	if filter.EventType != "" {
		db = db.Where("event_type = ?", filter.EventType)
	}
	if filter.ResourceID != nil {
		db = db.Where("resource_id = ?", *filter.ResourceID)
	}
	if filter.StartTime != nil {
		db = db.Where("created_at >= ?", *filter.StartTime)
	}
	if filter.EndTime != nil {
		db = db.Where("created_at <= ?", *filter.EndTime)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count audit logs: %w", err)
	}

	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	var logs []*model.AuditLog
	if err := db.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, 0, fmt.Errorf("list audit logs: %w", err)
	}
	return logs, total, nil
}

// NewAuditLog creates an AuditLog with standard fields populated.
// This is a helper for usecases to construct audit log entries consistently.
// If details serialization fails, logs an error and stores a fallback value.
func NewAuditLog(operatorID uuid.UUID, operatorIP, eventType, resourceType string, resourceID uuid.UUID, details any) *model.AuditLog {
	var raw datatypes.JSON
	if details != nil {
		b, err := json.Marshal(details)
		if err != nil {
			// Log serialization error but don't fail - audit log should still be created
			logger.Error(context.Background(), "audit_log.details_serialization_failed",
				zap.String("event_type", eventType),
				zap.String("resource_type", resourceType),
				zap.String("resource_id", resourceID.String()),
				zap.Error(err))
			// Store a fallback indicating serialization failed
			raw = datatypes.JSON(`{"error":"details serialization failed"}`)
		} else {
			raw = datatypes.JSON(b)
		}
	}
	return &model.AuditLog{
		OperatorID:   operatorID,
		OperatorIP:   operatorIP,
		EventType:    eventType,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Details:      raw,
	}
}
