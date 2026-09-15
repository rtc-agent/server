package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
)

// stopSubAgentTool stops a specific sub agent session and all its descendants.
//
// Like listSubAgentTool and getSubAgentMessageTool, this tool does NOT interrupt
// the turn. It creates two messages (toolcall_input + toolcall_output) and
// publishes them in a single transaction, then returns the result directly
// to the LLM.
//
// The stopping process:
//  1. Permission check: verify the target is a descendant of the current session tree.
//  2. Query all active descendants via ListByRoot.
//  3. Build parent→children map, DFS to find target + its descendants.
//  4. Stop each session (leaves first): CancelSession + close session.
//  5. CancelSession triggers cancelTurn callback → (enhanced) resume parent session.
type stopSubAgentTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

// stopSubAgentArgs is the input schema for the stop_sub_agent tool.
type stopSubAgentArgs struct {
	SubSessionID string `json:"sub_session_id"`
}

// stopSubAgentResult is the tool result.
type stopSubAgentResult struct {
	StoppedSessions []stoppedSession `json:"stopped_sessions"`
	TotalStopped    int              `json:"total_stopped"`
}

type stoppedSession struct {
	SubSessionID string `json:"sub_session_id"`
	Title        string `json:"title"`
}

func (t *stopSubAgentTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "stop_sub_agent",
		Desc: stopSubAgentDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"sub_session_id": {
				Type:     schema.String,
				Desc:     "The server-side UUID of the sub agent session to stop. Use list_sub_agent to get available session IDs.",
				Required: true,
			},
		}),
	}, nil
}

func (t *stopSubAgentTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	// 1. Parse arguments.
	var args stopSubAgentArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "stop_sub_agent", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	if args.SubSessionID == "" {
		return "Error: sub_session_id is required", nil
	}

	subSessionID, parseErr := uuid.Parse(args.SubSessionID)
	if parseErr != nil {
		return fmt.Sprintf("Error: invalid sub_session_id format: %s", parseErr.Error()), nil
	}

	// 2. Determine current root session ID.
	rootSessionID := t.session.ID
	if t.session.RootServerSessionID != uuid.Nil {
		rootSessionID = t.session.RootServerSessionID
	}

	// 3. Query target session.
	targetSession, dbErr := t.helpers.deps.SessionRepo.GetByID(ctx, subSessionID)
	if dbErr != nil {
		return fmt.Sprintf("Error: sub agent session not found: %s", dbErr.Error()), nil
	}

	// 4. Verify target is a descendant of the same session tree.
	if targetSession.RootServerSessionID != rootSessionID {
		return "Error: target session is not a descendant of the current session tree", nil
	}
	if targetSession.ID == t.session.ID {
		return "Error: cannot stop the current session itself", nil
	}

	// 5. Query all active descendants of the root session.
	allActive, err := t.helpers.deps.SessionRepo.ListByRoot(ctx, rootSessionID, string(protocol.SessionStatusActive))
	if err != nil {
		return "", fmt.Errorf("stop_sub_agent: list by root: %w", err)
	}

	// 6. Build parent→children map and DFS to find target + its descendants.
	childrenMap := make(map[uuid.UUID][]*model.Session)
	for _, s := range allActive {
		childrenMap[s.ParentServerSessionID] = append(childrenMap[s.ParentServerSessionID], s)
	}

	// Collect target and all its descendants (DFS).
	toStop := make([]*model.Session, 0)
	var dfs func(sessionID uuid.UUID)
	dfs = func(sessionID uuid.UUID) {
		for _, child := range childrenMap[sessionID] {
			toStop = append(toStop, child)
			dfs(child.ID)
		}
	}
	// Start DFS from target session.
	toStop = append(toStop, targetSession)
	dfs(targetSession.ID)

	// 7. Reverse to get leaves-first order (bottom-up).
	for i, j := 0, len(toStop)-1; i < j; i, j = i+1, j-1 {
		toStop[i], toStop[j] = toStop[j], toStop[i]
	}

	// 8. Stop each session: CancelSession + close session.
	stopped := make([]stoppedSession, 0, len(toStop))
	for _, s := range toStop {
		// StopActiveTurns handles CancelSession + turn cleanup + publish.
		primitives.StopActiveTurns(ctx, t.helpers.deps, t.helpers.queue, s.ID, "stopped_by_parent")

		// Explicitly close the session.
		if _, pubErr := t.helpers.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
			if err := primitives.UpdateSessionStatus(txCtx, t.helpers.deps, s.ID, protocol.SessionStatusClosed); err != nil {
				return nil, err
			}
			// Delete session memories (physical delete; they have served their purpose).
			if err := t.helpers.deps.SessionMemoryRepo.DeleteBySession(txCtx, s.ID); err != nil {
				t.helpers.logger.Info(ctx, "stopSubAgent.delete_memories_failed", map[string]any{
					"session_id": s.ID.String(),
					"error":      err.Error(),
				})
			}
			return primitives.BuildSessionCloseUpdates(s), nil
		}); pubErr != nil {
			t.helpers.logger.Info(ctx, "stopSubAgent.close_session_failed", map[string]any{
				"session_id": s.ID.String(),
				"error":      pubErr.Error(),
			})
		}

		stopped = append(stopped, stoppedSession{
			SubSessionID: s.ID.String(),
			Title:        s.Title,
		})

		t.helpers.logger.Info(ctx, "stopSubAgent.stopped", map[string]any{
			"session_id": s.ID.String(),
			"title":      s.Title,
		})
	}

	// 9. Build result JSON.
	result := stopSubAgentResult{
		StoppedSessions: stopped,
		TotalStopped:    len(stopped),
	}
	resultJSON := mustMarshalJSON(result)

	// 10. Publish toolcall_input + toolcall_output messages.
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "stop_sub_agent",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		return "", fmt.Errorf("stop_sub_agent: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "stopSubAgent.completed", map[string]any{
		"session_id":     t.session.ID.String(),
		"target_session": subSessionID.String(),
		"total_stopped":  len(stopped),
	})

	return resultJSON, nil
}
