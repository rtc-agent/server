package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Error Handling Conventions
//
// All tool execution functions in this package follow these error handling rules:
//
//  1. NEVER use panic for expected error conditions — return error instead.
//  2. NEVER silently ignore errors — at minimum, log with logger.Error/Logger.Warn.
//  3. Use errors.Is() for error comparison, not direct == (supports wrapped errors).
//  4. For non-critical operations, use log+degrade pattern:
//     log the error and continue with degraded functionality.
//  5. For goroutines, use logger.SafeGo() or add defer/recover to prevent
//     a single goroutine panic from crashing the process.
//  6. JSON marshal/unmarshal errors must always be checked.
//  7. Template rendering errors must be returned, not swallowed.
//  8. uuid.Parse and similar parsing errors must be checked and returned.

// saddExpireScript atomically adds a member to a set and sets the key TTL.
// This prevents the TOCTOU race where a process crash between SADD and EXPIRE
// leaves a key without TTL (permanent persistence).
//
// KEYS[1] = set key
// ARGV[1] = member to add
// ARGV[2] = TTL in seconds
// Returns: 1 on success.
var saddExpireScript = redis.NewScript(`
redis.call("SADD", KEYS[1], ARGV[1])
redis.call("EXPIRE", KEYS[1], tonumber(ARGV[2]))
return 1
`)

// Each tool's Info returns the tool metadata; InvokableRun delegates to
// rtcToolBase.InvokableRun which implements the full RTC tool logic
// (create Message + RTC record, publish updates, stateful interrupt).

// --- lsTool ---

type lsTool struct{ base *rtcToolBase }

func (t *lsTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "ls",
		Desc: lsDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"path": {Type: schema.String, Desc: "The directory path to list (defaults to current directory)", Required: false},
		}),
	}, nil
}

func (t *lsTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "ls", argumentsInJSON, opts...)
}

// --- readTool ---

type readTool struct{ base *rtcToolBase }

func (t *readTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "read",
		Desc: readDesc,
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
		Desc: writeDesc,
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
		Desc: grepDesc,
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
		Desc: findDesc,
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
		Desc: scriptDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"title": {
				Type:     schema.String,
				Desc:     "简短描述本次脚本执行的目的，不超过20个字，如 '分析销售数据趋势'",
				Required: true,
			},
			"action": {
				Type:     schema.String,
				Enum:     []string{"run", "save", "eval"},
				Desc:     "The action to perform: 'save' persists the inline code to /scripts/{name}.ts (requires both 'code' and 'name'); 'run' executes a previously saved script by name (requires 'name'); 'eval' executes the inline code directly (requires 'code'). Defaults to 'eval' when omitted.",
				Required: false,
			},
			"name": {Type: schema.String, Desc: "The script name. Required for 'save' and 'run'.", Required: false},
			"code": {Type: schema.String, Desc: "Inline JavaScript code to execute. Must not contain infinite loops (while, do...while, for(;;)); use for...of, for...in, or Array iteration methods instead.", Required: false},
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
//   - Logger calls use h.logger.Info instead of logger.Debug/Info directly.
func (r *rtcToolBase) InvokableRun(ctx context.Context, toolName string, argumentsInJSON string, _ ...tool.Option) (string, error) {
	ctx, span := r.helpers.tracer.Start(ctx, "rtcTool."+toolName,
		trace.WithAttributes(
			attribute.String("session_id", r.session.ID.String()),
			attribute.String("turn_id", r.turnID.String()),
			attribute.String("tool_name", toolName),
			attribute.Int("args_length", len(argumentsInJSON)),
		),
	)
	defer span.End()

	// === Resume path ===
	wasInterrupted, hasState, state := tool.GetInterruptState[rtcInterruptState](ctx)
	if wasInterrupted {
		if !hasState {
			span.RecordError(fmt.Errorf("state type mismatch"))
			span.SetStatus(codes.Error, "state_type_mismatch")
			return "", fmt.Errorf("rtc: state type mismatch on resume")
		}
		span.SetAttributes(attribute.Bool("resume", true))
		result, err := r.handleRtcResume(ctx, state)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "resume_failed")
		}
		return result, err
	}

	// === First-call path ===
	result, err := r.handleRtcFirstCall(ctx, toolName, argumentsInJSON)
	// handleRtcFirstCall always returns an error (the interrupt), so record it unconditionally.
	span.RecordError(err)
	span.SetStatus(codes.Error, "first_call_interrupted")
	return result, err
}

