package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ScriptExecution records each Script tool execution.
// Relationship with Rtc: after each script-type RTC completes, a ScriptExecution record is created.
type ScriptExecution struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	RtcID     uuid.UUID `gorm:"type:uuid;not null;uniqueIndex" json:"rtc_id"` // 1:1 with RTC
	SessionID uuid.UUID `gorm:"type:uuid;not null;index" json:"session_id"`
	TurnID    uuid.UUID `gorm:"type:uuid;not null" json:"turn_id"`
	UserID    uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`

	// Script-specific fields
	Title      string `gorm:"size:100;not null" json:"title"`        // LLM-generated execution description
	Action     string `gorm:"size:20;not null" json:"action"`        // save/run/eval
	ScriptName string `gorm:"size:255" json:"script_name,omitempty"` // script name (present for save/run)
	CodeHash   string `gorm:"size:64" json:"code_hash,omitempty"`    // SHA-256 hash of the code

	// Execution result
	Status       string `gorm:"size:20;not null" json:"status"` // success/failed/timeout
	DurationMs   int64  `json:"duration_ms,omitempty"`          // execution duration reported by frontend
	ResultSize   int64  `json:"result_size,omitempty"`          // result JSON size in bytes
	CodeSize     int64  `json:"code_size,omitempty"`            // code size in bytes
	ErrorMessage string `gorm:"type:text" json:"error_message,omitempty"`

	// Console output (StringArray -> PostgreSQL JSONB, same pattern as Session.TodoList)
	Logs     StringArray `gorm:"type:jsonb;not null;default:'[]'" json:"logs"`     // console.log output
	Warnings StringArray `gorm:"type:jsonb;not null;default:'[]'" json:"warnings"` // console.warn output
	Errors   StringArray `gorm:"type:jsonb;not null;default:'[]'" json:"errors"`   // console.error output

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (s *ScriptExecution) BeforeCreate(tx *gorm.DB) error {
	if s.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		s.ID = id
	}
	return nil
}
