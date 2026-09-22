// internal/rpchandler/action.forksession.go
package rpchandler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// clampForkLimit normalizes the fork limit to [1, 1000].
func clampForkLimit(ptr *int) int {
	const defaultLimit = 200
	if ptr == nil {
		return defaultLimit
	}
	l := *ptr
	if l < 1 {
		return 1
	}
	if l > 1000 {
		return 1000
	}
	return l
}

// buildForkMessages constructs the message list for a fork operation.
// All messages are copied from the old session, except the last one which
// is replaced with the new content.
//
// If newContent carries scenarios, old prompt messages with name="scenarios"
// are filtered out and a fresh prompt message is prepended, preventing stale
// scenario content from persisting in the forked session.
func buildForkMessages(
	oldMessages []*model.Message,
	creator usecase.UserCreator,
	newContent protocol.ContentData,
	newClientMsgID string,
	oldMsgID uuid.UUID,
	ctx context.Context,
) []primitives.MessageToCreate {
	// Determine if new content needs a fresh prompt message.
	needsNewPrompt := needsPromptMessage(newContent)

	// Filter out old prompt messages that would conflict with the new one.
	// When forking with new scenarios, the old "scenarios" prompt is stale.
	var filteredMessages []*model.Message
	for _, oldMsg := range oldMessages {
		if needsNewPrompt {
			contentData, parseErr := primitives.ParseContentData(oldMsg.Content)
			if parseErr == nil && contentData.Type == protocol.ContentTypePrompt {
				pc, pcErr := primitives.ParsePromptContent(contentData.Data)
				if pcErr == nil && pc.Name == "scenarios" {
					// Skip old scenarios prompt — a fresh one will be prepended.
					continue
				}
			}
		}
		filteredMessages = append(filteredMessages, oldMsg)
	}

	result := make([]primitives.MessageToCreate, 0, len(filteredMessages)+1)
	lastIdx := len(filteredMessages) - 1

	// Prepend new prompt message if needed (before all other messages).
	if needsNewPrompt {
		promptContent, buildErr := buildPromptContent(newContent)
		if buildErr != nil {
			logger.Warn(ctx, "[ForkSession] build prompt content failed", zap.Error(buildErr))
		} else if promptContent.Type == protocol.ContentTypePrompt {
			result = append(result, primitives.MessageToCreate{
				Role:    protocol.MessageRoleSystem,
				Creator: usecase.SystemCreator{},
				Content: promptContent,
				Status:  protocol.MessageStreamingCompleted,
			})
		}
	}

	for i, oldMsg := range filteredMessages {
		if i == lastIdx {
			result = append(result, primitives.MessageToCreate{
				Role:     protocol.MessageRoleUser,
				Creator:  creator,
				Content:  newContent,
				Status:   protocol.MessageStreamingPending,
				ClientID: newClientMsgID,
			})
		} else {
			content, parseErr := primitives.ParseContentData(oldMsg.Content)
			if parseErr != nil {
				logger.Warn(ctx, "[ForkSession] ParseContentData failed",
					zap.String("old_message", oldMsgID.String()),
					zap.Error(parseErr))
			}
			tokenUsage := oldMsg.TokenUsage()
			result = append(result, primitives.MessageToCreate{
				Role:       protocol.MessageRole(oldMsg.Role),
				Creator:    creator,
				Content:    content,
				Status:     protocol.MessageStreamingStatus(oldMsg.StreamingStatus),
				ClientID:   "",
				CreatedAt:  oldMsg.CreatedAt,
				UpdatedAt:  oldMsg.UpdatedAt,
				TokenUsage: &tokenUsage,
			})
		}
	}
	return result
}

