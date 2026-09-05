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

// MessageToCreate 批量创建消息的输入参数。
type MessageToCreate struct {
	Role     protocol.MessageRole
	Creator  usecase.Creator
	Content  protocol.ContentData
	Status   protocol.MessageStreamingStatus
	ClientID string // 空串则自动生成
	// CreatedAt/UpdatedAt 可选，用于保留原始时间戳（Fork 场景）
	// 零值则使用当前时间
	CreatedAt time.Time
	UpdatedAt time.Time
	// TokenUsage 可选，仅 assistant 消息需要。
	TokenUsage *model.TokenUsageUpdate
}

// CreateMessage 在事务内创建消息（调用方必须在 RunAndPublish 回调内）。
// Creator 写入新消息的 CreatorKind/CreatorRefID 字段（T2 加的列）。
// 内部调用 AllocateOffsets，调用方无需预分配 offset。
// turnID 为 nil 时表示消息不属于任何 Turn（TurnID/TurnOffset 字段留空）。
// parentMessageID 为 nil 时无父消息，非 nil 时设置 ParentMessageID（用于 toolcall_output 指向 toolcall_input）。
// content 参数为 ContentData 结构，会序列化为 JSON 存储到数据库 Content 字段。
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

	// client_id 有唯一约束。客户端消息由调用方传入保证幂等；
	// 系统生成的消息（assistant / system）调用方传空串，此处自动生成 UUID，
	// 避免多条系统消息的 client_id 同为空串触发唯一约束冲突。
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

// BatchCreateMessages 批量创建消息（一次 Redis + 一次 DB）。
// 所有消息共享相同的 sessionID 和 turnID。
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

	// 一次 Redis 调用分配所有 offsets
	startGlobalOffset, startTurnOffset, err := AllocateOffsets(txCtx, deps, sessionID, turnID, count)
	if err != nil {
		return nil, fmt.Errorf("allocate offsets: %w", err)
	}

	// 构造消息列表
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

		// 使用传入的时间戳，零值则使用当前时间
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

	// 一次 DB 批量插入
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
