// internal/rpchandler/action.stopturn.go
package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/infra/context"
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

	// Debug: trace the full stop request
	if logger.DebugMode {
		logger.Debug(ctx, "[StopTurn] stack_trace", zap.String("trace", logger.CaptureStack(0)))
	}

	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, sessionUUID, creator); err != nil {
		return nil, &APIError{Code: "permission_denied", Message: err.Error()}
	}

	// 停止 session 的所有活跃 turn（复用 CloseSession 的逻辑）
	h.stopActiveTurns(ctx, sessionUUID)

	return &protocol.StopTurnResponse{
		Result: protocol.StopTurnResult{Success: true},
	}, nil
}
