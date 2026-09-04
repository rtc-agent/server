package dbmodel

import (
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Session 会话模型
type Session struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	ClientID   string     `gorm:"size:255;uniqueIndex" json:"client_id,omitempty"` // 客户端生成的幂等 ID
	OwnerKind  string     `gorm:"size:32;default:'user'" json:"owner_kind"`
	OwnerRefID string     `gorm:"size:255;index" json:"owner_ref_id"`
	DeviceID   string     `gorm:"size:255" json:"device_id,omitempty"` // 仅 user-owned session 使用，待 protocol 迁移后删除
	Title      string     `gorm:"size:255" json:"title,omitempty"`
	Status     string     `gorm:"size:20;not null;default:active;index" json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
	DeletedAt  *time.Time `gorm:"index" json:"-"`
}

func (s *Session) BeforeCreate(tx *gorm.DB) error {
	if s.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		s.ID = id
	}
	return nil
}

// 会话状态常量（protocol 为单一真相源，此处 re-export 便于本包内使用）
const (
	SessionStatusActive = protocol.SessionStatusActive
	SessionStatusClosed = protocol.SessionStatusClosed
)

// ToProtocolSession 将 dbmodel.Session 转换为 protocol.Session。
// nil 输入返回零值 protocol.Session。
func ToProtocolSession(m *Session) protocol.Session {
	if m == nil {
		return protocol.Session{}
	}
	return protocol.Session{
		Id:         protocol.UUID(m.ID.String()),
		ClientId:   strPtr(m.ClientID),
		OwnerKind:  m.OwnerKind,
		OwnerRefId: m.OwnerRefID,
		Title:      strPtr(m.Title),
		Status:     protocol.SessionStatus(m.Status),
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
		ClosedAt:   m.ClosedAt,
		DeletedAt:  m.DeletedAt,
	}
}
