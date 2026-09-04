package agent

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	"sort"
	"strings"
	"sync"
	"time"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func init() {
	// eino's checkpoint uses encoding/gob to serialize interrupt state.
	// Concrete types stored in interface{} must be registered via gob.Register,
	// otherwise serialization fails.
	//
	// rtcInterruptState: StatefulInterrupt's state parameter -> InterruptState.State
	// rtcInterruptInfo:  StatefulInterrupt's info parameter  -> InterruptCtx.Info
	//
	// Mapping from old code: identical to the gob.Register calls in
	// internal/worker/tools.rtc.go's init().
	gob.Register(rtcInterruptState{})
	gob.Register(rtcInterruptInfo{})
}

// =============================================================================
// Data callbacks
// =============================================================================
//
// These four callbacks handle the data flow between the agent and the
// application. They use turn-agent's own types (Message, Event) — the
// application never writes code that consumes eino's flow primitives.

// loadMessages loads the conversation history for a session from the DB and
// converts it to turn-agent Message types.
//
// Called by eino's GenInput path (fresh turns only — eino does not call
// GenInput when resuming from a checkpoint).
//
// Mapping from old code: this replaces loadMessageHistory in
// internal/worker/context.go. The key difference is the return type:
// []*turnagent.Message instead of []*schema.Message. The conversion from
// model.Message to turnagent.Message mirrors the old conversion to
// schema.Message, but uses the pkg-level types.
func (h *helpers) loadMessages(ctx context.Context, sessionID string) ([]*turnagent.Message, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return nil, fmt.Errorf("loadMessages: invalid session ID %q: %w", sessionID, err)
	}

	const historyLimit = 200
	dbMsgs, err := h.deps.MessageRepo.ListRecentBySession(ctx, sid, historyLimit)
	if err != nil {
		return nil, fmt.Errorf("loadMessages: list messages for session %s: %w", sessionID, err)
	}

	// Find the most recent summary message (if any) and truncate the history
	// to start from it. Messages before the summary are already compressed
	// into it and should not be sent to the agent.
	//
	// ListRecentBySession returns messages in ASC order (oldest first within
	// the returned set), so we iterate backwards to find the first summary.
	tmpMsgs := make([]*model.Message, 0, len(dbMsgs))
	for i := len(dbMsgs) - 1; i >= 0; i-- {
		msg := dbMsgs[i]
		tmpMsgs = append(tmpMsgs, msg)
		contentData, parseErr := primitives.ParseContentData(msg.Content)
		if parseErr == nil && contentData.Type == protocol.ContentTypeSummary {
			break
		}
	}
	// Reverse to get chronological order (oldest first).
	sort.Slice(tmpMsgs, func(i, j int) bool {
		return tmpMsgs[i].GlobalOffset < tmpMsgs[j].GlobalOffset
	})
	dbMsgs = tmpMsgs

	// Convert DB messages to turn-agent Messages.
	// The conversion logic mirrors the old SchemaMessages method in context.go,
	// but produces turnagent.Message instead of schema.Message.
	var messages []*turnagent.Message
	for _, msg := range dbMsgs {
		converted := convertDBMessage(msg)
		messages = append(messages, converted...)
	}

	return messages, nil
}

// convertDBMessage converts a single model.Message to zero or more
// turnagent.Messages.
//
// A single DB message may produce multiple turnagent messages (e.g., a
// toolcall_input produces an assistant message with tool calls; a summary
// may expand into multiple messages).
func convertDBMessage(msg *model.Message) []*turnagent.Message {
	contentData, err := primitives.ParseContentData(msg.Content)
	if err != nil {
		return nil
	}

	switch contentData.Type {
	case protocol.ContentTypeSummary:
		return convertSummaryContent(contentData.Data)

	case protocol.ContentTypeText, protocol.ContentTypeMarkdown:
		text, _ := primitives.ContentDataString(contentData.Data)
		return []*turnagent.Message{{
			Role:    msg.Role,
			Content: text,
		}}

	case protocol.ContentTypeThinking:
		tk, _ := primitives.ContentDataString(contentData.Data)
		return []*turnagent.Message{{
			Role:             msg.Role,
			ReasoningContent: tk,
		}}

	case protocol.ContentTypeToolCallInput:
		toolCall, err := primitives.ParseContentDataToolCall(contentData.Data)
		if err != nil {
			return nil
		}
		// Produce an assistant message with tool calls.
		return []*turnagent.Message{{
			Role: string(schema.Assistant),
			ToolCalls: []turnagent.ToolCall{{
				ID:        string(toolCall.Id),
				Name:      toolCall.ToolName,
				Arguments: toolCall.Input,
			}},
		}}

	case protocol.ContentTypeToolCallOutput:
		toolCall, err := primitives.ParseContentDataToolCall(contentData.Data)
		if err != nil {
			return nil
		}
		content := formatToolCallOutput(toolCall)
		return []*turnagent.Message{{
			Role:       string(schema.Tool),
			Content:    content,
			ToolName:   toolCall.ToolName,
			ToolCallID: string(toolCall.Id),
		}}

	default:
		return nil
	}
}

// convertSummaryContent expands a summary content block into multiple messages.
func convertSummaryContent(data any) []*turnagent.Message {
	dataBytes, err := primitives.ContentDataBytes(data)
	if err != nil {
		return nil
	}
	var items []primitives.SummaryItem
	if err := json.Unmarshal(dataBytes, &items); err != nil {
		return nil
	}
	var msgs []*turnagent.Message
	for _, item := range items {
		msgs = append(msgs, &turnagent.Message{
			Role:    item.Role,
			Content: item.Content,
		})
	}
	return msgs
}

