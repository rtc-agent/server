// internal/rpchandler/action.forksession.go
package rpchandler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ForkSession 分叉对话：基于旧 session 创建新 session，批量复制消息并替换指定消息。
//
// 业务逻辑：
//   - 从 old_server_message_id 开始往前查询最多 limit 条消息
//   - 创建新 session，批量复制消息到新 session
//   - 最后一条消息用新的 content_data 替换
//   - 触发 AI 流程（通过 rtc-queue Publish）
func (h *Handler) ForkSession(ctx context.Context, req *protocol.ForkSessionRequest) (*protocol.ForkSessionResponse, error) {
	// 1. 获取用户身份
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	deviceID, _ := contextx.GetDeviceID(ctx)
	creator := usecase.UserCreator{UserID: userID, DeviceID: deviceID}

	// 2. 解析 UUID 参数
	oldSessionID, apiErr := parseUUID(req.OldServerSessionId, "old_server_session_id")
	if apiErr != nil {
		return nil, apiErr
	}
	oldMessageID, apiErr := parseUUID(req.OldServerMessageId, "old_server_message_id")
	if apiErr != nil {
		return nil, apiErr
	}

	// 3. 校验 limit 参数（默认200，最多1000）
	limit := 200
	if req.Limit != nil {
		limit = *req.Limit
		if limit > 1000 {
			limit = 1000
		}
		if limit < 1 {
			limit = 1
		}
	}

	logger.Info(ctx, "[ForkSession] start",
		zap.String("user", userID.String()),
		zap.String("old_session", oldSessionID.String()),
		zap.String("old_message", oldMessageID.String()),
		zap.Int("limit", limit))

	// 4. 校验旧 session 归属
	oldSession, err := h.deps.SessionRepo.GetByID(ctx, oldSessionID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{Code: "session.not_found", Message: fmt.Sprintf("old session %s not found", req.OldServerSessionId)}
		}
		return nil, h.internalError(ctx, "session.error", "internal error", err)
	}
	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, oldSessionID, creator); err != nil {
		return nil, h.ownershipError(ctx, err)
	}

	// 5. 校验旧消息存在并获取其 offset
	oldMessage, err := h.deps.Deps.MessageRepo.GetByID(ctx, oldMessageID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{Code: "message.not_found", Message: fmt.Sprintf("old message %s not found", req.OldServerMessageId)}
		}
		return nil, h.internalError(ctx, "message.error", "internal error", err)
	}

	// 6. 查询旧消息（从 oldMessage 往前最多 limit 条）
	oldMessages, err := h.deps.Deps.MessageRepo.ListBySessionBeforeOffset(ctx, oldSessionID, oldMessage.GlobalOffset, limit)
	if err != nil {
		return nil, h.internalError(ctx, "message.error", "internal error", err)
	}
	if len(oldMessages) == 0 {
		return nil, &APIError{Code: "message.not_found", Message: "no messages found to fork"}
	}

	// 7. 构造新 session
	newSession := &model.Session{
		ID:         uuid.Must(uuid.NewV7()),
		ClientID:   string(req.NewClientSessionId),
		OwnerKind:  string(creator.Kind()),
		OwnerRefID: creator.ReferenceID(),
		Title:      oldSession.Title, // 继承旧 session 标题
		Status:     string(protocol.SessionStatusActive),
		DeviceID:   deviceID,
	}

	// Generate a workID for debug tracing. Do NOT pre-create the turn — it
	// is created by turn-agent's CreateTurn callback when the worker picks
	// up the work item from rtc-queue. The turn lifecycle is managed by
	// turn-agent, not the API layer.
	workID := uuid.New().String()

	// 8. 构造批量消息列表
	messagesToCreate := make([]primitives.MessageToCreate, len(oldMessages))
	for i, oldMsg := range oldMessages {
		if i == len(oldMessages)-1 {
			// 最后一条：用新内容替换（使用当前时间）
			messagesToCreate[i] = primitives.MessageToCreate{
				Role:     protocol.MessageRoleUser,
				Creator:  creator,
				Content:  req.ContentData,
				Status:   protocol.MessageStreamingPending,
				ClientID: req.NewClientMessageId,
			}
		} else {
			// 复制旧消息（保留原始时间戳）
			content, _ := primitives.ParseContentData(oldMsg.Content)
			messagesToCreate[i] = primitives.MessageToCreate{
				Role:      protocol.MessageRole(oldMsg.Role),
				Creator:   creator,
				Content:   content,
				Status:    protocol.MessageStreamingStatus(oldMsg.StreamingStatus),
				ClientID:  "", // 系统生成新 client_id
				CreatedAt: oldMsg.CreatedAt,
				UpdatedAt: oldMsg.UpdatedAt,
			}
		}
	}

	// 9. 事务内创建 session + 批量创建消息 + 发布 Queue work item。
	//
	// Do NOT pre-create the turn here. The turn is created by turn-agent's
	// CreateTurn callback when the worker picks up the work item from
	// rtc-queue. Messages are created without a turnID (nil) — they are
	// associated with the session only.
	var createdMessages []*model.Message
	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		// 创建新 session
		if err := primitives.CreateSession(txCtx, h.deps.Deps, newSession); err != nil {
			return nil, fmt.Errorf("create session: %w", err)
		}

		// 批量创建消息（一次 Redis + 一次 DB）。
		// turnID is nil — the turn does not exist yet. It will be created
		// asynchronously by turn-agent when the worker processes the work
		// item published below.
		var err error
		createdMessages, err = primitives.BatchCreateMessages(txCtx, h.deps.Deps, newSession.ID, nil /* no turnID */, messagesToCreate)
		if err != nil {
			return nil, fmt.Errorf("batch create messages: %w", err)
		}

		// Queue.Publish as the LAST step inside the transaction.
		// If Publish fails, the transaction is rolled back — no orphan
		// messages without a work item. If Commit fails after Publish
		// succeeds, a reconciliation job cleans up the orphan work item.
		if h.deps.Queue != nil {
			payload, _ := json.Marshal(turnagent.WorkPayload{
				Kind:      turnagent.WorkKindSubmit,
				SessionID: newSession.ID.String(),
			})
			if _, err := h.deps.Queue.Publish(txCtx, newSession.ID.String(), string(payload), 0); err != nil {
				return nil, fmt.Errorf("queue publish: %w", err)
			}
			if logger.DebugMode {
				logger.Debug(txCtx, "[ForkSession] Queue.Publish success",
					zap.String("session", newSession.ID.String()),
					zap.String("work_id", workID))
			}
		}

		return primitives.BuildForkSessionUpdates(newSession, createdMessages), nil
	})
	if err != nil {
		return nil, h.internalError(ctx, "fork.error", "internal error", err)
	}

	// 11. 构建响应
	messageIDs := make([]protocol.UUID, len(createdMessages))
	for i, msg := range createdMessages {
		messageIDs[i] = protocol.UUID(msg.ID.String())
	}

	return &protocol.ForkSessionResponse{
		Result: protocol.ForkSessionResult{
			SessionId: protocol.UUID(newSession.ID.String()),
			// TurnId is empty — the turn is created asynchronously by
			// turn-agent when the worker picks up the work item.
			TurnId:     protocol.UUID(""),
			MessageIds: messageIDs,
		},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}
