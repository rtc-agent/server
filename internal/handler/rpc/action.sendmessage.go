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
	if h.deps.Deps.ChatModel == nil && h.deps.Deps.ChatModelFactory == nil {
		return
	}
	// Use dynamic config for title summarization when available.
	chatModel := agent.ResolveChatModel(ctx, h.deps.Deps)
	summarizer := agent.NewSessionTitleSummarizer(
		chatModel,
		h.deps.Deps.SessionRepo,
		h.deps.Deps.MessageRepo,
		h.deps.Deps.ConfigProvider,
		h.deps.Deps.LLMConfig.Provider,
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
	ctx, span := otel.GetTracerProvider().Tracer("rpc").Start(ctx, "rpc.send_message",
		trace.WithAttributes(
			attribute.String("client.session.id", req.ClientSessionId),
			attribute.String("client.id", req.ClientId),
		),
	)
	defer span.End()

	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		span.SetStatus(codes.Error, "missing user_id in context")
		return nil, &APIError{Code: ErrorCodeUnauthorized, Message: "missing user_id in context"}
	}
	span.SetAttributes(attribute.String("user.id", userID.String()))
	deviceID, _ := contextx.GetDeviceID(ctx)
	creator := usecase.UserCreator{UserID: userID, DeviceID: deviceID}

	// Validate request and extract content
	content, apiErr := h.validateSendMessageRequest(ctx, req, userID, span)
	if apiErr != nil {
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

	// Prepare session
	sessionUUIDPtr, apiErr := parseUUIDPtr(req.ServerSessionId, "server_session_id")
	if apiErr != nil {
		span.SetStatus(codes.Error, apiErr.Message)
		return nil, apiErr
	}

	session, isNew, err := h.prepareSession(ctx, req, sessionUUIDPtr, creator, span)
	if err != nil {
		return nil, err
	}

	workID := uuid.Must(uuid.NewV7()).String()

	// Create message and publish work
	createdMessage, pushUpdates, err := h.createMessageAndPublishWork(ctx, req, session, isNew, creator, userID.String(), workID, span)
	if err != nil {
		return nil, err
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

// validateSendMessageRequest validates the SendMessage request and extracts content.
func (h *Handler) validateSendMessageRequest(ctx context.Context, req *protocol.SendMessageRequest, userID uuid.UUID, span trace.Span) (string, *APIError) {
	// Validate file attachments exist before creating the message.
	if apiErr := h.validateFileAttachments(ctx, req.ContentData, userID, "SendMessage"); apiErr != nil {
		span.SetStatus(codes.Error, apiErr.Code)
		return "", apiErr
	}

	// Validate content type: only user-facing types are allowed.
	if req.ContentData.Type != protocol.ContentTypeText &&
		req.ContentData.Type != protocol.ContentTypeUserMessage {
		span.SetStatus(codes.Error, "validation_error")
		return "", &APIError{
			Code:    ErrorCodeInvalidArgument,
			Message: "invalid content_data type for user message",
		}
	}

	content := extractMessageContent(ctx, req.ContentData)

	if err := primitives.ValidateCreateMessageRequest(content); err != nil {
		span.SetStatus(codes.Error, "validation_error")
		return "", &APIError{Code: ErrorCodeInvalidArgument, Message: "Message content validation failed"}
	}

	if apiErr := h.checkClientIdIdempotency(ctx, req.ClientId); apiErr != nil {
		span.SetStatus(codes.Error, apiErr.Message)
		return "", apiErr
	}

	return content, nil
}

// prepareSession prepares the session for message creation.
func (h *Handler) prepareSession(ctx context.Context, req *protocol.SendMessageRequest, sessionUUIDPtr *uuid.UUID, creator usecase.UserCreator, span trace.Span) (*model.Session, bool, error) {
	content := extractMessageContent(ctx, req.ContentData)
	initialTitle := primitives.TruncateTitle(content, 50)
	session, isNew, err := primitives.PrepareSession(ctx, h.deps.Deps, sessionUUIDPtr, req.ClientSessionId, creator, initialTitle)
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		return nil, false, h.internalError(ctx, "session.error", "internal error", err)
	}

	const maxAgentPromptLength = 10 * 1024 // 10KB
	span.SetAttributes(attribute.String("session.id", session.ID.String()))
	if isNew && req.AgentPrompt != nil {
		if len(*req.AgentPrompt) > maxAgentPromptLength {
			span.SetStatus(codes.Error, "validation_error")
			return nil, false, &APIError{
				Code:    ErrorCodeInvalidArgument,
				Message: fmt.Sprintf("agent_prompt too long: %d bytes (max %d)", len(*req.AgentPrompt), maxAgentPromptLength),
			}
		}
		session.AgentPrompt = *req.AgentPrompt
	}
	if !isNew {
		if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, session.ID, creator); err != nil {
			span.SetStatus(codes.Error, "permission_denied")
			return nil, false, h.ownershipError(ctx, err)
		}
	}

	return session, isNew, nil
}

