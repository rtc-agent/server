// internal/rpchandler/action.stopturn.go
package rpchandler

import (
	"context"
	"time"

	"github.com/rtc-agent/server/internal/infra/contextx"
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

// StopTurn stops a running Turn (cancels the LLM call).
// Stops all active turns for the session (supports cross-node operation).
func (h *Handler) StopTurn(ctx context.Context, req *protocol.StopTurnRequest) (*protocol.StopTurnResponse, error) {
	ctx, span := otel.GetTracerProvider().Tracer("rpc").Start(ctx, "rpc.stopTurn",
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

	logger.Info(ctx, "[StopTurn]",
		zap.String("user", userID.String()),
		zap.String("session", req.SessionId))

	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, sessionUUID, creator); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, h.ownershipError(ctx, err)
	}

	// Stop all active turns asynchronously. The RPC returns immediately;
	// turn cancellation proceeds independently of the RPC context lifetime.
	// This mirrors CloseSession's approach: detaching from the RPC context
	// ensures multi-step cleanup (DB queries, Redis publish) is not aborted
	// by an early context cancellation (e.g. rpc_timeout).
	// Add timeout inside the goroutine to prevent goroutine leak if downstream
	// operations hang. The timeout must be created inside the goroutine, not
	// here, because defer would cancel it when the RPC handler returns.
	detachedCtx := context.WithoutCancel(ctx)
	logger.SafeGo("stop-active-turns", func() {
		timeoutCtx, cancel := context.WithTimeout(detachedCtx, 30*time.Second)
		defer cancel()
		h.stopActiveTurns(timeoutCtx, sessionUUID)
	})

	return &protocol.StopTurnResponse{
		Result: protocol.StopTurnResult{Success: true},
	}, nil
}
