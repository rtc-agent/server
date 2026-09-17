// internal/usecase/primitives/message.go
package primitives

import (
	"context"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MessageToCreate holds the input parameters for batch message creation.
type MessageToCreate struct {
	Role     protocol.MessageRole
	Creator  usecase.Creator
	Content  protocol.ContentData
	Status   protocol.MessageStreamingStatus
	ClientID string // empty string triggers auto-generation
	// CreatedAt/UpdatedAt are optional, used to preserve original timestamps
	// (e.g., Fork scenarios). Zero values use the current time.
	CreatedAt time.Time
	UpdatedAt time.Time
	// TokenUsage is optional, only needed for assistant messages.
	TokenUsage *model.TokenUsageUpdate
}

// CreateMessage creates a message inside a transaction (callers must invoke
// within a RunAndPublish callback). The Creator writes the new message's
// CreatorKind/CreatorRefID fields (columns added in T2). Internally calls
// AllocateOffsets; callers do not need to pre-allocate offsets. When turnID
// is nil the message does not belong to any Turn (TurnID/TurnOffset fields
// are left empty). When parentMessageID is non-nil it sets ParentMessageID
// (used for toolcall_output pointing to toolcall_input). The content parameter
// is a ContentData struct serialized as JSON and stored in the database
// Content field.
func CreateMessage(
	txCtx context.Context,
	deps *usecase.Dependencies,
	sessionID uuid.UUID,
	turnID *uuid.UUID,
	role protocol.MessageRole,
	creator usecase.Creator,
	content protocol.ContentData,
	status protocol.MessageStreamingStatus,
	clientID string,
	parentMessageID *uuid.UUID,
) (*model.Message, error) {
	globalOffset, turnOffset, err := AllocateOffsets(txCtx, deps, sessionID, turnID, 1)
	if err != nil {
		return nil, err
	}

	// Serialize ContentData to JSON string for storage
	contentStr, err := SerializeContentData(content)
	if err != nil {
		return nil, fmt.Errorf("serialize content data: %w", err)
	}

	// client_id has a unique constraint. Client messages are provided by the
	// caller to ensure idempotency. System-generated messages (assistant/system)
	// receive an empty string from the caller; we auto-generate a UUID here to
	// prevent multiple system messages from sharing an empty client_id and
	// triggering a unique constraint violation.
	if clientID == "" {
		clientID = uuid.Must(uuid.NewV7()).String()
	}

	message := &model.Message{
		ID:              uuid.Must(uuid.NewV7()),
		ClientID:        clientID,
		SessionID:       sessionID,
		TurnID:          turnID,
		GlobalOffset:    globalOffset,
		TurnOffset:      turnOffset,
		Role:            string(role),
		Content:         contentStr,
		StreamingStatus: string(status),
		CreatorKind:     string(creator.Kind()),
		CreatorRefID:    creator.ReferenceID(),
	}
	if parentMessageID != nil {
		message.ParentMessageID = parentMessageID
	}
	if err := deps.MessageRepo.Create(txCtx, message); err != nil {
		return nil, fmt.Errorf("create message: %w", err)
	}
	turnIDLog := "<nil>"
	if turnID != nil {
		turnIDLog = turnID.String()
	}
	logger.Info(txCtx, "[primitives.CreateMessage]",
		zap.String("id", message.ID.String()),
		zap.String("client_id", clientID),
		zap.String("session", sessionID.String()),
		zap.String("turn", turnIDLog),
		zap.String("role", string(role)),
		zap.String("creator_kind", string(creator.Kind())),
		zap.String("creator_ref_id", creator.ReferenceID()),
		zap.String("content_type", string(content.Type)))
	return message, nil
}

// BatchCreateMessages creates multiple messages in bulk (one Redis call + one
// DB call). All messages share the same sessionID and turnID.
func BatchCreateMessages(
	txCtx context.Context,
	deps *usecase.Dependencies,
	sessionID uuid.UUID,
	turnID *uuid.UUID,
	messages []MessageToCreate,
) ([]*model.Message, error) {
	count := len(messages)
	if count == 0 {
		return nil, nil
	}

	// Single Redis call allocates all offsets
	startGlobalOffset, startTurnOffset, err := AllocateOffsets(txCtx, deps, sessionID, turnID, count)
	if err != nil {
		return nil, fmt.Errorf("allocate offsets: %w", err)
	}

	// Build message list
	dbMessages := make([]*model.Message, count)
	now := time.Now()
	for i, m := range messages {
		globalOffset := startGlobalOffset + uint32(i)
		clientID := m.ClientID
		if clientID == "" {
			clientID = uuid.Must(uuid.NewV7()).String()
		}

		contentStr, err := SerializeContentData(m.Content)
		if err != nil {
			return nil, fmt.Errorf("serialize content data: %w", err)
		}

		// Use provided timestamps; zero values fall back to current time
		createdAt := m.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		updatedAt := m.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = now
		}

		msg := &model.Message{
			ID:              uuid.Must(uuid.NewV7()),
			ClientID:        clientID,
			SessionID:       sessionID,
			TurnID:          turnID,
			GlobalOffset:    globalOffset,
			Role:            string(m.Role),
			Content:         contentStr,
			StreamingStatus: string(m.Status),
			CreatorKind:     string(m.Creator.Kind()),
			CreatorRefID:    m.Creator.ReferenceID(),
			CreatedAt:       createdAt,
			UpdatedAt:       updatedAt,
		}
		if m.TokenUsage != nil {
			msg.InputTokens = &m.TokenUsage.InputTokens
			msg.OutputTokens = &m.TokenUsage.OutputTokens
			msg.TotalTokens = &m.TokenUsage.TotalTokens
			if m.TokenUsage.CachedTokens > 0 {
				msg.CachedTokens = &m.TokenUsage.CachedTokens
			}
			if m.TokenUsage.ReasoningTokens > 0 {
				msg.ReasoningTokens = &m.TokenUsage.ReasoningTokens
			}
		}
		if turnID != nil && startTurnOffset != nil {
			turnOffset := *startTurnOffset + uint32(i)
			msg.TurnOffset = &turnOffset
		}
		dbMessages[i] = msg
	}

	// Single DB batch insert
	if err := deps.MessageRepo.BatchCreate(txCtx, dbMessages); err != nil {
		return nil, fmt.Errorf("batch create messages: %w", err)
	}

	turnIDLog := "<nil>"
	if turnID != nil {
		turnIDLog = turnID.String()
	}
	logger.Info(txCtx, "[primitives.BatchCreateMessages]",
		zap.String("session", sessionID.String()),
		zap.String("turn", turnIDLog),
		zap.Int("count", count),
		zap.Uint32("start_offset", startGlobalOffset))

	return dbMessages, nil
}
