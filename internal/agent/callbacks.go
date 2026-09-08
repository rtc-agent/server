package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rtc-agent/server/internal/agent/command"
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
// Turn ownership callbacks
// =============================================================================
//
// These two callbacks are the integration point for turn allocation and lookup.
// The old worker package created turns in BuildInput (API layer created them
// before enqueueing). The new turn-agent package owns the turn lifecycle and
// delegates allocation/lookup to the application via these callbacks.

// createTurn allocates a new turn in the DB.
//
// Idempotency: workID is used as the turn's ClientID. If a turn with the same
// ClientID already exists (e.g., after a worker crash and rtc-queue retry),
// the existing turn is returned instead of creating a duplicate.
//
// Mapping from old code: the old worker expected the turn to already exist
// (created by the API layer in SubmitTurn). The new code creates the turn
// here, at the start of the work.
func (h *helpers) createTurn(ctx context.Context, sessionID string, workID string) (string, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return "", fmt.Errorf("createTurn: invalid session ID %q: %w", sessionID, err)
	}

	// Idempotency check: if a turn with this ClientID already exists, return it.
	// This handles rtc-queue retries after a worker crash.
	existing, err := h.deps.TurnRepo.FindByClientID(ctx, workID)
	if err != nil {
		return "", fmt.Errorf("createTurn: find existing turn by client_id %q: %w", workID, err)
	}
	if existing != nil {
		h.logIfEnabled(ctx, "createTurn.idempotent_hit", map[string]any{
			"session_id": sessionID,
			"work_id":    workID,
			"turn_id":    existing.ID.String(),
		})
		return existing.ID.String(), nil
	}

	// Create a new turn.
	turn := &model.Turn{
		SessionID: sid,
		ClientID:  workID, // workID as idempotency key
		Status:    string(model.TurnStatusPending),
	}
	if err := h.deps.TurnRepo.Create(ctx, turn); err != nil {
		return "", fmt.Errorf("createTurn: create turn: %w", err)
	}

	h.logIfEnabled(ctx, "createTurn.created", map[string]any{
		"session_id": sessionID,
		"work_id":    workID,
		"turn_id":    turn.ID.String(),
	})

	// Publish turn.created event so the frontend knows about the new turn.
	// This is the ONLY place where turn.created is emitted — all other turn
	// lifecycle transitions (begin, complete, interrupt, resume, fail, cancel)
	// publish turn.updated via publishTurnUpdated.
	// Publishing is best-effort: if it fails, the turn is still created in DB
	// and the frontend will learn about it via subsequent turn.updated events.
	if h.deps.UpdatePublisher != nil {
		session, sessErr := h.deps.SessionRepo.GetByID(ctx, sid)
		if sessErr != nil {
			h.logIfEnabled(ctx, "createTurn.load_session_failed", map[string]any{
				"session_id": sessionID,
				"error":      sessErr.Error(),
			})
		} else {
			updates := primitives.BuildTurnCreatedUpdates(session, turn.ID)
			if len(updates) > 0 {
				if _, err := h.deps.UpdatePublisher.Publish(ctx, updates...); err != nil {
					h.logIfEnabled(ctx, "createTurn.publish_failed", map[string]any{
						"session_id": sessionID,
						"turn_id":    turn.ID.String(),
						"error":      err.Error(),
					})
				}
			}
		}
	}

	return turn.ID.String(), nil
}