// formatToolCallOutput formats a tool call's output for injection into the
// agent's context. Mirrors the old formatToolCallOutput in context.go.
func formatToolCallOutput(toolCall protocol.ToolCall) string {
	output := ""
	if toolCall.Output != nil {
		output = *toolCall.Output
	}
	status := ""
	if toolCall.Status != nil {
		status = *toolCall.Status
	}

	switch protocol.RtcStatus(status) {
	case protocol.RtcStatusCompleted:
		return output
	case protocol.RtcStatusFailed:
		return fmt.Sprintf("[Tool Error] %s failed: %s", toolCall.ToolName, output)
	case protocol.RtcStatusTimeout:
		return fmt.Sprintf("[Tool Timeout] %s timed out", toolCall.ToolName)
	case protocol.RtcStatusRejected:
		return fmt.Sprintf("[Tool Rejected] %s was rejected by user", toolCall.ToolName)
	default:
		return fmt.Sprintf("[Tool Pending] %s is still %s", toolCall.ToolName, status)
	}
}

// createTools creates the tool list for a session's turn.
//
// turnID is provided so the implementation can inject it into the tool's
// context (e.g., so tool.InvokableRun can read it via context.Value).
//
// Mapping from old code: this replaces createTools in
// internal/worker/tools.rtc.go. The key difference is the callback signature:
// it takes turnID as a parameter (so the tool can be turn-aware) instead of
// getting it from a session-level context value.
//
// The tools (lsTool, readTool, etc.) are defined in this package with their
// own rtcToolBase type, adapted to the new turnID-passing pattern.
func (h *helpers) createTools(ctx context.Context, sessionID string, turnID string) ([]tool.BaseTool, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return nil, fmt.Errorf("createTools: invalid session ID %q: %w", sessionID, err)
	}
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return nil, fmt.Errorf("createTools: invalid turn ID %q: %w", turnID, err)
	}

	session, err := h.deps.SessionRepo.GetByID(ctx, sid)
	if err != nil {
		return nil, fmt.Errorf("createTools: get session %s: %w", sessionID, err)
	}

	// Create RTC tool base with session and turn awareness.
	// The BaseRTC needs access to the helpers (for DB operations) and the
	// current turn ID (for creating RTC records tied to the correct turn).
	base := &rtcToolBase{
		session: session,
		helpers: h,
		turnID:  tid,
	}

	tools := []tool.BaseTool{
		&lsTool{base: base},
		&readTool{base: base},
		&writeTool{base: base},
		&grepTool{base: base},
		&findTool{base: base},
		&scriptTool{base: base},
	}

	return tools, nil
}

// createAgent builds the eino agent from the session's tools.
//
// Mapping from old code: this replaces createAgent in internal/worker/agent.go.
// The key difference is that the summarization middleware is injected via
// Config.AgentMiddlewares (set up in New) rather than being created inline.
//
// SessionID threading: the context is wrapped with sessionID via
// withSessionID so that the summarization middleware's callbacks
// (CompressContext, OnCompress) can extract it via getSessionIDFromContext.
// This is necessary because the middleware does not receive sessionID
// natively — it only sees the context passed through the agent execution.
func (h *helpers) createAgent(ctx context.Context, sessionID string, turnID string, tools []tool.BaseTool) (adk.Agent, error) {
	// Validate required dependencies
	if h.deps.ChatModel == nil {
		return nil, fmt.Errorf("createAgent: ChatModel is nil (LLM not configured)")
	}
	if h.deps.SystemPrompt == "" {
		return nil, fmt.Errorf("createAgent: SystemPrompt is empty")
	}

	// Inject sessionID into context for summarization middleware callbacks.
	sid, parseErr := uuid.Parse(sessionID)
	if parseErr != nil {
		return nil, fmt.Errorf("createAgent: invalid session ID %q: %w", sessionID, parseErr)
	}
	ctx = withSessionID(ctx, sid)

	// Build handlers list, filtering out nil middleware
	var handlers []adk.ChatModelAgentMiddleware
	if h.summarizeMW != nil {
		handlers = append(handlers, h.summarizeMW)
	}

	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        fmt.Sprintf("session-%s", sessionID),
		Description: "RTC Agent session handler",
		Instruction: h.deps.SystemPrompt,
		Model:       h.deps.ChatModel,
		Handlers:    handlers,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: tools,
				UnknownToolsHandler: func(ctx context.Context, name, input string) (string, error) {
					return fmt.Sprintf("<system>\nExit with code: 404, tool %s not found\n</system>", name), nil
				},
				ExecuteSequentially: true,
				ToolCallMiddlewares: nil,
			},
			ReturnDirectly:     nil,
			EmitInternalEvents: false,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("createAgent: create chat model agent: %w", err)
	}

	return agent, nil
}

