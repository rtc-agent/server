package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ScriptExecution 记录每次 Script 工具的执行详情。
// 与 Rtc 表的关系：每个 script 类型的 RTC 完成后，产生一条 ScriptExecution 记录。
type ScriptExecution struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	RtcID     uuid.UUID `gorm:"type:uuid;not null;uniqueIndex" json:"rtc_id"`      // 1:1 关联 RTC
	SessionID uuid.UUID `gorm:"type:uuid;not null;index" json:"session_id"`
	TurnID    uuid.UUID `gorm:"type:uuid;not null" json:"turn_id"`
	UserID    uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`

	// Script 特有字段
	Title      string `gorm:"size:100;not null" json:"title"`                // LLM 生成的执行描述
	Action     string `gorm:"size:20;not null" json:"action"`                // save/run/eval
	ScriptName string `gorm:"size:255" json:"script_name,omitempty"`         // 脚本名称（save/run 时有值）
	CodeHash   string `gorm:"size:64" json:"code_hash,omitempty"`            // 代码 SHA-256 哈希

	// 执行结果
	Status       string  `gorm:"size:20;not null" json:"status"`             // success/failed/timeout
	DurationMs   int64   `json:"duration_ms,omitempty"`                       // 前端报告的执行耗时
	ResultSize   int64   `json:"result_size,omitempty"`                       // 结果 JSON 大小（字节）
	CodeSize     int64   `json:"code_size,omitempty"`                         // 代码大小（字节）
	ErrorMessage string  `gorm:"type:text" json:"error_message,omitempty"`

	// 控制台输出（StringArray → PostgreSQL JSONB，与 Session.TodoList 模式一致）
	Logs     StringArray `gorm:"type:jsonb;not null;default:'[]'" json:"logs"`    // console.log 输出
	Warnings StringArray `gorm:"type:jsonb;not null;default:'[]'" json:"warnings"` // console.warn 输出
	Errors   StringArray `gorm:"type:jsonb;not null;default:'[]'" json:"errors"`   // console.error 输出

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

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
