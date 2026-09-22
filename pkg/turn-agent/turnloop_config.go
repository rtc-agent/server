package turnagent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
)

// buildEinoConfig constructs the eino TurnLoopConfig for this manager.
func (mgr *SessionTurnManager) buildEinoConfig() adk.TurnLoopConfig[TurnWorkItem, *schema.Message] {
	return adk.TurnLoopConfig[TurnWorkItem, *schema.Message]{
		GenInput:      mgr.genInput,
		GenResume:     mgr.genResume(),
		PrepareAgent:  mgr.prepareAgent,
		OnAgentEvents: mgr.onAgentEvents,
		Store:         mgr.cfg.CheckpointStore,
		CheckpointID:  mgr.checkpointID,
	}
}

// genInput builds the initial input for a fresh turn by loading messages from
// the database and converting them to eino format.
func (mgr *SessionTurnManager) genInput(
	ctx context.Context,
	loop *adk.TurnLoop[TurnWorkItem, *schema.Message],
	items []TurnWorkItem,
) (*adk.GenInputResult[TurnWorkItem, *schema.Message], error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("turnagent: GenInput called with no items")
	}
	turnID := items[0].TurnID

	// Diagnostic: check if this is a resume work item that ended up in GenInput
	// (which means checkpoint was NOT found, so TurnLoop fell back to GenInput)
	var isResumeWork bool
	var resumeInterruptID string
	for _, item := range items {
		if item.Kind == WorkKindResume {
			isResumeWork = true
			resumeInterruptID = item.InterruptID
			break
		}
	}
	if isResumeWork {
		// UPGRADED to Error: Checkpoint loss during resume is a serious data
		// consistency issue. The LLM will see tool results in the DB but eino's
		// ToolNode won't know the tools were already executed, potentially causing
		// duplicate tool_result messages or incorrect state. This should trigger
		// monitoring alerts for immediate investigation.
		mgr.log(ctx, LogLevelError, "gen_input.checkpoint_lost_fallback", map[string]any{
			"session_id":    mgr.sessionID,
			"turn_id":       turnID,
			"interrupt_id":  resumeInterruptID,
			"checkpoint_id": mgr.checkpointID,
			"message":       "CRITICAL: Checkpoint NOT found during resume - falling back to fresh turn. This may cause tool result duplicates and state inconsistency. Investigate checkpoint store health immediately.",
			"action":        "Monitor for duplicate tool_results in LLM requests and checkpoint store errors",
		})
	}

	mgr.log(ctx, LogLevelInfo, "gen_input.start", map[string]any{
		"session_id":    mgr.sessionID,
		"turn_id":       turnID,
		"item_count":    len(items),
		"is_resume":     isResumeWork,
		"checkpoint_id": mgr.checkpointID,
	})

	ctx = WithSessionID(ctx, mgr.sessionID)
	ctx = WithTurnID(ctx, turnID)
	ctx = WithLoadSource(ctx, LoadSourceGenInput)

	if len(mgr.cfg.Callbacks) > 0 {
		ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{}, mgr.cfg.Callbacks...)
	}

	msgs, err := mgr.cfg.LoadMessages(ctx, mgr.sessionID)
	if err != nil {
		return nil, fmt.Errorf("turnagent: LoadMessages: %w", err)
	}
	if len(msgs) == 0 {
		mgr.log(ctx, LogLevelWarn, "turn.empty_messages", map[string]any{
			"session_id": mgr.sessionID,
			"turn_id":    turnID,
		})
	}

	return &adk.GenInputResult[TurnWorkItem, *schema.Message]{
		RunCtx: ctx,
		Input: &adk.TypedAgentInput[*schema.Message]{
			Messages:        toEinoMessages(msgs),
			EnableStreaming: true,
		},
		Consumed: items,
	}, nil
}

// genResume returns a GenResume callback that stores the cancel function on the
// manager so doCleanup can cancel it to prevent timer goroutine leaks.
func (mgr *SessionTurnManager) genResume() func(
	ctx context.Context,
	loop *adk.TurnLoop[TurnWorkItem, *schema.Message],
	interrupted, unhandled, newItems []TurnWorkItem,
) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
	return func(
		ctx context.Context,
		loop *adk.TurnLoop[TurnWorkItem, *schema.Message],
		interrupted, unhandled, newItems []TurnWorkItem,
	) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
		return mgr.genResumeImpl(interrupted, unhandled, newItems)
	}
}