// publishEvent handles flattened events from turn-agent's event stream.
//
// This is called once per event from the agent's execution. The pkg handles
// all eino stream consumption internally — this callback receives already-
// flattened events (chunks, end markers, complete messages, errors).
//
// Mapping from old code: this replaces the ~185-line handleEvents function
// in internal/worker/session_actor.go. The old code consumed eino's stream
// objects directly; the new code receives pre-processed events and handles
// them with a simple switch on EventKind.
//
// Streaming state (tracking the current streaming message row) is maintained
// per-turn via the streamState map, keyed by turnID.
func (h *helpers) publishEvent(ctx context.Context, sessionID string, turnID string, event *turnagent.Event) error {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return fmt.Errorf("publishEvent: invalid session ID %q: %w", sessionID, err)
	}
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("publishEvent: invalid turn ID %q: %w", turnID, err)
	}

	switch event.Kind {
	case turnagent.EventKindStreamChunk:
		return h.handleStreamChunk(ctx, sid, tid, event)
	case turnagent.EventKindStreamEnd:
		return h.handleStreamEnd(ctx, sid, tid, event)
	case turnagent.EventKindMessage:
		return h.handleMessage(ctx, sid, tid, event)
	case turnagent.EventKindError:
		return h.handleEventError(ctx, sid, tid, event)
	default:
		h.logIfEnabled(ctx, "publishEvent.unknown_kind", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"kind":       string(event.Kind),
		})
		return nil
	}
}

// =============================================================================
// Event handlers (internal)
// =============================================================================

// handleStreamChunk processes a single streaming chunk.
//
// For the first chunk of a stream, a new message record is created with
// streaming_status=streaming. For subsequent chunks, the content is appended
// to the Redis buffer. On the final chunk (FinishReason != ""), the message
// is finalized: all chunks are read from Redis, concatenated, and written
// to the DB with streaming_status=completed.
//
// This mirrors the old publishStreamChunkCore in stream_message.go.
func (h *helpers) handleStreamChunk(ctx context.Context, sessionID uuid.UUID, turnID uuid.UUID, event *turnagent.Event) error {
	state := h.streamState.getOrCreate(turnID.String())

	// Handle markdown content.
	if event.Content != "" {
		if err := h.appendStreamChunk(ctx, sessionID, turnID, event.Content, event.FinishReason, &state.markdownMsgID, &state.markdownFinalized, primitives.MarkdownContentData, "markdown"); err != nil {
			return err
		}
	}

	// Handle reasoning/thinking content.
	if event.ReasoningContent != "" {
		if err := h.appendStreamChunk(ctx, sessionID, turnID, event.ReasoningContent, event.FinishReason, &state.thinkingMsgID, &state.thinkingFinalized, primitives.ThinkingContentData, "thinking"); err != nil {
			return err
		}
	}

	// When markdown content arrives and thinking hasn't been finalized,
	// finalize the thinking message (thinking and content are mutually
	// exclusive phases).
	if event.Content != "" && state.thinkingMsgID != uuid.Nil && !state.thinkingFinalized {
		if err := h.finalizeStreamMessage(ctx, sessionID, turnID, &state.thinkingMsgID, primitives.ThinkingContentData, "thinking"); err != nil {
			return err
		}
		state.thinkingFinalized = true
	}

	// Finalize markdown if FinishReason is set.
	// IMPORTANT: If FinishReason is set but Content is empty, we still need to
	// call appendStreamChunk (via finalizeStreamMessage) to trigger the finalization
	// path that updates the DB status to "completed".
	if event.FinishReason != "" && state.markdownMsgID != uuid.Nil && !state.markdownFinalized {
		if event.Content == "" {
			// No content in this chunk, but we need to finalize the stream
			if err := h.finalizeStreamMessage(ctx, sessionID, turnID, &state.markdownMsgID, primitives.MarkdownContentData, "markdown"); err != nil {
				return err
			}
		}
		state.markdownFinalized = true
	}
	if event.FinishReason != "" && state.thinkingMsgID != uuid.Nil && !state.thinkingFinalized {
		if event.ReasoningContent == "" {
			// No reasoning content in this chunk, but we need to finalize the stream
			if err := h.finalizeStreamMessage(ctx, sessionID, turnID, &state.thinkingMsgID, primitives.ThinkingContentData, "thinking"); err != nil {
				return err
			}
		}
		state.thinkingFinalized = true
	}

	return nil
}