// lookupTurn finds the active turn for a session during a resume work.
//
// It queries for turns in "running" or "interrupted" status. If multiple
// exist (shouldn't happen in normal operation), the most recent is returned.
//
// Mapping from old code: the old worker got the turnID from the TurnItem
// pushed into the session's buffer. The new code must look it up from DB
// since the resume work item carries only {kind: "resume", sessionID}.
func (h *helpers) lookupTurn(ctx context.Context, sessionID string, workID string) (string, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return "", fmt.Errorf("lookupTurn: invalid session ID %q: %w", sessionID, err)
	}

	active, err := h.deps.TurnRepo.FindActiveBySession(ctx, sid)
	if err != nil {
		return "", fmt.Errorf("lookupTurn: find active turns for session %s: %w", sessionID, err)
	}
	if len(active) == 0 {
		// Wrap with ErrNoActiveTurn so Process can detect this specific case
		// and complete the work gracefully (turn was cancelled/completed).
		return "", fmt.Errorf("lookupTurn: %w: %s", turnagent.ErrNoActiveTurn, sessionID)
	}

	// Return the most recent active turn.
	// FindActiveBySession returns turns ordered by created_at ASC, so the
	// last element is the most recent.
	turn := active[len(active)-1]

	h.logIfEnabled(ctx, "lookupTurn.found", map[string]any{
		"session_id": sessionID,
		"work_id":    workID,
		"turn_id":    turn.ID.String(),
		"status":     turn.Status,
	})

	return turn.ID.String(), nil
}

// =============================================================================
// Turn state transition callbacks
// =============================================================================
//
// These six callbacks persist turn state transitions and publish turn.updated
// events. They are called by turn-agent at well-defined points in the turn's
// lifecycle. The application must NOT mutate turn state from other code paths.
//
// Common pattern: each callback updates the turn's status in the DB via
// TurnRepo.UpdateStatus, then publishes a turn.updated event via the
// UpdatePublisher. The DB update and publish are independent: the DB update
// must always succeed; the publish is best-effort (frontend may miss the
// event, but DB state remains correct).

// beginTurn marks a freshly created turn as "running".
//
// Called exactly once per turn, after CreateTurn. The turnID is then passed
// to every subsequent callback for the duration of the turn.
//
// Also sets the parent session's status to "active" to indicate that a turn
// is currently executing.
func (h *helpers) beginTurn(ctx context.Context, turnID string) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("beginTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusRunning, ""); err != nil {
		return fmt.Errorf("beginTurn: update status: %w", err)
	}

	// Batch publish: combine turn.updated + session.updated into a single
	// centrifuge call. Load the turn once to get the sessionID, update
	// session status in DB first (so the frontend sees consistent state
	// when it reacts to the event), then publish both events together.
	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "beginTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// DB update session status before publishing events.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusActive); err != nil {
		h.logIfEnabled(ctx, "beginTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "begin")

	h.logIfEnabled(ctx, "beginTurn.done", map[string]any{
		"turn_id": turnID,
	})

	// Sub Agent support: if this is a sub session, update the parent's sub_agent_invocation message status to "running".
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, turn.SessionID)
	if sessErr == nil && session.ParentServerSessionID != uuid.Nil && session.SubAgentParentMessageID != uuid.Nil {
		h.updateSubAgentInvocationStatus(ctx, session.SubAgentParentMessageID, "running")
	}

	return nil
}

