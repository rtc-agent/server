package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// LoopStatus represents the lifecycle state of a Loop.
type LoopStatus string

// Loop status constants define the lifecycle states of a Loop.
const (
	// LoopStatusActive means the loop is currently running on schedule.
	LoopStatusActive LoopStatus = "active"
	// LoopStatusPaused means the loop is temporarily suspended.
	LoopStatusPaused LoopStatus = "paused"
	// LoopStatusCompleted means the loop finished all scheduled iterations.
	LoopStatusCompleted LoopStatus = "completed"
	// LoopStatusCancelled means the loop was explicitly cancelled by the user.
	LoopStatusCancelled LoopStatus = "cancelled"
	// LoopStatusExhausted means the loop reached its maximum turn or token limit.
	LoopStatusExhausted LoopStatus = "exhausted"
)

// Loop represents a persistent recurring task.
//
// A Loop executes at regular intervals of IntervalSeconds, up to MaxTurns times.
// State transitions: active -> paused/completed/cancelled/exhausted.
//
// Mutually exclusive with Goal: a session cannot have an active goal and an
// active loop simultaneously.
type Loop struct {
	ID               uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	SessionID        uuid.UUID  `gorm:"type:uuid;not null;index" json:"session_id"`
	Prompt           string     `gorm:"type:text;not null" json:"prompt"`
	IntervalSeconds  int        `gorm:"not null" json:"interval_seconds"`
	MaxTurns         int        `gorm:"not null;default:10" json:"max_turns"`
	CompletedTurns   int        `gorm:"not null;default:0" json:"completed_turns"`
	RecoveryAttempts int        `gorm:"not null;default:0" json:"recovery_attempts"`
	Status           LoopStatus `gorm:"size:20;not null;default:'active';index" json:"status"`
	AsynqTaskID      string     `gorm:"size:255" json:"asynq_task_id,omitempty"`
	LastReason       *string    `gorm:"type:text" json:"last_reason,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	LastRunAt        *time.Time `json:"last_run_at,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (l *Loop) BeforeCreate(tx *gorm.DB) error {
	if err := l.Validate(); err != nil {
		return fmt.Errorf("validate loop: %w", err)
	}
	if l.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate loop ID: %w", err)
		}
		l.ID = id
	}
	return nil
}

// Validate checks that the Loop configuration is valid.
// Call this before creating a Loop to prevent invalid configurations that
// could cause performance issues or unexpected behavior.
func (l *Loop) Validate() error {
	if strings.TrimSpace(l.Prompt) == "" {
		return fmt.Errorf("prompt cannot be empty")
	}
	if l.IntervalSeconds <= 0 {
		return fmt.Errorf("interval_seconds must be positive, got %d", l.IntervalSeconds)
	}
	if l.MaxTurns <= 0 {
		return fmt.Errorf("max_turns must be positive, got %d", l.MaxTurns)
	}
	if l.RecoveryAttempts < 0 || l.RecoveryAttempts > 100 {
		return fmt.Errorf("recovery_attempts must be 0-100, got %d", l.RecoveryAttempts)
	}
	if l.IntervalSeconds > 86400*30 { // 30 days
		return fmt.Errorf("interval_seconds too large (max 30 days), got %d", l.IntervalSeconds)
	}
	if l.MaxTurns > 10000 {
		return fmt.Errorf("max_turns too large (max 10000), got %d", l.MaxTurns)
	}
	return nil
}

// IsTerminal reports whether the loop is in a terminal state.
func (l *Loop) IsTerminal() bool {
	return l.Status == LoopStatusCompleted ||
		l.Status == LoopStatusCancelled ||
		l.Status == LoopStatusExhausted
}

// IsPausable reports whether the loop can be paused (only active loops are pausable).
func (l *Loop) IsPausable() bool {
	return l.Status == LoopStatusActive
}

// IsResumable reports whether the loop can be resumed (only paused loops are resumable).
func (l *Loop) IsResumable() bool {
	return l.Status == LoopStatusPaused
}
