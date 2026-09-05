package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.uber.org/zap"
)

// GetSession 获取当前用户拥有的单个会话详情。
func (h *Handler) GetSession(ctx context.Context, req *protocol.GetSessionRequest) (*protocol.GetSessionResponse, error) {
	userID, err := h.requireUserID(ctx)
	if err != nil {
		return nil, err
	}

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
		zap.String("session", string(req.SessionId)))
	return &protocol.GetSessionResponse{
		Item: model.ToProtocolSession(session),
	}, nil
}
