package dbmodel

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// OAuth2User OAuth2 用户模型
type OAuth2User struct {
	ID        uuid.UUID    `gorm:"type:uuid;primaryKey" json:"id"`
	Provider  string       `gorm:"size:50;not null" json:"provider"`
	Sub       string       `gorm:"size:255;not null;uniqueIndex:idx_provider_sub" json:"sub"`
	Email     string       `gorm:"size:255" json:"email,omitempty"`
	Name      string       `gorm:"size:100" json:"name,omitempty"`
	AvatarURL string       `gorm:"size:500" json:"avatar_url,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	DeletedAt *time.Time   `gorm:"index" json:"-"`
}

func (u *OAuth2User) BeforeCreate(tx *gorm.DB) error {
	if u.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		u.ID = id
	}
	return nil
}
