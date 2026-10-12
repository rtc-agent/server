// internal/handler/rpc/action.sendmessage.go
package rpchandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/rtc-agent/server/internal/agent"
	"github.com/rtc-agent/server/internal/converter"
	"github.com/rtc-agent/server/internal/infra/contextx"
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
// Supports two content types:
//   - ContentTypeText: raw text content (used for simple text messages)
//   - ContentTypeUserMessage: structured user message with metadata (used for rich messages)
//
// Returns empty string for unsupported types or parsing errors.
// Errors are logged but not propagated to avoid blocking the message send flow.
func extractMessageContent(ctx context.Context, cd protocol.ContentData) string {
	switch cd.Type {
	case protocol.ContentTypeText:
		s, err := primitives.ContentDataString(cd.Data)
		if err != nil {
			logger.Error(ctx, "rpc.send_message.content_data_string_failed", zap.Error(err))
			return ""
		}
		return s
	case protocol.ContentTypeUserMessage:
		umc, err := primitives.ParseUserMessageContent(cd.Data)
		if err != nil {
			logger.Error(ctx, "rpc.send_message.parse_user_message_failed", zap.Error(err))
			return ""
		}
		return umc.Text
	default:
		return ""
	}
}

// summarizeTitleAsync runs title summarization in a background goroutine.
func (h *Handler) summarizeTitleAsync(ctx context.Context, session *converter.Session) {
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
	logger.SafeGo("rpc.send_message.title_summarize", func() {
		summarizeCtx, cancel := context.WithTimeout(detachedCtx, 30*time.Second)
		defer cancel()
		title, err := summarizer.SummarizeIfNeeded(summarizeCtx, session.ID)
		if err != nil || title == "" {
			logger.Warn(summarizeCtx, "rpc.send_message.title_summarize_failed",
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
			logger.Warn(updateCtx, "rpc.send_message.update_title_failed",
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

	logger.Info(ctx, "rpc.send_message",
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

	session, isNew, err := h.prepareSession(ctx, req, sessionUUIDPtr, creator, content, span)
	if err != nil {
		return nil, err
	}

	workID := uuid.Must(uuid.NewV7()).String()

	// Create message and publish work
	createdMessage, pushUpdates, err := h.createMessageAndPublishWork(ctx, req, session, isNew, creator, userID.String(), workID, span)
	if err != nil {
		// Check if this is a sync-required error (push failed after commit)
		var syncRequiredErr *ErrPushAfterCommitSyncRequired
		if errors.As(err, &syncRequiredErr) {
			// Data was committed successfully, but push failed
			// Return success with sync_required flag set
			syncRequired := true
			return &protocol.SendMessageResponse{
				Result: protocol.SendMessageResult{
					SessionId:    session.ID.String(),
					TurnId:       "",
					MessageId:    createdMessage.ID.String(),
					SyncRequired: &syncRequired,
				},
				Updates: updates.DerefUpdates(pushUpdates),
			}, nil
		}
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
	// Validate server_session_id UUID format if present
	if req.ServerSessionId != nil {
		if _, apiErr := parseUUID(*req.ServerSessionId, "server_session_id"); apiErr != nil {
			span.SetStatus(codes.Error, apiErr.Code)
			return "", apiErr
		}
	}

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

	// Validate agent_prompt length early to avoid wasting DB queries on invalid requests.
	// Default: 100KB. Configurable via ConfigProvider key "api.max_agent_prompt_bytes".
	maxAgentPromptLength := 100 * 1024 // 100KB default
	if h.deps.Deps.ConfigProvider != nil {
		if v, err := h.deps.Deps.ConfigProvider.GetEffectiveInt(ctx, "api.max_agent_prompt_bytes", &userID); err == nil && v > 0 {
			maxAgentPromptLength = v
		}
	}
	if req.AgentPrompt != nil && len(*req.AgentPrompt) > maxAgentPromptLength {
		span.SetStatus(codes.Error, "validation_error")
		return "", &APIError{
			Code:    ErrorCodeInvalidArgument,
			Message: fmt.Sprintf("agent_prompt too long: %d bytes (max %d)", len(*req.AgentPrompt), maxAgentPromptLength),
		}
	}

	if apiErr := h.checkClientIdIdempotency(ctx, req.ClientId); apiErr != nil {
		span.SetStatus(codes.Error, apiErr.Message)
		return "", apiErr
	}

	return content, nil
}

// prepareSession prepares the session for message creation.
// content is the extracted text content from ContentData, passed in to avoid redundant parsing.
func (h *Handler) prepareSession(ctx context.Context, req *protocol.SendMessageRequest, sessionUUIDPtr *uuid.UUID, creator usecase.UserCreator, content string, span trace.Span) (*converter.Session, bool, error) {
	initialTitle := primitives.TruncateTitle(content, 50)
	session, isNew, err := primitives.PrepareSession(ctx, h.deps.Deps, sessionUUIDPtr, req.ClientSessionId, creator, initialTitle)
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		return nil, false, h.internalError(ctx, "session.error", "internal error", err)
	}

	span.SetAttributes(attribute.String("session.id", session.ID.String()))
	if isNew && req.AgentPrompt != nil {
		// Validation already done in validateSendMessageRequest, safe to assign directly
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
func (h *Handler) createMessageAndPublishWork(ctx context.Context, req *protocol.SendMessageRequest, session *converter.Session, isNew bool, creator usecase.UserCreator, userID string, workID string, span trace.Span) (*converter.Message, []*protocol.Update, error) {
	var createdMessage *converter.Message
	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := h.touchOrCreateSession(txCtx, session, isNew); err != nil {
			return nil, err
		}

		// Create prompt message if needed (before user message to ensure correct global_offset).
		promptMsg := h.createPromptMessageIfNeeded(txCtx, ctx, req, session.ID)

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
			return nil, h.internalError(txCtx, ErrorCodeSendCreateMessage, "failed to create message", err)
		}
		createdMessage = msg

		// Record message sent metrics
		h.deps.Metrics.RecordMessageSent(txCtx, string(protocol.MessageRoleUser))

		if err := h.publishSubmitWork(txCtx, session.ID, userID); err != nil {
			return nil, err
		}
		if logger.IsDebugMode() {
			logger.Debug(txCtx, "rpc.send_message.queue_publish_success",
				zap.String("session", session.ID.String()),
				zap.String("message", msg.ID.String()),
				zap.String("work_id", workID))
		}

		// Collect message IDs for updates (prompt message + user message).
		// Pre-allocate with capacity for prompt + user message to avoid reallocation.
		messageIDs := make([]uuid.UUID, 0, 2)
		if promptMsg != nil {
			messageIDs = append(messageIDs, promptMsg.ID)
		}
		messageIDs = append(messageIDs, msg.ID)

		return primitives.BuildSendMessageUpdates(session, isNew, nil, messageIDs), nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "rpc.send_message.push_after_commit_failed", zap.Error(err))
			// Mark that sync is required so the client knows to fetch latest state
			// We'll set this in the response via a wrapper
			return createdMessage, pushUpdates, &ErrPushAfterCommitSyncRequired{OriginalError: err}
		} else if apiErr, ok := err.(*APIError); ok {
			// Already a structured API error from inner handler — pass through.
			return nil, nil, apiErr
		} else {
			span.SetStatus(codes.Error, "internal_error")
			span.RecordError(err)
			return nil, nil, h.internalError(ctx, "send.error", "internal error", err)
		}
	}

	return createdMessage, pushUpdates, nil
}

// ErrPushAfterCommitSyncRequired wraps ErrPushAfterCommit to signal that sync_required should be set
type ErrPushAfterCommitSyncRequired struct {
	OriginalError error
}

func (e *ErrPushAfterCommitSyncRequired) Error() string {
	return e.OriginalError.Error()
}

func (e *ErrPushAfterCommitSyncRequired) Unwrap() error {
	return e.OriginalError
}

// touchOrCreateSession touches or creates the session within the transaction.
func (h *Handler) touchOrCreateSession(txCtx context.Context, session *converter.Session, isNew bool) error {
	if isNew {
		if err := primitives.CreateSession(txCtx, h.deps.Deps, session); err != nil {
			return h.internalError(txCtx, ErrorCodeSendCreateSession, "failed to create session", err)
		}
		// Record session creation metric
		h.deps.Metrics.RecordSessionCreated(txCtx)
	} else {
		if err := primitives.TouchSession(txCtx, h.deps.Deps, session.ID); err != nil {
			if errors.Is(err, primitives.ErrSessionClosedOrNotFound) {
				return &APIError{Code: ErrorCodeSessionClosed, Message: "session is closed"}
			}
			return h.internalError(txCtx, ErrorCodeSendTouchSession, "failed to touch session", err)
		}
	}
	return nil
}

// createPromptMessageIfNeeded creates a prompt message if the content requires it.
// Returns nil if no prompt message is needed or if creation fails (degrades gracefully).
func (h *Handler) createPromptMessageIfNeeded(txCtx context.Context, ctx context.Context, req *protocol.SendMessageRequest, sessionID uuid.UUID) *converter.Message {
	if !needsPromptMessage(req.ContentData) {
		return nil
	}

	promptContent, buildErr := buildPromptContent(req.ContentData)
	if buildErr != nil {
		logger.Warn(ctx, "rpc.send_message.build_prompt_failed", zap.Error(buildErr))
		// Degrade gracefully: continue without prompt message.
		return nil
	}

	if promptContent.Type != protocol.ContentTypePrompt {
		return nil
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
		// Degrade gracefully: the user message is the primary payload; a prompt
		// message is supplementary. Log the failure and continue without it so
		// that a transient DB error does not abort the whole SendMessage flow.
		logger.Warn(ctx, "rpc.send_message.create_prompt_failed", zap.Error(createErr))
		return nil
	}

	logger.Info(ctx, "rpc.send_message.prompt_message_created",
		zap.String("prompt_msg_id", promptMsg.ID.String()))

	return promptMsg
}

// checkClientIdIdempotency checks if a client_id was already used.
func (h *Handler) checkClientIdIdempotency(ctx context.Context, clientID string) *APIError {
	if clientID == "" {
		return nil
	}
	// Validate length上限，防止过长 ID 导致数据库查询性能问题
	const maxClientIDLength = 255
	if len(clientID) > maxClientIDLength {
		return &APIError{
			Code:    ErrorCodeInvalidArgument,
			Message: fmt.Sprintf("client_id too long: %d bytes (max %d)", len(clientID), maxClientIDLength),
		}
	}
	existing, err := h.deps.Deps.RtcSessionUsecase.FindMessageByClientID(ctx, clientID)
	if err != nil {
		logger.Error(ctx, "idempotency check failed",
			zap.String("client_id", clientID),
			zap.Error(err))
		return &APIError{Code: ErrorCodeInternalError, Message: "idempotency check failed"}
	}
	if existing != nil {
		return &APIError{
			Code:    ErrorCodeMessageClientIDConflict,
			Message: "client_id already used",
		}
	}
	return nil
}

// publishSubmitWork publishes a submit work item inside the transaction.
// Uses exponential backoff with retry for transient Redis errors.
// Implements idempotent publishing with deterministic work IDs to prevent
// duplicate work items on retries.
func (h *Handler) publishSubmitWork(txCtx context.Context, sessionID uuid.UUID, userID string) error {
	if h.deps.Queue == nil {
		logger.Error(txCtx, "rpc.send_message.queue_uninitialized",
			zap.String("session", sessionID.String()))
		return h.internalError(txCtx, ErrorCodeSendQueueUninit, "queue not initialized", fmt.Errorf("queue dependency is nil"))
	}

	// Generate deterministic work ID based on session ID and current timestamp.
	// This ensures retries of the same logical work produce the same work ID,
	// preventing duplicate work items from being created.
	// Note: We use session ID + timestamp because message ID is not yet available
	// at this point (work is published before message creation completes).
	// The timestamp provides uniqueness across different SendMessage calls.
	workID := generateDeterministicWorkIDForSend(sessionID)

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
		return h.internalError(txCtx, ErrorCodeSendMarshalPayload, "failed to marshal work payload", err)
	}

	// Use exponential backoff with a maximum elapsed time to prevent
	// indefinite retry attempts when Redis is persistently unavailable.
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 100 * time.Millisecond // Faster initial retry (was 500ms default)
	bo.MaxInterval = 2 * time.Second
	bo.Multiplier = 2.0

	attempt := 0
	startTime := time.Now()

	_, err = backoff.Retry(txCtx, func() (string, error) {
		attempt++

		// Check if client has already timed out
		if txCtx.Err() != nil {
			logger.Error(txCtx, "rpc.send_message.queue_publish_client_timeout",
				zap.String("session", sessionID.String()),
				zap.Int("attempt", attempt),
				zap.Duration("elapsed", time.Since(startTime)))
			return "", backoff.Permanent(txCtx.Err())
		}

		// Use PublishWithID for idempotent publishing
		publishWorkID, publishErr := h.deps.Queue.PublishWithID(txCtx, sessionID.String(), workID, string(payload), rtcqueue.SubmitWorkPriority)
		if publishErr != nil {
			if !isTransientRedisError(publishErr) {
				return "", backoff.Permanent(publishErr)
			}
			logger.Warn(txCtx, "rpc.send_message.queue_publish_retry",
				zap.String("session", sessionID.String()),
				zap.String("work_id", publishWorkID),
				zap.Int("attempt", attempt),
				zap.Duration("elapsed", time.Since(startTime)),
				zap.Error(publishErr))
			return "", publishErr
		}

		logger.Info(txCtx, "rpc.send_message.queue_published",
			zap.String("session", sessionID.String()),
			zap.String("work_id", publishWorkID),
			zap.Int("attempts", attempt),
			zap.Duration("elapsed", time.Since(startTime)))
		return publishWorkID, nil
	},
		backoff.WithBackOff(bo),
		backoff.WithMaxTries(3),
		backoff.WithMaxElapsedTime(5*time.Second), // Reduced from 10s to 5s
	)
	if err != nil {
		return h.internalError(txCtx, ErrorCodeSendQueuePublish, "failed to publish to queue", err)
	}
	return nil
}

// generateDeterministicWorkIDForSend generates a deterministic work ID for
// SendMessage operations. Uses session ID and current timestamp to ensure
// uniqueness across different calls while allowing retries to produce the
// same ID.
//
// Note: This is a pragmatic solution. Ideally we would use message ID, but
// the work is published before message creation completes. The timestamp
// provides sufficient uniqueness for our use case.
func generateDeterministicWorkIDForSend(sessionID uuid.UUID) string {
	// Use microsecond precision timestamp for uniqueness
	timestamp := time.Now().UnixMicro()
	uniqueKey := fmt.Sprintf("send:%d", timestamp)
	return rtcqueue.GenerateDeterministicWorkID(sessionID.String(), uniqueKey)
}

// isTransientRedisError reports whether the error is a transient Redis failure
// that is safe to retry.
//
// Detection strategy:
//   - "connection reset" / "i/o timeout": Network-level errors from the Redis client.
//     These indicate temporary connectivity issues that typically resolve on retry.
//   - "NOSCRIPT": Redis Lua script not loaded (e.g., after a failover or restart).
//     The script will be re-loaded on the next EVALSHA call, making this safe to retry.
//   - "BUSY": Redis is currently running another script. The operation can be retried
//     after a short delay.
//   - "CLUSTERDOWN": Redis Cluster is down or reconfiguring. Clients should retry
//     after a brief backoff.
//   - "TRYAGAIN": Redis Cluster is redirecting the client to a different node.
//     This is a transient state that resolves on retry.
//
// Context cancellation errors (context.Canceled, context.DeadlineExceeded) are
// explicitly excluded because retrying a cancelled operation is pointless.
func isTransientRedisError(err error) bool {
	if err == nil {
		return false
	}
	// Exclude context cancellation errors - retrying is pointless
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "NOSCRIPT") ||
		strings.Contains(msg, "BUSY") ||
		strings.Contains(msg, "CLUSTERDOWN") ||
		strings.Contains(msg, "TRYAGAIN")
}