// handleRtcResume handles the resume path for RTC tools: checks if the RTC
// has reached terminal state, formats the result, or re-interrupts.
func (r *rtcToolBase) handleRtcResume(ctx context.Context, state rtcInterruptState) (string, error) {
	rtcID, parseErr := uuid.Parse(state.RtcID)
	if parseErr != nil {
		return "", fmt.Errorf("rtc: invalid rtc_id in state: %w", parseErr)
	}

	dbRtc, dbErr := r.helpers.deps.RtcRepo.GetByID(ctx, rtcID)
	if dbErr != nil {
		r.helpers.logger.Warn(ctx, "rtcToolBase.resume.db_error", map[string]any{
			"rtc_id": rtcID.String(),
			"error":  dbErr.Error(),
		})
		info := rtcInterruptInfo{Type: "rtc", ToolName: state.ToolName, RtcID: state.RtcID, MessageID: state.MessageID}
		return "", tool.StatefulInterrupt(ctx, info, state)
	}

	switch protocol.RtcStatus(dbRtc.Status) {
	case protocol.RtcStatusCompleted, protocol.RtcStatusFailed,
		protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
		var toolOutput string
		if r.formatResult != nil {
			toolOutput = r.formatResult(dbRtc)
		} else {
			// 方案 C：从 output message (TEXT 列) 读取工具结果，而不是从 rtcs.result (JSONB 列)
			// 这确保 resume 路径和 loadMessages 路径使用完全相同的数据源，避免 PostgreSQL JSONB 规范化差异
			if dbRtc.OutputMessageID != nil {
				outputMsg, err := r.helpers.deps.MessageRepo.GetByID(ctx, *dbRtc.OutputMessageID)
				if err == nil && outputMsg != nil {
					toolCall, parseErr := primitives.ParseContentDataToolCallRaw(outputMsg.Content)
					if parseErr == nil && toolCall.Output != nil {
						toolOutput = *toolCall.Output
					} else {
						// Fallback: 解析失败，使用 rtcs.result
						toolOutput = string(dbRtc.Result)
					}
				} else {
					// Fallback: output message 不存在，使用 rtcs.result
					toolOutput = string(dbRtc.Result)
				}
			} else {
				// Fallback: OutputMessageID 不存在（旧数据），使用 rtcs.result
				toolOutput = string(dbRtc.Result)
			}

			if dbRtc.Status == string(protocol.RtcStatusFailed) && dbRtc.ErrorMessage != "" {
				toolOutput = dbRtc.ErrorMessage
			}
		}
		tc := protocol.ToolCall{
			Id:       state.ToolCallID,
			ToolName: dbRtc.ToolName,
			Output:   &toolOutput,
			Status:   &dbRtc.Status,
		}
		return formatToolCallOutput(tc), nil
	}

	// RTC not yet terminal: re-interrupt.
	info := rtcInterruptInfo{Type: "rtc", ToolName: state.ToolName, RtcID: state.RtcID, MessageID: state.MessageID}
	return "", tool.StatefulInterrupt(ctx, info, state)
}

