package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RefreshToken is the database model for a refresh token.
// The token is stored as a SHA-256 hash; plaintext is never persisted.
type RefreshToken struct {
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	TokenHash string     `gorm:"size:64;not null;uniqueIndex" json:"-"`
	UserID    uuid.UUID  `gorm:"type:uuid;not null;index" json:"user_id"`
	DeviceID  string     `gorm:"size:100" json:"device_id"`
	ExpiresAt time.Time  `json:"expires_at"`
	Revoked   bool       `gorm:"not null;default:false" json:"revoked"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `gorm:"index" json:"-"`
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (r *RefreshToken) BeforeCreate(tx *gorm.DB) error {
	if r.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		r.ID = id
	}
	return nil
}
