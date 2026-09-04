package dbmodel

import (
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Turn 消息轮次模型
type Turn struct {
	ID           uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	ClientID     string     `gorm:"size:255;uniqueIndex" json:"client_id,omitempty"` // 客户端生成的幂等 ID
	SessionID    uuid.UUID  `gorm:"type:uuid;not null;index" json:"session_id"`
	Status       string     `gorm:"size:20;not null;default:pending;index" json:"status"`
	ErrorMessage string     `gorm:"type:text" json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	DeletedAt    *time.Time `gorm:"index" json:"-"`
}

func (t *Turn) BeforeCreate(tx *gorm.DB) error {
	if t.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		t.ID = id
	}
	if t.ClientID == "" {
		t.ClientID = uuid.New().String()
	}
	return nil
}

// Turn 状态常量（protocol 为单一真相源）
const (
	TurnStatusPending     = protocol.TurnStatusPending
	TurnStatusRunning     = protocol.TurnStatusRunning
	TurnStatusCompleted   = protocol.TurnStatusCompleted
	TurnStatusFailed      = protocol.TurnStatusFailed
	TurnStatusCancelled   = protocol.TurnStatusCancelled
	TurnStatusMerged      = protocol.TurnStatusMerged
	TurnStatusInterrupted = protocol.TurnStatusInterrupted
)

// ToProtocolTurn 将 dbmodel.Turn 转换为 protocol.Turn。
// nil 输入返回零值 protocol.Turn。
func ToProtocolTurn(t *Turn) protocol.Turn {
	if t == nil {
		return protocol.Turn{}
	}
	return protocol.Turn{
		Id:           protocol.UUID(t.ID.String()),
		ClientId:     strPtr(t.ClientID),
		SessionId:    protocol.UUID(t.SessionID.String()),
		Status:       protocol.TurnStatus(t.Status),
		CreatedAt:    t.CreatedAt,
		StartedAt:    t.StartedAt,
		CompletedAt:  t.CompletedAt,
		ErrorMessage: strPtr(t.ErrorMessage),
		DeletedAt:    t.DeletedAt,
	}
}
