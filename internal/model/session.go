package model

import (
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TodoItem 任务项（对齐 Claude Code 的 TodoItem 结构）
type TodoItem struct {
	Content    string `json:"content"`     // 任务描述（祈使句）
	Status     string `json:"status"`      // pending/in_progress/completed
	ActiveForm string `json:"active_form"` // 执行中的描述（进行时）
}

// Session 会话模型
type Session struct {
	ID          uuid.UUID       `gorm:"type:uuid;primaryKey" json:"id"`
	ClientID    string          `gorm:"size:255;uniqueIndex" json:"client_id,omitempty"` // 客户端生成的幂等 ID
	OwnerKind   string          `gorm:"size:32;default:'user'" json:"owner_kind"`
	OwnerRefID  string          `gorm:"size:255;index" json:"owner_ref_id"`
	DeviceID    string          `gorm:"size:255" json:"device_id,omitempty"` // 仅 user-owned session 使用，待 protocol 迁移后删除
	Title       string          `gorm:"size:255" json:"title,omitempty"`
	Status      string          `gorm:"size:20;not null;default:active;index" json:"status"`
	AgentPrompt string          `gorm:"type:text" json:"agent_prompt,omitempty"`
	TodoList    JSONB[TodoItem] `gorm:"type:jsonb;not null;default:'[]'" json:"todo_list"`
	// Sub Agent 层级关系
	ParentClientSessionID   string    `gorm:"size:255;index" json:"parent_client_session_id,omitempty"`     // 父 session 的 client ID
	ParentServerSessionID   uuid.UUID `gorm:"type:uuid;index" json:"parent_server_session_id,omitempty"`    // 父 session 的 server ID
	RootClientSessionID     string    `gorm:"size:255;index" json:"root_client_session_id,omitempty"`       // 根 session 的 client ID
	RootServerSessionID     uuid.UUID `gorm:"type:uuid;index" json:"root_server_session_id,omitempty"`      // 根 session 的 server ID
	SubAgentParentMessageID uuid.UUID `gorm:"type:uuid;index" json:"sub_agent_parent_message_id,omitempty"` // 父 session 中 sub_agent_invocation 消息的 ID
	SubAgentMode            string    `gorm:"size:20;default:''" json:"sub_agent_mode,omitempty"`           // sub agent 调用模式: "sync"(默认) 或 "async"

	// ========================================================================
	// Token 用量累计（Session 级别）
	// ========================================================================

	// TotalInputTokens 累计纯输入 token 数（不含 cached read/write）
	TotalInputTokens int64 `gorm:"default:0" json:"total_input_tokens"`

	// TotalOutputTokens 累计输出 token 数
	TotalOutputTokens int64 `gorm:"default:0" json:"total_output_tokens"`

	// TotalTokens 累计总 token 数（包含所有类型，与 eino TotalTokens 对齐）
	TotalTokens int64 `gorm:"default:0" json:"total_tokens"`

	// CurrentContextTokens 当前上下文实际 token 数。
	// 压缩后由 cumulativeTokenCounter 回写，非压缩轮次由 token_callback 近似更新。
	// 用于前端压缩进度计算（替代 TotalTokens，解决累计值永不减少导致进度 100% 的 bug）。
	// 默认 0 表示尚未初始化，ComputeTokenEstimate 会 fallback 到 TotalTokens。
	CurrentContextTokens int64 `gorm:"default:0" json:"current_context_tokens"`

	// TotalCachedReadTokens 累计缓存读取（cache hit）token 数
	TotalCachedReadTokens int64 `gorm:"default:0" json:"total_cached_read_tokens"`

	// TotalCachedWriteTokens 累计缓存写入（cache creation）token 数
	TotalCachedWriteTokens int64 `gorm:"default:0" json:"total_cached_write_tokens"`

	// TotalReasoningTokens 累计推理（thinking）token 数
	TotalReasoningTokens int64 `gorm:"default:0" json:"total_reasoning_tokens"`

	// ========================================================================
	// 成本统计
	// ========================================================================

	// TotalCostMicros 累计成本（微美元，1 USD = 1,000,000 micros）
	TotalCostMicros int64 `gorm:"default:0" json:"total_cost_micros"`

	// ========================================================================
	// Token 预估（持久化 EWMA，派生字段在 ToProtocolSession 中计算）
	// ========================================================================

	// TokenEstimateEWMA 指数加权移动平均，用于预估下一轮 token 增量。
	// 每次 LLM 调用后通过 EWMA 公式增量更新：ewma = α * old + (1-α) * roundDelta。
	// 压缩后按压缩比率调整：new_ewma = old_ewma * (tokensAfter / tokensBefore)。
	// 默认值 2000（首次调用前的初始增长率估计）。
	TokenEstimateEWMA float64 `gorm:"default:2000" json:"token_estimate_ewma"`

	// ========================================================================
	// 元数据
	// ========================================================================

	// LastTokenUpdateAt 最后一次 token 统计更新时间
	LastTokenUpdateAt *time.Time `json:"last_token_update_at"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
	DeletedAt *time.Time `gorm:"index" json:"-"`
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

// GetTotalCostUSD 返回美元成本
func (s *Session) GetTotalCostUSD() float64 {
	return float64(s.TotalCostMicros) / 1_000_000
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
		DeviceId:                strPtr(m.DeviceID),
		Title:                   strPtr(m.Title),
		Status:                  protocol.SessionStatus(m.Status),
		AgentPrompt:             strPtr(m.AgentPrompt),
		TodoList:                todoList,
		ParentClientSessionId:   parentClientSessionID,
		ParentServerSessionId:   parentServerSessionID,
		RootClientSessionId:     rootClientSessionID,
		RootServerSessionId:     rootServerSessionID,
		SubAgentParentMessageId: subAgentParentMessageID,
		// Token 用量累计
		TotalInputTokens:       ptrTo(m.TotalInputTokens),
		TotalOutputTokens:      ptrTo(m.TotalOutputTokens),
		TotalTokens:            ptrTo(m.TotalTokens),
		CurrentContextTokens:   ptrTo(m.CurrentContextTokens),
		TotalCachedReadTokens:  ptrTo(m.TotalCachedReadTokens),
		TotalCachedWriteTokens: ptrTo(m.TotalCachedWriteTokens),
		TotalReasoningTokens:   ptrTo(m.TotalReasoningTokens),
		TotalCostUsd:           ptrTo(m.GetTotalCostUSD()),
		LastTokenUpdateAt:      m.LastTokenUpdateAt,
		CreatedAt:              m.CreatedAt,
		UpdatedAt:              m.UpdatedAt,
		ClosedAt:               m.ClosedAt,
		DeletedAt:              m.DeletedAt,
	}
}

// TokenEstimateFields holds computed token estimate fields derived from EWMA.
type TokenEstimateFields struct {
	CompressionThreshold   int64
	CompressionProgress    float64
	RoundsUntilCompression int
	EstimatedNextRound     int64
}

// ComputeTokenEstimate computes derived token estimate fields from the
// persisted EWMA and the configured compression threshold.
//
// The threshold is typically `contextTokensLimit - autoCompactBufferTokens`.
// Returns nil if EWMA is not initialized (zero value).
func (s *Session) ComputeTokenEstimate(threshold int64) *TokenEstimateFields {
	if s.TokenEstimateEWMA <= 0 || threshold <= 0 {
		return nil
	}

	ewma := s.TokenEstimateEWMA
	// 优先使用 CurrentContextTokens（压缩后回写的实际上下文大小），
	// fallback 到 TotalTokens（旧 session 或压缩前的累计值）。
	currentTokens := s.CurrentContextTokens
	if currentTokens <= 0 {
		currentTokens = s.TotalTokens
	}

	// Compression progress (0-100)
	progress := float64(currentTokens) / float64(threshold) * 100
	if progress > 100 {
		progress = 100
	}

	// Rounds until compression
	var roundsUntil int
	if currentTokens >= threshold {
		roundsUntil = -1
	} else if ewma > 0 {
		remaining := float64(threshold - currentTokens)
		roundsUntil = int(remaining / ewma)
	} else {
		roundsUntil = 999
	}

	// Estimated next round tokens
	estimatedNext := currentTokens + int64(ewma)

	return &TokenEstimateFields{
		CompressionThreshold:   threshold,
		CompressionProgress:    progress,
		RoundsUntilCompression: roundsUntil,
		EstimatedNextRound:     estimatedNext,
	}
}
