// internal/rpchandler/action.stopturn.go
package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.uber.org/zap"
)

// StopTurn 停止正在执行的 Turn（取消 LLM 调用）。
// 停止 session 的所有活跃 turn（支持跨节点）。
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
		zap.String("session", string(req.SessionId)))

	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, sessionUUID, creator); err != nil {
		return nil, h.ownershipError(ctx, err)
	}

	// Stop all active turns asynchronously. The RPC returns immediately;
	// turn cancellation proceeds independently of the RPC context lifetime.
	// This mirrors CloseSession's approach: detaching from the RPC context
	// ensures multi-step cleanup (DB queries, Redis publish) is not aborted
	// by an early context cancellation (e.g. rpc_timeout).
	detachedCtx := context.WithoutCancel(ctx)
	logger.SafeGo("stop-active-turns", func() {
		h.stopActiveTurns(detachedCtx, sessionUUID)
	})

	return &protocol.StopTurnResponse{
		Result: protocol.StopTurnResult{Success: true},
	}, nil
}
