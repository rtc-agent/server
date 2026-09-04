// internal/rpchandler/action.sendmessage.go
package rpchandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rtc-agent/server/internal/infra/context"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/google/uuid"
)

// SendMessage 发送消息（自动创建 session + turn）。
func (h *Handler) SendMessage(ctx context.Context, req *protocol.SendMessageRequest) (*protocol.SendMessageResponse, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	deviceID, _ := contextx.GetDeviceID(ctx)
	creator := usecase.UserCreator{UserID: userID, DeviceID: deviceID}

	content := ""
	if req.ContentData.Type == protocol.ContentTypeText {
		content, _ = primitives.ContentDataString(req.ContentData.Data)
	}

	if err := primitives.ValidateCreateMessageRequest(content); err != nil {
		return nil, &APIError{Code: "invalid_argument", Message: err.Error()}
	}

	// 将 protocol.UUID 转换为 uuid.UUID，供 internal函数使用
	sessionUUIDPtr, apiErr := parseUUIDPtr(req.ServerSessionId, "server_session_id")
	if apiErr != nil {
		return nil, apiErr
	}

	// 幂等性检查：如果 client_id 已存在，返回已存在的消息
	if req.ClientId != "" {
		existing, err := h.deps.Deps.MessageRepo.FindByClientID(ctx, req.ClientId)
		if err != nil {
			return nil, &APIError{Code: "idempotency.error", Message: err.Error()}
		}
		if existing != nil {
			// client_id 已存在，返回冲突错误
			return nil, &APIError{
				Code:    "client_id_conflict",
				Message: fmt.Sprintf("client_id %s already used for message %s", req.ClientId, existing.ID),
			}
		}
	}

	logger.Info("[SendMessage] user=%s device=%s content_len=%d session_id=%v client_id=%s",
		userID, deviceID, len(content), req.ClientSessionId, req.ClientId)

	// Debug: trace the full request entry
	if logger.DebugMode {
		logger.Debug("[SendMessage] stack_trace:\n%s", logger.CaptureStack(0))
	}

	initialTitle := primitives.TruncateTitle(content, 50)
	session, isNew, err := primitives.PrepareSession(ctx, h.deps.Deps, sessionUUIDPtr, req.ClientSessionId, creator, initialTitle)
	if err != nil {
		return nil, &APIError{Code: "session.error", Message: err.Error()}
	}
	// 仅对已有 session 做归属校验；新 session 的 owner 由 PrepareSession 按 creator 设置，无需校验
	if !isNew {
		if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, session.ID, creator); err != nil {
			return nil, &APIError{Code: "permission_denied", Message: err.Error()}
		}
	}

	// Generate a workID up front for debug tracing. This is NOT pre-registered
	// as a turn ClientID — the turn is created by turn-agent when the worker
	// picks up the work item. The workID is used only for log correlation.
	workID := uuid.New().String()

	var createdMessage *model.Message
	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if isNew {
			if err := primitives.CreateSession(txCtx, h.deps.Deps, session); err != nil {
				return nil, fmt.Errorf("create session: %w", err)
			}
		} else {
			if err := primitives.TouchSession(txCtx, h.deps.Deps, session.ID); err != nil {
				if errors.Is(err, repo.ErrSessionClosedOrNotFound) {
					return nil, fmt.Errorf("session %s is closed: %w", session.ID, repo.ErrSessionClosed)
				}
				return nil, fmt.Errorf("touch session: %w", err)
			}
		}

		// Create message WITHOUT a turnID. The turn does not exist yet — it
		// will be created by turn-agent when the worker processes the work
		// item published below. Passing nil for turnID leaves the message's
		// TurnID column NULL and TurnOffset unset.
		msg, err := primitives.CreateMessage(
			txCtx, h.deps.Deps, session.ID, nil, /* no turnID — turn not created yet */
			protocol.MessageRoleUser, creator,
			req.ContentData, protocol.MessageStreamingPending,
			req.ClientId,
			nil, // no parent message
		)
		if err != nil {
			return nil, fmt.Errorf("create message: %w", err)
		}
		createdMessage = msg

		// Queue.Publish as the LAST step inside the transaction.
		//
		// Why inside the transaction? Atomicity — if Publish fails we roll
		// back the DB writes (no orphan message without a work item). If
		// Publish succeeds but Commit fails (partial failure), we log and
		// accept the inconsistency: the work item is in the queue but the
		// DB changes are not persisted. A reconciliation job can clean up
		// orphan work items later.
		if h.deps.Queue != nil {
			payload, _ := json.Marshal(turnagent.WorkPayload{
				Kind:      turnagent.WorkKindSubmit,
				SessionID: session.ID.String(),
			})
			if _, err := h.deps.Queue.Publish(txCtx, session.ID.String(), string(payload), 0); err != nil {
				return nil, fmt.Errorf("queue publish: %w", err)
			}
			if logger.DebugMode {
				logger.Debug("[SendMessage] Queue.Publish success: session=%s message=%s work_id=%s",
					session.ID, msg.ID, workID)
			}
		}

		// No turnID to report — the turn will be created asynchronously by
		// turn-agent. Pass nil so BuildSendMessageUpdates skips the turn
		// "created" update.
		return primitives.BuildSendMessageUpdates(session, isNew, nil, msg.ID), nil
	})
	if err != nil {
		return nil, &APIError{Code: "send.error", Message: err.Error()}
	}

	// TODO: Title summarization for new sessions. The old code pushed a
	// BackgroundTaskTypeSummarizeTitle to the worker's background stream.
	// In the new architecture this needs a separate mechanism (likely a
	// dedicated rtc-queue priority lane or a standalone background worker).
	// Re-enable once that mechanism is in place.
	// if isNew { ... }

	return &protocol.SendMessageResponse{
		Result: protocol.SendMessageResult{
			SessionId: protocol.UUID(session.ID.String()),
			// TurnId is empty — the turn is created asynchronously by
			// turn-agent after the worker picks up the work item.
			TurnId:    protocol.UUID(""),
			MessageId: protocol.UUID(createdMessage.ID.String()),
		},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}