// handleRtcFirstCall handles the first-call path for RTC tools: creates the
// RTC record, message, and triggers the interrupt.
func (r *rtcToolBase) handleRtcFirstCall(ctx context.Context, toolName, argumentsInJSON string) (string, error) {
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return "", fmt.Errorf("rtc: tool_call_id not set in context")
	}

	turnUUID := r.turnID
	if turnUUID == uuid.Nil {
		return "", fmt.Errorf("rtc: turn UUID is nil")
	}

	rtcID := uuid.Must(uuid.NewV7())
	clientID := uuid.Must(uuid.NewV7()).String()

	toolCallData := protocol.ToolCall{
		Id:       callID,
		ToolName: toolName,
		Input:    argumentsInJSON,
	}
	contentData := protocol.ContentData{
		Type: protocol.ContentTypeToolCallInput,
		Data: toolCallData,
	}

	msgID, err := r.createRtcAndMessage(ctx, rtcID, clientID, turnUUID, toolName, argumentsInJSON, contentData)
	if err != nil {
		return "", err
	}

	r.registerRtcBatch(ctx, rtcID, turnUUID)

	state := rtcInterruptState{
		RtcID:      rtcID.String(),
		ToolCallID: callID,
		MessageID:  msgID.String(),
		ToolName:   toolName,
	}
	info := rtcInterruptInfo{
		Type:      "rtc",
		ToolName:  toolName,
		RtcID:     rtcID.String(),
		MessageID: msgID.String(),
		Args:      argumentsInJSON,
	}
	return "", tool.StatefulInterrupt(ctx, info, state)
}

// createRtcAndMessage transactionally creates a Message and RTC record.
func (r *rtcToolBase) createRtcAndMessage(ctx context.Context, rtcID uuid.UUID, clientID string, turnUUID uuid.UUID, toolName, argumentsInJSON string, contentData protocol.ContentData) (uuid.UUID, error) {
	var msgID uuid.UUID
	_, err := r.helpers.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		rtcOffset, offsetErr := primitives.AllocateRtcOffset(txCtx, r.helpers.deps, r.session.ID)
		if offsetErr != nil {
			return nil, fmt.Errorf("allocate rtc offset: %w", offsetErr)
		}

		msg, createErr := primitives.CreateMessage(
			txCtx, r.helpers.deps,
			r.session.ID, &turnUUID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			contentData,
			protocol.MessageStreamingCompleted,
			"", nil,
		)
		if createErr != nil {
			return nil, fmt.Errorf("create toolcall message: %w", createErr)
		}
		msgID = msg.ID

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

		items := primitives.BuildRtcStatusUpdates(r.session, rtcID)
		if len(items) > 0 {
			items[0].Items = append(items[0].Items, protocol.UpdateItem{
				Entity:   protocol.EntityMessage,
				Action:   protocol.ActionCreated,
				EntityId: msgID.String(),
			})
		}
		return items, nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			r.helpers.logger.Info(ctx, "rtcToolBase.push_after_commit", map[string]any{"error": err.Error()})
		} else {
			return uuid.Nil, fmt.Errorf("create rtc and message: %w", err)
		}
	}

	r.helpers.logger.Info(ctx, "rtcToolBase.rtc_created", map[string]any{
		"tool_name":  toolName,
		"rtc_id":     rtcID.String(),
		"message_id": msgID.String(),
		"turn_id":    turnUUID.String(),
	})

	if toolName == "script" {
		r.logScriptStart(ctx, rtcID, turnUUID, argumentsInJSON)
	}

	return msgID, nil
}

