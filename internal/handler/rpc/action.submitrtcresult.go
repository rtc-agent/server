// internal/handler/rpc/action.submitrtcresult.go
package rpchandler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// SubmitRtcResult submits an RTC execution result, marks the RTC as completed, and continues the LLM flow.
func (h *Handler) SubmitRtcResult(ctx context.Context, req *protocol.SubmitRtcResultRequest) (*protocol.SubmitRtcResultResponse, error) {
	ctx, span := otel.GetTracerProvider().Tracer("rpc").Start(ctx, "rpc.submitRtcResult",
		trace.WithAttributes(
			attribute.String("rtc.id", req.RtcId),
			attribute.Bool("rtc.success", req.Success),
		),
	)
	defer span.End()

	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		span.SetStatus(codes.Error, "missing user_id in context")
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	creator := usecase.UserCreator{UserID: userID}

	rtcUUID, apiErr := parseUUID(req.RtcId, "rtc_id")
	if apiErr != nil {
		span.SetStatus(codes.Error, apiErr.Message)
		return nil, apiErr
	}

	span.SetAttributes(attribute.String("user.id", userID.String()))

	logger.Info(ctx, "[SubmitRtcResult]",
		zap.String("user", userID.String()),
		zap.String("rtc", req.RtcId),
		zap.Bool("success", req.Success))

	rtc, err := h.deps.Deps.RtcRepo.GetByID(ctx, rtcUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			span.SetStatus(codes.Error, "rtc.not_found")
			return nil, &APIError{Code: "rtc.not_found", Message: fmt.Sprintf("rtc %s not found", req.RtcId)}
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, h.internalError(ctx, "rtc.error", "internal error", err)
	}

	// Idempotency check for terminal-state RTCs.
	if idempotentResp, isIdempotent := h.checkIdempotentSubmit(ctx, rtc, req); isIdempotent {
		span.SetAttributes(attribute.Bool("rtc.idempotent", true))
		return idempotentResp, nil
	}

	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, rtc.SessionID, creator); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, h.ownershipError(ctx, err)
	}

	if req.ClientId != nil && *req.ClientId != rtc.ClientID {
		span.SetStatus(codes.Error, "rtc.client_id_mismatch")
		return nil, &APIError{
			Code:    "rtc.client_id_mismatch",
			Message: fmt.Sprintf("client_id mismatch: expected %s, got %s", rtc.ClientID, *req.ClientId),
		}
	}

	targetStatus := protocol.RtcStatusCompleted
	if !req.Success {
		targetStatus = protocol.RtcStatusFailed
	}

	var resultJSON *string
	if len(req.Result) > 0 {
		// req.Result 是 json.RawMessage，保留原始 JSON 字段顺序
		// 验证是否是合法 JSON
		if !json.Valid(req.Result) {
			span.SetStatus(codes.Error, "invalid JSON in result")
			return nil, &APIError{Code: "rtc.invalid_result", Message: "result is not valid JSON"}
		}
		// 转换为紧凑格式（移除多余空格），但保留 JSON key 的原始顺序
		// json.Compact 只移除空白字符，不改变 key 顺序
		var buf bytes.Buffer
		if err := json.Compact(&buf, req.Result); err != nil {
			span.SetStatus(codes.Error, "failed to compact result JSON")
			return nil, &APIError{Code: "rtc.invalid_result", Message: "failed to compact result JSON"}
		}
		s := buf.String()
		resultJSON = &s
	}

	// Pre-load session for event routing (best-effort, not critical).
	sessionBefore, sessionLoadErr := h.deps.SessionRepo.GetByID(ctx, rtc.SessionID)
	if sessionLoadErr != nil {
		logger.Warn(ctx, "[SubmitRtcResult] pre-load session failed (will use best-effort event publishing)",
			zap.String("session", rtc.SessionID.String()), zap.Error(sessionLoadErr))
	}

	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		return h.updateRtcAndCreateOutput(txCtx, rtc, rtcUUID, targetStatus, resultJSON, req.Error, sessionBefore)
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "[SubmitRtcResult] push failed after commit (data safe)", zap.Error(err))
		} else {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			return nil, h.internalError(ctx, "rtc.error", "internal error", err)
		}
	}

	if rtc.ToolName == "script" {
		// Use context.WithoutCancel because submit is async — the task may be
		// processed by a worker long after this RPC handler returns. The worker
		// will create its own timeout context for the DB operation.
		recorderCtx := context.WithoutCancel(ctx)
		h.recorder.submit(recorderCtx, rtc, req)
	}

	h.resumeTurnAfterRtc(ctx, rtc)

	return &protocol.SubmitRtcResultResponse{
		Result:  protocol.SubmitRtcResultResult{Success: true},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}

