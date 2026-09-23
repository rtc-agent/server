package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// sendMessageToSubAgentTool enables the parent agent to send a message to an
// async sub-agent session as a user. The message is delivered as a user-role
// message and triggers a new turn in the sub-agent session.
//
// Unlike the subAgent tool (which creates a new sub session), this tool sends
// an additional message to an existing sub-agent session. Useful for:
//   - Providing additional instructions or context
//   - Redirecting the sub-agent's focus
//   - Responding to the sub-agent's questions
type sendMessageToSubAgentTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

// sendMessageToSubAgentArgs is the input schema for the sendMessageToSubAgent tool.
type sendMessageToSubAgentArgs struct {
	SubSessionID string `json:"sub_session_id"`
	Message      string `json:"message"`
}

func (t *sendMessageToSubAgentTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "sendMessageToSubAgent",
		Desc: sendMessageToSubAgentDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"sub_session_id": {
				Type:     schema.String,
				Desc:     "The ID of the target async sub-agent session.",
				Required: true,
			},
			"message": {
				Type:     schema.String,
				Desc:     "The message content to send to the sub-agent.",
				Required: true,
			},
		}),
	}, nil
}

func (t *sendMessageToSubAgentTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.sendMessageToSubAgent",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
			attribute.Int("args_length", len(argumentsInJSON)),
		),
	)
	defer span.End()

	var args sendMessageToSubAgentArgs
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "sendMessageToSubAgent", argumentsInJSON, &args); !ok {
		span.SetStatus(codes.Error, "parse_args_failed")
		return errMsg, nil
	}

	subSessionID, err := uuid.Parse(args.SubSessionID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid_sub_session_id")
		return fmt.Sprintf("Error: invalid sub_session_id %q: %v", args.SubSessionID, err), nil
	}

	if args.Message == "" {
		span.SetStatus(codes.Error, "empty_message")
		return "Error: message must not be empty", nil
	}

	// Validate: target session exists and is a child of the current session.
	subSession, err := t.helpers.deps.SessionRepo.GetByID(ctx, subSessionID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_sub_session_failed")
		return "", fmt.Errorf("sendMessageToSubAgent: get sub session: %w", err)
	}
	if subSession == nil {
		span.SetStatus(codes.Error, "sub_session_not_found")
		return fmt.Sprintf("Error: sub-agent session %s not found", args.SubSessionID), nil
	}

	// Verify parent-child relationship.
	if subSession.ParentClientSessionID != t.session.ClientID &&
		subSession.ParentServerSessionID != t.session.ID {
		span.SetStatus(codes.Error, "not_a_child_session")
		return fmt.Sprintf("Error: session %s is not a sub-agent of the current session", args.SubSessionID), nil
	}

	// Only support async sub-agents.
	if subSession.SubAgentMode != model.SubAgentModeAsync {
		span.SetStatus(codes.Error, "sync_mode_not_supported")
		return fmt.Sprintf("Error: sendMessageToSubAgent only supports async sub-agents, but session %s is in %q mode", args.SubSessionID, subSession.SubAgentMode), nil
	}

	// Check sub-session is not closed.
	if protocol.SessionStatus(subSession.Status) == protocol.SessionStatusClosed {
		span.SetStatus(codes.Error, "sub_session_closed")
		return fmt.Sprintf("Error: sub-agent session %s is closed", args.SubSessionID), nil
	}

	if t.helpers.queue == nil {
		span.SetStatus(codes.Error, "queue_unavailable")
		return "", fmt.Errorf("sendMessageToSubAgent: queue not available")
	}

	// Step 1: Create user-role message in the sub-agent session.
	userContent := protocol.ContentData{
		Type: protocol.ContentTypeText,
		Data: args.Message,
	}
	_, err = t.helpers.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		msg, createErr := primitives.CreateMessage(
			txCtx, t.helpers.deps,
			subSessionID, nil, // no turn ID — picked up by the next Submit
			protocol.MessageRoleUser,
			usecase.SystemCreator{},
			userContent,
			protocol.MessageStreamingCompleted,
			"",  // auto-generate client ID
			nil, // no parent message
		)
		if createErr != nil {
			return nil, fmt.Errorf("create user message in sub session: %w", createErr)
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
		span.RecordError(err)
		span.SetStatus(codes.Error, "create_message_failed")
		return "", fmt.Errorf("sendMessageToSubAgent: create message: %w", err)
	}

	// Step 2: Publish submit work item to sub-agent's rtc-queue.
	traceID, spanID := turnagent.ExtractTraceFromCtx(ctx)
	payload, err := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: subSessionID.String(),
		TraceID:   traceID,
		SpanID:    spanID,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal_work_failed")
		return "", fmt.Errorf("sendMessageToSubAgent: marshal work: %w", err)
	}
	if _, err := t.helpers.queue.Publish(ctx, subSessionID.String(), string(payload), rtcqueue.SubmitWorkPriority); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_work_failed")
		return "", fmt.Errorf("sendMessageToSubAgent: publish work: %w", err)
	}

	// Step 3: Record toolcall_input + toolcall_output in the parent session.
	result := fmt.Sprintf("Message sent to sub-agent session %s successfully.", args.SubSessionID)
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "sendMessageToSubAgent",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      result,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_tool_messages_failed")
		// Non-fatal: message was already delivered to sub-agent.
		t.helpers.logger.Warn(ctx, "sendMessageToSubAgent.publish_tool_messages_failed", map[string]any{
			"sub_session_id": args.SubSessionID,
			"error":          err.Error(),
		})
	}

	span.SetStatus(codes.Ok, "")
	t.helpers.logger.Info(ctx, "sendMessageToSubAgent.done", map[string]any{
		"sub_session_id": args.SubSessionID,
		"message_length": len(args.Message),
	})

	return result, nil
}
