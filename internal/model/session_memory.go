package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// SessionMemory is the session memory model.
// Used to continuously extract key information during a session, providing summary content for automatic compression.
type SessionMemory struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	SessionID uuid.UUID `gorm:"type:uuid;not null;index" json:"session_id"`

	// Category: decision/context/progress/issue/learnings.
	Category string `gorm:"size:50;not null;index" json:"category"`

	// Content.
	Title   string `gorm:"size:200;not null" json:"title"`
	Content string `gorm:"type:text;not null" json:"content"`

	// Metadata in JSON format (stores related files, code snippets, etc.)
	Metadata JSONB[any] `gorm:"type:jsonb;default:'{}'" json:"metadata"`

	// Estimated token count (used to limit total size)
	TokenCount *int `json:"token_count,omitempty"`

	// Timestamps
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `gorm:"index" json:"-"`
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (m *SessionMemory) BeforeCreate(tx *gorm.DB) error {
	if m.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		m.ID = id
	}
	return nil
}

// Session Memory category constants.
const (
	SessionMemoryCategoryDecision  = "decision"
	SessionMemoryCategoryContext   = "context"
	SessionMemoryCategoryProgress  = "progress"
	SessionMemoryCategoryIssue     = "issue"
	SessionMemoryCategoryLearnings = "learnings"
)

// ValidSessionMemoryCategories lists all valid session memory categories.
var ValidSessionMemoryCategories = []string{
	SessionMemoryCategoryDecision,
	SessionMemoryCategoryContext,
	SessionMemoryCategoryProgress,
	SessionMemoryCategoryIssue,
	SessionMemoryCategoryLearnings,
}

// IsValidCategory checks whether the given category is valid.
func IsValidCategory(category string) bool {
	for _, c := range ValidSessionMemoryCategories {
		if c == category {
			return true
		}
	}
	return false
}

// ToProtocolSessionMemory converts model.SessionMemory to protocol.SessionMemory.
// Note: the protocol currently has no SessionMemory definition; this is reserved for future use.
// If exposed to the client, a corresponding type must be added to the protocol.
