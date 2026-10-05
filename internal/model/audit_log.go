package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// AuditLog records administrative actions for compliance and debugging.
type AuditLog struct {
	ID           uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	OperatorID   uuid.UUID      `gorm:"type:uuid;not null;index" json:"operator_id"`
	OperatorIP   string         `gorm:"size:45" json:"operator_ip,omitempty"`
	EventType    string         `gorm:"size:50;not null;index" json:"event_type"`
	ResourceType string         `gorm:"size:50;not null;index:idx_audit_logs_resource,priority:1" json:"resource_type"`
	ResourceID   uuid.UUID      `gorm:"type:uuid;index:idx_audit_logs_resource,priority:2" json:"resource_id,omitempty"`
	Details      datatypes.JSON `gorm:"type:jsonb" json:"details,omitempty"`
	CreatedAt    time.Time      `gorm:"not null;index:idx_audit_logs_created_at,sort:desc" json:"created_at"`
}

// TableName specifies the database table name for AuditLog.
func (AuditLog) TableName() string {
	return "audit_logs"
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (a *AuditLog) BeforeCreate(tx *gorm.DB) error {
	if a.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		a.ID = id
	}
	return nil
}

// AuditLogFilter defines optional filters for listing audit logs.
// Defined in the model package so both repo and handler layers can share it
// without creating a handler → repo dependency.
type AuditLogFilter struct {
	OperatorID   *uuid.UUID
	ResourceType string
	EventType    string
	ResourceID   *uuid.UUID
	StartTime    *time.Time
	EndTime      *time.Time
}