// appendStreamChunk handles one chunk of a streaming response.
//
// On the first chunk (msgID == uuid.Nil): create a message record with
// streaming_status=streaming, publish message.created, and start buffering
// chunks in Redis.
//
// On subsequent chunks: append to the Redis buffer and publish
// message.updated to the live channel (real-time, no persistence).
//
// On the final chunk (finishReason != ""): read all chunks from Redis,
// concatenate, update the DB with the full content, delete Redis buffer,
// and publish message.updated to the topic channel.
func (h *helpers) appendStreamChunk(
	ctx context.Context,
	sessionID uuid.UUID,
	turnID uuid.UUID,
	chunkContent string,
	finishReason string,
	msgID *uuid.UUID,
	finalized *bool,
	buildContent func(string) (protocol.ContentData, error),
	kind string,
) error {
	if h.deps.UpdatePublisher == nil {
		return nil
	}

	session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("appendStreamChunk: get session: %w", err)
	}
	topicCh := channel.UserTopic(session.OwnerRefID)
	liveCh := channel.UserLive(session.OwnerRefID)

	isFirst := *msgID == uuid.Nil
	isLast := finishReason != ""

	// First chunk: create message record.
	if isFirst {
		emptyContent, contentErr := buildContent("")
		if contentErr != nil {
			return fmt.Errorf("appendStreamChunk: create empty content: %w", contentErr)
		}
		newMsg, createErr := h.createAndPublishMessage(ctx, sessionID, turnID,
			protocol.MessageRoleAssistant, emptyContent,
			protocol.MessageStreamingStreaming, topicCh)
		if createErr != nil {
			return fmt.Errorf("appendStreamChunk: create message: %w", createErr)
		}
		*msgID = newMsg.ID
	}

	msgIDStr := msgID.String()

	// Append chunk to Redis buffer.
	streamStore := h.getStreamStore()
	if streamStore != nil {
		if _, appendErr := streamStore.AppendChunk(msgIDStr, chunkContent); appendErr != nil {
			h.logIfEnabled(ctx, "appendStreamChunk.redis_append_failed", map[string]any{
				"message_id": msgIDStr,
				"error":      appendErr.Error(),
			})
		}
	}

	// Not last chunk: publish to live channel (real-time update).
	if !isLast {
		if _, pubErr := h.deps.UpdatePublisher.Publish(ctx, updates.UpdatePublishItem{
			Channel: liveCh,
			Items: []protocol.UpdateItem{
				{Entity: protocol.EntityMessage, Action: protocol.ActionUpdated, EntityId: protocol.UUID(msgIDStr)},
			},
		}); pubErr != nil {
			h.logIfEnabled(ctx, "appendStreamChunk.live_publish_failed", map[string]any{
				"message_id": msgIDStr,
				"error":      pubErr.Error(),
			})
		}
		return nil
	}

	// Last chunk: finalize.
	// 1. Read all chunks from Redis and concatenate.
	var fullContent string
	if streamStore != nil {
		chunks, readErr := streamStore.GetAllChunks(msgIDStr)
		if readErr != nil {
			return fmt.Errorf("appendStreamChunk: get all chunks: %w", readErr)
		}
		fullContent = strings.Join(chunks, "")
	} else {
		fullContent = chunkContent
	}

	// 2. Serialize final content as ContentData JSON.
	finalContentData, contentErr := buildContent(fullContent)
	if contentErr != nil {
		return fmt.Errorf("appendStreamChunk: build final content: %w", contentErr)
	}
	serializedContent, serializeErr := primitives.SerializeContentData(finalContentData)
	if serializeErr != nil {
		return fmt.Errorf("appendStreamChunk: serialize final content: %w", serializeErr)
	}

	// 3. Update DB + publish message.updated to topic channel.
	if _, pubErr := h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := h.deps.MessageRepo.UpdateStreamingStatus(txCtx, *msgID, protocol.MessageStreamingCompleted, serializedContent); err != nil {
			return nil, fmt.Errorf("update streaming status: %w", err)
		}
		return []updates.UpdatePublishItem{{
			Channel: topicCh,
			Items: []protocol.UpdateItem{
				{Entity: protocol.EntityMessage, Action: protocol.ActionUpdated, EntityId: protocol.UUID(msgIDStr)},
			},
		}}, nil
	}); pubErr != nil {
		return fmt.Errorf("appendStreamChunk: run and publish: %w", pubErr)
	}

	// 4. Delete Redis chunks.
	if streamStore != nil {
		if delErr := streamStore.DeleteChunks(msgIDStr); delErr != nil {
			h.logIfEnabled(ctx, "appendStreamChunk.redis_delete_failed", map[string]any{
				"message_id": msgIDStr,
				"error":      delErr.Error(),
			})
		}
	}

	*finalized = true
	return nil
}

// handleStreamEnd processes a stream-end event.
//
// Finalizes any pending streaming messages that haven't received a
// FinishReason chunk (e.g., thinking messages that ended when content
// started arriving).
func (h *helpers) handleStreamEnd(ctx context.Context, sessionID uuid.UUID, turnID uuid.UUID, event *turnagent.Event) error {
	state := h.streamState.getOrCreate(turnID.String())

	// Finalize thinking if pending.
	if state.thinkingMsgID != uuid.Nil && !state.thinkingFinalized {
		if err := h.finalizeStreamMessage(ctx, sessionID, turnID, &state.thinkingMsgID, primitives.ThinkingContentData, "thinking"); err != nil {
			return err
		}
		state.thinkingFinalized = true
	}

	// Finalize markdown if pending.
	if state.markdownMsgID != uuid.Nil && !state.markdownFinalized {
		if err := h.finalizeStreamMessage(ctx, sessionID, turnID, &state.markdownMsgID, primitives.MarkdownContentData, "markdown"); err != nil {
			return err
		}
		state.markdownFinalized = true
	}

	// Clean up per-turn state.
	h.streamState.remove(turnID.String())

	return nil
}

