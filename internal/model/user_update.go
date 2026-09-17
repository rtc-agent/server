package model

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UserUpdate records a batch of entity changes for a user, used for offline
// recovery via the Topic channel. Each record corresponds to a batch of entity
// change events (created/updated/deleted for session/turn/message/rtc).
type UserUpdate struct {
	ID        uuid.UUID       `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID       `gorm:"type:uuid;index:idx_user_offset,unique;index;not null"` // owning user
	Offset    uint32          `gorm:"index:idx_user_offset,unique;not null"`                 // monotonically increasing per-user offset
	Items     UpdateItemArray `gorm:"type:jsonb;not null"`                                   // change entries
	DataList  StringArray     `gorm:"type:jsonb" json:"data_list,omitempty"`                 // rich content cache (optional)
	CreatedAt time.Time       `gorm:"autoCreateTime;not null"`
}

// TableName specifies the database table name.
func (UserUpdate) TableName() string {
	return "user_updates"
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (u *UserUpdate) BeforeCreate(tx *gorm.DB) error {
	if u.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		u.ID = id
	}

	if len(u.Items) == 0 {
		return fmt.Errorf("items must not be empty")
	}

	return nil
}
