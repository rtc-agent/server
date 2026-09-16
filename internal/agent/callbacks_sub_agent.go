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

// =============================================================================
// Sub Agent lifecycle helpers
// =============================================================================
//
// These helpers manage the parent-child relationship when a sub agent
// (sub session) completes. They are called from completeTurn / failTurn /
// cancelTurn callbacks to notify the parent session.

// resumeParentAfterSubAgentNewToolCallOutput creates a toolcall_output message
// for the sub agent result. This is called when a sub session completes or
// fails, to provide the result to the parent session's LLM context.
//
// Parameters:
//   - messageID: the toolcall_input message ID in the parent session
//   - status: "completed" or "failed"
//   - errorMessage: error message if status is "failed"
//   - result: the sub agent's result if status is "completed"
func (h *helpers) resumeParentAfterSubAgentNewToolCallOutput(ctx context.Context, messageID uuid.UUID, status string, errorMessage *string, result *string) {
	// Load the toolcall_input message.
	inputMsg, err := h.deps.MessageRepo.GetByID(ctx, messageID)
	if err != nil {
		h.logger.Info(ctx, "resumeParentAfterSubAgentNewToolCallOutput.load_input_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Parse the toolcall_input content.
	inputContentData, err := primitives.ParseContentData(inputMsg.Content)
	if err != nil {
		h.logger.Info(ctx, "resumeParentAfterSubAgentNewToolCallOutput.parse_input_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	inputToolCall, err := primitives.ParseContentDataToolCall(inputContentData.Data)
	if err != nil {
		h.logger.Info(ctx, "resumeParentAfterSubAgentNewToolCallOutput.parse_toolcall_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Build the toolcall_output content.
	toolOutput := ""
	if result != nil {
		toolOutput = *result
	} else if errorMessage != nil {
		toolOutput = *errorMessage
	}
	outputToolCall := protocol.ToolCall{
		Id:       inputToolCall.Id,
		ToolName: inputToolCall.ToolName,
		Input:    inputToolCall.Input,
		Output:   &toolOutput,
		Status:   &status,
	}
	outputContentData := protocol.ContentData{
		Type: protocol.ContentTypeToolCallOutput,
		Data: outputToolCall,
	}

	// Create the toolcall_output message in the parent session.
	parentMsgID := inputMsg.ID
	_, err = h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		outputMsg, createErr := primitives.CreateMessage(
			txCtx, h.deps,
			inputMsg.SessionID, inputMsg.TurnID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			outputContentData,
			protocol.MessageStreamingCompleted,
			"",
			&parentMsgID,
		)
		if createErr != nil {
			return nil, fmt.Errorf("create toolcall_output message: %w", createErr)
		}

		// Load session for event publishing.
		session, sessErr := h.deps.SessionRepo.GetByID(txCtx, inputMsg.SessionID)
		if sessErr != nil {
			h.logger.Info(ctx, "resumeParentAfterSubAgentNewToolCallOutput.load_session_failed", map[string]any{
				"session_id": inputMsg.SessionID.String(),
				"error":      sessErr.Error(),
			})
			return nil, nil
		}

		// Build updates for the new message.
		items := []updates.UpdatePublishItem{
			{
				Channel: channel.UserTopic(session.OwnerRefID),
				Items: []protocol.UpdateItem{
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: protocol.UUID(outputMsg.ID.String()),
					},
				},
			},
		}
		return items, nil
	})
	if err != nil {
		h.logger.Info(ctx, "resumeParentAfterSubAgentNewToolCallOutput.publish_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	h.logger.Info(ctx, "resumeParentAfterSubAgentNewToolCallOutput.done", map[string]any{
		"input_message_id":  messageID.String(),
		"status":            status,
		"has_result":        result != nil,
		"has_error_message": errorMessage != nil,
	})
}

// resumeParentAfterSubAgent publishes a Resume work item to the parent session's
// rtc-queue, carrying the sub session's final result.
//
// This is called when a sub session completes, so the parent session can resume
// from its interrupted state and receive the sub session's result.
//
// The function detaches from the caller's context: the turn-agent's callback
// context may be cancelled when the callback returns, but the resume operation
// must complete independently since it's fire-and-forget.
func (h *helpers) resumeParentAfterSubAgent(callerCtx context.Context, subSession *model.Session, lastMessage *turnagent.Message) {
	// Detach from the callback's context. The sub session's turn is already
	// completed; the resume is fire-and-forget and must not be aborted by
	// the callback context timeout/cancellation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), 30*time.Second)
	defer cancel()

	if h.queue == nil {
		h.logger.Info(ctx, "resumeParentAfterSubAgent.queue_nil", map[string]any{
			"sub_session_id": subSession.ID.String(),
		})
		return
	}

	parentSessionID := subSession.ParentServerSessionID.String()

	h.logger.Info(ctx, "resumeParentAfterSubAgent.start", map[string]any{
		"sub_session_id":    subSession.ID.String(),
		"parent_session_id": parentSessionID,
	})

	// Extract the sub session's final result from lastMessage.
	var subAgentResult *string
	if lastMessage != nil {
		subAgentResult = &lastMessage.Content
	}

	// Find the parent session's interrupted turn to get its InterruptID.
	var interruptID string
	if parentSID, parseErr := uuid.Parse(parentSessionID); parseErr == nil {
		activeTurns, findErr := h.deps.TurnRepo.FindActiveBySession(ctx, parentSID)
		if findErr == nil {
			for _, t := range activeTurns {
				if protocol.TurnStatus(t.Status) == protocol.TurnStatusInterrupted {
					interruptID = t.InterruptID
					break
				}
			}
		}
	}

	// Publish Resume work item to parent session's rtc-queue.
	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:            turnagent.WorkKindResume,
		SessionID:       parentSessionID,
		SubAgentResult:  subAgentResult,
		InterruptID:     interruptID,
		InterruptResult: subAgentResult, // Sub agent result is the interrupt resolution
	})
	if marshalErr != nil {
		h.logger.Info(ctx, "resumeParentAfterSubAgent.marshal_failed", map[string]any{
			"parent_session_id": parentSessionID,
			"error":             marshalErr.Error(),
		})
		return
	}

	// Use ResumePriority (100) to ensure the resume is claimed before any pending
	// Submit items, so the parent's checkpoint is still intact.
	const resumePriority int64 = 100
	if _, err := h.queue.Publish(ctx, parentSessionID, string(payload), resumePriority); err != nil {
		h.logger.Info(ctx, "resumeParentAfterSubAgent.publish_failed", map[string]any{
			"parent_session_id": parentSessionID,
			"error":             err.Error(),
		})
		return
	}

	h.logger.Info(ctx, "resumeParentAfterSubAgent.done", map[string]any{
		"sub_session_id":    subSession.ID.String(),
		"parent_session_id": parentSessionID,
		"has_result":        lastMessage != nil,
	})
}

// notifyParentAfterAsyncSubAgent creates a notification message in the parent
// session and publishes a Submit work item to trigger a new turn.
//
// This is used for async sub agents: the toolcall_output was already created
// when the tool returned immediately, so we cannot create another
// toolcall_output. Instead, we create a user-role notification message that
// the LLM will see in the next turn.
//
// Parameters:
//   - subSession: the sub session that completed/failed/was cancelled
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
		h.logger.Info(ctx, "notifyParentAfterAsyncSubAgent.parent_session_error", map[string]any{
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
	var notificationText string
	switch status {
	case "completed":
		result := "(no output)"
		if lastMessage != nil {
			result = lastMessage.Content
		}
		content := fmt.Sprintf(
			"The async sub agent task has completed.\n- Session ID: %s\n- Title: %s\n- Status: completed\n\nResult:\n%s",
			subSession.ID.String(),
			subSession.Title,
			result,
		)
		notificationText = turnagent.FormatSystemReminder(content)
	case "failed":
		errMsg := "(unknown error)"
		if errorMessage != nil {
			errMsg = *errorMessage
		}
		content := fmt.Sprintf(
			"The async sub agent task has failed.\n- Session ID: %s\n- Title: %s\n- Status: failed\n\nError:\n%s",
			subSession.ID.String(),
			subSession.Title,
			errMsg,
		)
		notificationText = turnagent.FormatSystemReminder(content)
	case "cancelled":
		reason := "(no reason given)"
		if errorMessage != nil {
			reason = *errorMessage
		}
		content := fmt.Sprintf(
			"The async sub agent task has been cancelled.\n- Session ID: %s\n- Title: %s\n- Status: cancelled\n\nReason:\n%s",
			subSession.ID.String(),
			subSession.Title,
			reason,
		)
		notificationText = turnagent.FormatSystemReminder(content)
	default:
		content := fmt.Sprintf(
			"The async sub agent task has ended with status: %s.\n- Session ID: %s\n- Title: %s",
			status,
			subSession.ID.String(),
			subSession.Title,
		)
		notificationText = turnagent.FormatSystemReminder(content)
	}

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
						EntityId: protocol.UUID(msg.ID.String()),
					},
				},
			},
		}, nil
	})
	if err != nil {
		h.logger.Info(ctx, "notifyParentAfterAsyncSubAgent.publish_failed", map[string]any{
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
		h.logger.Info(ctx, "notifyParentAfterAsyncSubAgent.marshal_failed", map[string]any{
			"parent_session_id": parentSessionID.String(),
			"error":             marshalErr.Error(),
		})
		return
	}

	if _, err := h.queue.Publish(ctx, parentSessionID.String(), string(payload), 0); err != nil {
		h.logger.Info(ctx, "notifyParentAfterAsyncSubAgent.submit_failed", map[string]any{
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

// updateSubAgentInvocationStatus updates the status field of a
// sub_agent_invocation message. This is used to track the sub agent's
// execution state (pending/running/failed/completed).
//
// NOTE: This is a read-modify-write operation without atomicity guarantees.
// A race could occur if two concurrent calls read the same content, modify
// the status, and write back (last writer wins). In practice this is safe
// because: (1) status transitions are sequential per sub-agent lifecycle
// (pending → running → completed/failed), (2) each transition is triggered
// by a different turn callback which runs sequentially per session. If this
// becomes a concern, a JSON-aware SQL UPDATE (e.g., jsonb_set) would be the
// proper fix.
func (h *helpers) updateSubAgentInvocationStatus(ctx context.Context, messageID uuid.UUID, status string) {
	// Load the message.
	msg, err := h.deps.MessageRepo.GetByID(ctx, messageID)
	if err != nil {
		h.logger.Info(ctx, "updateSubAgentInvocationStatus.load_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Parse the content.
	var content protocol.ContentData
	if err := json.Unmarshal([]byte(msg.Content), &content); err != nil {
		h.logger.Info(ctx, "updateSubAgentInvocationStatus.parse_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Update the status in the data map.
	if data, ok := content.Data.(map[string]any); ok {
		data["status"] = status
	} else {
		h.logger.Info(ctx, "updateSubAgentInvocationStatus.invalid_data_type", map[string]any{
			"message_id": messageID.String(),
		})
		return
	}

	// Serialize the updated content.
	updatedContent, err := json.Marshal(content)
	if err != nil {
		h.logger.Info(ctx, "updateSubAgentInvocationStatus.serialize_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Update the message in DB.
	if err := h.deps.MessageRepo.UpdateStreamingStatus(ctx, messageID, protocol.MessageStreamingCompleted, string(updatedContent)); err != nil {
		h.logger.Info(ctx, "updateSubAgentInvocationStatus.update_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Publish message.updated event.
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, msg.SessionID)
	if sessErr == nil {
		updates := []updates.UpdatePublishItem{
			{
				Channel: channel.UserTopic(session.OwnerRefID),
				Items: []protocol.UpdateItem{
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionUpdated,
						EntityId: protocol.UUID(messageID.String()),
					},
				},
			},
		}
		if _, err := h.deps.UpdatePublisher.Publish(ctx, updates...); err != nil {
			h.logger.Info(ctx, "updateSubAgentInvocationStatus.publish_failed", map[string]any{
				"message_id": messageID.String(),
				"error":      err.Error(),
			})
		}
	}

	h.logger.Info(ctx, "updateSubAgentInvocationStatus.done", map[string]any{
		"message_id": messageID.String(),
		"status":     status,
	})
}
