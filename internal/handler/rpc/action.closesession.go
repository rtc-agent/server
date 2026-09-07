// internal/handler/rpc/action.closesession.go
package rpchandler

import (
	"context"
	"errors"
	"fmt"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// CloseSession 关闭会话。
func (h *Handler) CloseSession(ctx context.Context, req *protocol.CloseSessionRequest) (*protocol.CloseSessionResponse, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	creator := usecase.UserCreator{UserID: userID}

	sessionUUID, apiErr := parseUUID(req.SessionId, "session_id")
	if apiErr != nil {
		return nil, apiErr
	}

	logger.Info(ctx, "[CloseSession]", zap.String("user", userID.String()), zap.String("session", string(req.SessionId)))

	session, err := h.deps.SessionRepo.GetByID(ctx, sessionUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{Code: "session.not_found", Message: fmt.Sprintf("session %s not found", req.SessionId)}
		}
		return nil, h.internalError(ctx, "session.error", "internal error", err)
	}
	if session.OwnerKind != string(creator.Kind()) || session.OwnerRefID != creator.ReferenceID() {
		return nil, &APIError{Code: "permission_denied", Message: fmt.Sprintf("session %s does not belong to user", session.ID)}
	}
	if protocol.SessionStatus(session.Status) == protocol.SessionStatusClosed {
		logger.Info(ctx, "[CloseSession] session already closed", zap.String("session", session.ID.String()))
		return &protocol.CloseSessionResponse{
			Result: protocol.CloseSessionResult{Success: true},
		}, nil
	}

	// 1. Close the session
	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := primitives.UpdateSessionStatus(txCtx, h.deps.Deps, session.ID, protocol.SessionStatusClosed); err != nil {
			return nil, err
		}
		return primitives.BuildSessionCloseUpdates(session), nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "[CloseSession] push failed after commit (data safe)", zap.Error(err))
		} else {
			return nil, h.internalError(ctx, "close.error", "internal error", err)
		}
	}

	// 2. Stop any active turns (best-effort, failures are OK).
	// Another process (worker reaper, idle timeout) will handle cleanup if this fails.
	//
	// stopActiveTurns is called ASYNCHRONOUSLY. The API returns immediately,
	// and the turns are stopped in the background. This is intentional — the
	// user doesn't need to wait for all turns to stop before the session
	// close response is returned.
	//
	// Detach from the RPC handler's context: it will be cancelled when the
	// RPC returns (or when rpc_timeout fires), but stopActiveTurns performs
	// multi-step cleanup (DB queries, Redis publish) that must complete
	// independently of the RPC lifetime.
	//
	// If synchronous behavior is needed in the future, this can be changed
	// to block until Queue.CancelSession completes. The trade-off is higher
	// API latency vs. stronger consistency guarantees on close.
	detachedCtx := context.WithoutCancel(ctx)
	logger.SafeGo("stop-active-turns", func() {
		h.stopActiveTurns(detachedCtx, session.ID)
	})

	return &protocol.CloseSessionResponse{
		Result:  protocol.CloseSessionResult{Success: true},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}

// stopActiveTurns stops all pending/running/interrupted turns for a session.
// Delegates to primitives.StopActiveTurns which handles:
//  1. Cancel all pending/processing work via rtc-queue CancelSession.
//  2. Query active turns and mark them cancelled in DB.
//  3. Publish turn.updated events for each cancelled turn.
func (h *Handler) stopActiveTurns(ctx context.Context, sessionID uuid.UUID) {
	primitives.StopActiveTurns(ctx, h.deps.Deps, h.deps.Queue, sessionID, "stopped_by_user")
}