// completeTurn marks a turn as "completed" after a clean exit.
//
// The eino checkpoint has already been deleted by eino's TurnLoop on clean
// exit. This callback persists the "completed" state and notifies frontend.
//
// Also sets the parent session's status to "idle" to indicate no turn is
// currently executing.
//
// Sub Agent support: if the session has a ParentServerSessionID (is a sub
// session), this callback also triggers the parent session's resume by
// publishing a Resume work item to the parent's rtc-queue, carrying the sub
// session's final result.
func (h *helpers) completeTurn(ctx context.Context, sessionID string, turnID string, lastMessage *turnagent.Message) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("completeTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusCompleted, ""); err != nil {
		return fmt.Errorf("completeTurn: update status: %w", err)
	}

	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return fmt.Errorf("completeTurn: invalid session ID %q: %w", sessionID, err)
	}

	// Load session to check Sub Agent hierarchy and publish events.
	session, sessionErr := h.deps.SessionRepo.GetByID(ctx, sid)
	if sessionErr != nil {
		h.logIfEnabled(ctx, "completeTurn.load_session_failed", map[string]any{
			"session_id": sessionID,
			"error":      sessionErr.Error(),
		})
		// Continue even if session load fails — turn status is already updated.
	}

	// DB update session status before publishing events.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, sid, protocol.SessionStatusIdle); err != nil {
		h.logIfEnabled(ctx, "completeTurn.update_session_status_failed", map[string]any{
			"session_id": sessionID,
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, sid, "complete")

	h.logIfEnabled(ctx, "completeTurn.done", map[string]any{"turn_id": turnID, "session_id": sessionID})

	// Sub Agent support: if this is a sub session, notify the parent.
	if session != nil && session.ParentServerSessionID != uuid.Nil {
		if session.SubAgentMode == "async" {
			// Async mode: toolcall_output was already created when the tool returned.
			// Create a notification message and trigger a new turn via Submit.
			h.notifyParentAfterAsyncSubAgent(ctx, session, lastMessage, "completed", nil)
		} else {
			// Sync mode: create toolcall_output and resume parent from checkpoint.
			if session.SubAgentParentMessageID != uuid.Nil {
				var resultPtr *string
				if lastMessage != nil {
					resultPtr = &lastMessage.Content
				}
				h.resumeParentAfterSubAgentNewToolCallOutput(ctx, session.SubAgentParentMessageID, "completed", nil, resultPtr)
			}
			h.resumeParentAfterSubAgent(ctx, session, lastMessage)
		}
	}

	// Slash-command framework: notify active commands of turn completion.
	// The /goal execution loop is now handled by GoalWorkflow.OnTurnComplete
	// (see goal_workflow.go) via the registry.
	if h.deps.CommandRegistry != nil {
		cmdCtx := command.Context{
			Context:   ctx,
			SessionID: sid,
			TurnID:    tid,
		}
		if errs := h.deps.CommandRegistry.OnTurnComplete(cmdCtx); len(errs) > 0 {
			for _, e := range errs {
				h.logIfEnabled(ctx, "completeTurn.command_hook_failed", map[string]any{
					"session_id": sessionID,
					"turn_id":    turnID,
					"error":      e.Error(),
				})
			}
		}
	}

	return nil
}