// genResumeImpl builds the resume input for a checkpoint recovery.
func (mgr *SessionTurnManager) genResumeImpl(
	interrupted, unhandled, newItems []TurnWorkItem,
) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
	// Cancel the previous resume context to release its timer.
	mgr.resumeCancelMu.Lock()
	if mgr.resumeCancel != nil {
		mgr.resumeCancel()
	}

	var turnID string
	if len(newItems) > 0 {
		turnID = newItems[0].TurnID
	} else if len(interrupted) > 0 {
		turnID = interrupted[0].TurnID
	} else if len(unhandled) > 0 {
		turnID = unhandled[0].TurnID
	}

	// Create a fresh context for this resume attempt.
	//
	// IMPORTANT: No timeout is set here. The resume context becomes the RunCtx
	// for the entire subsequent agent execution (including LLM streaming),
	// which can last arbitrarily long for complex tasks. A fixed timeout would
	// kill active streams mid-output — the user-visible symptom is "响应超时"
	// even though chunks are still flowing.
	//
	// Cancellation is still possible: the caller's turnCtx (from Process) is
	// the parent of the TurnLoop's execution, and StopTurn / lock-loss / worker
	// shutdown all cancel that context. The resumeCancelMu + doCleanup path
	// handles timer goroutine lifecycle (see session_manager.go doCleanup Step 2b).
	resumeCtx, cancel := context.WithCancel(context.Background())
	mgr.resumeCancel = cancel
	mgr.resumeCancelMu.Unlock()

	mgr.log(resumeCtx, LogLevelInfo, "gen_resume.called", map[string]any{
		"session_id":        mgr.sessionID,
		"turn_id":           turnID,
		"checkpoint_id":     mgr.checkpointID,
		"interrupted_count": len(interrupted),
		"unhandled_count":   len(unhandled),
		"newItems_count":    len(newItems),
	})

	for i, item := range newItems {
		mgr.log(resumeCtx, LogLevelDebug, "gen_resume.newItem", map[string]any{
			"index":            i,
			"kind":             item.Kind,
			"turn_id":          item.TurnID,
			"work_id":          item.WorkID,
			"interrupt_id":     item.InterruptID,
			"has_result":       item.InterruptResult != nil,
			"sub_agent_result": item.SubAgentResult != nil,
		})
	}

	resumeCtx = WithSessionID(resumeCtx, mgr.sessionID)
	if turnID != "" {
		resumeCtx = WithTurnID(resumeCtx, turnID)
	}

	if len(mgr.cfg.Callbacks) > 0 {
		resumeCtx = callbacks.InitCallbacks(resumeCtx, &callbacks.RunInfo{}, mgr.cfg.Callbacks...)
	}

	allItems := make([]TurnWorkItem, 0, len(interrupted)+len(unhandled)+len(newItems))
	allItems = append(allItems, interrupted...)
	allItems = append(allItems, unhandled...)
	allItems = append(allItems, newItems...)

	// Build ResumeParams from the resume item's interrupt fields.
	// With batch resume: BatchResumeItems takes precedence over single fields.
	resumeParams := mgr.buildResumeParams(resumeCtx, newItems, turnID)

	if resumeParams == nil {
		mgr.log(resumeCtx, LogLevelWarn, "gen_resume.no_resume_params", map[string]any{
			"session_id":     mgr.sessionID,
			"turn_id":        turnID,
			"message":        "No InterruptID found in newItems - ResumeParams will be nil",
			"newItems_count": len(newItems),
		})
	}

	// Build RunOpts with HistoryModifier to reload messages from database.
	// This fixes the bug where user messages sent during turn execution
	// (while the turn was interrupted waiting for tool results) were not
	// included in the LLM context after checkpoint resume.
	//
	// Without this, genResume would use the checkpoint's message snapshot,
	// missing any new user messages added to the database during the interrupt.
	runOpts := []adk.AgentRunOption{
		adk.WithHistoryModifier(func(ctx context.Context, msgs []*schema.Message) []*schema.Message {
			// Mark this as a resume load so loadMessages can skip command detection
			// and prompt persistence (avoid duplicates on checkpoint resume).
			ctx = WithLoadSource(ctx, LoadSourceGenResume)

			// Extract pending tool call IDs from checkpoint messages.
			// "Pending" means: the checkpoint has an assistant tool_call but no
			// matching tool result. On resume, eino's ToolNode will re-invoke
			// the tool and create a tool result message. We must skip these
			// from DB loading to avoid duplicate tool_result messages.
			//
			// This replaces the old crude approach of skipping ALL toolcall_output
			// on resume, which incorrectly dropped non-RTC tool results.
			pendingIDs := extractPendingToolCallIDs(msgs)
			if len(pendingIDs) > 0 {
				ctx = WithPendingToolCallIDs(ctx, pendingIDs)
			}

			// Reload messages from database to pick up any new user messages
			// that were added while the turn was interrupted.
			freshMsgs, err := mgr.cfg.LoadMessages(ctx, mgr.sessionID)
			if err != nil {
				// Log error but return original messages to avoid breaking the turn.
				// The turn can still complete with stale messages; losing it would be worse.
				mgr.log(ctx, LogLevelWarn, "gen_resume.history_modifier_load_failed", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"error":      err.Error(),
					"message":    "Failed to reload messages during checkpoint resume; using checkpoint messages",
				})
				return msgs
			}

			// Convert turn-agent messages to eino schema messages.
			freshSchemaMsgs := toEinoMessages(freshMsgs)

			mgr.log(ctx, LogLevelInfo, "gen_resume.history_modified", map[string]any{
				"session_id":      mgr.sessionID,
				"turn_id":         turnID,
				"checkpoint_msgs": len(msgs),
				"reloaded_msgs":   len(freshSchemaMsgs),
				"message":         "Reloaded messages from database during checkpoint resume",
			})

			return freshSchemaMsgs
		}),
	}

	mgr.log(resumeCtx, LogLevelInfo, "gen_resume.result", map[string]any{
		"session_id":        mgr.sessionID,
		"turn_id":           turnID,
		"consumed_count":    len(allItems),
		"has_resume_params": resumeParams != nil,
		"interrupted_count": len(interrupted),
		"unhandled_count":   len(unhandled),
		"newItems_count":    len(newItems),
	})

	return &adk.GenResumeResult[TurnWorkItem, *schema.Message]{
		RunCtx:       resumeCtx,
		Consumed:     allItems,
		ResumeParams: resumeParams,
		RunOpts:      runOpts,
	}, nil
}

