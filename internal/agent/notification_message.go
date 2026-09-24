package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.uber.org/zap"
)

// createNotificationMessage creates a user-role notification message in the session
// and publishes it to Centrifuge. This is the shared implementation used by both
// async sub-agent and loop notifications.
//
// Following the async sub-agent pattern:
// - User-role message with <system-reminder> XML tags
// - Fire-and-forget with context separation (30s timeout)
// - Published to Centrifuge for real-time updates
//
// Parameters:
//   - callerCtx: caller's context (will be detached for fire-and-forget)
//   - deps: usecase dependencies
//   - sessionID: target session ID
//   - notificationText: the notification content (should include <system-reminder> tags)
//
// Returns error if session is not found or message creation fails.
// Returns nil (no-op) if session is closed.
func createNotificationMessage(
	callerCtx context.Context,
	deps *usecase.Dependencies,
	sessionID uuid.UUID,
	notificationText string,
) error {
	// Detach from the caller's context — fire-and-forget with timeout.
	// This ensures the operation completes even if the caller's context is cancelled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), 30*time.Second)
	defer cancel()

	// Load session to check status and get OwnerRefID for channel publishing
	session, err := deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	if session == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Check session status — skip notification if the session is closed.
	// Creating a message and triggering a turn in a closed session is wasted work.
	if protocol.SessionStatus(session.Status) == protocol.SessionStatusClosed {
		logger.Info(ctx, "notificationMessage.skip_session_closed",
			zap.String("session_id", sessionID.String()),
			zap.String("status", session.Status),
		)
		return nil
	}

	// Create notification content
	notificationContent := protocol.ContentData{
		Type: protocol.ContentTypeText,
		Data: notificationText,
	}

	// Create the notification message in the session
	_, err = deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		msg, createErr := primitives.CreateMessage(
			txCtx, deps,
			sessionID, nil, // no turn ID — will be picked up by the next Submit
			protocol.MessageRoleUser, // user-role to trigger turn loop
			usecase.SystemCreator{},  // system-created message
			notificationContent,
			protocol.MessageStreamingCompleted,
			"",  // auto-generate client ID
			nil, // no parent message
		)
		if createErr != nil {
			return nil, fmt.Errorf("create notification message: %w", createErr)
		}

		ch := channel.UserTopic(session.OwnerRefID)
		return []updates.UpdatePublishItem{
			{
				Channel: ch,
				Items: []protocol.UpdateItem{
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: msg.ID.String(),
					},
				},
			},
		}, nil
	})
	if err != nil {
		return fmt.Errorf("publish notification: %w", err)
	}

	logger.Info(ctx, "notificationMessage.created",
		zap.String("session_id", sessionID.String()),
		zap.Int("text_len", len(notificationText)),
	)
	return nil
}

// CreateLoopNotification creates a notification creator function for loop tasks.
// This follows the async sub-agent pattern: insert a message into the conversation
// history before triggering a turn, so the LLM sees the notification as the
// most recent user message.
//
// This function is passed to loop.Worker as the NotificationCreator callback.
func CreateLoopNotification(
	deps *usecase.Dependencies,
) func(ctx context.Context, sessionID uuid.UUID, prompt string) error {
	return func(ctx context.Context, sessionID uuid.UUID, prompt string) error {
		return createNotificationMessage(ctx, deps, sessionID, prompt)
	}
}
