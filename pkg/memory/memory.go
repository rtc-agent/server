// Package memory defines the domain layer for the unified Memory model.
//
// Memory is an OKF (Open Knowledge Format) compatible knowledge unit,
// distinguished by scope via the Scope field: session, user, or global.
// This package contains domain models, validation logic, repository
// interface definitions, and formatters. It does not depend on any
// concrete storage implementation and can be consumed by agents, APIs,
// CLIs, and other components.
package memory

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ScopeType defines the persistence scope of a Memory.
type ScopeType string

// ScopeType constants define the memory persistence scope.
const (
	// ScopeSession is session-scoped; archived when the session ends.
	ScopeSession ScopeType = "session"
	// ScopeUser is user-scoped; persists across sessions.
	ScopeUser ScopeType = "user"
	// ScopeGlobal is shared across all users.
	ScopeGlobal ScopeType = "global"
)

// ValidScopeTypes lists all valid scope types.
var ValidScopeTypes = []ScopeType{ScopeSession, ScopeUser, ScopeGlobal}

// IsValidScopeType reports whether the given scope is a valid ScopeType.
func IsValidScopeType(scope ScopeType) bool {
	for _, s := range ValidScopeTypes {
		if s == scope {
			return true
		}
	}
	return false
}

// ValidMemoryTypes lists all valid memory type strings.
var ValidMemoryTypes = []string{
	"decision", "context", "progress", "issue", "learnings",
	"user", "feedback", "project", "reference",
}

// IsValidMemoryType reports whether the given type string is valid.
func IsValidMemoryType(t string) bool {
	for _, v := range ValidMemoryTypes {
		if v == t {
			return true
		}
	}
	return false
}

// Memory is an OKF-compatible knowledge unit.
type Memory struct {
	ID uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`

	// --- Scope ---
	Scope   ScopeType `json:"scope" gorm:"type:varchar(20);not null;index"`
	ScopeID uuid.UUID `json:"scopeId" gorm:"type:uuid;index"` // session_id / user_id / zero for global

	// --- OKF standard fields ---
	Type        string `json:"type" gorm:"type:varchar(50);not null;index"` // decision | context | progress | issue | learnings | ...
	Title       string `json:"title" gorm:"type:varchar(200);not null"`     // 5-10 word summary
	Description string `json:"description" gorm:"type:varchar(500)"`        // one-line description
	Content     string `json:"content" gorm:"type:text;not null"`           // markdown body
	// Tags uses JSONB array instead of PostgreSQL text[] because:
	// 1. JSONB is more flexible and supports nested structures (future extension)
	// 2. Consistent storage strategy with the Metadata field
	// 3. Query performance difference is negligible for small tag counts
	// Note: Differs from UserMemory.Tags (text[]); conversion needed during migration.
	Tags      StringArray `json:"tags" gorm:"type:jsonb;default:'[]'"` // tag array
	Resource  string      `json:"resource" gorm:"type:varchar(500)"`   // external link
	Timestamp time.Time   `json:"timestamp" gorm:"not null"`           // knowledge timestamp

	// --- Extensions ---
	Metadata   JSONBString `json:"metadata" gorm:"type:jsonb;default:'{}'"` // OKF Provenance/Trust/Lifecycle
	TokenCount int         `json:"tokenCount" gorm:"default:0"`             // estimated tokens

	// --- Audit ---
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// DeletedAt uses gorm.DeletedAt instead of *time.Time because:
	// 1. GORM handles soft-delete filtering automatically, reducing manual WHERE clauses
	// 2. More consistent with the GORM ecosystem, easier to use APIs like Unscoped()
	// Note: Differs from existing SessionMemory/UserMemory *time.Time;
	// migration needs unification or an adapter layer.
	DeletedAt gorm.DeletedAt `json:"deletedAt" gorm:"index"`
}

// TableName specifies the database table name.
func (Memory) TableName() string {
	return "memories"
}

// BeforeCreate auto-generates a UUID v7 if the ID is nil.
func (m *Memory) BeforeCreate(tx *gorm.DB) error {
	if m.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		m.ID = id
	}
	return nil
}

// Validate checks that all required model fields are populated.
func (m *Memory) Validate() error {
	if !IsValidScopeType(m.Scope) {
		return fmt.Errorf("%w: %q", ErrInvalidScope, m.Scope)
	}
	if !IsValidMemoryType(m.Type) {
		return fmt.Errorf("%w: %q", ErrInvalidType, m.Type)
	}
	if m.Title == "" {
		return fmt.Errorf("title: %w", ErrRequiredField)
	}
	if m.Content == "" {
		return fmt.Errorf("content: %w", ErrRequiredField)
	}
	if m.Timestamp.IsZero() {
		return fmt.Errorf("timestamp: %w", ErrRequiredField)
	}
	// ScopeID is required for session and user scopes.
	if (m.Scope == ScopeSession || m.Scope == ScopeUser) && m.ScopeID == uuid.Nil {
		return fmt.Errorf("scopeId is required for %s scope", m.Scope)
	}
	return nil
}

// --- MemoryLink ---

// MemoryLink represents a semantic relationship between concepts.
type MemoryLink struct {
	ID       uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`
	FromID   uuid.UUID `json:"fromId" gorm:"type:uuid;not null;index"`
	ToID     uuid.UUID `json:"toId" gorm:"type:uuid;not null;index"`
	Relation string    `json:"relation" gorm:"type:varchar(50);not null"` // related | depends_on | supersedes | derives_from

	CreatedAt time.Time `json:"createdAt"`
}

// TableName specifies the database table name.
func (MemoryLink) TableName() string {
	return "memory_links"
}

// BeforeCreate auto-generates a UUID v7 if the ID is nil.
func (l *MemoryLink) BeforeCreate(tx *gorm.DB) error {
	if l.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		l.ID = id
	}
	return nil
}

// ValidRelations lists all valid relation types.
var ValidRelations = []string{"related", "depends_on", "supersedes", "derives_from"}

// IsValidRelation reports whether the given relation type is valid.
func IsValidRelation(relation string) bool {
	for _, r := range ValidRelations {
		if r == relation {
			return true
		}
	}
	return false
}

// Validate checks that all link fields are valid.
func (l *MemoryLink) Validate() error {
	if l.FromID == uuid.Nil {
		return fmt.Errorf("fromId is required")
	}
	if l.ToID == uuid.Nil {
		return fmt.Errorf("toId is required")
	}
	if l.FromID == l.ToID {
		return fmt.Errorf("fromId and toId must be different")
	}
	if !IsValidRelation(l.Relation) {
		return fmt.Errorf("invalid relation: %q", l.Relation)
	}
	return nil
}
