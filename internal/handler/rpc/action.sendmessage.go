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

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// extractMessageContent extracts text content from ContentData.
func extractMessageContent(ctx context.Context, cd protocol.ContentData) string {
	switch cd.Type {
	case protocol.ContentTypeText:
		s, err := primitives.ContentDataString(cd.Data)
		if err != nil {
			logger.Warn(ctx, "[SendMessage] ContentDataString failed", zap.Error(err))
			return ""
		}
		return s
	case protocol.ContentTypeUserMessage:
		umc, err := primitives.ParseUserMessageContent(cd.Data)
		if err != nil {
			logger.Warn(ctx, "[SendMessage] ParseUserMessageContent failed", zap.Error(err))
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
		// Use a separate timeout for UpdateSession to prevent goroutine leaks if DB is unresponsive.
		updateCtx, updateCancel := context.WithTimeout(detachedCtx, 10*time.Second)
		defer updateCancel()
		if _, err := h.UpdateSession(updateCtx, &protocol.UpdateSessionRequest{
			SessionId: session.ID.String(),
			Title:     &title,
		}); err != nil {
			logger.Warn(updateCtx, "[SendMessage] UpdateSession title failed",
				zap.String("session", session.ID.String()), zap.Error(err))
		}
	})
}

// SendMessage sends a message (auto-creates session + turn).
func (h *Handler) SendMessage(ctx context.Context, req *protocol.SendMessageRequest) (*protocol.SendMessageResponse, error) {
	ctx, span := otel.GetTracerProvider().Tracer("rpc").Start(ctx, "rpc.sendMessage",
		trace.WithAttributes(
			attribute.String("client.session_id", req.ClientSessionId),
			attribute.String("client.id", req.ClientId),
		),
	)
	defer span.End()

	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		span.SetStatus(codes.Error, "missing user_id in context")
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	deviceID, _ := contextx.GetDeviceID(ctx)
	creator := usecase.UserCreator{UserID: userID, DeviceID: deviceID}

	content := extractMessageContent(ctx, req.ContentData)

	if err := primitives.ValidateCreateMessageRequest(content); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, &APIError{Code: "invalid_argument", Message: err.Error()}
	}

	sessionUUIDPtr, apiErr := parseUUIDPtr(req.ServerSessionId, "server_session_id")
	if apiErr != nil {
		span.SetStatus(codes.Error, apiErr.Message)
		return nil, apiErr
	}

	if apiErr := h.checkClientIdIdempotency(ctx, req.ClientId); apiErr != nil {
		span.SetStatus(codes.Error, apiErr.Message)
		return nil, apiErr
	}

	span.SetAttributes(
		attribute.String("user.id", userID.String()),
		attribute.Int("content.len", len(content)),
	)

	logger.Info(ctx, "[SendMessage]",
		zap.String("user", userID.String()),
		zap.String("device", deviceID),
		zap.Int("content_len", len(content)),
		zap.String("session_id", req.ClientSessionId),
		zap.String("client_id", req.ClientId))

	initialTitle := primitives.TruncateTitle(content, 50)
	session, isNew, err := primitives.PrepareSession(ctx, h.deps.Deps, sessionUUIDPtr, req.ClientSessionId, creator, initialTitle)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, h.internalError(ctx, "session.error", "internal error", err)
	}
	span.SetAttributes(attribute.String("session.id", session.ID.String()))
	if isNew && req.AgentPrompt != nil {
		session.AgentPrompt = *req.AgentPrompt
	}
	if !isNew {
		if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, session.ID, creator); err != nil {
			span.SetStatus(codes.Error, err.Error())
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

		// Create prompt message if needed (before user message to ensure correct global_offset).
		// Prompt messages persist system-level instructions (e.g., scenarios) across turns.
		var promptMsg *model.Message
		if needsPromptMessage(req.ContentData) {
			promptContent, buildErr := buildPromptContent(req.ContentData)
			if buildErr != nil {
				logger.Warn(ctx, "[SendMessage] build prompt content failed", zap.Error(buildErr))
				// Degrade gracefully: continue without prompt message.
			} else if promptContent.Type == protocol.ContentTypePrompt {
				systemCreator := usecase.SystemCreator{}
				var createErr error
				promptMsg, createErr = primitives.CreateMessage(
					txCtx, h.deps.Deps, session.ID, nil,
					protocol.MessageRoleSystem,
					systemCreator,
					promptContent,
					protocol.MessageStreamingCompleted, // Prompt messages are immediately complete.
					"",                                 // Empty client_id; CreateMessage generates UUID.
					nil,
				)
				if createErr != nil {
					return nil, fmt.Errorf("create prompt message: %w", createErr)
				}
				logger.Info(ctx, "[SendMessage] created prompt message",
					zap.String("prompt_msg_id", promptMsg.ID.String()))
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

		// Collect message IDs for updates (prompt message + user message).
		var messageIDs []uuid.UUID
		if promptMsg != nil {
			messageIDs = append(messageIDs, promptMsg.ID)
		}
		messageIDs = append(messageIDs, msg.ID)

		return primitives.BuildSendMessageUpdates(session, isNew, nil, messageIDs), nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "[SendMessage] push failed after commit (data safe)", zap.Error(err))
		} else {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			return nil, h.internalError(ctx, "send.error", "internal error", err)
		}
	}

	span.SetAttributes(
		attribute.Bool("session.is_new", isNew),
		attribute.String("message.id", createdMessage.ID.String()),
	)

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
	// Extract trace context from the transaction context for cross-process propagation.
	traceID, spanID := turnagent.ExtractTraceFromCtx(txCtx)
	payload, err := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: sessionID.String(),
		TraceID:   traceID,
		SpanID:    spanID,
	})
	if err != nil {
		return fmt.Errorf("marshal work payload: %w", err)
	}
	if _, err := h.deps.Queue.Publish(txCtx, sessionID.String(), string(payload), rtcqueue.SubmitWorkPriority); err != nil {
		return fmt.Errorf("queue publish: %w", err)
	}
	return nil
}