// handleMessage processes a complete, non-streaming message.
//
// For assistant messages: if the message has ReasoningContent, create a
// thinking message first, then create the markdown message.
//
// For tool messages: skip (tool messages are handled by the tool itself).
//
// Batching: when both thinking and main messages are created, they are created
// in a single RunAndPublish transaction with a single batch centrifuge publish,
// reducing Redis offset increments, DB round-trips, and centrifuge calls from
// 2 transactions to 1.
func (h *helpers) handleMessage(ctx context.Context, sessionID uuid.UUID, turnID uuid.UUID, event *turnagent.Event) error {
	if event.Message == nil {
		return nil
	}

	// Skip tool messages — they are created by the tool itself.
	if event.Message.Role == string(schema.Tool) {
		return nil
	}

	if h.deps.UpdatePublisher == nil {
		return nil
	}

	session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("handleMessage: get session: %w", err)
	}
	ch := channel.UserTopic(session.OwnerRefID)

	hasThinking := event.Message.ReasoningContent != ""

	// Prepare content for both message types up front (before the transaction).
	var thinkingContent protocol.ContentData
	if hasThinking {
		thinkingContent, err = primitives.ThinkingContentData(event.Message.ReasoningContent)
		if err != nil {
			return fmt.Errorf("handleMessage: create thinking content: %w", err)
		}
	}
	markdownContent, err := primitives.MarkdownContentData(event.Message.Content)
	if err != nil {
		return fmt.Errorf("handleMessage: create markdown content: %w", err)
	}

	// Build the list of messages to create in a single batch transaction.
	messagesToCreate := make([]primitives.MessageToCreate, 0, 2)
	if hasThinking {
		messagesToCreate = append(messagesToCreate, primitives.MessageToCreate{
			Role:    protocol.MessageRoleAssistant,
			Creator: usecase.SystemCreator{},
			Content: thinkingContent,
			Status:  protocol.MessageStreamingCompleted,
		})
	}
	messagesToCreate = append(messagesToCreate, primitives.MessageToCreate{
		Role:    protocol.MessageRoleAssistant,
		Creator: usecase.SystemCreator{},
		Content: markdownContent,
		Status:  protocol.MessageStreamingCompleted,
	})

	// Single RunAndPublish: create all messages in one DB batch and publish
	// all message.created events in one centrifuge call.
	_, err = h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		createdMsgs, createErr := primitives.BatchCreateMessages(txCtx, h.deps, sessionID, &turnID, messagesToCreate)
		if createErr != nil {
			return nil, fmt.Errorf("batch create messages: %w", createErr)
		}

		// Build one UpdatePublishItem with all message.created events merged.
		var allItems []protocol.UpdateItem
		for _, msg := range createdMsgs {
			allItems = append(allItems, protocol.UpdateItem{
				Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: protocol.UUID(msg.ID.String()),
			})
		}
		return []updates.UpdatePublishItem{{Channel: ch, Items: allItems}}, nil
	})
	if err != nil {
		return fmt.Errorf("handleMessage: %w", err)
	}

	return nil
}

// handleEventError processes an event-level error.
//
// These are errors attached to a single event in the agent's output stream,
// not turn-level failures (which go through FailTurn). The typical action
// is to log the error and continue; the turn may still complete successfully.
func (h *helpers) handleEventError(ctx context.Context, sessionID uuid.UUID, turnID uuid.UUID, event *turnagent.Event) error {
	errMsg := ""
	if event.Err != nil {
		errMsg = event.Err.Error()
	}
	h.logIfEnabled(ctx, "handleEventError", map[string]any{
		"session_id": sessionID.String(),
		"turn_id":    turnID.String(),
		"error":      errMsg,
	})
	return nil
}

// finalizeStreamMessage forces finalization of a streaming message that may
// not have received a FinishReason chunk. This mirrors the old
// finalizeStreamMessage in stream_message.go.
func (h *helpers) finalizeStreamMessage(
	ctx context.Context,
	sessionID uuid.UUID,
	turnID uuid.UUID,
	msgID *uuid.UUID,
	buildContent func(string) (protocol.ContentData, error),
	kind string,
) error {
	if *msgID == uuid.Nil {
		return nil
	}

	// Use a synthetic empty chunk with a FinishReason to trigger the
	// finalization path in appendStreamChunk.
	return h.appendStreamChunk(ctx, sessionID, turnID, "", "stream_finalize",
		msgID, new(bool), buildContent, kind)
}

// createAndPublishMessage creates a message record and publishes a
// message.created event atomically via RunAndPublish.
//
// Mirrors the old createAndPublishMessage in session_actor.go.
func (h *helpers) createAndPublishMessage(
	ctx context.Context,
	sessionID uuid.UUID,
	turnID uuid.UUID,
	role protocol.MessageRole,
	content protocol.ContentData,
	streamingStatus protocol.MessageStreamingStatus,
	topicCh string,
) (*model.Message, error) {
	var newMsg *model.Message
	_, err := h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		var createErr error
		newMsg, createErr = primitives.CreateMessage(
			txCtx, h.deps,
			sessionID, &turnID,
			role,
			usecase.SystemCreator{},
			content,
			streamingStatus,
			"",  // clientID — system-generated
			nil, // no parent message
		)
		if createErr != nil {
			return nil, fmt.Errorf("create message: %w", createErr)
		}

		return []updates.UpdatePublishItem{{
			Channel: topicCh,
			Items: []protocol.UpdateItem{
				{Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: protocol.UUID(newMsg.ID.String())},
			},
		}}, nil
	})
	if err != nil {
		return nil, err
	}
	return newMsg, nil
}

// =============================================================================
// Stream state tracking
// =============================================================================

// turnStreamState tracks the streaming message state for one turn.
// Each turn may have up to two concurrent streams: one for markdown content
// and one for thinking/reasoning content.
type turnStreamState struct {
	markdownMsgID     uuid.UUID
	markdownFinalized bool
	thinkingMsgID     uuid.UUID
	thinkingFinalized bool
}

// streamStateMap is a concurrent-safe map of turnID → *turnStreamState.
// It uses sync.Map for lock-free reads in the common case (each turn writes
// to its own key, so contention is minimal).
type streamStateMap struct {
	m sync.Map // map[string]*turnStreamState
}

