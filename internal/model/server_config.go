// Package model provides database models.
package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ServerConfig represents a dynamic configuration entry in the database.
// Composite primary key: (key, user_id). NULL user_id = system default;
// non-NULL user_id = per-user override.
type ServerConfig struct {
	Key         string         `gorm:"primaryKey;column:key" json:"key"`
	UserID      *uuid.UUID     `gorm:"primaryKey;type:uuid;column:user_id" json:"user_id,omitempty"`
	Value       datatypes.JSON `gorm:"type:jsonb;not null" json:"value"`
	ValueType   string         `gorm:"size:20;not null" json:"value_type"`
	Category    string         `gorm:"size:50;not null;index:idx_server_configs_category" json:"category"`
	Description string         `gorm:"type:text" json:"description"`
	Version     int            `gorm:"not null;default:1" json:"version"`
	UpdatedBy   *uuid.UUID     `gorm:"type:uuid" json:"updated_by,omitempty"`
	UpdatedAt   time.Time      `gorm:"not null" json:"updated_at"`
	CreatedAt   time.Time      `gorm:"not null" json:"created_at"`
}

// TableName specifies the database table name for ServerConfig.
func (ServerConfig) TableName() string {
	return "server_configs"
}

// BeforeCreate sets default timestamps.
func (sc *ServerConfig) BeforeCreate(tx *gorm.DB) error {
	now := time.Now()
	if sc.CreatedAt.IsZero() {
		sc.CreatedAt = now
	}
	if sc.UpdatedAt.IsZero() {
		sc.UpdatedAt = now
	}
	return nil
}

// ServerConfigHistory records changes to dynamic configuration entries.
type ServerConfigHistory struct {
	ID         uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	Key        string         `gorm:"not null;index:idx_config_history_key,priority:1" json:"key"`
	UserID     *uuid.UUID     `gorm:"type:uuid;index:idx_config_history_key,priority:2" json:"user_id,omitempty"`
	OldValue   datatypes.JSON `gorm:"type:jsonb" json:"old_value,omitempty"`
	NewValue   datatypes.JSON `gorm:"type:jsonb" json:"new_value,omitempty"`
	Version    int            `gorm:"not null" json:"version"`
	ChangedBy  uuid.UUID      `gorm:"type:uuid;not null" json:"changed_by"`
	ChangedAt  time.Time      `gorm:"not null;index:idx_config_history_changed_at,sort:desc" json:"changed_at"`
	ChangeNote string         `gorm:"type:text" json:"change_note,omitempty"`
}

// TableName specifies the database table name for ServerConfigHistory.
func (ServerConfigHistory) TableName() string {
	return "server_config_history"
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (h *ServerConfigHistory) BeforeCreate(tx *gorm.DB) error {
	if h.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		h.ID = id
	}
	if h.ChangedAt.IsZero() {
		h.ChangedAt = time.Now()
	}
	return nil
}

// ConfigFilter defines optional filters for listing server configs.
type ConfigFilter struct {
	Category string
	UserID   *uuid.UUID // nil = system configs only; non-nil = specific user's overrides
}

// ConfigHistoryFilter defines optional filters for listing config history.
type ConfigHistoryFilter struct {
	Key    string
	UserID *uuid.UUID
}
