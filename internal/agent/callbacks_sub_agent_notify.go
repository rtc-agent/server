package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/google/uuid"
)

// notifyParentAfterAsyncSubAgent handles the async sub-agent notification path.
// It creates a notification message in the parent session and enqueues a new
// Submit work item so the parent's turn loop can process the result.
//
// Parameters:
//   - subSession: the completed sub session
//   - lastMessage: the sub agent's last message (nil if failed/cancelled)
//   - status: "completed", "failed", or "cancelled"
//   - errorMessage: error/cancellation message if status is not "completed"
//
// The function detaches from the caller's context for fire-and-forget operation.
func (h *helpers) notifyParentAfterAsyncSubAgent(callerCtx context.Context, subSession *model.Session, lastMessage *turnagent.Message, status string, errorMessage *string) {
	// Detach from the callback's context — fire-and-forget.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), 30*time.Second)
	defer cancel()

	if h.queue == nil {
		h.logger.Info(ctx, "notifyParentAfterAsyncSubAgent.queue_nil", map[string]any{
			"sub_session_id": subSession.ID.String(),
		})
		return
	}

	parentSessionID := subSession.ParentServerSessionID

	// Check parent session status — skip notification if the parent is closed.
	// Creating a message and triggering a turn in a closed session is wasted work
	// and may confuse downstream workers.
	parentSession, sessErr := h.deps.SessionRepo.GetByID(ctx, parentSessionID)
	if sessErr != nil {
		h.logger.Warn(ctx, "notifyParentAfterAsyncSubAgent.parent_session_error", map[string]any{
			"sub_session_id":    subSession.ID.String(),
			"parent_session_id": parentSessionID.String(),
			"error":             sessErr.Error(),
		})
		return
	}
	if parentSession == nil || protocol.SessionStatus(parentSession.Status) == protocol.SessionStatusClosed {
		h.logger.Info(ctx, "notifyParentAfterAsyncSubAgent.parent_session_closed", map[string]any{
			"sub_session_id":    subSession.ID.String(),
			"parent_session_id": parentSessionID.String(),
		})
		return
	}

	h.logger.Info(ctx, "notifyParentAfterAsyncSubAgent.start", map[string]any{
		"sub_session_id":    subSession.ID.String(),
		"parent_session_id": parentSessionID.String(),
		"status":            status,
	})

	// Build the notification text wrapped in <system-reminder> tags.
	// This is a convention aligned with CCHH: user-role message with XML tags
	// telling the LLM "this is system-level context, not user input".
	notificationText := buildAsyncSubAgentNotificationText(subSession, lastMessage, status, errorMessage)

	// Create the notification message in the parent session.
	// Role is user (not system) because system-reminder is a convention:
	// user-role message with <system-reminder> XML tags.
	// This triggers the turn loop so the LLM can process the notification.
	notificationContent := protocol.ContentData{
		Type: protocol.ContentTypeText,
		Data: notificationText,
	}

	_, err := h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		msg, createErr := primitives.CreateMessage(
			txCtx, h.deps,
			parentSessionID, nil, // no turn ID — will be picked up by the next Submit
			protocol.MessageRoleUser, // user-role to trigger turn loop
			usecase.SystemCreator{},
			notificationContent,
			protocol.MessageStreamingCompleted,
			"",  // auto-generate client ID
			nil, // no parent message
		)
		if createErr != nil {
			return nil, fmt.Errorf("create async notification message: %w", createErr)
		}

		ch := channel.UserTopic(subSession.OwnerRefID)
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
		h.logger.Warn(ctx, "notifyParentAfterAsyncSubAgent.publish_failed", map[string]any{
			"sub_session_id":    subSession.ID.String(),
			"parent_session_id": parentSessionID.String(),
			"error":             err.Error(),
		})
		return
	}

	// Publish Submit work item to parent session's rtc-queue to trigger a new turn.
	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: parentSessionID.String(),
	})
	if marshalErr != nil {
		h.logger.Warn(ctx, "notifyParentAfterAsyncSubAgent.marshal_failed", map[string]any{
			"parent_session_id": parentSessionID.String(),
			"error":             marshalErr.Error(),
		})
		return
	}

	if _, err := h.queue.Publish(ctx, parentSessionID.String(), string(payload), 0); err != nil {
		h.logger.Warn(ctx, "notifyParentAfterAsyncSubAgent.submit_failed", map[string]any{
			"parent_session_id": parentSessionID.String(),
			"error":             err.Error(),
		})
		return
	}

	h.logger.Info(ctx, "notifyParentAfterAsyncSubAgent.done", map[string]any{
		"sub_session_id":    subSession.ID.String(),
		"parent_session_id": parentSessionID.String(),
		"status":            status,
	})
}

