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
	"go.uber.org/zap"
)

// StopTurn stops a running Turn (cancels the LLM call).
// Stops all active turns for the session (supports cross-node operation).
func (h *Handler) StopTurn(ctx context.Context, req *protocol.StopTurnRequest) (*protocol.StopTurnResponse, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	creator := usecase.UserCreator{UserID: userID}

	sessionUUID, apiErr := parseUUID(req.SessionId, "session_id")
	if apiErr != nil {
		return nil, apiErr
	}

	logger.Info(ctx, "[StopTurn]",
		zap.String("user", userID.String()),
		zap.String("session", req.SessionId))

	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, sessionUUID, creator); err != nil {
		return nil, h.ownershipError(ctx, err)
	}

	// Stop all active turns asynchronously. The RPC returns immediately;
	// turn cancellation proceeds independently of the RPC context lifetime.
	// This mirrors CloseSession's approach: detaching from the RPC context
	// ensures multi-step cleanup (DB queries, Redis publish) is not aborted
	// by an early context cancellation (e.g. rpc_timeout).
	// Add timeout to prevent goroutine leak if downstream operations hang.
	detachedCtx, detachedCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer detachedCancel()
	logger.SafeGo("stop-active-turns", func() {
		h.stopActiveTurns(detachedCtx, sessionUUID)
	})

	return &protocol.StopTurnResponse{
		Result: protocol.StopTurnResult{Success: true},
	}, nil
}
