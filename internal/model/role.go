// Package model defines the database models for the application.
package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Role is the database model for an RBAC role.
//
// Roles group permissions (resource+action pairs) and are assigned to users
// via the user_roles join table. System roles (IsSystem=true) cannot be deleted.
// The Version field is used for optimistic locking (manual implementation in role_repo.Update).
type Role struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	Name        string    `gorm:"size:100;uniqueIndex;not null" json:"name"`
	DisplayName string    `gorm:"size:200;not null" json:"display_name"`
	Description string    `gorm:"size:500" json:"description,omitempty"`
	IsSystem    bool      `gorm:"default:false" json:"is_system"`
	IsEnabled   bool      `gorm:"default:true;index" json:"is_enabled"`
	Version     int64     `gorm:"default:0" json:"version"` // Optimistic locking version counter
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TableName specifies the database table name for Role.
func (Role) TableName() string {
	return "roles"
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (r *Role) BeforeCreate(tx *gorm.DB) error {
	if r.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		r.ID = id
	}
	return nil
}