// resumeParentAfterSubAgentNewToolCallOutput creates a toolcall_output message for the sub agent result.
// This is called when a sub session completes or fails, to provide the result to the parent session's LLM context.
//
// Parameters:
// - messageID: the toolcall_input message ID in the parent session
// - status: "completed" or "failed"
// - errorMessage: error message if status is "failed"
// - result: the sub agent's result if status is "completed"
func (h *helpers) resumeParentAfterSubAgentNewToolCallOutput(ctx context.Context, messageID uuid.UUID, status string, errorMessage *string, result *string) {
	// Load the toolcall_input message.
	inputMsg, err := h.deps.MessageRepo.GetByID(ctx, messageID)
	if err != nil {
		h.logIfEnabled(ctx, "resumeParentAfterSubAgentNewToolCallOutput.load_input_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Parse the toolcall_input content.
	inputContentData, err := primitives.ParseContentData(inputMsg.Content)
	if err != nil {
		h.logIfEnabled(ctx, "resumeParentAfterSubAgentNewToolCallOutput.parse_input_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	inputToolCall, err := primitives.ParseContentDataToolCall(inputContentData.Data)
	if err != nil {
		h.logIfEnabled(ctx, "resumeParentAfterSubAgentNewToolCallOutput.parse_toolcall_failed", map[string]any{
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
			h.logIfEnabled(ctx, "resumeParentAfterSubAgentNewToolCallOutput.load_session_failed", map[string]any{
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
		h.logIfEnabled(ctx, "resumeParentAfterSubAgentNewToolCallOutput.publish_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	h.logIfEnabled(ctx, "resumeParentAfterSubAgentNewToolCallOutput.done", map[string]any{
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
	ctx := context.WithoutCancel(callerCtx)

	if h.queue == nil {
		h.logIfEnabled(ctx, "resumeParentAfterSubAgent.queue_nil", map[string]any{
			"sub_session_id": subSession.ID.String(),
		})
		return
	}

	parentSessionID := subSession.ParentServerSessionID.String()

	h.logIfEnabled(ctx, "resumeParentAfterSubAgent.start", map[string]any{
		"sub_session_id":    subSession.ID.String(),
		"parent_session_id": parentSessionID,
	})

	// Extract the sub session's final result from lastMessage.
	var subAgentResult *string
	if lastMessage != nil {
		subAgentResult = &lastMessage.Content
	}

	// Publish Resume work item to parent session's rtc-queue.
	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:           turnagent.WorkKindResume,
		SessionID:      parentSessionID,
		SubAgentResult: subAgentResult,
	})
	if marshalErr != nil {
		h.logIfEnabled(ctx, "resumeParentAfterSubAgent.marshal_failed", map[string]any{
			"parent_session_id": parentSessionID,
			"error":             marshalErr.Error(),
		})
		return
	}

	// Use ResumePriority (100) to ensure the resume is claimed before any pending
	// Submit items, so the parent's checkpoint is still intact.
	const resumePriority int64 = 100
	if _, err := h.queue.Publish(ctx, parentSessionID, string(payload), resumePriority); err != nil {
		h.logIfEnabled(ctx, "resumeParentAfterSubAgent.publish_failed", map[string]any{
			"parent_session_id": parentSessionID,
			"error":             err.Error(),
		})
		return
	}

	h.logIfEnabled(ctx, "resumeParentAfterSubAgent.done", map[string]any{
		"sub_session_id":    subSession.ID.String(),
		"parent_session_id": parentSessionID,
		"has_result":        lastMessage != nil,
	})
}

// notifyParentAfterAsyncSubAgent creates a notification message in the parent session
// and publishes a Submit work item to trigger a new turn.
//
// This is used for async sub agents: the toolcall_output was already created when
// the tool returned immediately, so we cannot create another toolcall_output.
// Instead, we create a user-role notification message that the LLM will see in
// the next turn.
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
	ctx := context.WithoutCancel(callerCtx)

	if h.queue == nil {
		h.logIfEnabled(ctx, "notifyParentAfterAsyncSubAgent.queue_nil", map[string]any{
			"sub_session_id": subSession.ID.String(),
		})
		return
	}

	parentSessionID := subSession.ParentServerSessionID

	h.logIfEnabled(ctx, "notifyParentAfterAsyncSubAgent.start", map[string]any{
		"sub_session_id":    subSession.ID.String(),
		"parent_session_id": parentSessionID.String(),
		"status":            status,
	})

	// Build the notification text.
	var notificationText string
	switch status {
	case "completed":
		result := "(no output)"
		if lastMessage != nil {
			result = lastMessage.Content
		}
		notificationText = fmt.Sprintf(
			"[System Notification] The async sub agent task has completed.\n- Session ID: %s\n- Title: %s\n- Status: completed\n\nResult:\n%s",
			subSession.ID.String(),
			subSession.Title,
			result,
		)
	case "failed":
		errMsg := "(unknown error)"
		if errorMessage != nil {
			errMsg = *errorMessage
		}
		notificationText = fmt.Sprintf(
			"[System Notification] The async sub agent task has failed.\n- Session ID: %s\n- Title: %s\n- Status: failed\n\nError:\n%s",
			subSession.ID.String(),
			subSession.Title,
			errMsg,
		)
	case "cancelled":
		reason := "(no reason given)"
		if errorMessage != nil {
			reason = *errorMessage
		}
		notificationText = fmt.Sprintf(
			"[System Notification] The async sub agent task has been cancelled.\n- Session ID: %s\n- Title: %s\n- Status: cancelled\n\nReason:\n%s",
			subSession.ID.String(),
			subSession.Title,
			reason,
		)
	default:
		notificationText = fmt.Sprintf(
			"[System Notification] The async sub agent task has ended with status: %s.\n- Session ID: %s\n- Title: %s",
			status,
			subSession.ID.String(),
			subSession.Title,
		)
	}

	// Create the notification message in the parent session.
	notificationContent := protocol.ContentData{
		Type: protocol.ContentTypeText,
		Data: notificationText,
	}

	_, err := h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		msg, createErr := primitives.CreateMessage(
			txCtx, h.deps,
			parentSessionID, nil, // no turn ID — will be picked up by the next Submit
			protocol.MessageRoleSystem,
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
		h.logIfEnabled(ctx, "notifyParentAfterAsyncSubAgent.publish_failed", map[string]any{
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
		h.logIfEnabled(ctx, "notifyParentAfterAsyncSubAgent.marshal_failed", map[string]any{
			"parent_session_id": parentSessionID.String(),
			"error":             marshalErr.Error(),
		})
		return
	}

	if _, err := h.queue.Publish(ctx, parentSessionID.String(), string(payload), 0); err != nil {
		h.logIfEnabled(ctx, "notifyParentAfterAsyncSubAgent.submit_failed", map[string]any{
			"parent_session_id": parentSessionID.String(),
			"error":             err.Error(),
		})
		return
	}

	h.logIfEnabled(ctx, "notifyParentAfterAsyncSubAgent.done", map[string]any{
		"sub_session_id":    subSession.ID.String(),
		"parent_session_id": parentSessionID.String(),
		"status":            status,
	})
}

// updateSubAgentInvocationStatus updates the status field of a sub_agent_invocation message.
// This is used to track the sub agent's execution state (pending/running/failed/completed).
// When status is "completed", result should contain the sub agent's output.
// When status is "failed", errorMessage should contain the error details.
func (h *helpers) updateSubAgentInvocationStatus(ctx context.Context, messageID uuid.UUID, status string) {
	// Load the message.
	msg, err := h.deps.MessageRepo.GetByID(ctx, messageID)
	if err != nil {
		h.logIfEnabled(ctx, "updateSubAgentInvocationStatus.load_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Parse the content.
	var content protocol.ContentData
	if err := json.Unmarshal([]byte(msg.Content), &content); err != nil {
		h.logIfEnabled(ctx, "updateSubAgentInvocationStatus.parse_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Update the status, error_message, and result in the data map.
	if data, ok := content.Data.(map[string]any); ok {
		data["status"] = status
	} else {
		h.logIfEnabled(ctx, "updateSubAgentInvocationStatus.invalid_data_type", map[string]any{
			"message_id": messageID.String(),
		})
		return
	}

	// Serialize the updated content.
	updatedContent, err := json.Marshal(content)
	if err != nil {
		h.logIfEnabled(ctx, "updateSubAgentInvocationStatus.serialize_failed", map[string]any{
			"message_id": messageID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Update the message in DB.
	if err := h.deps.MessageRepo.UpdateStreamingStatus(ctx, messageID, protocol.MessageStreamingCompleted, string(updatedContent)); err != nil {
		h.logIfEnabled(ctx, "updateSubAgentInvocationStatus.update_failed", map[string]any{
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
			h.logIfEnabled(ctx, "updateSubAgentInvocationStatus.publish_failed", map[string]any{
				"message_id": messageID.String(),
				"error":      err.Error(),
			})
		}
	}

	h.logIfEnabled(ctx, "updateSubAgentInvocationStatus.done", map[string]any{
		"message_id": messageID.String(),
		"status":     status,
	})
}

// interruptTurn marks a turn as "interrupted" when eino raises a stateful
// interrupt (typically from an RTC tool calling tool.StatefulInterrupt).
//
// interruptID is the eino-assigned ID of the root interrupt context.
// interruptInfo is the opaque info passed to StatefulInterrupt (typically
// rtcInterruptInfo, but the concrete type is application-defined).
//
// After this callback returns, turn-agent's Process returns nil, rtc-queue
// completes the work, and the session lock is released. The application is
// responsible for publishing a Resume work item to rtc-queue when the
// external event resolves (e.g., when SubmitRtcResult is called).
//
// TODO: The interrupt handling in the old code also subscribed to pub/sub
// channels and waited for answers. In the new model, the application's
// interrupt handling (e.g., SubmitRtcResult) is responsible for publishing
// a Resume work item to rtc-queue. The subscribe-and-wait logic is no longer
// needed here because turn-agent manages the turn lifecycle.
func (h *helpers) interruptTurn(ctx context.Context, turnID string, interruptID string, interruptInfo any) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("interruptTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusInterrupted, ""); err != nil {
		return fmt.Errorf("interruptTurn: update status: %w", err)
	}

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "interruptTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// Set session status to "idle" — turn is paused waiting for external input.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusIdle); err != nil {
		h.logIfEnabled(ctx, "interruptTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "interrupt")

	h.logIfEnabled(ctx, "interruptTurn.done", map[string]any{
		"turn_id":      turnID,
		"interrupt_id": interruptID,
		"info_type":    fmt.Sprintf("%T", interruptInfo),
	})

	return nil
}

// resumeTurn marks an interrupted turn as "running" again.
//
// Called after LookupTurn, before eino's TurnLoop re-enters the tool that
// previously interrupted.
//
// Also sets the parent session's status back to "active" to indicate that
// a turn is executing again after an interrupt resolution.
func (h *helpers) resumeTurn(ctx context.Context, turnID string) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("resumeTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusRunning, ""); err != nil {
		return fmt.Errorf("resumeTurn: update status: %w", err)
	}

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "resumeTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// Set session status back to "active" — a turn is executing again.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusActive); err != nil {
		h.logIfEnabled(ctx, "resumeTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "resume")

	h.logIfEnabled(ctx, "resumeTurn.done", map[string]any{"turn_id": turnID})

	return nil
}

// failTurn marks a turn as "failed" with an error message.
//
// Called when the turn ends due to an unexpected error. The eino checkpoint
// may or may not be present depending on where the failure occurred.
func (h *helpers) failTurn(ctx context.Context, turnID string, turnErr error) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("failTurn: invalid turn ID %q: %w", turnID, err)
	}

	errMsg := ""
	if turnErr != nil {
		errMsg = turnErr.Error()
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusFailed, errMsg); err != nil {
		return fmt.Errorf("failTurn: update status: %w", err)
	}

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "failTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// Set session status to "idle" — turn ended due to an error.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusIdle); err != nil {
		h.logIfEnabled(ctx, "failTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "fail")

	h.logIfEnabled(ctx, "failTurn.done", map[string]any{
		"turn_id": turnID,
		"error":   errMsg,
	})

	// Sub Agent support: if this is a sub session, notify the parent.
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, turn.SessionID)
	if sessErr == nil && session.ParentServerSessionID != uuid.Nil {
		if session.SubAgentMode == "async" {
			errMsgPtr := &errMsg
			h.notifyParentAfterAsyncSubAgent(ctx, session, nil, "failed", errMsgPtr)
		} else {
			// Sync mode: create toolcall_output and resume parent from checkpoint.
			if session.SubAgentParentMessageID != uuid.Nil {
				errMsgPtr := &errMsg
				h.resumeParentAfterSubAgentNewToolCallOutput(ctx, session.SubAgentParentMessageID, "failed", errMsgPtr, nil)
			}
			h.resumeParentAfterSubAgent(ctx, session, nil)
		}
	}

	return nil
}

// cancelTurn marks a turn as "cancelled".
//
// Called when the turn is cancelled via rtc-queue's Cancel admin operation.
// reason carries the CancelMessage.Reason from the publisher.
func (h *helpers) cancelTurn(ctx context.Context, turnID string, reason string) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("cancelTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusCancelled, reason); err != nil {
		return fmt.Errorf("cancelTurn: update status: %w", err)
	}

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "cancelTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// Set session status to "idle" — turn was cancelled.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusIdle); err != nil {
		h.logIfEnabled(ctx, "cancelTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "cancel")

	h.logIfEnabled(ctx, "cancelTurn.done", map[string]any{
		"turn_id": turnID,
		"reason":  reason,
	})

	// Sub Agent support: if this is a sub session, notify the parent.
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, turn.SessionID)
	if sessErr == nil && session.ParentServerSessionID != uuid.Nil {
		if session.SubAgentMode == "async" {
			reasonPtr := &reason
			h.notifyParentAfterAsyncSubAgent(ctx, session, nil, "cancelled", reasonPtr)
		} else {
			// Sync mode: create toolcall_output and resume parent from checkpoint.
			if session.SubAgentParentMessageID != uuid.Nil {
				reasonPtr := &reason
				h.resumeParentAfterSubAgentNewToolCallOutput(ctx, session.SubAgentParentMessageID, "cancelled", reasonPtr, nil)
			}
			h.resumeParentAfterSubAgent(ctx, session, nil)
		}
	}

	return nil
}