// getOrCreate returns the existing state for turnID, or creates a new one.
func (m *streamStateMap) getOrCreate(turnID string) *turnStreamState {
	if v, ok := m.m.Load(turnID); ok {
		return v.(*turnStreamState)
	}
	state := &turnStreamState{}
	actual, _ := m.m.LoadOrStore(turnID, state)
	return actual.(*turnStreamState)
}

// remove cleans up the state for a turn.
func (m *streamStateMap) remove(turnID string) {
	m.m.Delete(turnID)
}

// =============================================================================
// Stream store helper
// =============================================================================

// getStreamStore returns a StreamStore for buffering streaming message chunks
// in Redis. Returns nil if the Redis client is not available.
//
// TODO: The StreamStore is currently defined in internal/worker/stream_store.go.
// It should be moved to a shared package so both worker and agent can use it.
// For now, we create a lightweight Redis-based chunk buffer inline.
// NewStreamStore creates a stream store accessor backed by Redis.
// The returned value satisfies updates.StreamStoreAccessor and can be injected
// into UpdatePublisher via SetStreamStore. It uses the same Redis key format
// (cache.MessageStream) as the stream buffering logic in this package.
func NewStreamStore(rdb redis.UniversalClient, chunkTTL time.Duration) *StreamStore {
	return &StreamStore{rdb: rdb, chunkTTL: chunkTTL}
}

func (h *helpers) getStreamStore() *StreamStore {
	return &StreamStore{rdb: h.rdb, chunkTTL: h.streamChunkTTL}
}

// StreamStore is a lightweight Redis-based buffer for streaming message
// chunks. It uses Redis Lists to append chunks atomically and read them all
// at finalization time.
//
// The stream store is also exposed via NewStreamStore for injection into
// UpdatePublisher, so that event publishing can resolve streaming messages
// by reading their buffered chunks.
type StreamStore struct {
	rdb      redis.UniversalClient
	chunkTTL time.Duration
}

// AppendChunk atomically appends a chunk to the Redis list and refreshes
// the TTL using the AppendChunk Lua script from cache.
// This replaces the previous non-atomic RPush + Expire two-command sequence.
func (s *StreamStore) AppendChunk(messageID string, chunk string) (int64, error) {
	key := cache.MessageStream(messageID)
	ttlSeconds := int(s.chunkTTL / time.Second)
	result, err := cache.AppendChunk.Run(
		context.Background(), s.rdb,
		[]string{key},
		ttlSeconds, chunk,
	).Int64()
	if err != nil {
		return 0, fmt.Errorf("append chunk: %w", err)
	}
	return result, nil
}

func (s *StreamStore) GetAllChunks(messageID string) ([]string, error) {
	key := cache.MessageStream(messageID)
	chunks, err := s.rdb.LRange(context.Background(), key, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("get all chunks: %w", err)
	}
	return chunks, nil
}

func (s *StreamStore) DeleteChunks(messageID string) error {
	key := cache.MessageStream(messageID)
	if err := s.rdb.Del(context.Background(), key).Err(); err != nil {
		return fmt.Errorf("delete chunks: %w", err)
	}
	return nil
}

// =============================================================================
// RTC tool types
// =============================================================================
//
// The RTC (Remote Tool Call) tools enable the LLM to request operations on the
// client's virtual filesystem (ls, read, write, grep, find, script). Each tool
// call creates a Message + RTC record in the DB, publishes updates to the
// frontend, and pauses the turn via tool.StatefulInterrupt. The client executes
// the operation in the browser and submits the result, which triggers a resume
// work item that re-enters the tool to retrieve the result.
//
// Mapping from old code: the old internal/worker/tools.go defined identical
// tool types (LSTool, ReadTool, etc.) that delegated to a BaseRTC. The new
// code defines them here in the agent package with the same delegation pattern.
// Tool extraction to a shared package (TODO 4) is deferred; the current
// implementation is fully functional within the agent package.

// rtcToolBase is the base for all RTC tools in the new agent package.
// It holds the session, helpers (for DB access), and the current turn ID.
type rtcToolBase struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

// rtcInterruptInfo is passed to StatefulInterrupt as the info parameter.
// It identifies the interrupt type and carries the RTC ID so the interrupt
// handler can route to the correct pub/sub channel.
// Must be gob-serializable (struct with exported fields).
//
// Mapping from old code: identical to rtcInterruptInfo in
// internal/worker/tools.rtc.go.
type rtcInterruptInfo struct {
	Type      string // "rtc"
	ToolName  string
	RtcID     string
	MessageID string
	Args      string // JSON-encoded tool arguments
}

// rtcInterruptState is passed to StatefulInterrupt as the state parameter.
// When the tool resumes from a checkpoint, it reads this state to find the
// RTC result.
// Must be gob-serializable (struct with exported fields).
//
// Mapping from old code: identical to rtcInterruptState in
// internal/worker/tools.rtc.go.
type rtcInterruptState struct {
	RtcID      string
	ToolCallID string
	MessageID  string
	ToolName   string
}

// Each tool's Info returns the tool metadata; InvokableRun delegates to
// rtcToolBase.InvokableRun which implements the full RTC tool logic
// (create Message + RTC record, publish updates, stateful interrupt).

// --- lsTool ---

type lsTool struct{ base *rtcToolBase }

