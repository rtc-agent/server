package model

import (
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Turn is the message turn model.
type Turn struct {
	ID           uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	ClientID     string     `gorm:"size:255;uniqueIndex" json:"client_id,omitempty"` // client-generated idempotency ID
	SessionID    uuid.UUID  `gorm:"type:uuid;not null;index" json:"session_id"`
	Status       string     `gorm:"size:20;not null;default:pending;index" json:"status"`
	ErrorMessage string     `gorm:"type:text" json:"error_message,omitempty"`
	InterruptID  string     `gorm:"size:255" json:"interrupt_id,omitempty"` // eino interrupt ID, used to build ResumeParams on resume
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	DeletedAt    *time.Time `gorm:"index" json:"-"`
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
func (t *Turn) BeforeCreate(tx *gorm.DB) error {
	if t.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		t.ID = id
	}
	if t.ClientID == "" {
		t.ClientID = uuid.Must(uuid.NewV7()).String()
	}
	return nil
}

// Turn status constants (protocol is the single source of truth).
const (
	TurnStatusPending     = protocol.TurnStatusPending
	TurnStatusRunning     = protocol.TurnStatusRunning
	TurnStatusCompleted   = protocol.TurnStatusCompleted
	TurnStatusFailed      = protocol.TurnStatusFailed
	TurnStatusCancelled   = protocol.TurnStatusCancelled
	TurnStatusMerged      = protocol.TurnStatusMerged
	TurnStatusInterrupted = protocol.TurnStatusInterrupted
)

// ToProtocolTurn converts a dbmodel.Turn to a protocol.Turn.
// A nil input returns a zero-value protocol.Turn.
func ToProtocolTurn(t *Turn) protocol.Turn {
	if t == nil {
		return protocol.Turn{}
	}
	return protocol.Turn{
		Id:           t.ID.String(),
		ClientId:     StrPtr(t.ClientID),
		SessionId:    t.SessionID.String(),
		Status:       protocol.TurnStatus(t.Status),
		CreatedAt:    t.CreatedAt,
		StartedAt:    t.StartedAt,
		CompletedAt:  t.CompletedAt,
		ErrorMessage: StrPtr(t.ErrorMessage),
		DeletedAt:    t.DeletedAt,
	}
}
