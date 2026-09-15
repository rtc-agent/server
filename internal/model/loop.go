package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// LoopStatus loop 状态
type LoopStatus string

const (
	LoopStatusActive    LoopStatus = "active"
	LoopStatusPaused    LoopStatus = "paused"
	LoopStatusCompleted LoopStatus = "completed"
	LoopStatusCancelled LoopStatus = "cancelled"
	LoopStatusExhausted LoopStatus = "exhausted"
)

// Loop 持久化循环任务
//
// Loop 代表一个定时循环执行的任务，每隔 IntervalSeconds 秒触发一次，
// 最多执行 MaxTurns 次。状态流转：active → paused/completed/cancelled/exhausted。
//
// 与 Goal 互斥：同一 session 不能同时存在 active goal 和 active loop。
type Loop struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	SessionID       uuid.UUID  `gorm:"type:uuid;not null;index" json:"session_id"`
	Prompt          string     `gorm:"type:text;not null" json:"prompt"`
	IntervalSeconds int        `gorm:"not null" json:"interval_seconds"`
	MaxTurns        int        `gorm:"not null;default:10" json:"max_turns"`
	CompletedTurns  int        `gorm:"not null;default:0" json:"completed_turns"`
	Status          LoopStatus `gorm:"size:20;not null;default:'active';index" json:"status"`
	AsynqTaskID     string     `gorm:"size:255" json:"asynq_task_id,omitempty"`
	LastReason      *string    `gorm:"type:text" json:"last_reason,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	LastRunAt       *time.Time `json:"last_run_at,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

func (l *Loop) BeforeCreate(tx *gorm.DB) error {
	if l.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		l.ID = id
	}
	return nil
}

// IsTerminal 判断 loop 是否处于终态
func (l *Loop) IsTerminal() bool {
	return l.Status == LoopStatusCompleted ||
		l.Status == LoopStatusCancelled ||
		l.Status == LoopStatusExhausted
}

// IsPausable 判断 loop 是否可暂停（仅 active 状态可暂停）
func (l *Loop) IsPausable() bool {
	return l.Status == LoopStatusActive
}

// IsResumable 判断 loop 是否可恢复（仅 paused 状态可恢复）
func (l *Loop) IsResumable() bool {
	return l.Status == LoopStatusPaused
}