func (l *lsTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "ls",
		Desc: "List directory contents at the given path",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"path": {Type: schema.String, Desc: "The directory path to list (defaults to current directory)", Required: false},
		}),
	}, nil
}

func (l *lsTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return l.base.InvokableRun(ctx, "ls", argumentsInJSON, opts...)
}

// --- readTool ---

type readTool struct{ base *rtcToolBase }

func (t *readTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "read",
		Desc: "Read file contents from the virtual filesystem",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"path":   {Type: schema.String, Desc: "The file path to read", Required: true},
			"offset": {Type: schema.Integer, Desc: "Byte offset to start reading from (default: 0)", Required: false},
			"limit":  {Type: schema.Integer, Desc: "Maximum number of bytes to read (default: unlimited)", Required: false},
		}),
	}, nil
}

func (t *readTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "read", argumentsInJSON, opts...)
}

// --- writeTool ---

type writeTool struct{ base *rtcToolBase }

func (t *writeTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "write",
		Desc: "Write or create a file in the virtual filesystem",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"path":    {Type: schema.String, Desc: "The file path to write to", Required: true},
			"content": {Type: schema.String, Desc: "The content to write", Required: true},
			"mode":    {Type: schema.String, Desc: "Write mode: 'overwrite' (default) or 'append'", Required: false},
		}),
	}, nil
}

func (t *writeTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "write", argumentsInJSON, opts...)
}

// --- grepTool ---

type grepTool struct{ base *rtcToolBase }

func (t *grepTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "grep",
		Desc: "Search for a pattern across file contents in the virtual filesystem",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"pattern":        {Type: schema.String, Desc: "Regex pattern to search for", Required: true},
			"path":           {Type: schema.String, Desc: "File or directory path to search in (default: root '/')", Required: false},
			"case_sensitive": {Type: schema.Boolean, Desc: "Whether the search is case-sensitive (default: false)", Required: false},
		}),
	}, nil
}

func (t *grepTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "grep", argumentsInJSON, opts...)
}

// --- findTool ---

type findTool struct{ base *rtcToolBase }

func (t *findTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "find",
		Desc: "Find files by name pattern in the virtual filesystem",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"pattern": {Type: schema.String, Desc: "Glob pattern to match file names (e.g. '*.ts', '**/*.go')", Required: true},
			"path":    {Type: schema.String, Desc: "Directory path to search in (default: root '/')", Required: false},
		}),
	}, nil
}

func (t *findTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "find", argumentsInJSON, opts...)
}

// --- scriptTool ---

type scriptTool struct{ base *rtcToolBase }

func (t *scriptTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "script",
		Desc: "Execute JavaScript code in the browser environment. Provide either a file path or inline code (mutually exclusive)",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"action": {
				Type:     schema.String,
				Enum:     []string{"run", "save", "eval"},
				Desc:     "The action to perform: 'save' persists the inline code to /scripts/{name}.ts (requires both 'code' and 'name'); 'run' executes a previously saved script by name (requires 'name'); 'eval' executes the inline code directly (requires 'code'). Defaults to 'eval' when omitted.",
				Required: false,
			},
			"name": {Type: schema.String, Desc: "The script name. Required for 'save' and 'run'.", Required: false},
			"code": {Type: schema.String, Desc: "Inline JavaScript code to execute", Required: false},
		}),
	}, nil
}

func (t *scriptTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "script", argumentsInJSON, opts...)
}

