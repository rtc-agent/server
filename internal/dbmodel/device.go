package dbmodel

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Device 设备模型
type Device struct {
	ID           uuid.UUID    `gorm:"type:uuid;primaryKey" json:"id"`
	UserID       uuid.UUID    `gorm:"type:uuid;not null;uniqueIndex:idx_user_device;index" json:"user_id"`
	DeviceID     string       `gorm:"size:100;not null;uniqueIndex:idx_user_device" json:"device_id"`
	Name         string       `gorm:"size:100" json:"name,omitempty"`
	UserAgent    string       `gorm:"size:500" json:"user_agent,omitempty"`
	LastActiveAt time.Time    `json:"last_active_at"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
	DeletedAt    *time.Time   `gorm:"index" json:"-"`
}

func (d *Device) BeforeCreate(tx *gorm.DB) error {
	if d.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		d.ID = id
	}
	return nil
}