// checkIdempotentSubmit handles the idempotency check for RTCs already in a terminal state.
// Returns (response, true) if the request was handled idempotently, or (nil, false) to continue.
func (h *Handler) checkIdempotentSubmit(ctx context.Context, rtc *model.Rtc, req *protocol.SubmitRtcResultRequest) (*protocol.SubmitRtcResultResponse, bool) {
	switch protocol.RtcStatus(rtc.Status) {
	case protocol.RtcStatusCompleted, protocol.RtcStatusFailed,
		protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
		if req.ClientId != nil && *req.ClientId == rtc.ClientID {
			logger.Info(ctx, "[SubmitRtcResult] idempotent repeat",
				zap.String("rtc", req.RtcId),
				zap.String("status", rtc.Status),
				zap.String("client_id", rtc.ClientID))
			return &protocol.SubmitRtcResultResponse{
				Result: protocol.SubmitRtcResultResult{Success: true},
			}, true
		}
		ups := []protocol.Update{{
			DataList: new([]interface{}{model.ToProtocolRtc(rtc)}),
			Id:       uuid.Must(uuid.NewV7()).String(),
			Items: []protocol.UpdateItem{{
				Action:   protocol.ActionUpdated,
				Entity:   protocol.EntityRtc,
				EntityId: rtc.ID.String(),
			}},
			Offset: 0,
		}}
		return &protocol.SubmitRtcResultResponse{
			Result:  protocol.SubmitRtcResultResult{Success: true},
			Updates: &ups,
		}, true
	}
	return nil, false
}

