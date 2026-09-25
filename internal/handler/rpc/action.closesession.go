// internal/handler/rpc/action.closesession.go
package rpchandler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/loop"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/memory"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// CloseSession closes a session.
func (h *Handler) CloseSession(ctx context.Context, req *protocol.CloseSessionRequest) (*protocol.CloseSessionResponse, error) {
	ctx, span := otel.GetTracerProvider().Tracer("rpc").Start(ctx, "rpc.closeSession",
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

	logger.Info(ctx, "[CloseSession]", zap.String("user", userID.String()), zap.String("session", req.SessionId))

	session, err := h.deps.SessionRepo.GetByID(ctx, sessionUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			span.SetStatus(codes.Error, "session.not_found")
			return nil, &APIError{Code: "session.not_found", Message: fmt.Sprintf("session %s not found", req.SessionId)}
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, h.internalError(ctx, "session.error", "internal error", err)
	}
	if session.OwnerKind != string(creator.Kind()) || session.OwnerRefID != creator.ReferenceID() {
		span.SetStatus(codes.Error, "permission_denied")
		return nil, &APIError{Code: "permission_denied", Message: fmt.Sprintf("session %s does not belong to user", session.ID)}
	}
	if protocol.SessionStatus(session.Status) == protocol.SessionStatusClosed {
		logger.Info(ctx, "[CloseSession] session already closed", zap.String("session", session.ID.String()))
		return &protocol.CloseSessionResponse{
			Result: protocol.CloseSessionResult{Success: true},
		}, nil
	}

	// 1. Close the session and delete session memories.
	// Session memories are transient (used for context compression within a session).
	// Once the session is closed, they are no longer needed and should be cleaned up.
	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := primitives.UpdateSessionStatus(txCtx, h.deps.Deps, session.ID, protocol.SessionStatusClosed); err != nil {
			return nil, err
		}

		// Delete session memories (soft delete; they have served their purpose for compression).
		if err := h.deps.Deps.MemoryRepo.DeleteByScope(txCtx, memory.ScopeSession, session.ID); err != nil {
			// Non-fatal: log but don't block session close.
			logger.Warn(ctx, "[CloseSession] failed to delete session memories (non-fatal)",
				zap.String("session", session.ID.String()),
				zap.Error(err))
		}
		return primitives.BuildSessionCloseUpdates(session), nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "[CloseSession] push failed after commit (data safe)", zap.Error(err))
		} else {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
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
	// Add timeout inside the goroutine to prevent goroutine leak if downstream
	// operations hang. The timeout must be created inside the goroutine, not
	// here, because defer would cancel it when the RPC handler returns.
	detachedCtx := context.WithoutCancel(ctx)
	logger.SafeGo("stop-active-turns", func() {
		timeoutCtx, cancel := context.WithTimeout(detachedCtx, 30*time.Second)
		defer cancel()
		h.stopActiveTurns(timeoutCtx, session.ID)

		// Cleanup CommandRegistry activated entries for this session.
		// The CommandRegistry is a process-level singleton; its activated map
		// tracks per-session command state that would otherwise leak memory.
		// Only done on session close (not on StopTurn) since the session is
		// ending and no further turns will use the activated commands.
		if h.deps.Deps.CommandRegistry != nil {
			h.deps.Deps.CommandRegistry.CleanupSession(session.ID)
		}
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
//  4. Cancel active loops and goals (cleanup orphaned asynq tasks).
func (h *Handler) stopActiveTurns(ctx context.Context, sessionID uuid.UUID) {
	primitives.StopActiveTurns(ctx, h.deps.Deps, h.deps.Queue, sessionID, "stopped_by_user")

	// 3. Cancel active loops and goals for the session.
	// This ensures no orphaned asynq tasks remain after session close.
	loop.CancelAllForSession(ctx, h.deps.Deps.LoopRepo, h.deps.Deps.GoalRepo, h.deps.AsynqInspector, sessionID)
}