// InvokableRun implements the full RTC tool logic for all tool types.
//
// First-call path: creates a Message (type=toolcall_input) + RTC record in DB,
// publishes message.created and rtc.updated events, then calls
// tool.StatefulInterrupt to pause the turn and wait for client execution.
//
// Resume path: called again after checkpoint restore. Checks if the RTC record
// has reached a terminal state (completed/failed/timeout/rejected). If so,
// returns the formatted result. If not, re-interrupts with the same RTC ID.
//
// Mapping from old code: this is the direct equivalent of BaseRTC.InvokableRun
// in internal/worker/tools.rtc.go. Key differences:
//   - turnID is stored on rtcToolBase (set in createTools) instead of read
//     from context via getTurnUUID(ctx). The old code threaded turnID through
//     context because the tool was created once per session; the new code
//     creates tools per-turn so turnID is known at construction time.
//   - r.manager.deps -> r.helpers.deps (the integration struct is helpers, not Manager)
//   - Logger calls use h.logIfEnabled instead of logger.Debug/Info directly.
func (r *rtcToolBase) InvokableRun(ctx context.Context, toolName string, argumentsInJSON string, _ ...tool.Option) (string, error) {
	// === Resume path ===
	wasInterrupted, hasState, state := tool.GetInterruptState[rtcInterruptState](ctx)
	if wasInterrupted {
		if !hasState {
			return "", fmt.Errorf("rtc: state type mismatch on resume")
		}

		// Checkpoint recovery: check if the RTC already has a result in DB
		// (the client may have submitted it while the worker was down).
		rtcID, parseErr := uuid.Parse(state.RtcID)
		if parseErr != nil {
			return "", fmt.Errorf("rtc: invalid rtc_id in state: %w", parseErr)
		}
		dbRtc, dbErr := r.helpers.deps.RtcRepo.GetByID(ctx, rtcID)
		if dbErr != nil {
			// DB query failed: degrade to re-interrupt and wait for client to
			// re-submit. This matches the old code's fallback behavior.
			r.helpers.logIfEnabled(ctx, "rtcToolBase.resume.db_error", map[string]any{
				"rtc_id": rtcID.String(),
				"error":  dbErr.Error(),
			})
			info := rtcInterruptInfo{
				Type:      "rtc",
				ToolName:  state.ToolName,
				RtcID:     state.RtcID,
				MessageID: state.MessageID,
			}
			return "", tool.StatefulInterrupt(ctx, info, state)
		}

		// RTC reached terminal state -> return formatted result, no more interrupt.
		switch protocol.RtcStatus(dbRtc.Status) {
		case protocol.RtcStatusCompleted, protocol.RtcStatusFailed,
			protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
			toolOutput := string(dbRtc.Result)
			if dbRtc.Status == string(protocol.RtcStatusFailed) && dbRtc.ErrorMessage != "" {
				toolOutput = dbRtc.ErrorMessage
			}
			tc := protocol.ToolCall{
				Id:       protocol.UUID(state.ToolCallID),
				ToolName: dbRtc.ToolName,
				Output:   &toolOutput,
				Status:   &dbRtc.Status,
			}
			return formatToolCallOutput(tc), nil
		}

		// RTC not yet terminal (pending/sent/executing) -> re-interrupt with
		// same RTC ID so the tool waits again.
		info := rtcInterruptInfo{
			Type:      "rtc",
			ToolName:  state.ToolName,
			RtcID:     state.RtcID,
			MessageID: state.MessageID,
		}
		return "", tool.StatefulInterrupt(ctx, info, state)
	}

	// === First-call path ===
	// 1. Get tool_call_id (eino injects it into context before calling the tool).
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return "", fmt.Errorf("rtc: tool_call_id not set in context")
	}

	// 2. Turn ID is already known (stored on rtcToolBase at construction time).
	// In the old code, this was read from context via getTurnUUID(ctx).
	turnUUID := r.turnID
	if turnUUID == uuid.Nil {
		return "", fmt.Errorf("rtc: turn UUID is nil")
	}

	// 3. Generate RTC ID and ClientID.
	rtcID := uuid.Must(uuid.NewV7())
	clientID := uuid.Must(uuid.NewV7()).String()

	// 4. Build toolcall_input ContentData.
	toolCallData := protocol.ToolCall{
		Id:       protocol.UUID(callID),
		ToolName: toolName,
		Input:    argumentsInJSON,
	}
	contentData := protocol.ContentData{
		Type: protocol.ContentTypeToolCallInput,
		Data: toolCallData,
	}

	// 5. Transactionally create Message + RTC record + publish updates.
	var msgID uuid.UUID
	_, err := r.helpers.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		// Allocate RTC offset (reuses the session:msg_offset counter).
		rtcOffset, _, offsetErr := primitives.AllocateOffsets(txCtx, r.helpers.deps, r.session.ID, &turnUUID, 1)
		if offsetErr != nil {
			return nil, fmt.Errorf("allocate rtc offset: %w", offsetErr)
		}

		// Create Message (role=tool, type=toolcall_input).
		msg, createErr := primitives.CreateMessage(
			txCtx, r.helpers.deps,
			r.session.ID, &turnUUID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			contentData,
			protocol.MessageStreamingCompleted,
			"",  // system-generated
			nil, // no parent message
		)
		if createErr != nil {
			return nil, fmt.Errorf("create toolcall message: %w", createErr)
		}
		msgID = msg.ID

		// Create RTC record (MessageID points to the toolcall_input message).
		rtc := &model.Rtc{
			ID:         rtcID,
			ClientID:   clientID,
			SessionID:  r.session.ID,
			TurnID:     turnUUID,
			MessageID:  msgID,
			Offset:     rtcOffset,
			ToolName:   toolName,
			Parameters: model.JSONBString(argumentsInJSON),
			Status:     string(model.RtcStatusPending),
		}
		if err := r.helpers.deps.RtcRepo.Create(txCtx, rtc); err != nil {
			return nil, fmt.Errorf("create rtc: %w", err)
		}

		// Build publish updates (RTC status + toolcall_input message created).
		items := primitives.BuildRtcStatusUpdates(r.session, rtcID)
		if len(items) > 0 {
			items[0].Items = append(items[0].Items, protocol.UpdateItem{
				Entity:   protocol.EntityMessage,
				Action:   protocol.ActionCreated,
				EntityId: protocol.UUID(msgID.String()),
			})
		}
		return items, nil
	})
	if err != nil {
		return "", fmt.Errorf("create rtc and message: %w", err)
	}

	r.helpers.logIfEnabled(ctx, "rtcToolBase.rtc_created", map[string]any{
		"tool_name":  toolName,
		"rtc_id":     rtcID.String(),
		"message_id": msgID.String(),
		"turn_id":    turnUUID.String(),
	})

	// 6. Build interrupt state.
	state = rtcInterruptState{
		RtcID:      rtcID.String(),
		ToolCallID: callID,
		MessageID:  msgID.String(),
		ToolName:   toolName,
	}

	// 7. Build interrupt info.
	info := rtcInterruptInfo{
		Type:      "rtc",
		ToolName:  toolName,
		RtcID:     rtcID.String(),
		MessageID: msgID.String(),
		Args:      argumentsInJSON,
	}

	// 8. Pause the turn — eino will persist the checkpoint and return control
	// to turn-agent, which calls InterruptTurn.
	return "", tool.StatefulInterrupt(ctx, info, state)
}