// updateRtcAndCreateOutput performs the transactional update: marks the RTC result,
// creates the toolcall_output message, and builds update items for event publishing.
func (h *Handler) updateRtcAndCreateOutput(txCtx context.Context, rtc *model.Rtc, rtcUUID uuid.UUID, targetStatus protocol.RtcStatus, resultJSON *string, reqError *string, session *model.Session) ([]updates.UpdatePublishItem, error) {
	if err := primitives.UpdateRtcResult(txCtx, h.deps.Deps, rtcUUID, targetStatus, resultJSON, model.DerefStr(reqError)); err != nil {
		return nil, err
	}

	inputMsg, err := h.deps.Deps.MessageRepo.GetByID(txCtx, rtc.MessageID)
	if err != nil {
		return nil, fmt.Errorf("get toolcall_input message %s: %w", rtc.MessageID, err)
	}
	inputContentData, err := primitives.ParseContentData(inputMsg.Content)
	if err != nil {
		return nil, fmt.Errorf("parse toolcall_input content: %w", err)
	}
	inputToolCall, err := primitives.ParseContentDataToolCall(inputContentData.Data)
	if err != nil {
		return nil, fmt.Errorf("parse toolcall_input data: %w", err)
	}

	toolOutput := ""
	if resultJSON != nil {
		toolOutput = *resultJSON
	} else if reqError != nil {
		toolOutput = *reqError
	}
	targetStatusStr := string(targetStatus)
	outputContentData := protocol.ContentData{
		Type: protocol.ContentTypeToolCallOutput,
		Data: protocol.ToolCall{
			Id:       inputToolCall.Id,
			ToolName: rtc.ToolName,
			Input:    inputToolCall.Input,
			Output:   &toolOutput,
			Status:   &targetStatusStr,
		},
	}

	parentMsgID := rtc.MessageID
	outputMsg, createErr := primitives.CreateMessage(
		txCtx, h.deps.Deps,
		rtc.SessionID, &rtc.TurnID,
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

	if err := h.deps.Deps.RtcRepo.UpdateOutputMessageID(txCtx, rtcUUID, outputMsg.ID); err != nil {
		logger.Error(txCtx, "[SubmitRtcResult] update output_message_id",
			zap.String("rtc", rtcUUID.String()), zap.Error(err))
	}

	items := primitives.BuildRtcResultUpdates(session, rtcUUID)
	if len(items) > 0 {
		items[0].Items = append(items[0].Items, protocol.UpdateItem{
			Entity:   protocol.EntityMessage,
			Action:   protocol.ActionCreated,
			EntityId: outputMsg.ID.String(),
		})
	}
	return items, nil
}

// resumeTurnAfterRtc publishes a Resume work item to rtc-queue so the
// turn-agent can continue the interrupted turn from its eino checkpoint.
//
// With batch resume: if this RTC is part of a batch (multiple RTCs in the
// same turn), we wait until all RTCs in the batch complete before publishing
// a single Resume work item. This prevents checkpoint corruption from
// multiple concurrent interrupts.
//
// If the turn that created the RTC is no longer active (e.g. worker crash,
// turn already terminal), we publish a Submit work item instead. The turn
// will be created by turn-agent's CreateTurn callback when the worker
// processes the work item — the API layer does NOT pre-create the turn.
//
// Errors are logged but never returned — the RTC result has already been
// persisted, and a failure here just means the agent won't auto-continue.
//
// The function detaches from the caller's context: the RPC handler's
// context may be cancelled when the RPC returns, but the resume/submit
// operations (DB queries, Redis SetNX, Queue.Publish) must complete
// independently since they are fire-and-forget.
func (h *Handler) resumeTurnAfterRtc(callerCtx context.Context, rtc *model.Rtc) {
	// Detach from the RPC handler's context. The RTC result is already
	// persisted; the resume is fire-and-forget and must not be aborted
	// by the RPC context timeout/cancellation.
	// Add timeout to prevent goroutine leak if downstream operations hang.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), 30*time.Second)
	defer cancel()

	ctx, span := otel.GetTracerProvider().Tracer("rpc").Start(ctx, "rpc.resumeTurnAfterRtc",
		trace.WithAttributes(
			attribute.String("rtc.id", rtc.ID.String()),
			attribute.String("session.id", rtc.SessionID.String()),
			attribute.String("turn.id", rtc.TurnID.String()),
		),
	)
	defer span.End()

	if h.deps.Queue == nil {
		span.SetStatus(codes.Error, "queue_unavailable")
		logger.Warn(ctx, "[resumeTurnAfterRtc] Queue is nil, cannot resume",
			zap.String("rtc", rtc.ID.String()))
		return
	}

	if logger.IsDebugMode() {
		logger.Debug(ctx, "[resumeTurnAfterRtc] entry",
			zap.String("rtc", rtc.ID.String()),
			zap.String("session", rtc.SessionID.String()))
	}

	// === Batch Resume Logic ===
	batchResumeItems, batchDone := h.tryCollectBatchResume(ctx, rtc)
	if batchDone {
		// Batch still has pending RTCs; resume will happen when the last one completes.
		return
	}

	// Pre-check: skip triggering a new turn if session is already closed.
	session, err := h.deps.SessionRepo.GetByID(ctx, rtc.SessionID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		logger.Error(ctx, "[resumeTurnAfterRtc] get session",
			zap.String("session", rtc.SessionID.String()),
			zap.Error(err))
		return
	}
	if protocol.SessionStatus(session.Status) == protocol.SessionStatusClosed {
		span.SetAttributes(attribute.String("skip.reason", "session_closed"))
		logger.Info(ctx, "[resumeTurnAfterRtc] skip: session closed",
			zap.String("session", rtc.SessionID.String()))
		return
	}

	// Check if there's an active turn for this session.
	activeTurns, err := h.deps.Deps.TurnRepo.FindActiveBySession(ctx, rtc.SessionID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		logger.Error(ctx, "[resumeTurnAfterRtc] find active turns",
			zap.String("session", rtc.SessionID.String()),
			zap.Error(err))
		return
	}

	if len(activeTurns) > 0 {
		span.SetAttributes(attribute.Bool("resume.active_turn", true))
		h.resumeActiveTurn(ctx, rtc, activeTurns, batchResumeItems)
		return
	}

	// Orphan path: no active turn (worker crash or turn already terminal).
	span.SetAttributes(attribute.Bool("resume.orphan_submit", true))
	h.publishOrphanSubmit(ctx, rtc)
}
