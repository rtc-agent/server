package model

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TemporaryCredential represents a temporary access credential for S3 API.
type TemporaryCredential struct {
	ID              uuid.UUID `gorm:"type:uuid;primary_key" json:"id"`
	UserID          string    `gorm:"type:varchar(255);not null;index:idx_credentials_user_id" json:"user_id"`
	AccessKeyID     string    `gorm:"type:varchar(255);not null;uniqueIndex:idx_credentials_access_key_id" json:"access_key_id"`
	SecretAccessKey string    `gorm:"type:varchar(255);not null" json:"secret_access_key"` // AES-256-GCM encrypted
	SessionToken    string    `gorm:"type:text;not null" json:"session_token"`             // AES-256-GCM encrypted
	ExpiresAt       time.Time `gorm:"not null;index:idx_credentials_expires_at" json:"expires_at"`
	CreatedAt       time.Time `json:"created_at"`
}

// TableName specifies the table name for TemporaryCredential model.
func (TemporaryCredential) TableName() string {
	return "temporary_credentials"
}

// BeforeCreate sets default values before creating a TemporaryCredential record.
func (c *TemporaryCredential) BeforeCreate(tx *gorm.DB) error {
	if c.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate temporary credential ID: %w", err)
		}
		c.ID = id
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	return nil
}

// IsExpired returns true if the credential has expired.
func (c *TemporaryCredential) IsExpired() bool {
	return time.Now().After(c.ExpiresAt)
}