// createMessageAndPublishWork creates the message and publishes work to the queue.
func (h *Handler) createMessageAndPublishWork(ctx context.Context, req *protocol.SendMessageRequest, session *model.Session, isNew bool, creator usecase.UserCreator, userID string, workID string, span trace.Span) (*model.Message, []*protocol.Update, error) {
	var createdMessage *model.Message
	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := h.touchOrCreateSession(txCtx, session, isNew); err != nil {
			return nil, err
		}

		// Create prompt message if needed (before user message to ensure correct global_offset).
		promptMsg, err := h.createPromptMessageIfNeeded(txCtx, ctx, req, session.ID)
		if err != nil {
			return nil, err
		}

		// Create message WITHOUT a turnID — the turn is created by turn-agent
		// when the worker processes the work item published below.
		msg, err := primitives.CreateMessage(
			txCtx, h.deps.Deps, session.ID, nil,
			protocol.MessageRoleUser, creator,
			req.ContentData, protocol.MessageStreamingCompleted,
			req.ClientId,
			nil,
		)
		if err != nil {
			return nil, fmt.Errorf("create message failed")
		}
		createdMessage = msg

		// Record message sent metrics
		h.deps.Metrics.RecordMessageSent(txCtx, string(protocol.MessageRoleUser))

		if err := h.publishSubmitWork(txCtx, session.ID, userID); err != nil {
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
			span.SetStatus(codes.Error, "internal_error")
			span.RecordError(err)
			return nil, nil, h.internalError(ctx, "send.error", "internal error", err)
		}
	}

	return createdMessage, pushUpdates, nil
}

// touchOrCreateSession touches or creates the session within the transaction.
func (h *Handler) touchOrCreateSession(txCtx context.Context, session *model.Session, isNew bool) error {
	if isNew {
		if err := primitives.CreateSession(txCtx, h.deps.Deps, session); err != nil {
			return fmt.Errorf("create session failed")
		}
		// Record session creation metric
		h.deps.Metrics.RecordSessionCreated(txCtx)
	} else {
		if err := primitives.TouchSession(txCtx, h.deps.Deps, session.ID); err != nil {
			if errors.Is(err, primitives.ErrSessionClosedOrNotFound) {
				return fmt.Errorf("session is closed")
			}
			return fmt.Errorf("touch session failed")
		}
	}
	return nil
}

// createPromptMessageIfNeeded creates a prompt message if the content requires it.
func (h *Handler) createPromptMessageIfNeeded(txCtx context.Context, ctx context.Context, req *protocol.SendMessageRequest, sessionID uuid.UUID) (*model.Message, error) {
	if !needsPromptMessage(req.ContentData) {
		return nil, nil
	}

	promptContent, buildErr := buildPromptContent(req.ContentData)
	if buildErr != nil {
		logger.Warn(ctx, "[SendMessage] build prompt content failed", zap.Error(buildErr))
		// Degrade gracefully: continue without prompt message.
		return nil, nil
	}

	if promptContent.Type != protocol.ContentTypePrompt {
		return nil, nil
	}

	systemCreator := usecase.SystemCreator{}
	promptMsg, createErr := primitives.CreateMessage(
		txCtx, h.deps.Deps, sessionID, nil,
		protocol.MessageRoleSystem,
		systemCreator,
		promptContent,
		protocol.MessageStreamingCompleted, // Prompt messages are immediately complete.
		"",                                 // Empty client_id; CreateMessage generates UUID.
		nil,
	)
	if createErr != nil {
		return nil, fmt.Errorf("create prompt message failed")
	}

	logger.Info(ctx, "[SendMessage] created prompt message",
		zap.String("prompt_msg_id", promptMsg.ID.String()))

	return promptMsg, nil
}

// checkClientIdIdempotency checks if a client_id was already used.
func (h *Handler) checkClientIdIdempotency(ctx context.Context, clientID string) *APIError {
	if clientID == "" {
		return nil
	}
	existing, err := h.deps.Deps.MessageRepo.FindByClientID(ctx, clientID)
	if err != nil {
		return &APIError{Code: ErrorCodeInternalError, Message: "idempotency check failed"}
	}
	if existing != nil {
		return &APIError{
			Code:    ErrorCodeMessageClientIDConflict,
			Message: fmt.Sprintf("client_id %s already used for message %s", clientID, existing.ID),
		}
	}
	return nil
}

// publishSubmitWork publishes a submit work item inside the transaction.
func (h *Handler) publishSubmitWork(txCtx context.Context, sessionID uuid.UUID, userID string) error {
	if h.deps.Queue == nil {
		logger.Error(txCtx, "[SendMessage] queue is nil, message will not be processed by agent",
			zap.String("session", sessionID.String()))
		return fmt.Errorf("queue not initialized")
	}
	// Extract trace context from the transaction context for cross-process propagation.
	traceID, spanID := turnagent.ExtractTraceFromCtx(txCtx)
	payload, err := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: sessionID.String(),
		UserID:    userID,
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
