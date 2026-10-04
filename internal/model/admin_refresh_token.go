package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AdminRefreshToken stores refresh tokens for admin-server login sessions.
//
// This is separate from model.RefreshToken (refresh_tokens table), which
// belongs to RTC Agent Server's OAuth2 flow and includes device_id.
// AdminRefreshToken is exclusively used by admin-server for email+password
// login sessions in admin-ui.
type AdminRefreshToken struct {
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	TokenHash string     `gorm:"size:64;not null;uniqueIndex" json:"-"`
	UserID    uuid.UUID  `gorm:"type:uuid;not null;index" json:"user_id"`
	ExpiresAt time.Time  `json:"expires_at"`
	Revoked   bool       `gorm:"not null;default:false" json:"revoked"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `gorm:"index" json:"-"`
}

// TableName overrides the default table name for AdminRefreshToken.
func (AdminRefreshToken) TableName() string {
	return "admin_refresh_tokens"
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (r *AdminRefreshToken) BeforeCreate(tx *gorm.DB) error {
	if r.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		r.ID = id
	}
	return nil
}
