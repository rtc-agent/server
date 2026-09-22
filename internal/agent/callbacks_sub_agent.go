package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
func (h *helpers) resumeParentAfterSubAgentNewToolCallOutput(callerCtx context.Context, messageID uuid.UUID, status string, errorMessage *string, result *string) {
	// Detach from the caller's context — fire-and-forget.
	// The callback's context may be cancelled when the callback returns,
	// but the toolcall_output creation must complete independently.
	// Mirrors resumeParentAfterSubAgent's fire-and-forget pattern.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), 30*time.Second)
	defer cancel()

	// Load the toolcall_input message.
	inputMsg, err := h.deps.MessageRepo.GetByID(ctx, messageID)
	if err != nil {
		h.logger.Warn(ctx, "resumeParentAfterSubAgentNewToolCallOutput.load_input_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Parse the toolcall_input content.
	inputContentData, err := primitives.ParseContentData(inputMsg.Content)
	if err != nil {
		h.logger.Warn(ctx, "resumeParentAfterSubAgentNewToolCallOutput.parse_input_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	inputToolCall, err := primitives.ParseContentDataToolCall(inputContentData.Data)
	if err != nil {
		h.logger.Warn(ctx, "resumeParentAfterSubAgentNewToolCallOutput.parse_toolcall_failed", map[string]any{
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
	// The message creation and session lookup are intentionally split into
	// separate steps: the message is created first (its own transaction),
	// then the session is loaded for event publishing. If the session lookup
	// fails, the message is preserved in DB — the frontend will see it on
	// the next page load even if the real-time notification is missed.
	// Splitting avoids a transaction rollback that would permanently lose
	// the sub-agent result on a transient session load failure.
	parentMsgID := inputMsg.ID
	outputMsg, createErr := primitives.CreateMessage(
		ctx, h.deps,
		inputMsg.SessionID, inputMsg.TurnID,
		protocol.MessageRoleTool,
		usecase.SystemCreator{},
		outputContentData,
		protocol.MessageStreamingCompleted,
		"",
		&parentMsgID,
	)
	if createErr != nil {
		h.logger.Warn(ctx, "resumeParentAfterSubAgentNewToolCallOutput.create_message_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      createErr.Error(),
		})
		return
	}

	// Publish message.created event. Session lookup is done separately so a
	// transient failure here does not rollback the already-created message.
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, inputMsg.SessionID)
	if sessErr != nil {
		// Message is in DB; only the real-time notification is lost. The
		// normalizer's repairToolPairing and the frontend's next page load
		// will recover the message content.
		h.logger.Warn(ctx, "resumeParentAfterSubAgentNewToolCallOutput.load_session_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      sessErr.Error(),
			"message":    "toolcall_output created in DB but frontend notification skipped",
		})
		return
	}

	items := []updates.UpdatePublishItem{
		{
			Channel: channel.UserTopic(session.OwnerRefID),
			Items: []protocol.UpdateItem{
				{
					Entity:   protocol.EntityMessage,
					Action:   protocol.ActionCreated,
					EntityId: outputMsg.ID.String(),
				},
			},
		},
	}
	if _, pubErr := h.deps.UpdatePublisher.Publish(ctx, items...); pubErr != nil {
		h.logger.Warn(ctx, "resumeParentAfterSubAgentNewToolCallOutput.publish_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      pubErr.Error(),
		})
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
		if findErr != nil {
			// Log the error so operators can diagnose DB issues. Without this,
			// a transient query failure silently produces a Resume without
			// InterruptID, potentially stalling the parent session.
			h.logger.Warn(ctx, "resumeParentAfterSubAgent.find_active_turns_failed", map[string]any{
				"parent_session_id": parentSessionID,
				"error":             findErr.Error(),
			})
		} else {
			for _, t := range activeTurns {
				if protocol.TurnStatus(t.Status) == protocol.TurnStatusInterrupted {
					interruptID = t.InterruptID
					break
				}
			}
		}
	}

	// Publish Resume work item to parent session's rtc-queue.
	// Extract trace context for cross-process propagation.
	ctx, span := h.tracer.Start(ctx, "subAgent.resumeParent",
		trace.WithAttributes(
			attribute.String("sub_session_id", subSession.ID.String()),
			attribute.String("parent_session_id", parentSessionID),
		),
	)
	defer span.End()

	traceID, spanID := turnagent.ExtractTraceFromCtx(ctx)
	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:            turnagent.WorkKindResume,
		SessionID:       parentSessionID,
		SubAgentResult:  subAgentResult,
		InterruptID:     interruptID,
		InterruptResult: subAgentResult, // Sub agent result is the interrupt resolution
		TraceID:         traceID,
		SpanID:          spanID,
	})
	if marshalErr != nil {
		span.RecordError(marshalErr)
		span.SetStatus(codes.Error, "marshal_failed")
		h.logger.Warn(ctx, "resumeParentAfterSubAgent.marshal_failed", map[string]any{
			"parent_session_id": parentSessionID,
			"error":             marshalErr.Error(),
		})
		return
	}

	// Use ResumeWorkPriority to ensure the resume is claimed before any pending
	// Submit items, so the parent's checkpoint is still intact.
	if _, err := h.queue.Publish(ctx, parentSessionID, string(payload), rtcqueue.ResumeWorkPriority); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		h.logger.Warn(ctx, "resumeParentAfterSubAgent.publish_failed", map[string]any{
			"parent_session_id": parentSessionID,
			"error":             err.Error(),
		})
		return
	}

	span.SetStatus(codes.Ok, "")
	h.logger.Info(ctx, "resumeParentAfterSubAgent.done", map[string]any{
		"sub_session_id":    subSession.ID.String(),
		"parent_session_id": parentSessionID,
		"has_result":        lastMessage != nil,
	})
}

// updateSubAgentInvocationStatus updates the status field of a
// subAgentInvocation message. This is used to track the sub agent's
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
		h.logger.Warn(ctx, "updateSubAgentInvocationStatus.load_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Parse the content.
	var content protocol.ContentData
	if err := json.Unmarshal([]byte(msg.Content), &content); err != nil {
		h.logger.Warn(ctx, "updateSubAgentInvocationStatus.parse_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Update the status in the data map.
	if data, ok := content.Data.(map[string]any); ok {
		data["status"] = status
	} else {
		h.logger.Warn(ctx, "updateSubAgentInvocationStatus.invalid_data_type", map[string]any{
			"message_id": messageID.String(),
		})
		return
	}

	// Serialize the updated content.
	updatedContent, err := json.Marshal(content)
	if err != nil {
		h.logger.Warn(ctx, "updateSubAgentInvocationStatus.serialize_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Update the message in DB.
	if err := h.deps.MessageRepo.UpdateStreamingStatus(ctx, messageID, protocol.MessageStreamingCompleted, string(updatedContent)); err != nil {
		h.logger.Warn(ctx, "updateSubAgentInvocationStatus.update_failed", map[string]any{
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
						EntityId: messageID.String(),
					},
				},
			},
		}
		if _, err := h.deps.UpdatePublisher.Publish(ctx, updates...); err != nil {
			h.logger.Warn(ctx, "updateSubAgentInvocationStatus.publish_failed", map[string]any{
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
