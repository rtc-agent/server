// internal/handler/rpc/action.opensession.go
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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// OpenSession reopens a closed session (status: closed -> idle).
func (h *Handler) OpenSession(ctx context.Context, req *protocol.OpenSessionRequest) (*protocol.OpenSessionResponse, error) {
	ctx, span := otel.GetTracerProvider().Tracer("rpc").Start(ctx, "rpc.openSession",
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

	logger.Info(ctx, "[OpenSession]", zap.String("user", userID.String()), zap.String("session", req.SessionId))

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

	// Idempotent: if session is not closed, return success without modifying.
	if protocol.SessionStatus(session.Status) != protocol.SessionStatusClosed {
		logger.Info(ctx, "[OpenSession] session not closed, skipping", zap.String("session", session.ID.String()), zap.String("status", session.Status))
		return &protocol.OpenSessionResponse{
			Result: protocol.OpenSessionResult{Success: true},
		}, nil
	}

	// Reopen the session: status -> idle, clear closed_at.
	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := primitives.UpdateSessionStatus(txCtx, h.deps.Deps, session.ID, protocol.SessionStatusIdle); err != nil {
			return nil, err
		}

		// Clear closed_at so the session is fully reopened.
		if err := h.deps.Deps.SessionRepo.Update(txCtx, session.ID, map[string]any{"closed_at": nil}); err != nil {
			return nil, fmt.Errorf("clear closed_at: %w", err)
		}

		// Update in-memory session for event publishing.
		session.Status = string(protocol.SessionStatusIdle)
		session.ClosedAt = nil

		return primitives.BuildSessionUpdatedUpdates(session), nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "[OpenSession] push failed after commit (data safe)", zap.Error(err))
		} else {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			return nil, h.internalError(ctx, "open.error", "internal error", err)
		}
	}

	return &protocol.OpenSessionResponse{
		Result:  protocol.OpenSessionResult{Success: true},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}
