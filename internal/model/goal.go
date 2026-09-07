package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GoalStatus goal 状态
type GoalStatus string

const (
	GoalStatusActive    GoalStatus = "active"
	GoalStatusCompleted GoalStatus = "completed"
	GoalStatusCancelled GoalStatus = "cancelled"
	GoalStatusExhausted GoalStatus = "exhausted"
)

// Goal 持久化目标
type Goal struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	SessionID      uuid.UUID  `gorm:"type:uuid;not null;index" json:"session_id"`
	Condition      string     `gorm:"type:text;not null" json:"condition"`
	Status         string     `gorm:"size:20;not null;default:'active';index" json:"status"`
	CompletedTurns int        `gorm:"not null;default:0" json:"completed_turns"`
	TokenUsage     int        `gorm:"not null;default:0" json:"token_usage"`
	MaxTurns       int        `gorm:"not null;default:50" json:"max_turns"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	LastReason     *string    `json:"last_reason,omitempty"`
}

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

// IsTerminal 判断 goal 是否处于终态
func (g *Goal) IsTerminal() bool {
	return g.Status == string(GoalStatusCompleted) ||
		g.Status == string(GoalStatusCancelled) ||
		g.Status == string(GoalStatusExhausted)
}
