package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/converter"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// GetSession retrieves a single session owned by the current user.
func (h *Handler) GetSession(ctx context.Context, req *protocol.GetSessionRequest) (*protocol.GetSessionResponse, error) {
	ctx, span := otel.GetTracerProvider().Tracer("rpc").Start(ctx, "rpc.get_session",
		trace.WithAttributes(
			attribute.String("session.id", req.SessionId),
		),
	)
	defer span.End()

	userID, err := h.requireUserID(ctx)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.String("user.id", userID.String()))

	sessionUUID, apiErr := parseUUID(req.SessionId, "session_id")
	if apiErr != nil {
		return nil, apiErr
	}

	session, err := h.loadOwnedSession(ctx, sessionUUID, userID)
	if err != nil {
		return nil, err
	}

	logger.Info(ctx, "[GetSession]",
		zap.String("user", userID.String()),
		zap.String("session", req.SessionId))
	return &protocol.GetSessionResponse{
		Item: converter.ToProtocolSession(session),
	}, nil
}
