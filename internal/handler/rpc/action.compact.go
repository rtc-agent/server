// internal/handler/rpc/action.compact.go
package rpchandler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// CompactSession triggers an explicit context compression for a session.
// The compact work item is enqueued with SubmitWorkPriority (same as submit).
//
// Dedup: if a compact work item is already pending or processing for this
// session, the request is rejected with "compact.already_pending".
func (h *Handler) CompactSession(ctx context.Context, req *protocol.CompactSessionRequest) (*protocol.CompactSessionResponse, error) {
	ctx, span := otel.GetTracerProvider().Tracer("rpc").Start(ctx, "rpc.compact",
		trace.WithAttributes(
			attribute.String("session.id", req.SessionId),
		),
	)
	defer span.End()

	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		span.SetStatus(codes.Error, "missing user_id in context")
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	creator := usecase.UserCreator{UserID: userID}

	sessionUUID, apiErr := parseUUID(req.SessionId, "session_id")
	if apiErr != nil {
		span.SetStatus(codes.Error, apiErr.Message)
		return nil, apiErr
	}

	span.SetAttributes(attribute.String("user.id", userID.String()))

	logger.Info(ctx, "[CompactSession]",
		zap.String("user", userID.String()),
		zap.String("session", req.SessionId))

	// 1. Verify session existence.
	session, err := h.deps.SessionRepo.GetByID(ctx, sessionUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			span.SetStatus(codes.Error, "session.not_found")
			return nil, &APIError{
				Code:    "session.not_found",
				Message: fmt.Sprintf("session %s not found", req.SessionId),
			}
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, h.internalError(ctx, "session.error", "internal error", err)
	}

	// 2. Permission check.
	if session.OwnerKind != string(creator.Kind()) || session.OwnerRefID != creator.ReferenceID() {
		span.SetStatus(codes.Error, "permission_denied")
		return nil, &APIError{
			Code:    "permission_denied",
			Message: fmt.Sprintf("session %s does not belong to user", session.ID),
		}
	}

	// 3. Verify queue availability.
	if h.deps.Queue == nil {
		span.SetStatus(codes.Error, "compact.queue_unavailable")
		return nil, h.internalError(ctx, "compact.queue_unavailable", "compact service unavailable", nil)
	}

	// 4. Dedup: check if a compact task for this session is already in the queue.
	hasPending, checkErr := h.deps.Queue.HasPendingWorkByKind(ctx, session.ID.String(), string(turnagent.WorkKindCompact))
	if checkErr != nil {
		span.SetStatus(codes.Error, checkErr.Error())
		span.RecordError(checkErr)
		return nil, h.internalError(ctx, "compact.dedup_error", "internal error", checkErr)
	}
	if hasPending {
		span.SetStatus(codes.Error, "compact.already_pending")
		return nil, &APIError{
			Code:    "compact.already_pending",
			Message: "a compact task is already pending or processing for this session",
		}
	}

	// 5. Build payload and enqueue.
	// Extract trace context for cross-process propagation.
	traceID, spanID := turnagent.ExtractTraceFromCtx(ctx)
	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:              turnagent.WorkKindCompact,
		SessionID:         session.ID.String(),
		CustomInstruction: req.CustomInstruction,
		TraceID:           traceID,
		SpanID:            spanID,
	})
	if marshalErr != nil {
		span.SetStatus(codes.Error, marshalErr.Error())
		span.RecordError(marshalErr)
		return nil, h.internalError(ctx, "compact.marshal_error", "internal error", marshalErr)
	}

	if _, err := h.deps.Queue.Publish(ctx, session.ID.String(), string(payload), rtcqueue.SubmitWorkPriority); err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, h.internalError(ctx, "compact.queue_error", "failed to enqueue compact task", err)
	}

	logger.Info(ctx, "[CompactSession] enqueued",
		zap.String("session", session.ID.String()),
		zap.Bool("has_custom_instruction", req.CustomInstruction != nil))

	return &protocol.CompactSessionResponse{
		Result: protocol.CompactSessionResult{Success: true},
	}, nil
}
