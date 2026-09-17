package model

import (
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Message is the database model for a conversation message.
type Message struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	ClientID        string     `gorm:"size:255;uniqueIndex" json:"client_id,omitempty"` // client-generated idempotency key
	SessionID       uuid.UUID  `gorm:"type:uuid;not null;index" json:"session_id"`
	TurnID          *uuid.UUID `gorm:"type:uuid;index" json:"turn_id,omitempty"`
	ParentMessageID *uuid.UUID `gorm:"type:uuid;index" json:"parent_message_id,omitempty"` // toolcall_output points to toolcall_input
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

	// Token usage (populated only for assistant messages; NULL for other roles).
	// Design note: toolcall_input/toolcall_output messages do not record token
	// usage because:
	// 1. The Session table already accumulates all tokens via AtomicAddTokenUsage
	//    (no data loss).
	// 2. A single LLM call may produce multiple tool_calls, making it impossible
	//    to fairly apportion tokens to individual messages.
	// 3. For message-level statistics, use the Session table's cumulative fields.
	InputTokens     *int `json:"input_tokens,omitempty"`
	OutputTokens    *int `json:"output_tokens,omitempty"`
	TotalTokens     *int `json:"total_tokens,omitempty"`
	CachedTokens    *int `json:"cached_tokens,omitempty"`
	ReasoningTokens *int `json:"reasoning_tokens,omitempty"`
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
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

// Message role constants (protocol is the single source of truth).
const (
	MessageRoleUser      = protocol.MessageRoleUser
	MessageRoleAssistant = protocol.MessageRoleAssistant
	MessageRoleSystem    = protocol.MessageRoleSystem
)

// Message streaming status constants (protocol is the single source of truth).
const (
	MessageStreamingPending   = protocol.MessageStreamingPending
	MessageStreamingStreaming = protocol.MessageStreamingStreaming
	MessageStreamingCompleted = protocol.MessageStreamingCompleted
	MessageStreamingFailed    = protocol.MessageStreamingFailed
)

// TokenUsageUpdate carries the token usage values for a message update.
// InputTokens here follows the same semantics as TokenUsage.InputTokens
// (i.e., includes cached read/write), consistent with eino's schema.TokenUsage.
type TokenUsageUpdate struct {
	InputTokens     int
	OutputTokens    int
	TotalTokens     int
	CachedTokens    int
	ReasoningTokens int
}

// TokenUsage returns a TokenUsageUpdate populated from the message's
// nullable token fields. Nil fields are treated as zero.
// Extracted to avoid verbose copy boilerplate in fork/session operations.
func (m *Message) TokenUsage() TokenUsageUpdate {
	var u TokenUsageUpdate
	if m.InputTokens != nil {
		u.InputTokens = *m.InputTokens
	}
	if m.OutputTokens != nil {
		u.OutputTokens = *m.OutputTokens
	}
	if m.TotalTokens != nil {
		u.TotalTokens = *m.TotalTokens
	}
	if m.CachedTokens != nil {
		u.CachedTokens = *m.CachedTokens
	}
	if m.ReasoningTokens != nil {
		u.ReasoningTokens = *m.ReasoningTokens
	}
	return u
}

// ToProtocolMessage converts a dbmodel Message to a protocol.Message.
// A nil input returns a zero-value protocol.Message.
func ToProtocolMessage(m *Message) protocol.Message {
	if m == nil {
		return protocol.Message{}
	}
	result := protocol.Message{
		Id:              m.ID.String(),
		ClientId:        StrPtr(m.ClientID),
		SessionId:       m.SessionID.String(),
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
		tid := m.TurnID.String()
		result.TurnId = &tid
	}
	if m.ParentMessageID != nil {
		pid := m.ParentMessageID.String()
		result.ParentMessageId = &pid
	}
	result.InputTokens = m.InputTokens
	result.OutputTokens = m.OutputTokens
	result.TotalTokens = m.TotalTokens
	result.CachedTokens = m.CachedTokens
	result.ReasoningTokens = m.ReasoningTokens
	return result
}
