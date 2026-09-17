package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GoalStatus represents the lifecycle state of a Goal.
type GoalStatus string

// Goal status constants define the lifecycle states of a Goal.
const (
	// GoalStatusActive means the goal is currently being pursued.
	GoalStatusActive GoalStatus = "active"
	// GoalStatusCompleted means the goal condition has been satisfied.
	GoalStatusCompleted GoalStatus = "completed"
	// GoalStatusCancelled means the goal was explicitly cancelled by the user.
	GoalStatusCancelled GoalStatus = "cancelled"
	// GoalStatusExhausted means the goal reached its maximum turn or token limit.
	GoalStatusExhausted GoalStatus = "exhausted"
)

// Goal is the database model for a persistent goal.
type Goal struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	SessionID      uuid.UUID  `gorm:"type:uuid;not null;index" json:"session_id"`
	Condition      string     `gorm:"type:text;not null" json:"condition"`
	Status         GoalStatus `gorm:"size:20;not null;default:'active';index" json:"status"`
	CompletedTurns int        `gorm:"not null;default:0" json:"completed_turns"`
	TokenUsage     int        `gorm:"not null;default:0" json:"token_usage"`
	MaxTurns       int        `gorm:"not null;default:50" json:"max_turns"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	LastReason     *string    `json:"last_reason,omitempty"`
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (g *Goal) BeforeCreate(tx *gorm.DB) error {
	if g.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		g.ID = id
	}
	return nil
}

// IsTerminal reports whether the goal is in a terminal state.
func (g *Goal) IsTerminal() bool {
	return g.Status == GoalStatusCompleted ||
		g.Status == GoalStatusCancelled ||
		g.Status == GoalStatusExhausted
}
