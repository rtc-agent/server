// internal/handler/rpc/action.sendmessage.go
package rpchandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/agent"
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

// extractMessageContent extracts text content from ContentData.
func extractMessageContent(cd protocol.ContentData) string {
	switch cd.Type {
	case protocol.ContentTypeText:
		s, err := primitives.ContentDataString(cd.Data)
		if err != nil {
			logger.Warn(context.Background(), "[SendMessage] ContentDataString failed", zap.Error(err))
			return ""
		}
		return s
	case protocol.ContentTypeUserMessage:
		umc, err := primitives.ParseUserMessageContent(cd.Data)
		if err != nil {
			logger.Warn(context.Background(), "[SendMessage] ParseUserMessageContent failed", zap.Error(err))
			return ""
		}
		return umc.Text
	default:
		return ""
	}
}

// summarizeTitleAsync runs title summarization in a background goroutine.
func (h *Handler) summarizeTitleAsync(ctx context.Context, session *model.Session) {
	if h.deps.Deps.ChatModel == nil {
		return
	}
	summarizer := agent.NewSessionTitleSummarizer(
		h.deps.Deps.ChatModel,
		h.deps.Deps.SessionRepo,
		h.deps.Deps.MessageRepo,
		h.deps.Deps.LLMConfig,
		h.deps.Deps.TokenCallbackHandler,
	)
	detachedCtx := context.WithoutCancel(ctx)
	logger.SafeGo("sendmessage.titleSummarize", func() {
		summarizeCtx, cancel := context.WithTimeout(detachedCtx, 30*time.Second)
		defer cancel()
		title, err := summarizer.SummarizeIfNeeded(summarizeCtx, session.ID)
		if err != nil || title == "" {
			logger.Warn(summarizeCtx, "[SendMessage] title summarization failed",
				zap.String("session", session.ID.String()), zap.Error(err))
			return
		}
		if _, err := h.UpdateSession(detachedCtx, &protocol.UpdateSessionRequest{
			SessionId: session.ID.String(),
			Title:     &title,
		}); err != nil {
			logger.Warn(detachedCtx, "[SendMessage] UpdateSession title failed",
				zap.String("session", session.ID.String()), zap.Error(err))
		}
	})
}

// SendMessage sends a message (auto-creates session + turn).
func (h *Handler) SendMessage(ctx context.Context, req *protocol.SendMessageRequest) (*protocol.SendMessageResponse, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	deviceID, _ := contextx.GetDeviceID(ctx)
	creator := usecase.UserCreator{UserID: userID, DeviceID: deviceID}

	content := extractMessageContent(req.ContentData)

	if err := primitives.ValidateCreateMessageRequest(content); err != nil {
		return nil, &APIError{Code: "invalid_argument", Message: err.Error()}
	}

	sessionUUIDPtr, apiErr := parseUUIDPtr(req.ServerSessionId, "server_session_id")
	if apiErr != nil {
		return nil, apiErr
	}

	if apiErr := h.checkClientIdIdempotency(ctx, req.ClientId); apiErr != nil {
		return nil, apiErr
	}

	logger.Info(ctx, "[SendMessage]",
		zap.String("user", userID.String()),
		zap.String("device", deviceID),
		zap.Int("content_len", len(content)),
		zap.String("session_id", req.ClientSessionId),
		zap.String("client_id", req.ClientId))

	initialTitle := primitives.TruncateTitle(content, 50)
	session, isNew, err := primitives.PrepareSession(ctx, h.deps.Deps, sessionUUIDPtr, req.ClientSessionId, creator, initialTitle)
	if err != nil {
		return nil, h.internalError(ctx, "session.error", "internal error", err)
	}
	if isNew && req.AgentPrompt != nil {
		session.AgentPrompt = *req.AgentPrompt
	}
	if !isNew {
		if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, session.ID, creator); err != nil {
			return nil, h.ownershipError(ctx, err)
		}
	}

	workID := uuid.Must(uuid.NewV7()).String()

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

		// Create message WITHOUT a turnID — the turn is created by turn-agent
		// when the worker processes the work item published below.
		msg, err := primitives.CreateMessage(
			txCtx, h.deps.Deps, session.ID, nil,
			protocol.MessageRoleUser, creator,
			req.ContentData, protocol.MessageStreamingPending,
			req.ClientId,
			nil,
		)
		if err != nil {
			return nil, fmt.Errorf("create message: %w", err)
		}
		createdMessage = msg

		if err := h.publishSubmitWork(txCtx, session.ID); err != nil {
			return nil, err
		}
		if logger.IsDebugMode() {
			logger.Debug(txCtx, "[SendMessage] Queue.Publish success",
				zap.String("session", session.ID.String()),
				zap.String("message", msg.ID.String()),
				zap.String("work_id", workID))
		}

		return primitives.BuildSendMessageUpdates(session, isNew, nil, msg.ID), nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "[SendMessage] push failed after commit (data safe)", zap.Error(err))
		} else {
			return nil, h.internalError(ctx, "send.error", "internal error", err)
		}
	}

	if isNew {
		h.summarizeTitleAsync(ctx, session)
	}

	return &protocol.SendMessageResponse{
		Result: protocol.SendMessageResult{
			SessionId: session.ID.String(),
			TurnId:    "",
			MessageId: createdMessage.ID.String(),
		},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}

// checkClientIdIdempotency checks if a client_id was already used.
func (h *Handler) checkClientIdIdempotency(ctx context.Context, clientID string) *APIError {
	if clientID == "" {
		return nil
	}
	existing, err := h.deps.Deps.MessageRepo.FindByClientID(ctx, clientID)
	if err != nil {
		return &APIError{Code: "internal_error", Message: "idempotency check failed"}
	}
	if existing != nil {
		return &APIError{
			Code:    "client_id_conflict",
			Message: fmt.Sprintf("client_id %s already used for message %s", clientID, existing.ID),
		}
	}
	return nil
}

// publishSubmitWork publishes a submit work item inside the transaction.
func (h *Handler) publishSubmitWork(txCtx context.Context, sessionID uuid.UUID) error {
	if h.deps.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: sessionID.String(),
	})
	if err != nil {
		return fmt.Errorf("marshal work payload: %w", err)
	}
	if _, err := h.deps.Queue.Publish(txCtx, sessionID.String(), string(payload), 0); err != nil {
		return fmt.Errorf("queue publish: %w", err)
	}
	return nil
}