// buildResumeParams constructs ResumeParams from newItems, handling both
// batch resume and single interrupt cases.
func (mgr *SessionTurnManager) buildResumeParams(
	ctx context.Context,
	newItems []TurnWorkItem,
	turnID string,
) *adk.ResumeParams {
	var resumeParams *adk.ResumeParams
	for _, item := range newItems {
		// Check for batch resume items first
		if len(item.BatchResumeItems) > 0 {
			if resumeParams == nil {
				resumeParams = &adk.ResumeParams{Targets: map[string]any{}}
			}
			for _, batchItem := range item.BatchResumeItems {
				resumeParams.Targets[batchItem.InterruptID] = batchItem.Result
				mgr.log(ctx, LogLevelInfo, "gen_resume.batch_resume_item", map[string]any{
					"session_id":   mgr.sessionID,
					"turn_id":      turnID,
					"interrupt_id": batchItem.InterruptID,
				})
			}
			mgr.log(ctx, LogLevelInfo, "gen_resume.batch_resume_complete", map[string]any{
				"session_id": mgr.sessionID,
				"turn_id":    turnID,
				"item_count": len(item.BatchResumeItems),
			})
			continue
		}

		// Fall back to single interrupt
		if item.InterruptID != "" {
			if resumeParams == nil {
				resumeParams = &adk.ResumeParams{Targets: map[string]any{}}
			}
			var result any
			if item.InterruptResult != nil {
				result = *item.InterruptResult
			} else if item.SubAgentResult != nil {
				result = *item.SubAgentResult
			}
			resumeParams.Targets[item.InterruptID] = result
			mgr.log(ctx, LogLevelInfo, "gen_resume.resume_params", map[string]any{
				"session_id":   mgr.sessionID,
				"turn_id":      turnID,
				"interrupt_id": item.InterruptID,
				"has_result":   result != nil,
			})
		}
	}
	return resumeParams
}

// extractPendingToolCallIDs identifies tool call IDs that have assistant tool
// calls but no matching tool results in the given messages. These are the
// tool calls that eino's ToolNode will create results for during resume.
//
// During checkpoint resume, the eino framework's ToolNode re-invokes interrupted
// tools and creates tool result messages from their return values. If we also
// load these tool results from the database via HistoryModifier, we get duplicates.
// By identifying which tool calls are "pending" (no matching result in checkpoint),
// we can skip loading their results from DB and let eino create them.
func extractPendingToolCallIDs(msgs []*schema.Message) map[string]bool {
	// Collect all tool call IDs from assistant messages.
	allCallIDs := make(map[string]bool)
	for _, msg := range msgs {
		if msg.Role == schema.Assistant {
			for _, tc := range msg.ToolCalls {
				allCallIDs[tc.ID] = true
			}
		}
	}
	// Remove IDs that have matching tool results.
	for _, msg := range msgs {
		if msg.Role == schema.Tool && msg.ToolCallID != "" {
			delete(allCallIDs, msg.ToolCallID)
		}
	}
	return allCallIDs
}
