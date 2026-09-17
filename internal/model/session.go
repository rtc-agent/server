package model

import (
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TodoItem represents a task item (aligned with Claude Code's TodoItem structure).
type TodoItem struct {
	Content    string `json:"content"`     // task description (imperative)
	Status     string `json:"status"`      // pending/in_progress/completed
	ActiveForm string `json:"active_form"` // in-progress description (present tense)
}

// Session is the session model.
type Session struct {
	ID          uuid.UUID       `gorm:"type:uuid;primaryKey" json:"id"`
	ClientID    string          `gorm:"size:255;uniqueIndex" json:"client_id,omitempty"` // client-generated idempotency ID
	OwnerKind   string          `gorm:"size:32;default:'user'" json:"owner_kind"`
	OwnerRefID  string          `gorm:"size:255;index" json:"owner_ref_id"`
	DeviceID    string          `gorm:"size:255" json:"device_id,omitempty"` // only for user-owned sessions; remove after protocol migration
	Title       string          `gorm:"size:255" json:"title,omitempty"`
	Status      string          `gorm:"size:20;not null;default:active;index" json:"status"`
	AgentPrompt string          `gorm:"type:text" json:"agent_prompt,omitempty"`
	TodoList    JSONB[TodoItem] `gorm:"type:jsonb;not null;default:'[]'" json:"todo_list"`
	// Sub-agent hierarchy.
	ParentClientSessionID   string    `gorm:"size:255;index" json:"parent_client_session_id,omitempty"`     // parent session's client ID
	ParentServerSessionID   uuid.UUID `gorm:"type:uuid;index" json:"parent_server_session_id,omitempty"`    // parent session's server ID
	RootClientSessionID     string    `gorm:"size:255;index" json:"root_client_session_id,omitempty"`       // root session's client ID
	RootServerSessionID     uuid.UUID `gorm:"type:uuid;index" json:"root_server_session_id,omitempty"`      // root session's server ID
	SubAgentParentMessageID uuid.UUID `gorm:"type:uuid;index" json:"sub_agent_parent_message_id,omitempty"` // sub_agent_invocation message ID in the parent session
	SubAgentMode            string    `gorm:"size:20;default:''" json:"sub_agent_mode,omitempty"`           // sub-agent invocation mode: "sync" (default) or "async"

	// ========================================================================
	// Cumulative token usage (session-level)
	// ========================================================================

	// TotalInputTokens cumulative pure input tokens (excluding cached read/write)
	TotalInputTokens int64 `gorm:"default:0" json:"total_input_tokens"`

	// TotalOutputTokens cumulative output tokens
	TotalOutputTokens int64 `gorm:"default:0" json:"total_output_tokens"`

	// TotalTokens cumulative total tokens (includes all types, aligned with eino TotalTokens)
	TotalTokens int64 `gorm:"default:0" json:"total_tokens"`

	// CurrentContextTokens is the actual current context token count.
	// Written back by cumulativeTokenCounter after compaction; approximately
	// updated by token_callback on non-compaction turns.
	// Used for frontend compression progress calculation (replaces TotalTokens
	// to fix the bug where cumulative values never decrease, causing 100% progress).
	// Default 0 means not yet initialized; ComputeTokenEstimate falls back to TotalTokens.
	CurrentContextTokens int64 `gorm:"default:0" json:"current_context_tokens"`

	// TotalCachedReadTokens cumulative cache-hit tokens
	TotalCachedReadTokens int64 `gorm:"default:0" json:"total_cached_read_tokens"`

	// TotalCachedWriteTokens cumulative cache-creation tokens
	TotalCachedWriteTokens int64 `gorm:"default:0" json:"total_cached_write_tokens"`

	// TotalReasoningTokens cumulative reasoning (thinking) tokens
	TotalReasoningTokens int64 `gorm:"default:0" json:"total_reasoning_tokens"`

	// ========================================================================
	// Cost statistics
	// ========================================================================

	// TotalCostMicros cumulative cost in micro-USD (1 USD = 1,000,000 micros)
	TotalCostMicros int64 `gorm:"default:0" json:"total_cost_micros"`

	// ========================================================================
	// Token estimation (persisted EWMA; derived fields computed in ToProtocolSession)
	// ========================================================================

	// TokenEstimateEWMA is the exponentially weighted moving average used to
	// estimate the next round's token increment.
	// Incrementally updated after each LLM call: ewma = alpha * old + (1-alpha) * roundDelta.
	// Adjusted after compaction by compression ratio: new_ewma = old_ewma * (tokensAfter / tokensBefore).
	// Default 2000 (initial growth rate estimate before the first call).
	TokenEstimateEWMA float64 `gorm:"default:2000" json:"token_estimate_ewma"`

	// ========================================================================
	// Memory extraction state (for session memory background extractor)
	// ========================================================================

	// MemoryExtractionLastTokens is the token count at the last session
	// memory extraction. Updated by triggerSessionMemoryExtraction after
	// a successful extraction. 0 means never extracted.
	MemoryExtractionLastTokens int `gorm:"default:0" json:"memory_extraction_last_tokens"`

	// MemoryExtractionLastMessages is the message count at the last session
	// memory extraction. Used to compute the incremental tool call count
	// since the last extraction.
	MemoryExtractionLastMessages int `gorm:"default:0" json:"memory_extraction_last_messages"`

	// ========================================================================
	// Metadata
	// ========================================================================

	// LastTokenUpdateAt is the last time token statistics were updated.
	LastTokenUpdateAt *time.Time `json:"last_token_update_at"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
	DeletedAt *time.Time `gorm:"index" json:"-"`
}

// BeforeCreate generates a UUID v7 identifier if one is not already set.
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

// GetTotalCostUSD returns the cost in USD.
func (s *Session) GetTotalCostUSD() float64 {
	return float64(s.TotalCostMicros) / 1_000_000
}

// Session status constants (protocol is the single source of truth; these
// are re-exported here for convenience within this package).
const (
	SessionStatusActive = protocol.SessionStatusActive
	SessionStatusIdle   = protocol.SessionStatusIdle
	SessionStatusClosed = protocol.SessionStatusClosed
)

// ToProtocolSession converts a dbmodel.Session to a protocol.Session.
// A nil input returns a zero-value protocol.Session.
func ToProtocolSession(m *Session) protocol.Session {
	if m == nil {
		return protocol.Session{}
	}
	// Convert TodoList.
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
	// Convert sub-agent hierarchy fields.
	var parentClientSessionID *string
	if m.ParentClientSessionID != "" {
		parentClientSessionID = &m.ParentClientSessionID
	}
	var parentServerSessionID *protocol.UUID
	if m.ParentServerSessionID != uuid.Nil {
		pid := m.ParentServerSessionID.String()
		parentServerSessionID = &pid
	}
	var rootClientSessionID *string
	if m.RootClientSessionID != "" {
		rootClientSessionID = &m.RootClientSessionID
	}
	var rootServerSessionID *protocol.UUID
	if m.RootServerSessionID != uuid.Nil {
		rid := m.RootServerSessionID.String()
		rootServerSessionID = &rid
	}
	var subAgentParentMessageID *protocol.UUID
	if m.SubAgentParentMessageID != uuid.Nil {
		mid := m.SubAgentParentMessageID.String()
		subAgentParentMessageID = &mid
	}
	return protocol.Session{
		Id:                      m.ID.String(),
		ClientId:                StrPtr(m.ClientID),
		OwnerKind:               m.OwnerKind,
		OwnerRefId:              m.OwnerRefID,
		DeviceId:                StrPtr(m.DeviceID),
		Title:                   StrPtr(m.Title),
		Status:                  protocol.SessionStatus(m.Status),
		AgentPrompt:             StrPtr(m.AgentPrompt),
		TodoList:                todoList,
		ParentClientSessionId:   parentClientSessionID,
		ParentServerSessionId:   parentServerSessionID,
		RootClientSessionId:     rootClientSessionID,
		RootServerSessionId:     rootServerSessionID,
		SubAgentParentMessageId: subAgentParentMessageID,
		// Cumulative token usage.
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
	// Prefer CurrentContextTokens (actual context size written back after
	// compaction); fall back to TotalTokens (cumulative value for old
	// sessions or pre-compaction state).
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