// logScriptStart logs the start of a script execution for monitoring.
func (r *rtcToolBase) logScriptStart(ctx context.Context, rtcID, turnUUID uuid.UUID, argumentsInJSON string) {
	var scriptArgs struct {
		Title  string `json:"title"`
		Action string `json:"action"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal([]byte(argumentsInJSON), &scriptArgs); err != nil {
		logger.Warn(ctx, "script.parse_args_failed",
			zap.String("rtc_id", rtcID.String()),
			zap.Error(err),
		)
	}
	logger.Info(ctx, "script.execution_started",
		zap.String("rtc_id", rtcID.String()),
		zap.String("session_id", r.session.ID.String()),
		zap.String("turn_id", turnUUID.String()),
		zap.String("title", scriptArgs.Title),
		zap.String("action", scriptArgs.Action),
		zap.String("name", scriptArgs.Name),
	)
}

// registerRtcBatch registers the RTC in the batch pending set for batch resume.
func (r *rtcToolBase) registerRtcBatch(ctx context.Context, rtcID, turnUUID uuid.UUID) {
	if r.helpers.deps.Redis == nil {
		return
	}
	batchKey := cache.RtcBatchPending(turnUUID.String())
	if err := saddExpireScript.Run(ctx, r.helpers.deps.Redis, []string{batchKey}, rtcID.String(), int(10*time.Minute/time.Second)).Err(); err != nil {
		r.helpers.logger.Warn(ctx, "rtcToolBase.batch_register_failed", map[string]any{
			"rtc_id":  rtcID.String(),
			"turn_id": turnUUID.String(),
			"error":   err.Error(),
		})
	}
}

// parseToolArgs safely parses JSON tool arguments.
//
// On failure it returns (false, friendlyMessage) instead of a Go error.
// This is critical: returning an error from InvokableRun causes Eino's
// ToolNode to wrap it as a NodeRunError and terminate the entire turn.
// By returning a message, the LLM sees the parse failure and can retry
// with correct arguments.
//
// The caller should check the bool: if false, return the string directly
// from InvokableRun (with nil error).
func parseToolArgs(ctx context.Context, h *helpers, toolName string, argumentsInJSON string, args any) (ok bool, errorMsg string) {
	if err := json.Unmarshal([]byte(argumentsInJSON), args); err != nil {
		// Truncate arguments for logging to avoid flooding logs with large payloads.
		argPreview := argumentsInJSON
		const maxPreviewLen = 200
		if len(argPreview) > maxPreviewLen {
			argPreview = argPreview[:maxPreviewLen] + "...(truncated)"
		}
		h.logger.Warn(ctx, "tool.parse_arguments_failed", map[string]any{
			"tool_name":   toolName,
			"error":       err.Error(),
			"raw_length":  len(argumentsInJSON),
			"raw_preview": argPreview,
		})
		return false, formatParseError(err.Error(), argPreview)
	}
	return true, ""
}

// parseToolArgsWithPersist safely parses JSON tool arguments and persists
// parse errors to the database as toolcall_output messages.
//
// This is critical for cache consistency: when parseToolArgs fails, the error
// message must be persisted to DB so that checkpoint resume (HistoryModifier)
// produces the same message structure as the original execution.
//
// Without this, the error message exists only in eino's in-memory state during
// the current turn, but disappears when the turn is resumed from checkpoint
// (because DB doesn't have the error record), causing cache invalidation.
func parseToolArgsWithPersist(
	ctx context.Context,
	h *helpers,
	sessionID uuid.UUID,
	ownerRefID string,
	turnID uuid.UUID,
	toolName string,
	argumentsInJSON string,
	args any,
) (ok bool, errorMsg string) {
	if err := json.Unmarshal([]byte(argumentsInJSON), args); err != nil {
		// Truncate arguments for logging to avoid flooding logs with large payloads.
		argPreview := argumentsInJSON
		const maxPreviewLen = 200
		if len(argPreview) > maxPreviewLen {
			argPreview = argPreview[:maxPreviewLen] + "...(truncated)"
		}
		h.logger.Warn(ctx, "tool.parse_arguments_failed", map[string]any{
			"tool_name":   toolName,
			"error":       err.Error(),
			"raw_length":  len(argumentsInJSON),
			"raw_preview": argPreview,
		})
		errMsg := formatParseError(err.Error(), argPreview)

		// 持久化错误消息到 DB，确保 checkpoint resume 时消息结构一致
		// 这对 LLM 缓存命中至关重要
		if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
			Helpers:         h,
			SessionID:       sessionID,
			OwnerRefID:      ownerRefID,
			TurnID:          turnID,
			ToolName:        toolName,
			ArgumentsInJSON: argumentsInJSON,
			ResultJSON:      errMsg,
		}); publishErr != nil {
			h.logger.Warn(ctx, "tool.persist_parse_error_failed", map[string]any{
				"tool_name": toolName,
				"error":     publishErr.Error(),
			})
		}

		return false, errMsg
	}
	return true, ""
}
