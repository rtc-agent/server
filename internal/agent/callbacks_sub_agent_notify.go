package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/model"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
	// Defensive nil check: caller (notifyParentAfterSubAgentSession) guards
	// against nil, but protect against future call sites.
	if subSession == nil {
		return
	}

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

	h.logger.Info(ctx, "notifyParentAfterAsyncSubAgent.start", map[string]any{
		"sub_session_id":    subSession.ID.String(),
		"parent_session_id": parentSessionID.String(),
		"status":            status,
	})

	// Build the notification text wrapped in <system-reminder> tags.
	// This is a convention aligned with CCHH: user-role message with XML tags
	// telling the LLM "this is system-level context, not user input".
	notificationText := buildAsyncSubAgentNotificationText(subSession, lastMessage, status, errorMessage)

	// Create the notification message in the parent session using the shared helper.
	// This handles context separation, session status check, message creation,
	// and Centrifuge publishing.
	if err := createNotificationMessage(ctx, h.deps, parentSessionID, notificationText); err != nil {
		h.logger.Warn(ctx, "notifyParentAfterAsyncSubAgent.create_notification_failed", map[string]any{
			"sub_session_id":    subSession.ID.String(),
			"parent_session_id": parentSessionID.String(),
			"error":             err.Error(),
		})
		return
	}

	// Publish Submit work item to parent session's rtc-queue to trigger a new turn.
	// Extract trace context for cross-process propagation.
	ctx, span := h.tracer.Start(ctx, "subAgent.notify",
		trace.WithAttributes(
			attribute.String("sub_session_id", subSession.ID.String()),
			attribute.String("parent_session_id", parentSessionID.String()),
			attribute.String("status", status),
		),
	)
	defer span.End()

	traceID, spanID := turnagent.ExtractTraceFromCtx(ctx)
	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: parentSessionID.String(),
		TraceID:   traceID,
		SpanID:    spanID,
	})
	if marshalErr != nil {
		span.RecordError(marshalErr)
		span.SetStatus(codes.Error, "marshal_failed")
		h.logger.Warn(ctx, "notifyParentAfterAsyncSubAgent.marshal_failed", map[string]any{
			"parent_session_id": parentSessionID.String(),
			"error":             marshalErr.Error(),
		})
		return
	}

	if _, err := h.queue.Publish(ctx, parentSessionID.String(), string(payload), rtcqueue.SubmitWorkPriority); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		h.logger.Warn(ctx, "notifyParentAfterAsyncSubAgent.submit_failed", map[string]any{
			"parent_session_id": parentSessionID.String(),
			"error":             err.Error(),
		})
		return
	}

	span.SetStatus(codes.Ok, "")
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

	if session.SubAgentMode == model.SubAgentModeAsync {
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
