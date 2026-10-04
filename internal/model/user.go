// Package model defines the database models for the application.
package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// User is the database model for an admin user with password-based authentication.
type User struct {
	ID           uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	Email        string     `gorm:"size:255;not null;uniqueIndex" json:"email"`
	Name         string     `gorm:"size:100" json:"name,omitempty"`
	AvatarURL    string     `gorm:"size:500" json:"avatar_url,omitempty"`
	PasswordHash string     `gorm:"size:60;not null" json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DeletedAt    *time.Time `gorm:"index" json:"-"`
}

// TableName specifies the database table name for User.
func (User) TableName() string {
	return "users"
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (u *User) BeforeCreate(tx *gorm.DB) error {
	if u.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		u.ID = id
	}
	return nil
}
