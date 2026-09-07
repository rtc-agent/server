package model

import (
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TodoItem 任务项（对齐 Claude Code 的 TodoItem 结构）
type TodoItem struct {
	Content    string `json:"content"`      // 任务描述（祈使句）
	Status     string `json:"status"`       // pending/in_progress/completed
	ActiveForm string `json:"active_form"`  // 执行中的描述（进行时）
}

// Session 会话模型
type Session struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	ClientID   string     `gorm:"size:255;uniqueIndex" json:"client_id,omitempty"` // 客户端生成的幂等 ID
	OwnerKind  string     `gorm:"size:32;default:'user'" json:"owner_kind"`
	OwnerRefID string     `gorm:"size:255;index" json:"owner_ref_id"`
	DeviceID   string     `gorm:"size:255" json:"device_id,omitempty"` // 仅 user-owned session 使用，待 protocol 迁移后删除
	Title      string     `gorm:"size:255" json:"title,omitempty"`
	Status     string     `gorm:"size:20;not null;default:active;index" json:"status"`
	AgentPrompt string    `gorm:"type:text" json:"agent_prompt,omitempty"`
	TodoList   JSONB[TodoItem] `gorm:"type:jsonb;not null;default:'[]'" json:"todo_list"`
	// Sub Agent 层级关系
	ParentClientSessionID   string    `gorm:"size:255;index" json:"parent_client_session_id,omitempty"`       // 父 session 的 client ID
	ParentServerSessionID   uuid.UUID `gorm:"type:uuid;index" json:"parent_server_session_id,omitempty"`      // 父 session 的 server ID
	RootClientSessionID     string    `gorm:"size:255;index" json:"root_client_session_id,omitempty"`         // 根 session 的 client ID
	RootServerSessionID     uuid.UUID `gorm:"type:uuid;index" json:"root_server_session_id,omitempty"`        // 根 session 的 server ID
	SubAgentParentMessageID uuid.UUID `gorm:"type:uuid;index" json:"sub_agent_parent_message_id,omitempty"`   // 父 session 中 sub_agent_invocation 消息的 ID
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
	SessionStatusIdle   = protocol.SessionStatusIdle
	SessionStatusClosed = protocol.SessionStatusClosed
)

// ToProtocolSession 将 dbmodel.Session 转换为 protocol.Session。
// nil 输入返回零值 protocol.Session。
func ToProtocolSession(m *Session) protocol.Session {
	if m == nil {
		return protocol.Session{}
	}
	// 转换 TodoList
	var todoList *[]protocol.TodoItem
	if len(m.TodoList) > 0 {
		items := make([]protocol.TodoItem, len(m.TodoList))
		for i, item := range m.TodoList {
			items[i] = protocol.TodoItem{
				Content:    item.Content,
				Status:     protocol.TodoItemStatus(item.Status),
				ActiveForm: item.ActiveForm,
			}
		}
		todoList = &items
	}
	// 转换 Sub Agent 层级关系字段
	var parentClientSessionID *string
	if m.ParentClientSessionID != "" {
		parentClientSessionID = &m.ParentClientSessionID
	}
	var parentServerSessionID *protocol.UUID
	if m.ParentServerSessionID != uuid.Nil {
		pid := protocol.UUID(m.ParentServerSessionID.String())
		parentServerSessionID = &pid
	}
	var rootClientSessionID *string
	if m.RootClientSessionID != "" {
		rootClientSessionID = &m.RootClientSessionID
	}
	var rootServerSessionID *protocol.UUID
	if m.RootServerSessionID != uuid.Nil {
		rid := protocol.UUID(m.RootServerSessionID.String())
		rootServerSessionID = &rid
	}
	var subAgentParentMessageID *protocol.UUID
	if m.SubAgentParentMessageID != uuid.Nil {
		mid := protocol.UUID(m.SubAgentParentMessageID.String())
		subAgentParentMessageID = &mid
	}
	return protocol.Session{
		Id:                      protocol.UUID(m.ID.String()),
		ClientId:                strPtr(m.ClientID),
		OwnerKind:               m.OwnerKind,
		OwnerRefId:              m.OwnerRefID,
		Title:                   strPtr(m.Title),
		Status:                  protocol.SessionStatus(m.Status),
		AgentPrompt:             strPtr(m.AgentPrompt),
		TodoList:                todoList,
		ParentClientSessionId:   parentClientSessionID,
		ParentServerSessionId:   parentServerSessionID,
		RootClientSessionId:     rootClientSessionID,
		RootServerSessionId:     rootServerSessionID,
		SubAgentParentMessageId: subAgentParentMessageID,
		CreatedAt:               m.CreatedAt,
		UpdatedAt:               m.UpdatedAt,
		ClosedAt:                m.ClosedAt,
		DeletedAt:               m.DeletedAt,
	}
}
