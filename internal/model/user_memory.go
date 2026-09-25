package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UserMemory is the user-level memory model.
// Used to maintain user preferences, project info, tech stacks, and other
// long-term knowledge across sessions.
type UserMemory struct {
	ID     uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	UserID uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`

	// Category (aligned with Claude Code): user/feedback/project/reference
	Category   string `gorm:"size:50;not null;index" json:"category"`
	Importance string `gorm:"size:20;not null;default:'medium';index" json:"importance"` // low/medium/high/critical

	// Content
	Title       string  `gorm:"size:200;not null" json:"title"`
	Content     string  `gorm:"type:text;not null" json:"content"`
	Description *string `gorm:"size:500" json:"description,omitempty"` // one-line description for retrieval

	// Tags for keyword matching (stored as JSONB array)
	Tags StringArray `gorm:"type:jsonb" json:"tags,omitempty"`

	// Metadata
	Metadata        JSONObject `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	SourceSessionID *uuid.UUID `gorm:"type:uuid" json:"source_session_id,omitempty"`

	// Timestamps
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `gorm:"index" json:"-"`
	LastAccessedAt *time.Time `json:"last_accessed_at,omitempty"`

	// Retrieval statistics
	AccessCount int `gorm:"default:0" json:"access_count"`
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (m *UserMemory) BeforeCreate(tx *gorm.DB) error {
	if m.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		m.ID = id
	}
	return nil
}

// User Memory category constants (aligned with Claude Code).
const (
	UserMemoryCategoryUser      = "user"
	UserMemoryCategoryFeedback  = "feedback"
	UserMemoryCategoryProject   = "project"
	UserMemoryCategoryReference = "reference"
)

// ValidUserMemoryCategories lists all valid memory categories.
var ValidUserMemoryCategories = []string{
	UserMemoryCategoryUser,
	UserMemoryCategoryFeedback,
	UserMemoryCategoryProject,
	UserMemoryCategoryReference,
}

// IsValidUserMemoryCategory checks whether the given category is valid.
func IsValidUserMemoryCategory(category string) bool {
	for _, c := range ValidUserMemoryCategories {
		if c == category {
			return true
		}
	}
	return false
}

// Importance level constants.
const (
	ImportanceLow      = "low"
	ImportanceMedium   = "medium"
	ImportanceHigh     = "high"
	ImportanceCritical = "critical"
)

// ValidImportances lists all valid importance levels.
var ValidImportances = []string{
	ImportanceLow,
	ImportanceMedium,
	ImportanceHigh,
	ImportanceCritical,
}

// IsValidImportance checks whether the given importance level is valid.
func IsValidImportance(importance string) bool {
	for _, i := range ValidImportances {
		if i == importance {
			return true
		}
	}
	return false
}

// ImportanceWeight returns the weight for a given importance level (used for retrieval ranking).
func ImportanceWeight(importance string) float64 {
	switch importance {
	case ImportanceCritical:
		return 1.5
	case ImportanceHigh:
		return 1.3
	case ImportanceMedium:
		return 1.0
	case ImportanceLow:
		return 0.7
	default:
		return 1.0
	}
}