// buildAsyncSubAgentNotificationText constructs the notification text for an
// async sub-agent completion, wrapped in <system-reminder> XML tags.
// The text format varies by status: completed/failed/cancelled each produce
// a different template. Unknown statuses produce a generic notification.
func buildAsyncSubAgentNotificationText(subSession *model.Session, lastMessage *turnagent.Message, status string, errorMessage *string) string {
	sessionID := subSession.ID.String()
	title := subSession.Title

	var content string
	switch status {
	case "completed":
		result := "(no output)"
		if lastMessage != nil {
			result = lastMessage.Content
		}
		content = fmt.Sprintf(
			"The async sub agent task has completed.\n- Session ID: %s\n- Title: %s\n- Status: completed\n\nResult:\n%s",
			sessionID, title, result,
		)
	case "failed":
		errMsg := "(unknown error)"
		if errorMessage != nil {
			errMsg = *errorMessage
		}
		content = fmt.Sprintf(
			"The async sub agent task has failed.\n- Session ID: %s\n- Title: %s\n- Status: failed\n\nError:\n%s",
			sessionID, title, errMsg,
		)
	case "cancelled":
		reason := "(no reason given)"
		if errorMessage != nil {
			reason = *errorMessage
		}
		content = fmt.Sprintf(
			"The async sub agent task has been cancelled.\n- Session ID: %s\n- Title: %s\n- Status: cancelled\n\nReason:\n%s",
			sessionID, title, reason,
		)
	default:
		content = fmt.Sprintf(
			"The async sub agent task has ended with status: %s.\n- Session ID: %s\n- Title: %s",
			status, sessionID, title,
		)
	}
	return turnagent.FormatSystemReminder(content)
}

// notifyParentAfterSubAgentSession dispatches the parent notification based on
// the sub-agent mode (async vs sync).
//
// This extracts the duplicated sub-agent notification logic from completeTurn,
// failTurn, and cancelTurn into a single place.
//
// Parameters:
//   - session: the sub session that reached a terminal state
//   - lastMessage: the sub agent's last message (nil if failed/cancelled)
//   - status: "completed", "failed", or "cancelled"
//   - errorMessage: error/cancellation message if status is not "completed"
func (h *helpers) notifyParentAfterSubAgentSession(
	ctx context.Context,
	session *model.Session,
	lastMessage *turnagent.Message,
	status string,
	errorMessage *string,
) {
	if session == nil || session.ParentServerSessionID == uuid.Nil {
		return
	}

	if session.SubAgentMode == "async" {
		// Async mode: toolcall_output was already created when the tool returned.
		// Create a notification message and trigger a new turn via Submit.
		h.notifyParentAfterAsyncSubAgent(ctx, session, lastMessage, status, errorMessage)
		return
	}

	// Sync mode: create toolcall_output and resume parent from checkpoint.
	if session.SubAgentParentMessageID != uuid.Nil {
		var resultPtr *string
		if lastMessage != nil {
			resultPtr = &lastMessage.Content
		}
		h.resumeParentAfterSubAgentNewToolCallOutput(ctx, session.SubAgentParentMessageID, status, errorMessage, resultPtr)
	}
	h.resumeParentAfterSubAgent(ctx, session, lastMessage)
}
