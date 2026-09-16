package turnagent

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
)

// buildEinoConfig constructs the eino TurnLoopConfig for this manager.
func (mgr *SessionTurnManager) buildEinoConfig() adk.TurnLoopConfig[TurnWorkItem, *schema.Message] {
	// prevResumeCancel tracks the cancel function from the most recent GenResume call.
	// Each GenResume creates a fresh context.WithTimeout; the previous one must be
	// cancelled to release its timer resources.
	var prevResumeCancel context.CancelFunc

	return adk.TurnLoopConfig[TurnWorkItem, *schema.Message]{
		GenInput:      mgr.genInput,
		GenResume:     mgr.genResume(&prevResumeCancel),
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
		mgr.log(ctx, LogLevelWarn, "gen_input.resume_work_fallback", map[string]any{
			"session_id":    mgr.sessionID,
			"turn_id":       turnID,
			"interrupt_id":  resumeInterruptID,
			"message":       "Resume work item in GenInput - checkpoint was NOT found, starting fresh turn",
			"checkpoint_id": mgr.checkpointID,
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

// genResume returns a GenResume callback that captures prevResumeCancel by
// pointer so it can cancel the previous resume context on each new call.
func (mgr *SessionTurnManager) genResume(prevResumeCancel *context.CancelFunc) func(
	ctx context.Context,
	loop *adk.TurnLoop[TurnWorkItem, *schema.Message],
	interrupted, unhandled, newItems []TurnWorkItem,
) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
	return func(
		ctx context.Context,
		loop *adk.TurnLoop[TurnWorkItem, *schema.Message],
		interrupted, unhandled, newItems []TurnWorkItem,
	) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
		return mgr.genResumeImpl(ctx, interrupted, unhandled, newItems, prevResumeCancel)
	}
}

// genResumeImpl builds the resume input for a checkpoint recovery.
func (mgr *SessionTurnManager) genResumeImpl(
	ctx context.Context,
	interrupted, unhandled, newItems []TurnWorkItem,
	prevResumeCancel *context.CancelFunc,
) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
	// Cancel the previous resume context to release its timer.
	if *prevResumeCancel != nil {
		(*prevResumeCancel)()
	}

	var turnID string
	if len(newItems) > 0 {
		turnID = newItems[0].TurnID
	} else if len(interrupted) > 0 {
		turnID = interrupted[0].TurnID
	} else if len(unhandled) > 0 {
		turnID = unhandled[0].TurnID
	}

	// Create a fresh context with timeout for this resume attempt.
	resumeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	*prevResumeCancel = cancel

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
