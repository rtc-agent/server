package dbmodel

import (
	"encoding/json"
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Rtc 远程 Tool 调用模型
type Rtc struct {
	ID              uuid.UUID   `gorm:"type:uuid;primaryKey" json:"id"`
	ClientID        string      `gorm:"size:255;uniqueIndex" json:"client_id,omitempty"` // 客户端生成的幂等 ID
	SessionID       uuid.UUID   `gorm:"type:uuid;not null;index" json:"session_id"`
	TurnID          uuid.UUID   `gorm:"type:uuid;not null;index" json:"turn_id"`
	MessageID       uuid.UUID   `gorm:"type:uuid;index" json:"message_id,omitempty"`        // 关联的 toolcall_input Message
	OutputMessageID *uuid.UUID  `gorm:"type:uuid;index" json:"output_message_id,omitempty"` // 关联的 toolcall_output Message
	Offset          uint32      `gorm:"not null" json:"offset"`
	ToolName        string      `gorm:"size:100;not null" json:"tool_name"`
	Parameters      JSONBString `gorm:"type:jsonb" json:"parameters"`
	Status          string      `gorm:"size:20;not null;default:pending;index" json:"status"`
	Result          JSONBString `gorm:"type:jsonb" json:"result,omitempty"`
	ErrorMessage    string      `gorm:"type:text" json:"error_message,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	CompletedAt     *time.Time  `json:"completed_at,omitempty"`
	DeletedAt       *time.Time  `gorm:"index" json:"-"`
}

func (r *Rtc) BeforeCreate(tx *gorm.DB) error {
	if r.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		r.ID = id
	}
	return nil
}

// RTC 状态常量（protocol 为单一真相源）
const (
	RtcStatusPending   = protocol.RtcStatusPending
	RtcStatusSent      = protocol.RtcStatusSent
	RtcStatusExecuting = protocol.RtcStatusExecuting
	RtcStatusCompleted = protocol.RtcStatusCompleted
	RtcStatusFailed    = protocol.RtcStatusFailed
	RtcStatusTimeout   = protocol.RtcStatusTimeout
	RtcStatusRejected  = protocol.RtcStatusRejected
)

// ToProtocolRtc 将 dbmodel.Rtc 转换为 protocol.Rtc。
// nil 输入返回零值 protocol.Rtc。
// Parameters 与 Result 从 JSONB 字符串反序列化为 any；解析失败时保留为 nil。
func ToProtocolRtc(r *Rtc) protocol.Rtc {
	if r == nil {
		return protocol.Rtc{}
	}
	result := protocol.Rtc{
		Id:           protocol.UUID(r.ID.String()),
		ClientId:     strPtr(r.ClientID),
		SessionId:    protocol.UUID(r.SessionID.String()),
		TurnId:       protocol.UUID(r.TurnID.String()),
		Offset:       r.Offset,
		ToolName:     r.ToolName,
		Status:       protocol.RtcStatus(r.Status),
		ErrorMessage: strPtr(r.ErrorMessage),
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
		CompletedAt:  r.CompletedAt,
		DeletedAt:    r.DeletedAt,
	}
	if r.MessageID != uuid.Nil {
		msgId := protocol.UUID(r.MessageID.String())
		result.MessageId = &msgId
	}
	if r.OutputMessageID != nil {
		outMsgId := protocol.UUID(r.OutputMessageID.String())
		result.OutputMessageId = &outMsgId
	}
	if r.Parameters != "" {
		_ = json.Unmarshal([]byte(r.Parameters), &result.Parameters)
	}
	if r.Result != "" {
		_ = json.Unmarshal([]byte(r.Result), &result.Result)
	}
	return result
}
