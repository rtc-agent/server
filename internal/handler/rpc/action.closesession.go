// internal/rpchandler/action.closesession.go
package rpchandler

import (
	"context"
	"errors"
	"fmt"

	"github.com/rtc-agent/server/internal/channel"
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

	// Debug: trace close entry
	if logger.DebugMode {
		logger.Debug(ctx, "[CloseSession] stack_trace", zap.String("stack", logger.CaptureStack(0)))
	}

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
	// If synchronous behavior is needed in the future, this can be changed
	// to block until Queue.CancelSession completes. The trade-off is higher
	// API latency vs. stronger consistency guarantees on close.
	go h.stopActiveTurns(ctx, session.ID)

	return &protocol.CloseSessionResponse{
		Result:  protocol.CloseSessionResult{Success: true},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}

// stopActiveTurns stops all pending/running/interrupted turns for a session.
// This is a best-effort operation called asynchronously after session close
// or synchronously from StopTurn.
//
// New architecture:
//  1. Cancel all pending/processing work via rtc-queue CancelSession (clears
//     the queue and notifies any worker currently processing).
//  2. Query active turns BEFORE the bulk DB update (so we have turn IDs to
//     publish events for).
//  3. Mark remaining active turns as cancelled in the DB (belt-and-suspenders
//     for races where a turn was created but not yet published to rtc-queue).
//  4. Publish turn.updated events for each cancelled turn so the frontend
//     learns that the turns are no longer active. Without this, the frontend
//     would show stale "running"/"pending" state for the cancelled turns.
func (h *Handler) stopActiveTurns(ctx context.Context, sessionID uuid.UUID) {
	// 1. Cancel all pending/processing work via rtc-queue
	if h.deps.Queue != nil {
		if err := h.deps.Queue.CancelSession(ctx, sessionID.String(), "stopped_by_user"); err != nil {
			logger.Error(ctx, "[stopActiveTurns] cancel session failed", zap.String("session", sessionID.String()), zap.Error(err))
		}
	}

	// 2. Query active turns BEFORE updating (so we can publish events for each)
	activeTurns, err := h.deps.Deps.TurnRepo.FindActiveBySession(ctx, sessionID)
	if err != nil {
		logger.Error(ctx, "[stopActiveTurns] find active turns failed", zap.String("session", sessionID.String()), zap.Error(err))
		return
	}

	// 3. Mark remaining active turns as cancelled in DB (belt-and-suspenders)
	if affected, err := h.deps.Deps.TurnRepo.UpdateStatusBySession(
		ctx, sessionID,
		[]string{"pending", "running", "interrupted"},
		"cancelled",
	); err != nil {
		logger.Error(ctx, "[stopActiveTurns] update turns status failed", zap.String("session", sessionID.String()), zap.Error(err))
	} else if affected > 0 {
		logger.Info(ctx, "[stopActiveTurns] cancelled turns", zap.Int("affected", int(affected)), zap.String("session", sessionID.String()))
	}

	// 4. Publish turn.updated events for all cancelled turns in a single batch.
	// The frontend uses these events to clear stale turn state.
	// Batching: N active turns previously caused N separate Publish calls;
	// now all updates are merged into one UpdatePublishItem and published once,
	// reducing Redis offset increments, DB inserts, and centrifuge round-trips.
	if len(activeTurns) == 0 || h.deps.Deps.UpdatePublisher == nil {
		return
	}

	// Load session once for all events (need OwnerRefID for routing).
	session, sessErr := h.deps.Deps.SessionRepo.GetByID(ctx, sessionID)
	if sessErr != nil {
		logger.Error(ctx, "[stopActiveTurns] load session failed", zap.String("session", sessionID.String()), zap.Error(sessErr))
		return
	}

	// Collect all turn.updated UpdateItems into a single UpdatePublishItem.
	var allItems []protocol.UpdateItem
	for _, turn := range activeTurns {
		turnUpdates := primitives.BuildTurnUpdatedUpdates(session, turn.ID)
		for _, u := range turnUpdates {
			allItems = append(allItems, u.Items...)
		}
	}
	if len(allItems) == 0 {
		return
	}

	ch := channel.UserTopic(session.OwnerRefID)
	merged := []updates.UpdatePublishItem{{Channel: ch, Items: allItems}}
	if _, err := h.deps.Deps.UpdatePublisher.Publish(ctx, merged...); err != nil {
		logger.Error(ctx, "[stopActiveTurns] batch publish turn updates failed", zap.String("session", sessionID.String()), zap.Int("turn_count", len(activeTurns)), zap.Error(err))
	}
}
