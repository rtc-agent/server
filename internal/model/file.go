package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// File represents a file metadata record in object storage.
type File struct {
	ID          uuid.UUID      `gorm:"type:uuid;primary_key" json:"id"`
	UserID      string         `gorm:"type:varchar(255);not null;index:idx_files_user_id" json:"user_id"`
	Bucket      string         `gorm:"type:varchar(255);not null" json:"bucket"`
	Key         string         `gorm:"type:varchar(1024);not null;uniqueIndex:idx_files_user_key,priority:2" json:"key"`
	Size        int64          `gorm:"type:bigint;not null" json:"size"`
	ContentType string         `gorm:"type:varchar(255)" json:"content_type"`
	ETag        string         `gorm:"type:varchar(255)" json:"etag"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// TableName specifies the table name for File model.
func (File) TableName() string {
	return "files"
}

// BeforeCreate sets default values before creating a File record.
func (f *File) BeforeCreate(tx *gorm.DB) error {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	if f.Bucket == "" {
		f.Bucket = "rtc-agent"
	}
	now := time.Now()
	if f.CreatedAt.IsZero() {
		f.CreatedAt = now
	}
	if f.UpdatedAt.IsZero() {
		f.UpdatedAt = now
	}
	return nil
}
