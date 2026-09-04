package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RefreshToken 刷新令牌模型（存储 SHA-256 哈希，不存明文）
type RefreshToken struct {
	ID        uuid.UUID    `gorm:"type:uuid;primaryKey" json:"id"`
	TokenHash string       `gorm:"size:64;not null;uniqueIndex" json:"-"`
	UserID    uuid.UUID    `gorm:"type:uuid;not null;index" json:"user_id"`
	DeviceID  string       `gorm:"size:100" json:"device_id"`
	ExpiresAt time.Time    `json:"expires_at"`
	Revoked   bool         `gorm:"not null;default:false" json:"revoked"`
	CreatedAt time.Time    `json:"created_at"`
	DeletedAt *time.Time   `gorm:"index" json:"-"`
}

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
