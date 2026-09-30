package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// MultipartUpload represents a multipart upload session record.
type MultipartUpload struct {
	ID            uuid.UUID  `gorm:"type:uuid;primary_key" json:"id"`
	UserID        string     `gorm:"type:varchar(255);not null;index:idx_multipart_uploads_user_id" json:"user_id"`
	Key           string     `gorm:"type:varchar(1024);not null" json:"key"`
	UploadID      string     `gorm:"type:varchar(255);not null;uniqueIndex:idx_multipart_uploads_upload_id" json:"upload_id"` // Storage backend upload ID
	Status        string     `gorm:"type:varchar(50);not null" json:"status"`                                                 // "uploading", "completed", "aborted"
	TotalParts    int        `gorm:"type:int" json:"total_parts"`
	UploadedParts int        `gorm:"type:int" json:"uploaded_parts"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `gorm:"not null;index:idx_multipart_uploads_expires_at" json:"expires_at"` // 24h after creation
	CompletedAt   *time.Time `gorm:"type:timestamptz" json:"completed_at,omitempty"`
	AbortedAt     *time.Time `gorm:"type:timestamptz" json:"aborted_at,omitempty"`
}

// TableName specifies the table name for MultipartUpload model.
func (MultipartUpload) TableName() string {
	return "multipart_uploads"
}

// BeforeCreate sets default values before creating a MultipartUpload record.
func (m *MultipartUpload) BeforeCreate(tx *gorm.DB) error {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	if m.Status == "" {
		m.Status = "uploading"
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	return nil
}

// MultipartUploadPart represents a single part in a multipart upload.
type MultipartUploadPart struct {
	ID         uuid.UUID `gorm:"type:uuid;primary_key" json:"id"`
	UploadID   uuid.UUID `gorm:"type:uuid;not null;index:idx_parts_upload_id;uniqueIndex:idx_parts_upload_part,priority:1" json:"upload_id"` // References MultipartUpload.ID
	PartNumber int       `gorm:"type:int;not null;uniqueIndex:idx_parts_upload_part,priority:2" json:"part_number"`
	Size       int64     `gorm:"type:bigint;not null" json:"size"`
	ETag       string    `gorm:"type:varchar(255);not null" json:"etag"`
	CreatedAt  time.Time `json:"created_at"`
}

// TableName specifies the table name for MultipartUploadPart model.
func (MultipartUploadPart) TableName() string {
	return "multipart_upload_parts"
}

// BeforeCreate sets default values before creating a MultipartUploadPart record.
func (p *MultipartUploadPart) BeforeCreate(tx *gorm.DB) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	return nil
}