// ForkSession forks a conversation: creates a new session based on an old one,
// batch-copies messages and replaces a specified message.
//
// Business logic:
//   - Query at most `limit` messages backwards from old_server_message_id
//   - Create a new session and batch-copy messages to it
//   - Replace the last message with the new content_data
//   - Trigger the AI flow (via rtc-queue Publish)
func (h *Handler) ForkSession(ctx context.Context, req *protocol.ForkSessionRequest) (*protocol.ForkSessionResponse, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	deviceID, _ := contextx.GetDeviceID(ctx)
	creator := usecase.UserCreator{UserID: userID, DeviceID: deviceID}

	oldSessionID, apiErr := parseUUID(req.OldServerSessionId, "old_server_session_id")
	if apiErr != nil {
		return nil, apiErr
	}
	oldMessageID, apiErr := parseUUID(req.OldServerMessageId, "old_server_message_id")
	if apiErr != nil {
		return nil, apiErr
	}

	limit := clampForkLimit(req.Limit)

	logger.Info(ctx, "[ForkSession] start",
		zap.String("user", userID.String()),
		zap.String("old_session", oldSessionID.String()),
		zap.String("old_message", oldMessageID.String()),
		zap.Int("limit", limit))

	oldSession, err := h.validateForkSource(ctx, oldSessionID, creator)
	if err != nil {
		return nil, err
	}

	oldMessage, err := h.deps.Deps.MessageRepo.GetByID(ctx, oldMessageID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{Code: "message.not_found", Message: fmt.Sprintf("old message %s not found", req.OldServerMessageId)}
		}
		return nil, h.internalError(ctx, "message.error", "internal error", err)
	}

	oldMessages, err := h.deps.Deps.MessageRepo.ListBySessionBeforeOffset(ctx, oldSessionID, oldMessage.GlobalOffset, limit)
	if err != nil {
		return nil, h.internalError(ctx, "message.error", "internal error", err)
	}
	if len(oldMessages) == 0 {
		return nil, &APIError{Code: "message.not_found", Message: "no messages found to fork"}
	}

	newSession := buildForkSessionModel(oldSession, req.NewClientSessionId, creator, deviceID)
	messagesToCreate := buildForkMessages(oldMessages, creator, req.ContentData, req.NewClientMessageId, oldMessageID, ctx)

	var createdMessages []*model.Message
	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := primitives.CreateSession(txCtx, h.deps.Deps, newSession); err != nil {
			return nil, fmt.Errorf("create session: %w", err)
		}

		// turnID is nil — the turn is created asynchronously by turn-agent.
		createdMessages, err = primitives.BatchCreateMessages(txCtx, h.deps.Deps, newSession.ID, nil, messagesToCreate)
		if err != nil {
			return nil, fmt.Errorf("batch create messages: %w", err)
		}

		if err := h.publishSubmitWork(txCtx, newSession.ID); err != nil {
			return nil, err
		}

		return primitives.BuildForkSessionUpdates(newSession, createdMessages), nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "[ForkSession] push failed after commit (data safe)", zap.Error(err))
		} else {
			return nil, h.internalError(ctx, "fork.error", "internal error", err)
		}
	}

	messageIDs := make([]protocol.UUID, len(createdMessages))
	for i, msg := range createdMessages {
		messageIDs[i] = msg.ID.String()
	}

	return &protocol.ForkSessionResponse{
		Result: protocol.ForkSessionResult{
			SessionId:  newSession.ID.String(),
			TurnId:     "",
			MessageIds: messageIDs,
		},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}

// validateForkSource checks that the old session exists and belongs to the user.
func (h *Handler) validateForkSource(ctx context.Context, oldSessionID uuid.UUID, creator usecase.UserCreator) (*model.Session, error) {
	oldSession, err := h.deps.SessionRepo.GetByID(ctx, oldSessionID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{Code: "session.not_found", Message: fmt.Sprintf("old session %s not found", oldSessionID)}
		}
		return nil, h.internalError(ctx, "session.error", "internal error", err)
	}
	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, oldSessionID, creator); err != nil {
		return nil, h.ownershipError(ctx, err)
	}
	return oldSession, nil
}

// buildForkSessionModel creates the new session model for a fork operation.
func buildForkSessionModel(old *model.Session, clientID string, creator usecase.UserCreator, deviceID string) *model.Session {
	now := time.Now()
	return &model.Session{
		ID:          uuid.Must(uuid.NewV7()),
		ClientID:    clientID,
		OwnerKind:   string(creator.Kind()),
		OwnerRefID:  creator.ReferenceID(),
		DeviceID:    deviceID,
		Title:       old.Title,
		Status:      string(protocol.SessionStatusActive),
		AgentPrompt: old.AgentPrompt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}
