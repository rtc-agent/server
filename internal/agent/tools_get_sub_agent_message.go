package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// getSubAgentMessageTool queries the last message of a specific sub agent session.
//
// Like listSubAgentTool, this tool does NOT interrupt the turn.
// It creates two messages (toolcall_input + toolcall_output) and
// publishes them in a single transaction, then returns the result
// directly to the LLM.
type getSubAgentMessageTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

// getSubAgentMessageArgs is the input schema for the getSubAgentMessage tool.
type getSubAgentMessageArgs struct {
	SubSessionID string `json:"sub_session_id"`
}

// getSubAgentMessageResult is the tool result.
type getSubAgentMessageResult struct {
	SubSessionID     string  `json:"sub_session_id"`
	SubSessionTitle  string  `json:"sub_session_title"`
	SubSessionStatus string  `json:"sub_session_status"`
	MessageID        *string `json:"message_id"`
	Role             *string `json:"role"`
	Content          string  `json:"content"`
	CreatedAt        *string `json:"created_at"`
}

func (t *getSubAgentMessageTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "getSubAgentMessage",
		Desc: getSubAgentMessageDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"sub_session_id": {
				Type:     schema.String,
				Desc:     "The server-side UUID of the sub agent session to query. Use listSubAgent to get available session IDs.",
				Required: true,
			},
		}),
	}, nil
}

func (t *getSubAgentMessageTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.getSubAgentMessage",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	// 1. Parse arguments.
	var args getSubAgentMessageArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "getSubAgentMessage", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	if args.SubSessionID == "" {
		return "Error: sub_session_id is required", nil
	}

	subSessionID, parseErr := uuid.Parse(args.SubSessionID)
	if parseErr != nil {
		return fmt.Sprintf("Error: invalid sub_session_id format: %s", parseErr.Error()), nil
	}
	span.SetAttributes(attribute.String("target_session_id", subSessionID.String()))

	// 2. Determine current root session ID.
	rootSessionID := t.session.ID
	if t.session.RootServerSessionID != uuid.Nil {
		rootSessionID = t.session.RootServerSessionID
	}

	// 3. Query target session.
	targetSession, dbErr := t.helpers.deps.SessionRepo.GetByID(ctx, subSessionID)
	if dbErr != nil {
		span.RecordError(dbErr)
		span.SetStatus(codes.Error, "session_lookup_failed")
		return fmt.Sprintf("Error: sub agent session not found: %s", dbErr.Error()), nil
	}

	// 4. Verify target is a descendant of the same session tree.
	if targetSession.RootServerSessionID != rootSessionID {
		span.SetStatus(codes.Error, "not_descendant")
		return "Error: target session is not a descendant of the current session tree", nil
	}
	if targetSession.ID == t.session.ID {
		span.SetStatus(codes.Error, "self_query")
		return "Error: cannot query the current session itself", nil
	}

	// 5. Query recent messages and find the last one.
	recentMsgs, msgErr := t.helpers.deps.MessageRepo.ListRecentBySession(ctx, subSessionID, 50)
	if msgErr != nil {
		span.RecordError(msgErr)
		span.SetStatus(codes.Error, "list_messages_failed")
		return fmt.Sprintf("Error: failed to list messages: %s", msgErr.Error()), nil
	}

	result := getSubAgentMessageResult{
		SubSessionID:     targetSession.ID.String(),
		SubSessionTitle:  targetSession.Title,
		SubSessionStatus: targetSession.Status,
	}

	if len(recentMsgs) > 0 {
		lastMsg := recentMsgs[len(recentMsgs)-1]

		// Resolve content (handles streaming messages by aggregating chunks from Redis).
		contentStr := t.helpers.deps.UpdatePublisher.ResolveMessageContent(lastMsg)
		var content protocol.ContentData
		if err := json.Unmarshal([]byte(contentStr), &content); err != nil {
			result.Content = "(failed to parse message content)"
		} else {
			result.Content = extractTextFromContent(content)
		}

		msgIDStr := lastMsg.ID.String()
		roleStr := lastMsg.Role
		createdAtStr := lastMsg.CreatedAt.Format(time.RFC3339)
		result.MessageID = &msgIDStr
		result.Role = &roleStr
		result.CreatedAt = &createdAtStr
	} else {
		result.Content = "No messages found in this session."
	}

	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal_failed")
		return "", fmt.Errorf("getSubAgentMessage: marshal result: %w", err)
	}

	// 6. Publish toolcall_input + toolcall_output messages.
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "getSubAgentMessage",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_messages_failed")
		return "", fmt.Errorf("getSubAgentMessage: publish messages: %w", err)
	}

	span.SetAttributes(attribute.Bool("has_message", result.MessageID != nil))
	t.helpers.logger.Info(ctx, "getSubAgentMessage.completed", map[string]any{
		"session_id":     t.session.ID.String(),
		"target_session": subSessionID.String(),
		"has_message":    result.MessageID != nil,
	})

	return resultJSON, nil
}
