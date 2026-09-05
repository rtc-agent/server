package model

import (
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Message 消息模型
type Message struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	ClientID        string     `gorm:"size:255;uniqueIndex" json:"client_id,omitempty"` // 客户端生成的幂等 ID
	SessionID       uuid.UUID  `gorm:"type:uuid;not null;index" json:"session_id"`
	TurnID          *uuid.UUID `gorm:"type:uuid;index" json:"turn_id,omitempty"`
	ParentMessageID *uuid.UUID `gorm:"type:uuid;index" json:"parent_message_id,omitempty"` // toolcall_output 指向 toolcall_input
	GlobalOffset    uint32     `gorm:"not null;index" json:"global_offset"`
	TurnOffset      *uint32    `json:"turn_offset"`
	Role            string     `gorm:"size:20;not null" json:"role"`
	Content         string     `gorm:"type:text" json:"content,omitempty"`
	StreamingStatus string     `gorm:"size:20;not null;default:pending" json:"streaming_status"`
	CreatorKind     string     `gorm:"size:32;default:'user'" json:"creator_kind"`
	CreatorRefID    string     `gorm:"size:255" json:"creator_ref_id"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DeletedAt       *time.Time `gorm:"index" json:"-"`

	// Token 用量（仅 assistant 消息有值，其余角色通常为 NULL）
	InputTokens     *int `json:"input_tokens,omitempty"`
	OutputTokens    *int `json:"output_tokens,omitempty"`
	TotalTokens     *int `json:"total_tokens,omitempty"`
	CachedTokens    *int `json:"cached_tokens,omitempty"`
	ReasoningTokens *int `json:"reasoning_tokens,omitempty"`
}

func (m *Message) BeforeCreate(tx *gorm.DB) error {
	if m.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		m.ID = id
	}
	return nil
}

// 消息角色常量（protocol 为单一真相源）
const (
	MessageRoleUser      = protocol.MessageRoleUser
	MessageRoleAssistant = protocol.MessageRoleAssistant
	MessageRoleSystem    = protocol.MessageRoleSystem
)

// 消息流式状态常量（protocol 为单一真相源）
const (
	MessageStreamingPending   = protocol.MessageStreamingPending
	MessageStreamingStreaming = protocol.MessageStreamingStreaming
	MessageStreamingCompleted = protocol.MessageStreamingCompleted
	MessageStreamingFailed    = protocol.MessageStreamingFailed
)

// TokenUsageUpdate carries the token usage values for a message update.
type TokenUsageUpdate struct {
	InputTokens     int
	OutputTokens    int
	TotalTokens     int
	CachedTokens    int
	ReasoningTokens int
}

// ToProtocolMessage 将 dbmodel.Message 转换为 protocol.Message。
// nil 输入返回零值 protocol.Message。
func ToProtocolMessage(m *Message) protocol.Message {
	if m == nil {
		return protocol.Message{}
	}
	result := protocol.Message{
		Id:              protocol.UUID(m.ID.String()),
		ClientId:        strPtr(m.ClientID),
		SessionId:       protocol.UUID(m.SessionID.String()),
		GlobalOffset:    m.GlobalOffset,
		TurnOffset:      m.TurnOffset,
		Role:            protocol.MessageRole(m.Role),
		StreamingStatus: protocol.MessageStreamingStatus(m.StreamingStatus),
		CreatorKind:     m.CreatorKind,
		CreatorRefId:    m.CreatorRefID,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
		DeletedAt:       m.DeletedAt,
	}
	if m.Content != "" {
		content := m.Content
		result.Content = &content
	}
	if m.TurnID != nil {
		tid := protocol.UUID(m.TurnID.String())
		result.TurnId = &tid
	}
	if m.ParentMessageID != nil {
		pid := protocol.UUID(m.ParentMessageID.String())
		result.ParentMessageId = &pid
	}
	result.InputTokens = m.InputTokens
	result.OutputTokens = m.OutputTokens
	result.TotalTokens = m.TotalTokens
	result.CachedTokens = m.CachedTokens
	result.ReasoningTokens = m.ReasoningTokens
	return result
}
