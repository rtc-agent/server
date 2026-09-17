package model

import (
	"encoding/json"
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Rtc is the database model for a remote tool call (RTC).
type Rtc struct {
	ID              uuid.UUID   `gorm:"type:uuid;primaryKey" json:"id"`
	ClientID        string      `gorm:"size:255;uniqueIndex" json:"client_id,omitempty"` // client-generated idempotency key
	SessionID       uuid.UUID   `gorm:"type:uuid;not null;index" json:"session_id"`
	TurnID          uuid.UUID   `gorm:"type:uuid;not null;index" json:"turn_id"`
	MessageID       uuid.UUID   `gorm:"type:uuid;index" json:"message_id,omitempty"`        // associated toolcall_input Message
	OutputMessageID *uuid.UUID  `gorm:"type:uuid;index" json:"output_message_id,omitempty"` // associated toolcall_output Message
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

// BeforeCreate generates a UUID v7 identifier if one is not already set.
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

// RTC status constants (protocol is the single source of truth).
const (
	RtcStatusPending   = protocol.RtcStatusPending
	RtcStatusSent      = protocol.RtcStatusSent
	RtcStatusExecuting = protocol.RtcStatusExecuting
	RtcStatusCompleted = protocol.RtcStatusCompleted
	RtcStatusFailed    = protocol.RtcStatusFailed
	RtcStatusTimeout   = protocol.RtcStatusTimeout
	RtcStatusRejected  = protocol.RtcStatusRejected
)

// ToProtocolRtc converts a dbmodel Rtc to a protocol.Rtc.
// A nil input returns a zero-value protocol.Rtc.
// Parameters and Result are deserialized from JSONB strings to any;
// parse failures leave the fields as nil.
func ToProtocolRtc(r *Rtc) protocol.Rtc {
	if r == nil {
		return protocol.Rtc{}
	}
	result := protocol.Rtc{
		Id:           r.ID.String(),
		ClientId:     StrPtr(r.ClientID),
		SessionId:    r.SessionID.String(),
		TurnId:       r.TurnID.String(),
		Offset:       r.Offset,
		ToolName:     r.ToolName,
		Status:       protocol.RtcStatus(r.Status),
		ErrorMessage: StrPtr(r.ErrorMessage),
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
		CompletedAt:  r.CompletedAt,
		DeletedAt:    r.DeletedAt,
	}
	if r.MessageID != uuid.Nil {
		msgId := r.MessageID.String()
		result.MessageId = &msgId
	}
	if r.OutputMessageID != nil {
		outMsgId := r.OutputMessageID.String()
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
